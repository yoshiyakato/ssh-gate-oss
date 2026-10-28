package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHeadlessBrowserOperations is the REQ-015 acceptance check that a
// browser can do the four jobs of the GUI against a real --headless process,
// using only what the shim uses (POST /api/call/<Method> + GET /api/events
// with the session cookie and the CSRF header):
//
//   - connection edit: SaveConnection / TestConnection / GetInitialData
//   - settings: GetMCPSettings / SaveMCPSettings
//   - approval: an agent queues a command over MCP, ApproveCommand runs it
//   - terminal: StartTerminal / SendTerminalInput, output on the SSE stream
//
// It also logs in with the admin password (the optional credential), so the
// login page's {"password"} path is covered end to end.
func TestHeadlessBrowserOperations(t *testing.T) {
	dir := isolateEnv(t)
	webPort := wireFreePort(t)
	mcpPort := wireFreePort(t)
	const mcpBearer = "mcp-bearer-token-for-ops-0123456789"
	t.Setenv("SSH_GETE_MCP_ADDR", "127.0.0.1:"+strconv.Itoa(mcpPort))
	t.Setenv("SSH_GETE_TOKEN", mcpBearer)

	const adminPassword = "correct horse battery staple"
	if err := setAdminPasswordFile(filepath.Join(dir, adminPasswordFileName), adminPassword, nil); err != nil {
		t.Fatal(err)
	}

	srv := newSSHTestServer(t)

	var out, errOut bytes.Buffer
	started := make(chan struct{})
	deps := headlessHooks
	deps.stdout = &syncWriter{w: &out}
	deps.stderr = &syncWriter{w: &errOut}
	deps.signals = func() (<-chan os.Signal, func()) {
		close(started)
		return make(chan os.Signal), func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	opts := CLIOptions{Headless: true, DataDir: dir, Listen: "127.0.0.1:" + strconv.Itoa(webPort)}
	go func() { done <- runHeadlessWith(ctx, opts, embeddedDistForTest(t), deps) }()
	select {
	case <-started:
	case code := <-done:
		t.Fatalf("exit %d before start; stderr:\n%s", code, errOut.String())
	case <-time.After(20 * time.Second):
		t.Fatal("startup timed out")
	}
	defer func() {
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Errorf("exit code %d; stderr:\n%s", code, errOut.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("shutdown timed out")
		}
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(webPort)
	jar, _ := cookiejar.New(nil)
	client := withSessionKey(&http.Client{Jar: jar, Timeout: 30 * time.Second})
	api := map[string]string{"Content-Type": "application/json", "X-SSH-Gete-CSRF": "1", "Origin": base}
	post := func(path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		for k, v := range api {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	// call is what the shim's window.go.main.App.<Method>(...) does.
	call := func(method string, args ...any) json.RawMessage {
		t.Helper()
		if args == nil {
			args = []any{}
		}
		body, _ := json.Marshal(args)
		code, b := post("/api/call/"+method, string(body))
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", method, code, b)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		if err := json.Unmarshal(b, &env); err != nil || env.Error != "" {
			t.Fatalf("%s: %s (%v)", method, b, err)
		}
		return env.Result
	}
	mustOK := func(method string, args ...any) ActionResult {
		t.Helper()
		var r ActionResult
		if err := json.Unmarshal(call(method, args...), &r); err != nil || !r.OK {
			t.Fatalf("%s: %+v (%v)", method, r, err)
		}
		return r
	}

	// Login with the admin password; the session sees passwordSet.
	if code, b := post("/api/login", `{"password":"wrong password here"}`); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d %s", code, b)
	}
	if code, b := post("/api/login", `{"password":"`+adminPassword+`"}`); code != http.StatusNoContent {
		t.Fatalf("password login: %d %s", code, b)
	}

	// Open the event stream first, the way the shim does, and collect it.
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	sreq, _ := http.NewRequestWithContext(sctx, "GET", base+"/api/events", nil)
	sreq.Header.Set("X-SSH-Gete-CSRF", "1")
	sres, err := (&http.Client{Jar: jar, Transport: client.Transport}).Do(sreq)
	if err != nil || sres.StatusCode != http.StatusOK {
		t.Fatalf("events: %v %v", sres, err)
	}
	defer sres.Body.Close()
	termOut := make(chan string, 256)
	go func() {
		defer close(termOut)
		sc := bufio.NewScanner(sres.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var w wireEvent
			if json.Unmarshal([]byte(line[6:]), &w) != nil || w.N != terminalDataEvent {
				continue
			}
			var p terminalDataPayload
			if json.Unmarshal(w.D, &p) != nil {
				continue
			}
			if raw, err := base64.StdEncoding.DecodeString(p.Data); err == nil {
				select {
				case termOut <- string(raw):
				default:
				}
			}
		}
	}()

	// Connection edit: create, trust on first use, rename, read back.
	conn := srv.Connection("web-ops")
	saved := mustOK("SaveConnection", conn)
	if saved.ID == "" {
		t.Fatal("SaveConnection returned no id")
	}
	conn.ID = saved.ID
	mustOK("TestConnection", conn.ID)
	conn.Description = "edited from the browser"
	conn.Credential = "" // write-only: empty keeps the stored password
	mustOK("SaveConnection", conn)
	var dash DashboardData
	if err := json.Unmarshal(call("GetInitialData"), &dash); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range dash.Connections {
		if c.ID == conn.ID {
			found = c.Description == "edited from the browser" && c.Credential == "" && c.CredentialSet
		}
	}
	if !found {
		t.Fatalf("edited connection not in GetInitialData: %+v", dash.Connections)
	}

	// Settings: change a value and read it back.
	var settings MCPSettings
	if err := json.Unmarshal(call("GetMCPSettings"), &settings); err != nil {
		t.Fatal(err)
	}
	settings.DefaultCommandTimeout = 42
	mustOK("SaveMCPSettings", settings)
	var again MCPSettings
	if err := json.Unmarshal(call("GetMCPSettings"), &again); err != nil || again.DefaultCommandTimeout != 42 {
		t.Fatalf("settings not saved: %+v (%v)", again.DefaultCommandTimeout, err)
	}

	// Approval: an agent queues a command over MCP; the browser approves it.
	mcpReq, _ := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(mcpPort)+"/mcp",
		strings.NewReader(toolCall("request_command_execution",
			`{"host":"web-ops","command":"sudo systemctl restart nginx","reason":"ops test","agent_name":"ops-agent"}`)))
	mcpReq.Header.Set("Content-Type", "application/json")
	mcpReq.Header.Set("Accept", "application/json, text/event-stream")
	mcpReq.Header.Set("Authorization", "Bearer "+mcpBearer)
	mres, err := (&http.Client{Timeout: 30 * time.Second}).Do(mcpReq)
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := io.ReadAll(mres.Body)
	mres.Body.Close()
	if mres.StatusCode != http.StatusOK {
		t.Fatalf("mcp request: %d %s", mres.StatusCode, mb)
	}
	var pending CommandRequest
	deadline := time.Now().Add(10 * time.Second)
	for pending.ID == "" && time.Now().Before(deadline) {
		if err := json.Unmarshal(call("GetInitialData"), &dash); err != nil {
			t.Fatal(err)
		}
		for _, r := range dash.Requests {
			if r.RequestedBy == "ops-agent" && r.Status == statePending {
				pending = r
			}
		}
		if pending.ID == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if pending.ID == "" {
		t.Fatalf("queued request not visible to the browser: %s", mb)
	}
	var ar ActionResult
	if err := json.Unmarshal(call("ApproveCommand", pending.ID, pending.Command), &ar); err != nil {
		t.Fatal(err)
	}
	if !ar.OK && ar.Code == "risk_increased" {
		ar = mustOK("ApproveCommandConfirmed", pending.ID, pending.Command)
	} else if !ar.OK {
		t.Fatalf("ApproveCommand: %+v", ar)
	}
	deadline = time.Now().Add(15 * time.Second)
	ran := false
	for !ran && time.Now().Before(deadline) {
		for _, c := range srv.Execs() {
			if strings.Contains(c, "systemctl restart nginx") {
				ran = true
			}
		}
		if !ran {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !ran {
		t.Fatalf("approved command never reached the server: %v", srv.Execs())
	}

	// Terminal: open a tab, type, and read the echo from the event stream.
	const termID = "TERM-web-ops"
	if r := mustOK("StartTerminal", termID, conn.ID, 80, 24); r.SessionID != termID {
		t.Fatalf("StartTerminal session = %q", r.SessionID)
	}
	mustOK("SendTerminalInput", termID, "echo from-the-browser\r")
	var seen strings.Builder
	timeout := time.After(15 * time.Second)
	for !strings.Contains(seen.String(), "from-the-browser\r\n") {
		select {
		case s, ok := <-termOut:
			if !ok {
				t.Fatalf("event stream ended; terminal output so far %q", seen.String())
			}
			seen.WriteString(s)
		case <-timeout:
			t.Fatalf("terminal echo not on the event stream; got %q", seen.String())
		}
	}
	mustOK("CloseTerminal", termID)
}
