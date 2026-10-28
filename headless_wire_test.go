package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func wireFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestHeadlessWireFillsHooks(t *testing.T) {
	h := headlessHooks
	if h.selectSecrets == nil || h.installSecrets == nil || h.newAuditSink == nil || h.prepareWeb == nil || h.adminToken == nil {
		t.Fatalf("headlessHooks not fully wired: %+v", h)
	}
}

func TestHeadlessSelectSecretsNilBinderIsNilInterface(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SSH_GETE_KEYRING", "")
	store, binder, err := headlessSelectSecrets(SecretsConfig{Backend: "memory"}, "memory", t.TempDir())
	if err != nil || store == nil || binder == nil {
		t.Fatalf("memory: store=%v binder=%v err=%v", store, binder, err)
	}
	// SSH_GETE_KEYRING=memory overrides a file backend; secrets.file is ignored then.
	t.Setenv("SSH_GETE_KEYRING", "memory")
	if _, _, err := headlessSelectSecrets(SecretsConfig{Backend: "file", File: "x.json"}, "memory", t.TempDir()); err != nil {
		t.Fatalf("override: %v", err)
	}
}

func TestAdminAuditWriterRoundTrip(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	defer app.shutdown(context.Background())
	w, err := newAdminAuditWriter(app.store)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	w.logf = func(format string, args ...any) { logged.WriteString(format) }
	w.record(adminAuditEntry{Actor: "web:abcd1234", Remote: "127.0.0.1", Action: "login_ok", OK: true})
	w.record(adminAuditEntry{Actor: "config", Action: "config_create", Target: "c1", OK: true})
	w.Close(time.Second)
	w.record(adminAuditEntry{Actor: "late", Action: "x"}) // after Close: dropped, no panic
	got, err := listAdminAudit(app.store, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "config_create" || got[1].Action != "login_ok" || !got[1].OK || got[1].At == "" {
		t.Fatalf("entries = %+v", got)
	}
}

// TestHeadlessEndToEnd runs runHeadless with the real wiring on loopback:
// login, a call, the event stream, and the guards (§6.1, §6.4, §6.5).
func TestHeadlessEndToEnd(t *testing.T) {
	dir := isolateEnv(t)
	webPort := wireFreePort(t)
	mcpPort := wireFreePort(t)
	t.Setenv("SSH_GETE_MCP_ADDR", "127.0.0.1:"+strconv.Itoa(mcpPort))
	t.Setenv("SSH_GETE_TOKEN", "mcp-bearer-token-for-test-0123456789")

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
				t.Errorf("exit code %d", code)
			}
		case <-time.After(30 * time.Second):
			t.Error("shutdown timed out")
		}
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(webPort)
	origin := base
	tokenBytes, err := os.ReadFile(filepath.Join(dir, "admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if strings.Contains(out.String(), token) {
		t.Fatal("admin token printed on stdout")
	}

	jar, _ := cookiejar.New(nil)
	client := withSessionKey(&http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	do := func(method, path, body string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return res
	}
	api := map[string]string{"Content-Type": "application/json", "X-SSH-Gete-CSRF": "1", "Origin": origin}
	expect := func(res *http.Response, want int, what string) []byte {
		t.Helper()
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s: status %d, want %d: %s", what, res.StatusCode, want, b)
		}
		return b
	}

	if b := expect(do("GET", "/login", "", nil), 200, "login page"); !bytes.Contains(b, []byte("/login-assets/login.js")) {
		t.Fatalf("login page: %s", b)
	}
	expect(do("GET", "/login-assets/login.js", "", nil), 200, "login.js")
	if res := do("GET", "/", "", nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("index without session: %d", res.StatusCode)
	}
	expect(do("POST", "/api/call/GetInitialData", "[]", api), 401, "call without session")
	expect(do("POST", "/api/login", `{"token":"sgadm_wrong"}`, api), 401, "wrong token")
	expect(do("POST", "/api/login", `{"token":"`+token+`"}`, api), 204, "login")

	if b := expect(do("GET", "/", "", nil), 200, "index"); !bytes.Contains(b, []byte(`<script src="/headless-shim.js"></script>`)) {
		t.Fatal("shim not injected")
	}
	b := expect(do("POST", "/api/call/GetInitialData", "[]", api), 200, "GetInitialData")
	var call struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(b, &call); err != nil || call.Result == nil {
		t.Fatalf("GetInitialData body %s (%v)", b, err)
	}

	// Review finding 5: the cookie replayed by another client (forged Origin,
	// CSRF header) without the session key is refused.
	{
		replay, _ := http.NewRequest("POST", base+"/api/call/GetInitialData", strings.NewReader("[]"))
		for k, v := range api {
			replay.Header.Set(k, v)
		}
		for _, c := range jar.Cookies(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(webPort), Path: "/"}) {
			replay.AddCookie(c)
		}
		if res, err := (&http.Client{}).Do(replay); err != nil || res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("cookie replay without the session key: %v %v", res, err)
		} else {
			res.Body.Close()
		}
	}

	// Review finding 4: at runtime the admin token cannot become the MCP
	// Bearer, and the MCP Bearer cannot become the admin password.
	if b := expect(do("POST", "/api/call/SaveMCPSettings", `[{"tokenInput":"`+token+`","bearerEnabled":true}]`, api), 200, "SaveMCPSettings"); !bytes.Contains(b, []byte(`"ok":false`)) {
		t.Fatalf("SaveMCPSettings accepted the admin token: %s", b)
	}
	expect(do("POST", "/api/admin/password", `{"current":"`+token+`","password":"mcp-bearer-token-for-test-0123456789"}`, api), 400, "MCP bearer as the admin password")

	// Cross-origin, missing CSRF header, MCP Bearer: all refused.
	evil := map[string]string{"Content-Type": "application/json", "X-SSH-Gete-CSRF": "1", "Origin": "http://evil.example"}
	expect(do("POST", "/api/call/GetInitialData", "[]", evil), 403, "foreign origin")
	expect(do("POST", "/api/call/GetInitialData", "[]", map[string]string{"Content-Type": "application/json", "Origin": origin}), 403, "no csrf header")
	bearer := map[string]string{"Content-Type": "application/json", "X-SSH-Gete-CSRF": "1", "Origin": origin, "Authorization": "Bearer mcp-bearer-token-for-test-0123456789"}
	expect(do("POST", "/api/call/GetInitialData", "[]", bearer), 400, "bearer on admin api")
	plain := &http.Client{}
	req, _ := http.NewRequest("POST", base+"/api/call/GetInitialData", strings.NewReader("[]"))
	for k, v := range bearer {
		req.Header.Set(k, v)
	}
	req.Header.Del("Authorization")
	req.Header.Set("Authorization", "Bearer "+token)
	if res, err := plain.Do(req); err != nil || res.StatusCode == 200 {
		t.Fatalf("admin token as bearer must not work: %v %v", res, err)
	}

	// SSE: the first event is headless:resync.
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	sreq, _ := http.NewRequestWithContext(sctx, "GET", base+"/api/events", nil)
	sreq.Header.Set("X-SSH-Gete-CSRF", "1")
	sres, err := client.Do(sreq)
	if err != nil || sres.StatusCode != 200 {
		t.Fatalf("events: %v %v", sres, err)
	}
	sc := bufio.NewScanner(sres.Body)
	sawResync := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"headless:resync"`) {
			sawResync = true
			break
		}
	}
	sres.Body.Close()
	if !sawResync {
		t.Fatal("no headless:resync on the event stream")
	}

	// Audit: login and the call were recorded, without the token.
	ab := expect(do("GET", "/api/admin/audit?limit=50", "", map[string]string{"X-SSH-Gete-CSRF": "1"}), 200, "audit")
	if !bytes.Contains(ab, []byte(`"login_ok"`)) || !bytes.Contains(ab, []byte(`"login_fail"`)) || bytes.Contains(ab, []byte(token)) {
		t.Fatalf("audit: %s", ab)
	}

	// The event stream needs a session and refuses foreign origins.
	noSess, _ := http.NewRequest("GET", base+"/api/events", nil)
	noSess.Header.Set("X-SSH-Gete-CSRF", "1")
	if res, err := plain.Do(noSess); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("events without session: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	expect(do("GET", "/api/events", "", map[string]string{"X-SSH-Gete-CSRF": "1", "Origin": "http://evil.example"}), 403, "events foreign origin")
	expect(do("GET", "/api/events", "", map[string]string{"X-SSH-Gete-CSRF": "1", "Sec-Fetch-Site": "cross-site"}), 403, "events cross-site")
	expect(do("GET", "/api/events", "", nil), 403, "events without csrf header")
	expect(do("GET", "/api/events", "", map[string]string{"X-SSH-Gete-CSRF": "1", "Authorization": "Bearer mcp-bearer-token-for-test-0123456789"}), 400, "events with bearer")

	// An agent token (issued through the admin API itself) cannot call the
	// admin API, with or without the admin cookie alongside it.
	ib := expect(do("POST", "/api/call/IssueAgentToken", `["agent-a"]`, api), 200, "IssueAgentToken")
	var issued struct {
		Result ActionResult `json:"result"`
	}
	if err := json.Unmarshal(ib, &issued); err != nil || !issued.Result.OK || issued.Result.Token == "" {
		t.Fatalf("IssueAgentToken: %s (%v)", ib, err)
	}
	if bytes.Contains(expect(do("GET", "/api/admin/audit?limit=50", "", map[string]string{"X-SSH-Gete-CSRF": "1"}), 200, "audit after issue"), []byte(issued.Result.Token)) {
		t.Fatal("agent token leaked into the admin audit")
	}
	agentToken := issued.Result.Token
	withAgent := map[string]string{"Content-Type": "application/json", "X-SSH-Gete-CSRF": "1", "Origin": origin, "Authorization": "Bearer " + agentToken}
	expect(do("POST", "/api/call/GetInitialData", "[]", withAgent), 400, "agent token with cookie")
	expect(do("POST", "/api/call/ListAgentTokens", "[]", withAgent), 400, "agent token on ListAgentTokens")
	req, _ = http.NewRequest("POST", base+"/api/call/GetInitialData", strings.NewReader("[]"))
	for k, v := range withAgent {
		req.Header.Set(k, v)
	}
	if res, err := plain.Do(req); err != nil || res.StatusCode == 200 {
		t.Fatalf("agent token without cookie must not work: %v %v", res, err)
	} else {
		res.Body.Close()
	}

	// Neither the agent token nor the MCP Bearer is an admin login credential.
	expect(do("POST", "/api/login", `{"token":"`+agentToken+`"}`, api), 401, "login with the agent token")
	expect(do("POST", "/api/login", `{"token":"mcp-bearer-token-for-test-0123456789"}`, api), 401, "login with the MCP bearer")
	expect(do("POST", "/api/call/GetInitialData", "[]", api), 200, "session still valid after failed logins")

	// The other direction: the MCP listener does not accept the admin token
	// or the admin cookie. The agent token and the MCP Bearer are the
	// positive controls.
	mcpURL := "http://127.0.0.1:" + strconv.Itoa(mcpPort) + "/mcp"
	mcpPost := func(auth string, cookies []*http.Cookie) int {
		t.Helper()
		req, _ := http.NewRequest("POST", mcpURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		res, err := plain.Do(req)
		if err != nil {
			t.Fatalf("mcp: %v", err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	adminCookies := jar.Cookies(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(webPort), Path: "/"})
	if len(adminCookies) == 0 {
		t.Fatal("no admin cookie in the jar")
	}
	if code := mcpPost("", nil); code != http.StatusUnauthorized {
		t.Fatalf("mcp without auth: %d", code)
	}
	if code := mcpPost("Bearer "+token, nil); code != http.StatusUnauthorized {
		t.Fatalf("mcp with the admin token: %d", code)
	}
	if code := mcpPost("", adminCookies); code != http.StatusUnauthorized {
		t.Fatalf("mcp with the admin cookie: %d", code)
	}
	for _, c := range adminCookies {
		if code := mcpPost("Bearer "+c.Value, nil); code != http.StatusUnauthorized {
			t.Fatalf("mcp with the admin cookie value as bearer: %d", code)
		}
	}
	if code := mcpPost("Bearer "+agentToken, nil); code != http.StatusOK {
		t.Fatal("mcp refused the agent token (positive control)")
	}
	if code := mcpPost("Bearer mcp-bearer-token-for-test-0123456789", nil); code != http.StatusOK {
		t.Fatal("mcp refused its own bearer (positive control)")
	}

	// Logout ends an open event stream and the session's API access.
	lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer lcancel()
	lreq, _ := http.NewRequestWithContext(lctx, "GET", base+"/api/events", nil)
	lreq.Header.Set("X-SSH-Gete-CSRF", "1")
	lres, err := client.Do(lreq)
	if err != nil || lres.StatusCode != 200 {
		t.Fatalf("events before logout: %v %v", lres, err)
	}
	streamDone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, lres.Body)
		close(streamDone)
	}()
	expect(do("POST", "/api/logout", "", api), 204, "logout")
	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream still open after logout")
	}
	lres.Body.Close()
	for _, c := range adminCookies { // the old cookie value is dead even if replayed
		req, _ := http.NewRequest("POST", base+"/api/call/GetInitialData", strings.NewReader("[]"))
		for k, v := range api {
			req.Header.Set(k, v)
		}
		req.AddCookie(c)
		if res, err := plain.Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("replayed cookie after logout: %v %v", res, err)
		} else {
			res.Body.Close()
		}
	}
}

// embeddedDistForTest is a stub frontend/dist: an index.html with <head>
// (the shim is injected after it).
func embeddedDistForTest(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{"index.html": {Data: []byte("<!doctype html><html><head><title>t</title></head><body></body></html>")}}
}
