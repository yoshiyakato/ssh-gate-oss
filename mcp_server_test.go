package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestApp isolates the app from the developer's machine: config dir, home
// (keys, known_hosts), ssh-agent and SSH_GETE_* overrides.
func newTestApp(t *testing.T) *App {
	t.Helper()
	isolateEnv(t)
	app := NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	return app
}

func isolateEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SSH_GETE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("SSH_GETE_TOKEN", "")
	t.Setenv("SSH_GETE_MCP_ADDR", "")
	t.Setenv("SSH_GETE_STRICT_HOSTKEY", "")
	t.Setenv("SSH_GETE_NO_SEED", "")
	return dir
}

// addTestConnection saves conn through the public API and returns it with ID.
func addTestConnection(t *testing.T, app *App, conn Connection) Connection {
	t.Helper()
	result := app.SaveConnection(conn)
	if !result.OK {
		t.Fatalf("save connection: %s", result.Message)
	}
	saved, ok := app.connectionByID(result.ID)
	if !ok {
		t.Fatalf("saved connection %s not found", result.ID)
	}
	return saved
}

type mcpCall struct {
	method      string
	path        string
	body        string
	host        string
	origin      string
	contentType string
	auth        string
}

func (app *App) serveTest(t *testing.T, c mcpCall) *httptest.ResponseRecorder {
	t.Helper()
	app.mu.RLock()
	settings := app.mcp
	token := app.effectiveTokenLocked()
	app.mu.RUnlock()
	if c.method == "" {
		c.method = http.MethodPost
	}
	if c.path == "" {
		c.path = settings.MCPPath
	}
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	req.Host = "127.0.0.1:" + strconv.Itoa(settings.Port)
	if c.host != "" {
		req.Host = c.host
	}
	if c.contentType == "" && c.method == http.MethodPost {
		c.contentType = "application/json"
	}
	if c.contentType != "-" && c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	rec := httptest.NewRecorder()
	app.newMCPHandler(settings, token).ServeHTTP(rec, req)
	return rec
}

