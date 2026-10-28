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

	"golang.org/x/crypto/ssh/knownhosts"
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

// addTrustedConnection saves conn and runs the GUI connection test once, so
// TOFU records the host key the way a human first connect would. MCP-origin
// executions against an unrecorded host fail with hostkey_unknown.
func addTrustedConnection(t *testing.T, app *App, conn Connection) Connection {
	t.Helper()
	saved := addTestConnection(t, app, conn)
	if result := app.TestConnection(saved.ID); !result.OK {
		t.Fatalf("trust %s: %s", saved.Name, result.Message)
	}
	return saved
}

// issueAgentToken issues a per-agent token (agent_tokens). It calls the API
// directly so a signature change fails to compile instead of skipping tests.
func issueAgentToken(t *testing.T, app *App, agentName string) string {
	t.Helper()
	result := app.IssueAgentToken(agentName)
	if !result.OK || result.Token == "" {
		t.Fatalf("agent token for %s not issued: %+v", agentName, result)
	}
	return result.Token
}

// callToolAuth is callTool with an Authorization: Bearer header.
func callToolAuth(t *testing.T, app *App, token string, name string, args string) (map[string]any, bool) {
	t.Helper()
	rec := app.serveTest(t, mcpCall{auth: "Bearer " + token, body: toolCall(name, args)})
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
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
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
	conn := addTrustedConnection(t, app, srv.Connection("lab"))

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
	enableBearer(t, app)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	prod := srv.Connection("prod-db")
	prod.Tags = []string{"prod"}
	addTrustedConnection(t, app, prod)
	app.setAgentApprovalBypass("trusted", true)
	token := issueAgentToken(t, app, "trusted")

	call := func(host string, command string) map[string]any {
		out, _ := callToolAuth(t, app, token, "execute_command",
			`{"host":"`+host+`","command":"`+command+`","agent_name":"trusted"}`)
		return out
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

// statusOf runs get_request_status for one id and returns its entry.
func statusOf(t *testing.T, app *App, id string, waitSeconds int) map[string]any {
	t.Helper()
	out, isErr := callTool(t, app, "get_request_status",
		`{"request_ids":["`+id+`"],"wait_seconds":`+strconv.Itoa(waitSeconds)+`}`)
	if isErr {
		t.Fatalf("get_request_status error: %v", out)
	}
	items, _ := out["requests"].([]any)
	if len(items) != 1 {
		t.Fatalf("get_request_status returned %d entries: %v", len(items), out)
	}
	return items[0].(map[string]any)
}

// TestRequestStatusLongPoll: a request queued through MCP is returned by
// get_request_status within wait_seconds once a human approves it.
func TestRequestStatusLongPoll(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	queued, isErr := callTool(t, app, "execute_command", `{"host":"lab","command":"echo polled","agent_name":"agent"}`)
	if isErr || queued["state"] != statePending || queued["requires_approval"] != true {
		t.Fatalf("expected a queued request: %v", queued)
	}
	id := queued["request_id"].(string)

	go func() {
		time.Sleep(200 * time.Millisecond)
		app.ApproveCommand(id, "")
	}()
	const wait = 10
	start := time.Now()
	got := statusOf(t, app, id, wait)
	if got["state"] != stateSucceeded || !strings.Contains(got["stdout"].(string), "polled") {
		t.Fatalf("unexpected status: %v", got)
	}
	if elapsed := time.Since(start); elapsed >= wait*time.Second {
		t.Fatalf("long-poll did not wake on change (took %s)", elapsed)
	}
}

// TestRequestStatusLongPollTimeout: with nothing changing, the long-poll
// returns the still-pending state once wait_seconds elapses.
func TestRequestStatusLongPollTimeout(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo never", "agent")

	start := time.Now()
	got := statusOf(t, app, req.ID, 1)
	elapsed := time.Since(start)
	if got["state"] != statePending {
		t.Fatalf("unexpected status: %v", got)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("long-poll returned before wait_seconds (%s)", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("long-poll overran wait_seconds (%s)", elapsed)
	}
	if n := len(srv.Execs()); n != 0 {
		t.Fatalf("pending request executed: %d", n)
	}
}

// TestClientRequestIDIsIdempotent: resending the same client_request_id,
// before and after approval, never creates or runs a second request.
func TestClientRequestIDIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	args := `{"host":"lab","command":"echo once","agent_name":"a","client_request_id":"k1"}`
	first, _ := callTool(t, app, "execute_command", args)
	second, _ := callTool(t, app, "execute_command", args)
	if first["request_id"] == nil || first["request_id"] != second["request_id"] || second["deduplicated"] != true {
		t.Fatalf("retry created a new request: %v / %v", first, second)
	}
	id := first["request_id"].(string)
	if r := app.ApproveCommand(id, ""); !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	third, _ := callTool(t, app, "execute_command", args)
	if third["request_id"] != id || third["state"] != stateSucceeded {
		t.Fatalf("retry after execution: %v", third)
	}
	if n := len(app.GetInitialData().Requests); n != 1 {
		t.Fatalf("requests=%d", n)
	}
	if execs := srv.Execs(); len(execs) != 1 {
		t.Fatalf("execLog=%v, want exactly one exec", execs)
	}
	// Another agent may reuse the same key independently.
	other, _ := callTool(t, app, "execute_command", `{"host":"lab","command":"echo once","agent_name":"b","client_request_id":"k1"}`)
	if other["request_id"] == id {
		t.Fatalf("client_request_id must be scoped to agent_name")
	}
}

// TestMCPUnknownHostKeyFails: TOFU never trusts a key on an MCP-origin run.
// A host the GUI has not connected to yet fails with hostkey_unknown, and
// nothing is recorded, so a retry fails the same way.
func TestMCPUnknownHostKeyFails(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	addTestConnection(t, app, srv.Connection("lab"))

	for attempt := 0; attempt < 2; attempt++ {
		queued, _ := callTool(t, app, "execute_command", `{"host":"lab","command":"echo x","agent_name":"agent"}`)
		id, _ := queued["request_id"].(string)
		if id == "" {
			t.Fatalf("expected a queued request: %v", queued)
		}
		app.ApproveCommand(id, "")
		got := statusOf(t, app, id, 5)
		if got["state"] != stateFailed || got["error_kind"] != "hostkey_unknown" {
			t.Fatalf("attempt %d: want failed/hostkey_unknown, got %v", attempt, got)
		}
	}
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("command reached an unverified host: %v", execs)
	}
}

// TestHostKeyMismatchAlwaysRejected: once a key is trusted, a changed key is
// refused for MCP runs and GUI tests alike, with or without StrictHostKey,
// and the stored key is not silently replaced.
func TestHostKeyMismatchAlwaysRejected(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run("strict="+strconv.FormatBool(strict), func(t *testing.T) {
			configDir := isolateEnv(t)
			app := NewApp()
			t.Cleanup(func() { app.shutdown(context.Background()) })
			app.mu.Lock()
			app.mcp.StrictHostKey = strict
			app.mu.Unlock()
			srv := newSSHTestServer(t)
			var conn Connection
			if strict {
				// Strict mode never records; trust comes from ~/.ssh/known_hosts.
				conn = addTestConnection(t, app, srv.Connection("lab"))
				home, _ := os.UserHomeDir()
				if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
					t.Fatal(err)
				}
				line := knownhosts.Line([]string{knownhosts.Normalize(hostKeyAddress(conn))}, srv.HostKey()) + "\n"
				if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte(line), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				conn = addTrustedConnection(t, app, srv.Connection("lab"))
				// The trusted key lives in ssh-gete's own known_hosts (0600),
				// not in the user's ~/.ssh/known_hosts.
				path := filepath.Join(configDir, "known_hosts")
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("no dedicated known_hosts: %v", err)
				}
				if os.PathSeparator == '/' && info.Mode().Perm() != 0o600 {
					t.Fatalf("known_hosts mode %v, want 0600", info.Mode().Perm())
				}
				if home, err := os.UserHomeDir(); err == nil && fileExists(filepath.Join(home, ".ssh", "known_hosts")) {
					t.Fatalf("TOFU wrote to the user's ~/.ssh/known_hosts")
				}
			}

			before := queueOn(t, app, conn, "echo trusted", "agent")
			if r := app.ApproveCommand(before.ID, ""); !r.OK {
				t.Fatalf("approve on trusted key: %+v", r)
			}
			if got, _ := app.findRequest(before.ID); got.Status != stateSucceeded {
				t.Fatalf("trusted key run: %+v", got)
			}

			srv.RotateHostKey(t)
			after := queueOn(t, app, conn, "echo spoofed", "agent")
			app.ApproveCommand(after.ID, "")
			got, _ := app.findRequest(after.ID)
			if got.Status != stateFailed || got.ErrorKind != "hostkey" {
				t.Fatalf("mismatched key must fail with hostkey: %+v", got)
			}
			if r := app.TestConnection(conn.ID); r.OK {
				t.Fatalf("GUI test accepted a changed host key")
			}
			// The GUI test above must not have re-recorded the new key.
			again := queueOn(t, app, conn, "echo spoofed2", "agent")
			app.ApproveCommand(again.ID, "")
			if got, _ := app.findRequest(again.ID); got.Status != stateFailed || got.ErrorKind != "hostkey" {
				t.Fatalf("mismatch accepted after GUI retry: %+v", got)
			}
			if execs := srv.Execs(); len(execs) != 1 || execs[0] != "echo trusted" {
				t.Fatalf("command reached a host with a changed key: %v", execs)
			}
		})
	}
}