func toolCall(name string, args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

// callTool runs a tools/call and returns structuredContent plus isError.
func callTool(t *testing.T, app *App, name string, args string) (map[string]any, bool) {
	t.Helper()
	rec := app.serveTest(t, mcpCall{body: toolCall(name, args)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			StructuredContent map[string]any `json:"structuredContent"`
			IsError           bool           `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	return resp.Result.StructuredContent, resp.Result.IsError
}

func enableBearer(t *testing.T, app *App) string {
	t.Helper()
	result := app.GenerateMCPToken()
	if result.Token == "" {
		t.Fatalf("token not generated: %s", result.Message)
	}
	return result.Token
}

func TestMCPHTTPServerHealth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	app := newTestApp(t)
	app.mcp.ListenAddress = "127.0.0.1"
	app.mcp.Port = port
	app.mcp.Enabled = true
	if err := app.startMCPServer(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestGuardBlocksBrowserAttackPaths(t *testing.T) {
	app := newTestApp(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	port := strconv.Itoa(app.mcp.Port)
	cases := []struct {
		name string
		call mcpCall
		want int
	}{
		{"ok", mcpCall{body: body}, http.StatusOK},
		{"localhost host", mcpCall{body: body, host: "localhost:" + port}, http.StatusOK},
		{"text/plain simple request", mcpCall{body: body, contentType: "text/plain"}, http.StatusUnsupportedMediaType},
		{"no content type", mcpCall{body: body, contentType: "-"}, http.StatusUnsupportedMediaType},
		{"foreign origin", mcpCall{body: body, origin: "https://evil.example"}, http.StatusForbidden},
		{"null origin", mcpCall{body: body, origin: "null"}, http.StatusForbidden},
		{"localhost page origin", mcpCall{body: body, origin: "http://localhost:3000"}, http.StatusForbidden},
		{"dns rebinding host", mcpCall{body: body, host: "evil.example:" + port}, http.StatusForbidden},
		{"wrong port", mcpCall{body: body, host: "127.0.0.1:1"}, http.StatusForbidden},
		{"audit with origin", mcpCall{method: http.MethodGet, path: "/audit/events", origin: "https://evil.example"}, http.StatusForbidden},
		{"preflight from page", mcpCall{method: http.MethodOptions, origin: "https://evil.example"}, http.StatusForbidden},
		{"get on mcp", mcpCall{method: http.MethodGet}, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := app.serveTest(t, tc.call)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestBearerProtectsMCPAndAudit(t *testing.T) {
	app := newTestApp(t)
	token := enableBearer(t, app)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if rec := app.serveTest(t, mcpCall{body: body}); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("mcp without token: %d", rec.Code)
	}
	if rec := app.serveTest(t, mcpCall{method: http.MethodGet, path: "/audit/events"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("audit without token: %d", rec.Code)
	}
	if rec := app.serveTest(t, mcpCall{body: body, auth: "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("mcp with wrong token: %d", rec.Code)
	}
	if rec := app.serveTest(t, mcpCall{body: body, auth: "Bearer " + token}); rec.Code != http.StatusOK {
		t.Fatalf("mcp with token: %d", rec.Code)
	}
	if rec := app.serveTest(t, mcpCall{method: http.MethodGet, path: "/audit/events", auth: "Bearer " + token}); rec.Code != http.StatusOK {
		t.Fatalf("audit with token: %d", rec.Code)
	}
}

func TestJSONRPCConformance(t *testing.T) {
	app := newTestApp(t)
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return out
	}

	rec := app.serveTest(t, mcpCall{body: `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`})
	if out := decode(rec); out["error"].(map[string]any)["code"].(float64) != -32600 {
		t.Fatalf("batch: %s", rec.Body.String())
	}

	rec = app.serveTest(t, mcpCall{body: `{not json`})
	out := decode(rec)
	if _, hasID := out["id"]; !hasID || out["id"] != nil || out["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatalf("parse error must carry id:null: %s", rec.Body.String())
	}

	rec = app.serveTest(t, mcpCall{body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`})
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", rec.Code, rec.Body.String())
	}

	rec = app.serveTest(t, mcpCall{body: `{"jsonrpc":"2.0","id":"p","method":"ping"}`})
	if out := decode(rec); out["id"] != "p" || out["error"] != nil {
		t.Fatalf("ping: %s", rec.Body.String())
	}

	rec = app.serveTest(t, mcpCall{body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`})
	if v := decode(rec)["result"].(map[string]any)["protocolVersion"]; v != latestProtocolVersion {
		t.Fatalf("unsupported version must negotiate to latest, got %v", v)
	}
}

func TestMCPToolsList(t *testing.T) {
	app := newTestApp(t)
	rec := app.serveTest(t, mcpCall{body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`})
	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range resp.Result.Tools {
		names[tool["name"].(string)] = true
	}
	for _, want := range []string{"list_hosts", "execute_command", "request_command_execution", "get_request_status", "get_command_history"} {
		if !names[want] {
			t.Fatalf("tool %s missing: %v", want, names)
		}
	}
}

func TestMCPRequestCommandExecutionQueues(t *testing.T) {
	app := newTestApp(t)
	out, isErr := callTool(t, app, "request_command_execution",
		`{"host":"ローカル確認用","command":"sudo systemctl restart nginx","reason":"restart requested","agent_name":"codex-test"}`)
	if isErr || out["state"] != statePending || out["risk"] != "High" {
		t.Fatalf("unexpected result: %v", out)
	}
	data := app.GetInitialData()
	if len(data.Requests) != 1 || data.Requests[0].RequestedBy != "codex-test" || data.Requests[0].ConnectionID == "" {
		t.Fatalf("unexpected requests: %+v", data.Requests)
	}
}

func TestMCPCommandRequiresAgentName(t *testing.T) {
	app := newTestApp(t)
	rec := app.serveTest(t, mcpCall{body: toolCall("execute_command", `{"host":"ローカル確認用","command":"whoami"}`)})
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Message != "agent_name is required" {
		t.Fatalf("unexpected rpc response: %+v", resp)
	}
	if len(app.GetInitialData().Requests) != 0 {
		t.Fatalf("anonymous command was queued")
	}
}

func TestMCPHostErrors(t *testing.T) {
	app := newTestApp(t)
	addTestConnection(t, app, Connection{Name: "uart", Type: "Serial", SerialPort: "/dev/ttyUSB0", BaudRate: 115200})
	addTestConnection(t, app, Connection{Name: "a", Host: "10.0.0.5", User: "u"})
	addTestConnection(t, app, Connection{Name: "b", Host: "10.0.0.5", User: "u"})

	out, isErr := callTool(t, app, "execute_command", `{"host":"uart","command":"whoami","agent_name":"x"}`)
	if !isErr || out["error_kind"] != "serial_not_supported" {
		t.Fatalf("serial: %v", out)
	}
	out, isErr = callTool(t, app, "execute_command", `{"host":"nope","command":"whoami","agent_name":"x"}`)
	if !isErr || out["error_kind"] != "host_not_found" {
		t.Fatalf("not found: %v", out)
	}
	out, isErr = callTool(t, app, "execute_command", `{"host":"10.0.0.5","command":"whoami","agent_name":"x"}`)
	if !isErr || out["error_kind"] != "ambiguous_host" {
		t.Fatalf("ambiguous IP must not pick a connection: %v", out)
	}
	if len(app.GetInitialData().Requests) != 0 {
		t.Fatalf("nothing should be queued")
	}
}

func queueOn(t *testing.T, app *App, conn Connection, command string, agent string) CommandRequest {
	t.Helper()
	req, err := app.queueCommand(conn, command, "test", agent, classifyRisk(command), "")
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestConcurrentApprovalExecutesOnce(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo hello", "agent")

	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if app.ApproveCommand(req.ID, "").OK {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got := len(srv.Execs()); got != 1 || okCount != 1 {
		t.Fatalf("execs=%d ok=%d, want exactly one", got, okCount)
	}
	final, _ := app.findRequest(req.ID)
	if final.Status != stateSucceeded || final.ExitCode == nil || *final.ExitCode != 0 || !strings.Contains(final.Stdout, "hello") {
		t.Fatalf("unexpected final request: %+v", final)
	}
	if final.DecidedBy != decidedByGUI || final.DecidedAt == "" || final.ExecutedCommand != "echo hello" {
		t.Fatalf("approval not audited: %+v", final)
	}
}

func TestRejectAndApproveAreExclusive(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))

	rejected := queueOn(t, app, conn, "echo a", "agent")
	if !app.RejectCommand(rejected.ID).OK {
		t.Fatal("reject failed")
	}
	if r := app.ApproveCommand(rejected.ID, ""); r.OK || r.Code != "not_pending" {
		t.Fatalf("approving a rejected request: %+v", r)
	}

	approved := queueOn(t, app, conn, "echo b", "agent")
	if !app.ApproveCommand(approved.ID, "").OK {
		t.Fatal("approve failed")
	}
	if r := app.RejectCommand(approved.ID); r.OK {
		t.Fatalf("rejecting an executed request must fail")
	}
	got, _ := app.findRequest(approved.ID)
	if got.Status != stateSucceeded {
		t.Fatalf("executed request was overwritten: %s", got.Status)
	}
	if n := len(srv.Execs()); n != 1 {
		t.Fatalf("execs=%d", n)
	}
}

func TestBypassOnProcessedRequestDoesNotGrantPolicy(t *testing.T) {
	app := newTestApp(t)
	enableBearer(t, app)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo x", "sneaky")
	app.RejectCommand(req.ID)
	if r := app.ApproveCommandAndBypassAgent(req.ID, ""); r.OK {
		t.Fatalf("bypass on rejected request succeeded")
	}
	if app.agentApprovalBypassEnabled("sneaky") {
		t.Fatalf("policy was granted although approval failed")
	}
}

func TestBypassRequiresBearer(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo x", "claude-desktop")
	if r := app.ApproveCommandAndBypassAgent(req.ID, ""); r.OK || r.Code != "bearer_required" {
		t.Fatalf("bypass without bearer: %+v", r)
	}
	// Even a stored policy is ignored while Bearer is off.
	app.setAgentApprovalBypass("claude-desktop", true)
	out, _ := callTool(t, app, "execute_command", `{"host":"lab","command":"echo hi","agent_name":"claude-desktop"}`)
	if out["state"] != statePending {
		t.Fatalf("name-only agent must be queued: %v", out)
	}
	if len(srv.Execs()) != 0 {
		t.Fatalf("command ran without approval")
	}
}

func TestBypassPolicyLimits(t *testing.T) {
	app := newTestApp(t)
	token := enableBearer(t, app)
	srv := newSSHTestServer(t)
	addTestConnection(t, app, srv.Connection("lab"))
	prod := srv.Connection("prod-db")
	prod.Tags = []string{"prod"}
	addTestConnection(t, app, prod)
	app.setAgentApprovalBypass("trusted", true)

	call := func(host string, command string) map[string]any {
		rec := app.serveTest(t, mcpCall{auth: "Bearer " + token, body: toolCall("execute_command",
			`{"host":"`+host+`","command":"`+command+`","agent_name":"trusted"}`)})
		var resp struct {
			Result struct {
				StructuredContent map[string]any `json:"structuredContent"`
			} `json:"result"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Result.StructuredContent
	}
	if out := call("lab", "echo ok"); out["state"] != stateAutoSucceeded || out["exit_code"] != float64(0) {
		t.Fatalf("low risk should auto-run: %v", out)
	}
	if out := call("lab", "rm -rf /tmp/x"); out["state"] != statePending {
		t.Fatalf("high risk must queue: %v", out)
	}
	if out := call("lab", "sudo echo x"); out["state"] != statePending {
		t.Fatalf("sudo must queue: %v", out)
	}
	if out := call("prod-db", "echo ok"); out["state"] != statePending {
		t.Fatalf("prod tag must queue: %v", out)
	}
	if out := call("lab", "exit 3"); out["state"] != stateAutoFailed || out["exit_code"] != float64(3) || out["error_kind"] != "exit_nonzero" {
		t.Fatalf("non-zero exit: %v", out)
	}
}

func TestRequestStatusLongPoll(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo polled", "agent")

	go func() {
		time.Sleep(200 * time.Millisecond)
		app.ApproveCommand(req.ID, "")
	}()
	start := time.Now()
	out, _ := callTool(t, app, "get_request_status", `{"request_ids":["`+req.ID+`"],"wait_seconds":10}`)
	items := out["requests"].([]any)
	got := items[0].(map[string]any)
	if got["state"] != stateSucceeded || !strings.Contains(got["stdout"].(string), "polled") {
		t.Fatalf("unexpected status: %v", got)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("long-poll did not wake on change")
	}
}

func TestClientRequestIDIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	args := `{"host":"ローカル確認用","command":"whoami","agent_name":"a","client_request_id":"k1"}`
	first, _ := callTool(t, app, "execute_command", args)
	second, _ := callTool(t, app, "execute_command", args)
	if first["request_id"] != second["request_id"] || second["deduplicated"] != true {
		t.Fatalf("retry created a new request: %v / %v", first, second)
	}
	if n := len(app.GetInitialData().Requests); n != 1 {
		t.Fatalf("requests=%d", n)
	}
}

func TestHandshakeDeadline(t *testing.T) {
	isolateEnv(t)
	port := newSilentServer(t)
	conn := Connection{Host: "127.0.0.1", Port: port, User: "u", AuthMethod: "パスワード保管", Credential: "p", ConnectTimeout: 1}
	start := time.Now()
	err := testSSHConnection(context.Background(), conn, false)
	if err == nil {
		t.Fatal("expected handshake failure")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("handshake hung for %s", elapsed)
	}
}

func TestCommandTimeoutWithStreamingOutput(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")
	conn.CommandTimeout = 1
	conn = addTestConnection(t, app, conn)
	result, err := app.executeSSH(context.Background(), conn, "yes", 0)
	if err == nil || classifyExecError(err) != "timeout" {
		t.Fatalf("expected timeout, got %v", err)
	}
	if !strings.HasSuffix(result.Stdout, "[ssh-gete: output truncated]") {
		t.Fatalf("expected truncated output marker")
	}
}

func TestInterruptedRequestsRecoveredOnRestart(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	conn := app.conns[0]
	req := queueOn(t, app, conn, "echo x", "agent")
	if _, err := app.transitionRequest(req.ID, []string{statePending}, func(r *CommandRequest) { r.Status = stateRunning }); err != nil {
		t.Fatal(err)
	}
	app.shutdown(context.Background())

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	got, _ := reloaded.findRequest(req.ID)
	if got.Status != stateInterrupted {
		t.Fatalf("running request after restart: %s", got.Status)
	}
	if r := reloaded.ApproveCommand(req.ID, ""); r.OK {
		t.Fatalf("interrupted request must not be re-approvable")
	}
}

func TestSQLitePersistsCommandRequests(t *testing.T) {
	configDir := isolateEnv(t)
	app := NewApp()
	req := queueOn(t, app, app.conns[0], "sudo systemctl restart nginx", "local-agent")
	app.RejectCommand(req.ID)
	app.shutdown(context.Background())

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if len(reloaded.requests) != 1 || reloaded.requests[0].ID != req.ID || reloaded.requests[0].Status != stateRejected {
		t.Fatalf("unexpected persisted requests: %+v", reloaded.requests)
	}
	if !strings.Contains(reloaded.requests[0].RequestedAt, "T") {
		t.Fatalf("requestedAt must carry the date: %q", reloaded.requests[0].RequestedAt)
	}
	if reloaded.dbPath != filepath.Join(configDir, "ssh-gete.sqlite") {
		t.Fatalf("unexpected db path: %s", reloaded.dbPath)
	}
	if info, err := os.Stat(reloaded.dbPath); err == nil && info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("db file permissions too open: %v", info.Mode().Perm())
	}
}

func TestRapidCommandRequestsPersistDistinctly(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	const n = 50
	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		req := queueOn(t, app, app.conns[0], "whoami", "codex-local")
		if ids[req.ID] {
			t.Fatalf("duplicate request ID generated: %s", req.ID)
		}
		ids[req.ID] = true
	}
	app.shutdown(context.Background())

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if len(reloaded.requests) != n {
		t.Fatalf("persisted requests=%d, want %d", len(reloaded.requests), n)
	}
}

func TestConnectionRenameAndDuplicateName(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	conn := addTestConnection(t, app, Connection{Name: "web", Host: "10.0.0.1", User: "u"})
	other := addTestConnection(t, app, Connection{Name: "db", Host: "10.0.0.2", User: "u"})
	req := queueOn(t, app, conn, "whoami", "agent")

	if r := app.SaveConnection(Connection{Name: "db", Host: "10.0.0.9", User: "u"}); r.OK {
		t.Fatalf("new connection with existing name must be rejected")
	}
	renamed := conn
	renamed.Name = "web-01"
	if r := app.SaveConnection(renamed); !r.OK || r.ID != conn.ID {
		t.Fatalf("rename failed: %+v", r)
	}
	clash := other
	clash.Name = "web-01"
	if r := app.SaveConnection(clash); r.OK {
		t.Fatalf("rename onto an existing name must be rejected")
	}
	app.shutdown(context.Background())

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	names := map[string]bool{}
	for _, c := range reloaded.conns {
		names[c.Name] = true
	}
	if names["web"] || !names["web-01"] || !names["db"] {
		t.Fatalf("rename left a duplicate or lost a row: %v", names)
	}
	got, _ := reloaded.findRequest(req.ID)
	if got.Host != "web-01" || got.ConnectionID != conn.ID {
		t.Fatalf("pending request did not follow the rename: %+v", got)
	}
}

func TestPasswordIsWriteOnly(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))
	for _, c := range app.GetInitialData().Connections {
		if c.ID == conn.ID && (c.Credential != "" || !c.CredentialSet) {
			t.Fatalf("password leaked to UI: %+v", c)
		}
	}
	edited := conn
	edited.Credential = ""
	edited.Description = "edited"
	if r := app.SaveConnection(edited); !r.OK {
		t.Fatal(r.Message)
	}
	if r := app.TestConnectionConfig(edited, ""); !r.OK {
		t.Fatalf("stored password not kept: %s", r.Message)
	}
	saved, _ := app.connectionByID(conn.ID)
	if saved.LastTestResult != "ok" {
		t.Fatalf("test result not recorded: %+v", saved)
	}
}

func TestUnsavedEditTestDoesNotLabelSavedConnection(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))
	edited := conn
	edited.Port = newSilentServer(t)
	edited.ConnectTimeout = 1
	app.TestConnectionConfig(edited, "")
	saved, _ := app.connectionByID(conn.ID)
	if saved.LastTestResult != "" {
		t.Fatalf("unsaved edit changed saved state: %+v", saved)
	}
}

func TestDeleteConnectionPersists(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	if r := app.DeleteConnection(app.conns[0].ID); !r.OK {
		t.Fatalf("delete failed: %s", r.Message)
	}
	app.shutdown(context.Background())

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if len(reloaded.GetInitialData().Connections) != 0 {
		t.Fatalf("deleted connection came back: %+v", reloaded.GetInitialData().Connections)
	}
}

func TestLegacyJSONImportedOnlyOnce(t *testing.T) {
	dir := isolateEnv(t)
	legacy := `[{"name":"legacy","host":"10.1.1.1","port":22,"user":"u","authMethod":"パスワード保管","credential":"pw"}]`
	if err := os.WriteFile(filepath.Join(dir, "connections.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-settings.json"), []byte(`{"enabled":true,"token":"oldtoken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	var legacyID string
	for _, c := range app.conns {
		if c.Name == "legacy" {
			legacyID = c.ID
		}
	}
	if legacyID == "" {
		t.Fatalf("legacy connection not imported: %+v", app.conns)
	}
	for _, c := range app.conns {
		app.DeleteConnection(c.ID)
	}
	app.DeleteMCPSettings()
	app.shutdown(context.Background())
	if fileExists(filepath.Join(dir, "connections.json")) || !fileExists(filepath.Join(dir, "connections.json.migrated")) {
		t.Fatalf("legacy file was not retired")
	}

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if len(reloaded.conns) != 0 {
		t.Fatalf("legacy data resurrected: %+v", reloaded.conns)
	}
	if reloaded.mcp.Enabled || reloaded.mcpToken != "" {
		t.Fatalf("deleted MCP settings came back: enabled=%v token=%q", reloaded.mcp.Enabled, reloaded.mcpToken)
	}
}

func TestMigrationFromV0Database(t *testing.T) {
	dir := isolateEnv(t)
	path := filepath.Join(dir, "ssh-gete.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE connections (name TEXT PRIMARY KEY, host TEXT NOT NULL, port INTEGER NOT NULL, user TEXT NOT NULL,
			auth_method TEXT NOT NULL, credential TEXT NOT NULL, connect_timeout INTEGER NOT NULL, command_timeout INTEGER NOT NULL,
			tags_json TEXT NOT NULL, description TEXT NOT NULL, status TEXT NOT NULL, last_checked TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE command_requests (id TEXT PRIMARY KEY, host TEXT NOT NULL, user TEXT NOT NULL, command TEXT NOT NULL,
			reason TEXT NOT NULL, requested_by TEXT NOT NULL, requested_at TEXT NOT NULL, risk TEXT NOT NULL, status TEXT NOT NULL,
			stdout TEXT NOT NULL, stderr TEXT NOT NULL, duration TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO connections VALUES ('old','10.0.0.1',22,'u','SSH鍵認証','~/.ssh/id_ed25519',10,30,'[]','','Online','12:00','')`,
		`INSERT INTO command_requests VALUES ('REQ-1700000000000-1','old','assistant','whoami','','a','12:00:00','Low','承認待ち','','','-',1700000000000,'')`,
		`INSERT INTO command_requests VALUES ('REQ-1700000000001-2','old','assistant','ls','','a','12:00:01','Low','実行済み','out','','1s',1700000000001,'')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	app := NewApp()
	defer app.shutdown(context.Background())
	if len(app.conns) != 1 || app.conns[0].ID == "" {
		t.Fatalf("connection id not assigned: %+v", app.conns)
	}
	states := map[string]string{}
	for _, r := range app.requests {
		states[r.ID] = r.Status
		if r.ConnectionID != app.conns[0].ID || !strings.Contains(r.RequestedAt, "T") {
			t.Fatalf("request not migrated: %+v", r)
		}
	}
	if states["REQ-1700000000000-1"] != statePending || states["REQ-1700000000001-2"] != stateSucceeded {
		t.Fatalf("status codes not migrated: %v", states)
	}
	if !fileExists(path + ".bak-v0") {
		t.Fatalf("pre-migration backup missing")
	}
}

func TestNonLoopbackListenRequiresBearer(t *testing.T) {
	app := newTestApp(t)
	settings := app.GetMCPSettings()
	settings.ListenAddress = "0.0.0.0"
	settings.BearerEnabled = false
	if r := app.SaveMCPSettings(settings); r.OK {
		t.Fatalf("LAN listen without bearer was accepted")
	}
}

func TestEnvTokenIsNotPersisted(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SSH_GETE_TOKEN", "from-env-token")
	// A free port, so the test does not depend on 8787 being unused (an
	// installed ssh-gete may be listening there).
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_GETE_MCP_ADDR", listener.Addr().String())
	_ = listener.Close()
	app := NewApp()
	if r := app.SaveMCPSettings(app.GetMCPSettings()); !r.OK {
		t.Fatal(r.Message)
	}
	app.shutdown(context.Background())
	t.Setenv("SSH_GETE_TOKEN", "")
	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if reloaded.mcpToken != "" {
		t.Fatalf("env token was written to SQLite")
	}
}

func TestConnectURLs(t *testing.T) {
	settings := sanitizeMCPSettings(defaultMCPSettings(""), "", "", "")
	fillConnectURLs(&settings)
	if settings.LocalURL != "http://127.0.0.1:8787/mcp" {
		t.Fatalf("local url: %s", settings.LocalURL)
	}
	if settings.LANReachable {
		t.Fatalf("loopback listener must not be reachable from LAN")
	}
	if settings.LANAddress != "" && !strings.Contains(settings.LANURL, settings.LANAddress) {
		t.Fatalf("lan url %q does not use %q", settings.LANURL, settings.LANAddress)
	}
	settings.ListenAddress = "0.0.0.0"
	fillConnectURLs(&settings)
	if settings.LANAddress != "" && !settings.LANReachable {
		t.Fatalf("0.0.0.0 listener should be reachable via %s", settings.LANAddress)
	}
}

func TestAgentApprovalBypassPersists(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	if r := app.setAgentApprovalBypass("codex-local", true); !r.OK {
		t.Fatalf("set bypass failed: %s", r.Message)
	}
	app.shutdown(context.Background())

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if !reloaded.agentApprovalBypassEnabled("codex-local") {
		t.Fatalf("bypass not persisted")
	}
	if r := reloaded.RevokeAgentApprovalBypass("codex-local"); !r.OK || reloaded.agentApprovalBypassEnabled("codex-local") {
		t.Fatalf("revoke failed")
	}
}