// TestHostKeyResetRetrusts: after the human resets the fingerprint, the next
// GUI connection records the new key and MCP runs work again.
func TestHostKeyResetRetrusts(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	srv.RotateHostKey(t)
	if r := app.TestConnection(conn.ID); r.OK {
		t.Fatalf("changed key accepted before reset")
	}
	if n, err := app.resetHostKey(conn.ID); err != nil || n == 0 {
		t.Fatalf("reset: removed=%d err=%v", n, err)
	}
	if r := app.TestConnection(conn.ID); !r.OK {
		t.Fatalf("GUI test after reset: %s", r.Message)
	}
	req := queueOn(t, app, conn, "echo again", "agent")
	app.ApproveCommand(req.ID, "")
	if got, _ := app.findRequest(req.ID); got.Status != stateSucceeded {
		t.Fatalf("run after re-trust: %+v", got)
	}
}

// TestSharedTokenCannotClaimBypass: the shared Bearer token proves only
// "some client of this gateway", so an agent_name with a bypass policy is
// still queued. Only that agent's own token unlocks bypass, and it does not
// work under another agent_name.
func TestSharedTokenCannotClaimBypass(t *testing.T) {
	app := newTestApp(t)
	shared := enableBearer(t, app)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	app.setAgentApprovalBypass("trusted", true)
	app.setAgentApprovalBypass("other", true)

	out, _ := callToolAuth(t, app, shared, "execute_command", `{"host":"lab","command":"echo shared","agent_name":"trusted"}`)
	if out["state"] != statePending && out["error_kind"] != "unauthorized_bypass" {
		t.Fatalf("shared token claimed bypass: %v", out)
	}
	if out["auto_executed"] == true {
		t.Fatalf("shared token auto-executed: %v", out)
	}
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("command ran on the shared token: %v", execs)
	}

	token := issueAgentToken(t, app, "trusted")
	// Now "trusted" holds a token with bypass on and an active grant: the
	// shared token naming it must still be queued.
	out, _ = callToolAuth(t, app, shared, "execute_command", `{"host":"lab","command":"echo shared","agent_name":"trusted"}`)
	if out["state"] != statePending || out["auto_executed"] == true {
		t.Fatalf("shared token claimed bypass of an agent that has a token: %v", out)
	}
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("command ran on the shared token after the agent token was issued: %v", execs)
	}
	out, _ = callToolAuth(t, app, token, "execute_command", `{"host":"lab","command":"echo own","agent_name":"other"}`)
	if out["auto_executed"] == true || (out["state"] != statePending && out["error_kind"] != "unauthorized_bypass") {
		t.Fatalf("agent token was accepted for a different agent_name: %v", out)
	}
	out, _ = callToolAuth(t, app, token, "execute_command", `{"host":"lab","command":"echo own","agent_name":"trusted"}`)
	if out["state"] != stateAutoSucceeded || out["auto_executed"] != true {
		t.Fatalf("agent token with bypass policy should auto-run: %v", out)
	}
	if execs := srv.Execs(); len(execs) != 1 || execs[0] != "echo own" {
		t.Fatalf("execLog=%v", execs)
	}
}

// TestListHostsSerialRowsHideSecrets: serial rows carry only name, type,
// executable=false and in_use; SSH details never leak through them.
func TestListHostsSerialRowsHideSecrets(t *testing.T) {
	app := newTestApp(t)
	addTestConnection(t, app, Connection{
		Name: "uart", Type: "Serial", SerialPort: "/dev/cu.usbserial-SECRET", BaudRate: 115200,
		Host: "10.9.9.9", User: "leak-user", AuthMethod: "パスワード保管", Credential: "leak-password",
		Description: "leak-description", Tags: []string{"bench"},
	})
	rec := app.serveTest(t, mcpCall{body: toolCall("list_hosts", `{}`)})
	raw := rec.Body.String()
	for _, secret := range []string{"/dev/cu.usbserial-SECRET", "10.9.9.9", "leak-user", "leak-password", "leak-description"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("list_hosts leaked %q: %s", secret, raw)
		}
	}
	out, isErr := callTool(t, app, "list_hosts", `{}`)
	if isErr {
		t.Fatalf("list_hosts error: %v", out)
	}
	hosts, _ := out["hosts"].([]any)
	var serial map[string]any
	for _, h := range hosts {
		if row := h.(map[string]any); row["name"] == "uart" {
			serial = row
		}
	}
	if serial == nil {
		t.Fatalf("serial row missing: %v", out)
	}
	allowed := map[string]bool{"name": true, "type": true, "executable": true, "in_use": true}
	for key := range serial {
		if !allowed[key] {
			t.Fatalf("serial row carries %q: %v", key, serial)
		}
	}
	if serial["type"] != "serial" || serial["executable"] != false {
		t.Fatalf("serial row: %v", serial)
	}
	if _, ok := serial["in_use"].(bool); !ok {
		t.Fatalf("serial row needs in_use bool: %v", serial)
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
	conn = addTrustedConnection(t, app, conn)
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

// freeLoopbackAddr returns a 127.0.0.1 address with a port free right now.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestEnvTokenIsNotPersisted(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SSH_GETE_TOKEN", "from-env-token")
	// SaveMCPSettings starts the listener; keep it off the default port,
	// which a running ssh-gete (or another test binary) may hold.
	t.Setenv("SSH_GETE_MCP_ADDR", freeLoopbackAddr(t))
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

func TestApprovalTargetShowsResolvedConnectionAndFPChange(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo a", "agent")
	wantFP := conn.User + "@" + hostKeyAddress(conn)
	if req.TargetFP != wantFP {
		t.Fatalf("target fp=%q want %q", req.TargetFP, wantFP)
	}
	target := app.GetApprovalTarget(req.ID)
	if !target.Found || target.ResolvedBy != "id" || target.ConnID != conn.ID || target.FPChanged || !target.Executable {
		t.Fatalf("unexpected target: %+v", target)
	}
	if !target.HostKey.Known || len(target.HostKey.Keys) == 0 {
		t.Fatalf("recorded host key not shown: %+v", target.HostKey)
	}

	// target_fp survives a restart.
	app.shutdown(context.Background())
	app = NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	reloaded, ok := app.findRequest(req.ID)
	if !ok || reloaded.TargetFP != wantFP {
		t.Fatalf("target fp not persisted: %+v", reloaded)
	}

	// Re-pointing the connection after the request shows as a change.
	moved, _ := app.connectionByID(conn.ID)
	moved.User = "someone-else"
	if r := app.SaveConnection(moved); !r.OK {
		t.Fatalf("save: %s", r.Message)
	}
	target = app.GetApprovalTarget(req.ID)
	if !target.FPChanged || target.RequestedFP != wantFP || !strings.HasPrefix(target.CurrentFP, "someone-else@") {
		t.Fatalf("fp change not reported: %+v", target)
	}
	// ...and approval is refused, for every approve path.
	execsBefore := len(srv.Execs())
	for name, approve := range map[string]func(string, string) ActionResult{
		"plain": app.ApproveCommand, "confirmed": app.ApproveCommandConfirmed, "bypass": app.ApproveCommandAndBypassAgent,
	} {
		if r := approve(req.ID, ""); r.OK || r.Code != "target_changed" {
			t.Fatalf("%s approve after target change: %+v", name, r)
		}
	}
	if n := len(srv.Execs()); n != execsBefore {
		t.Fatalf("execs=%d after refused approve, want %d", n, execsBefore)
	}
	if got, _ := app.findRequest(req.ID); got.Status != statePending {
		t.Fatalf("refused approve changed state to %s", got.Status)
	}

	// A deleted connection is not replaced by a new one with the same name.
	if r := app.DeleteConnection(conn.ID); !r.OK {
		t.Fatalf("delete: %s", r.Message)
	}
	addTestConnection(t, app, srv.Connection("lab"))
	if target := app.GetApprovalTarget(req.ID); target.Found {
		t.Fatalf("deleted connection resolved by name: %+v", target)
	}
	before := len(srv.Execs())
	if r := app.ApproveCommand(req.ID, ""); r.OK {
		t.Fatalf("approve ran on a replacement connection: %+v", r)
	}
	if n := len(srv.Execs()); n != before {
		t.Fatalf("execs=%d, want %d", n, before)
	}
}

// TestSwitchToSerialDropsSSHAndBlocksMCP: phase 4. Turning an SSH connection
// into a serial one clears host/user/password (in memory and in SQLite),
// removes it from Host matching, and stops MCP execution and approval.
func TestSwitchToSerialDropsSSHAndBlocksMCP(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SSH_GETE_NO_SEED", "1") // the seeded connection is also 127.0.0.1
	app := NewApp()
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	pending := queueOn(t, app, conn, "echo a", "agent")

	serial := conn
	serial.Type = "Serial"
	serial.SerialPort = "/dev/cu.usbserial-1"
	if r := app.SaveConnection(serial); !r.OK {
		t.Fatalf("switch to serial: %s", r.Message)
	}
	check := func(c Connection) {
		t.Helper()
		if c.Host != "" || c.User != "" || c.Credential != "" || c.MCPFirstTrust {
			t.Fatalf("SSH leftovers on serial connection: %+v", c)
		}
		if c.DataBits != 8 || c.Parity != "none" || c.StopBits != "1" || c.BaudRate != 115200 {
			t.Fatalf("serial defaults not applied: %+v", c)
		}
	}
	saved, _ := app.connectionByID(conn.ID)
	check(saved)

	out, isErr := callTool(t, app, "execute_command", `{"host":"127.0.0.1","command":"whoami","agent_name":"x"}`)
	if !isErr || out["error_kind"] != "host_not_found" {
		t.Fatalf("serial connection matched by host: %v", out)
	}
	out, isErr = callTool(t, app, "execute_command", `{"host":"lab","command":"whoami","agent_name":"x"}`)
	if !isErr || out["error_kind"] != "serial_not_supported" {
		t.Fatalf("serial executable via MCP: %v", out)
	}
	if target := app.GetApprovalTarget(pending.ID); target.Executable || !target.FPChanged {
		t.Fatalf("approval target for serial: %+v", target)
	}
	if r := app.ApproveCommand(pending.ID, ""); r.OK {
		t.Fatalf("approval ran on a serial connection: %+v", r)
	}
	if n := len(srv.Execs()); n != 0 {
		t.Fatalf("execs=%d", n)
	}

	app.shutdown(context.Background())
	app = NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	reloaded, _ := app.connectionByID(conn.ID)
	check(reloaded)
}

// TestClassifyRiskSeesChainedAndPathCommands: shell separators, full paths
// and wrappers must not hide a High command from the classifier, since
// approval bypass relies on High always being queued.
func TestClassifyRiskSeesChainedAndPathCommands(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		{"true;rm x", "High"},
		{"a&&rm x", "High"},
		{"/bin/rm x", "High"},
		{"true;rm /etc/passwd", "High"},
		{"true&&rm -f /var/lib/x", "High"},
		{"echo ok|xargs rm", "High"},
		{"sh -c 'rm x'", "High"},
		{"$(rm x)", "High"},
		{"`rm x`", "High"},
		{"sudo -u root /usr/bin/rm x", "High"},
		{"x;passwd root", "High"},
		{"find / -delete", "High"},
		{"find . -name '*.log' -exec rm {} ;", "High"},
		{"FOO=1 /sbin/reboot", "High"},
		{"mkfs.ext4 /dev/sdb1", "High"},
		{"true;systemctl restart nginx", "High"},
		{"chown -R root /", "High"},
		{"rm -rf /tmp/x", "High"},
		{"true;sudo ls", "Medium"},
		{"true&&mv a b", "Medium"},
		{"docker stop x", "Medium"},
		{"echo ok", "Low"},
		{"cat /etc/passwd", "Low"},
		{"ls /usr/bin/", "Low"},
		{"uptime; df -h", "Low"},
		{"grep -r form .", "Low"},
	}
	for _, c := range cases {
		if got := classifyRisk(c.command); got != c.want {
			t.Errorf("classifyRisk(%q) = %s, want %s", c.command, got, c.want)
		}
	}
}

// bypassAgent sets up a connection "lab" and agent "trusted" with its own
// token and an active bypass grant (default max_risk Medium).
func bypassAgent(t *testing.T, app *App) (*sshTestServer, string) {
	t.Helper()
	enableBearer(t, app)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	app.setAgentApprovalBypass("trusted", true)
	return srv, issueAgentToken(t, app, "trusted")
}

// TestBypassQueuesChainedHighRisk: a High command hidden behind ; && or a
// full path is queued even for an agent with a bypass grant.
func TestBypassQueuesChainedHighRisk(t *testing.T) {
	app := newTestApp(t)
	srv, token := bypassAgent(t, app)
	for _, command := range []string{"true;rm x", "a&&rm x", "/bin/rm x"} {
		args, _ := json.Marshal(map[string]any{"host": "lab", "command": command, "agent_name": "trusted"})
		out, _ := callToolAuth(t, app, token, "execute_command", string(args))
		if out["state"] != statePending || out["risk"] != "High" || out["auto_executed"] == true {
			t.Fatalf("%q must queue as High: %v", command, out)
		}
	}
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("High command ran without approval: %v", execs)
	}
}

// TestConcurrentClientRequestIDRunsOnce: concurrent resends of the same
// client_request_id create one request and run at most once, both when the
// agent's command auto-runs and when it is queued.
func TestConcurrentClientRequestIDRunsOnce(t *testing.T) {
	app := newTestApp(t)
	srv, token := bypassAgent(t, app)
	app.setAgentApprovalBypass("queued", true) // no token: always queued
	shared := app.effectiveTokenLocked()

	for _, c := range []struct{ agent, token string }{{"trusted", token}, {"queued", shared}} {
		args := `{"host":"lab","command":"echo once","agent_name":"` + c.agent + `","client_request_id":"k-concurrent"}`
		const n = 8
		var wg sync.WaitGroup
		ids := make(chan any, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, _ := callToolAuth(t, app, c.token, "execute_command", args)
				ids <- out["request_id"]
			}()
		}
		wg.Wait()
		close(ids)
		var first any
		for id := range ids {
			if id == nil || (first != nil && id != first) {
				t.Fatalf("%s: concurrent resends got different requests: %v vs %v", c.agent, first, id)
			}
			first = id
		}
		rows := 0
		for _, req := range app.GetInitialData().Requests {
			if req.RequestedBy == c.agent && req.ClientRequestID == "k-concurrent" {
				rows++
			}
		}
		if rows != 1 {
			t.Fatalf("%s: %d requests for one client_request_id", c.agent, rows)
		}
	}
	if execs := srv.Execs(); len(execs) > 1 {
		t.Fatalf("same client_request_id ran %d times: %v", len(execs), execs)
	}
}

// TestDeduplicatedFailureKeepsIsError: a retry of a failed bypassed run
// reports the same isError as the original answer.
func TestDeduplicatedFailureKeepsIsError(t *testing.T) {
	app := newTestApp(t)
	_, token := bypassAgent(t, app)
	args := `{"host":"lab","command":"exit 3","agent_name":"trusted","client_request_id":"k-fail"}`
	first, firstErr := callToolAuth(t, app, token, "execute_command", args)
	if first["state"] != stateAutoFailed || !firstErr {
		t.Fatalf("original: isError=%v %v", firstErr, first)
	}
	retry, retryErr := callToolAuth(t, app, token, "execute_command", args)
	if retry["deduplicated"] != true || retry["request_id"] != first["request_id"] || !retryErr {
		t.Fatalf("retry: isError=%v %v", retryErr, retry)
	}
	ok, okErr := callToolAuth(t, app, token, "execute_command", `{"host":"lab","command":"echo fine","agent_name":"trusted","client_request_id":"k-ok"}`)
	again, againErr := callToolAuth(t, app, token, "execute_command", `{"host":"lab","command":"echo fine","agent_name":"trusted","client_request_id":"k-ok"}`)
	if okErr || againErr || ok["state"] != stateAutoSucceeded || again["deduplicated"] != true {
		t.Fatalf("successful retry: %v/%v %v %v", okErr, againErr, ok, again)
	}
}

// TestHostCandidatesDoNotRankByAddress: the host_not_found candidates must
// not reveal which connection's hidden address contains the probe.
func TestHostCandidatesDoNotRankByAddress(t *testing.T) {
	app := newTestApp(t)
	alpha := Connection{Name: "alpha", Type: "SSH", Host: "10.0.3.1", Port: 22, User: "u", Tags: []string{}}
	beta := Connection{Name: "beta", Type: "SSH", Host: "192.168.7.9", Port: 22, User: "u", Tags: []string{}}
	addTestConnection(t, app, alpha)
	addTestConnection(t, app, beta)
	baseline := app.hostCandidates("no-such-name", maxCandidates)
	for _, probe := range []string{"10.0.", "10.0.3", "192.168", "192.168.7.9"} {
		out, isErr := callTool(t, app, "execute_command", `{"host":"`+probe+`.x","command":"echo","agent_name":"a"}`)
		if !isErr || out["error_kind"] != "host_not_found" {
			t.Fatalf("probe %s: %v", probe, out)
		}
		if got := app.hostCandidates(probe, maxCandidates); strings.Join(got, ",") != strings.Join(baseline, ",") {
			t.Fatalf("probe %q reordered candidates by address: %v (baseline %v)", probe, got, baseline)
		}
	}
	if got := app.hostCandidates("bet", maxCandidates); len(got) == 0 || got[0] != "beta" {
		t.Fatalf("name match should rank first: %v", got)
	}
}

// TestAgentViewsHideLocalErrorDetail: a failed run's local error (address,
// fingerprint) stays in the GUI/DB record but not in what agents read
// through get_request_status, execute_command or the audit endpoint.
func TestAgentViewsHideLocalErrorDetail(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab")) // never trusted
	req := queueOn(t, app, conn, "echo x", "agent")
	app.ApproveCommand(req.ID, "")
	stored, _ := app.findRequest(req.ID)
	port := strconv.Itoa(srv.Port())
	if stored.ErrorKind != "hostkey_unknown" || !strings.Contains(stored.Stderr, port) || !strings.Contains(stored.Stderr, "SHA256:") {
		t.Fatalf("GUI record should keep the detail: %+v", stored)
	}
	got := statusOf(t, app, req.ID, 0)
	stderr, _ := got["stderr"].(string)
	if got["error_kind"] != "hostkey_unknown" || stderr == "" {
		t.Fatalf("status: %v", got)
	}
	audit := app.serveTest(t, mcpCall{method: http.MethodGet, path: app.mcp.AuditPath, contentType: "-"}).Body.String()
	for _, leaked := range []string{port, "SHA256:", "127.0.0.1"} {
		if strings.Contains(stderr, leaked) {
			t.Fatalf("get_request_status stderr leaked %q: %q", leaked, stderr)
		}
		if strings.Contains(audit, leaked) {
			t.Fatalf("audit leaked %q: %s", leaked, audit)
		}
	}
	if agentStderr("remote said no\n"+execErrorMarker+"dial tcp 10.1.2.3:22: refused", "connect") != "remote said no\n"+execErrorMarker+"could not connect to the host" {
		t.Fatalf("remote stderr before the marker must be kept")
	}
}

// TestDeleteRefusesRunningRequest: the delete is a CAS on the status, in
// memory and in SQLite, so a request that is running keeps its audit row.
func TestDeleteRefusesRunningRequest(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo x", "agent")
	if _, err := app.transitionRequest(req.ID, []string{statePending}, func(r *CommandRequest) { r.Status = stateRunning }); err != nil {
		t.Fatal(err)
	}
	if r := app.DeleteCommandRequest(req.ID); r.OK {
		t.Fatalf("running request deleted: %+v", r)
	}
	if err := app.store.deleteCommandRequestUnlessRunning(req.ID); err != errStateConflict {
		t.Fatalf("store delete of a running row: %v", err)
	}
	if _, found, err := app.store.loadCommandRequest(req.ID); err != nil || !found {
		t.Fatalf("running row gone: found=%v err=%v", found, err)
	}
	app.finishRequest(func() CommandRequest { r, _ := app.findRequest(req.ID); return r }(), stateSucceeded, commandResult{}, nil)
	if r := app.DeleteCommandRequest(req.ID); !r.OK {
		t.Fatalf("finished request not deleted: %+v", r)
	}
	if _, found, _ := app.store.loadCommandRequest(req.ID); found {
		t.Fatalf("finished row still in SQLite")
	}
}

// TestShutdownDrainsGUIApprovedRun: a queued command approved in the GUI is
// waited for by drain, then cancelled and persisted as interrupted before
// the store closes.
func TestShutdownDrainsGUIApprovedRun(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "sleep 30", "agent")
	done := make(chan ActionResult, 1)
	go func() { done <- app.ApproveCommand(req.ID, "") }()
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.Execs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("approved command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finished := app.drainMCPCommands(100 * time.Millisecond); finished {
		t.Fatalf("drain did not wait for the GUI-approved run")
	}
	got, _ := app.findRequest(req.ID)
	if got.Status != stateInterrupted || got.ErrorKind != "canceled" {
		t.Fatalf("after drain: %+v", got)
	}
	if row, _, _ := app.store.loadCommandRequest(req.ID); row.Status != stateInterrupted {
		t.Fatalf("interrupted state not persisted: %+v", row)
	}
	if r := <-done; r.OK {
		t.Fatalf("approve reported success for an interrupted run: %+v", r)
	}
	// Once draining, new approvals are refused rather than run untracked.
	late := queueOn(t, app, conn, "echo late", "agent")
	if r := app.ApproveCommand(late.ID, ""); r.OK {
		t.Fatalf("approval during shutdown ran: %+v", r)
	}
}

// TestListenerRestartDetachesBypassedRun: restarting the MCP listener while
// an agent waits on a bypassed run answers state=auto_running and lets the
// command finish; it is not cancelled as if the client had disconnected.
func TestListenerRestartDetachesBypassedRun(t *testing.T) {
	app := newTestApp(t)
	_, token := bypassAgent(t, app)
	host, portText, _ := net.SplitHostPort(freeLoopbackAddr(t))
	port, _ := strconv.Atoi(portText)
	app.mu.Lock()
	app.mcp.ListenAddress, app.mcp.Port, app.mcp.Enabled, app.mcp.RequestTimeout = host, port, true, 30
	path := app.mcp.MCPPath
	app.mu.Unlock()
	if err := app.restartMCPServer(); err != nil {
		t.Fatal(err)
	}

	answer := make(chan map[string]any, 1)
	go func() {
		httpReq, _ := http.NewRequest(http.MethodPost, "http://"+net.JoinHostPort(host, portText)+path,
			strings.NewReader(toolCall("execute_command", `{"host":"lab","command":"sleep 3","agent_name":"trusted"}`)))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			answer <- map[string]any{"transport_error": err.Error()}
			return
		}
		defer resp.Body.Close()
		var body struct {
			Result struct {
				StructuredContent map[string]any `json:"structuredContent"`
			} `json:"result"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		answer <- body.Result.StructuredContent
	}()

	var id string
	deadline := time.Now().Add(5 * time.Second)
	for id == "" {
		for _, req := range app.GetInitialData().Requests {
			if req.Status == stateAutoRunning {
				id = req.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("bypassed run did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if err := app.restartMCPServer(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("restart waited %s for the synchronous handler", elapsed)
	}
	select {
	case out := <-answer:
		if out["request_id"] != id || out["state"] != stateAutoRunning {
			t.Fatalf("answer on restart: %v", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no answer after restart")
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		got, _ := app.findRequest(id)
		if requestIsFinal(got.Status) {
			if got.Status != stateAutoSucceeded {
				t.Fatalf("run was cut by the restart: %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBypassGrantRevokeStayConsistent: concurrent grant and revoke for the
// same agent end in the same state in memory and in SQLite.
func TestBypassGrantRevokeStayConsistent(t *testing.T) {
	app := newTestApp(t)
	for i := 0; i < 50; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); app.setAgentApprovalBypass("racer", true) }()
		go func() { defer wg.Done(); app.RevokeAgentApprovalBypass("racer") }()
		wg.Wait()
		app.mu.RLock()
		mem := app.agentPolicies["racer"].ApprovalBypass
		app.mu.RUnlock()
		stored, _, err := app.store.loadAgentPolicy("racer")
		if err != nil {
			t.Fatal(err)
		}
		if stored.ApprovalBypass != mem {
			t.Fatalf("round %d: memory bypass=%v, SQLite bypass=%v", i, mem, stored.ApprovalBypass)
		}
	}
}
