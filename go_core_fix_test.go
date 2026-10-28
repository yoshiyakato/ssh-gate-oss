package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Regression tests for the go-core findings of audit-2 (N12, N13, N14, N27,
// N28) and the follow-up E4.

// agentCaller is the mcpCaller authenticateMCP would build for a request made
// with agentName's current token.
func agentCaller(app *App, agentName string) mcpCaller {
	app.mu.RLock()
	defer app.mu.RUnlock()
	return mcpCaller{Agent: agentName, TokenHash: app.agentTokens[agentName].TokenHash}
}

// statusEntries runs get_request_status with token (shared or per-agent;
// "" = none) and returns its entries.
func statusEntries(t *testing.T, app *App, token string, ids ...string) []map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"request_ids": ids})
	var out map[string]any
	var isErr bool
	if token == "" {
		out, isErr = callTool(t, app, "get_request_status", string(body))
	} else {
		out, isErr = callToolAuth(t, app, token, "get_request_status", string(body))
	}
	if isErr {
		t.Fatalf("get_request_status error: %v", out)
	}
	items, _ := out["requests"].([]any)
	entries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entries = append(entries, item.(map[string]any))
	}
	return entries
}

// N12: an agent token reads only its own agent's requests; IDs carry a random
// tail so they cannot be enumerated from the time and the sequence.
func TestRequestStatusScopedToAgentToken(t *testing.T) {
	app := newTestApp(t)
	shared := enableBearer(t, app)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	alice := issueAgentToken(t, app, "alice")
	issueAgentToken(t, app, "bob")

	own := queueOn(t, app, conn, "echo mine", "alice")
	other := queueOn(t, app, conn, "echo secret", "bob")
	if !regexp.MustCompile(`^REQ-\d+-\d+-[0-9a-f]{12}$`).MatchString(other.ID) {
		t.Fatalf("request id %q has no random part", other.ID)
	}
	if r := app.ApproveCommand(other.ID, ""); !r.OK {
		t.Fatalf("approve: %+v", r)
	}

	got := statusEntries(t, app, alice, own.ID, other.ID, "REQ-unknown")
	if len(got) != 3 {
		t.Fatalf("entries: %v", got)
	}
	if got[0]["request_id"] != own.ID || got[0]["state"] != statePending {
		t.Fatalf("own request: %v", got[0])
	}
	if got[1]["request_id"] != other.ID || got[1]["error_kind"] != "not_found" || got[1]["stdout"] != nil {
		t.Fatalf("another agent's request leaked: %v", got[1])
	}
	if got[2]["error_kind"] != "not_found" {
		t.Fatalf("unknown id: %v", got[2])
	}

	// A long-poll mixing in a foreign ID answers at once, as an unknown ID does.
	start := time.Now()
	body, _ := json.Marshal(map[string]any{"request_ids": []string{own.ID, other.ID}, "wait_seconds": 5})
	callToolAuth(t, app, alice, "get_request_status", string(body))
	if time.Since(start) > 2*time.Second {
		t.Fatalf("long-poll with a foreign id waited %s", time.Since(start))
	}

	// The shared token (and the GUI) still sees everything.
	if got := statusEntries(t, app, shared, other.ID); got[0]["state"] != stateSucceeded || got[0]["stdout"] != "secret\n" {
		t.Fatalf("shared token view: %v", got[0])
	}
}

// N13: /audit/events answers in the agent-facing shape: no SSH login user,
// connection ID or target, and an agent token sees only its own requests.
func TestAuditHidesUserAndConnectionID(t *testing.T) {
	app := newTestApp(t)
	shared := enableBearer(t, app)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	alice := issueAgentToken(t, app, "alice")
	queueOn(t, app, conn, "echo a", "alice")
	queueOn(t, app, conn, "echo b", "bob")

	audit := func(token string) []map[string]any {
		t.Helper()
		rec := app.serveTest(t, mcpCall{method: http.MethodGet, path: app.mcp.AuditPath, contentType: "-", auth: "Bearer " + token})
		if rec.Code != http.StatusOK {
			t.Fatalf("audit status %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, leaked := range []string{conn.ID, `"user"`, `"connectionId"`, `"targetFp"`, sshTestUser + "@"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("audit leaked %q: %s", leaked, body)
			}
		}
		var resp struct {
			Requests []map[string]any `json:"requests"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Requests
	}
	if got := audit(shared); len(got) != 2 || got[0]["requested_command"] == nil {
		t.Fatalf("shared audit: %v", got)
	}
	got := audit(alice)
	if len(got) != 1 || got[0]["requested_by"] != "alice" {
		t.Fatalf("agent-token audit not scoped: %v", got)
	}
}

// N14: without the agent's own token, its name cannot be used, so nobody can
// take its client_request_id first.
func TestAgentNameWithTokenNeedsThatToken(t *testing.T) {
	app := newTestApp(t)
	shared := enableBearer(t, app)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	token := issueAgentToken(t, app, "trusted")

	args := `{"host":"lab","command":"echo hi","agent_name":"trusted","client_request_id":"k1"}`
	out, isErr := callToolAuth(t, app, shared, "execute_command", args)
	if !isErr || out["error_kind"] != "unauthorized_bypass" {
		t.Fatalf("shared token used a token-bound agent name: %v", out)
	}
	// Bearer off, unauthenticated loopback: refused the same way.
	app.mu.Lock()
	app.mcp.BearerEnabled = false
	app.mu.Unlock()
	if out, isErr := callTool(t, app, "execute_command", args); !isErr || out["error_kind"] != "unauthorized_bypass" {
		t.Fatalf("anonymous caller used a token-bound agent name: %v", out)
	}
	// The real agent's first try is a new request, not someone else's.
	out, _ = callToolAuth(t, app, token, "execute_command", args)
	if out["deduplicated"] == true || out["state"] != statePending {
		t.Fatalf("real agent: %v", out)
	}
	// A name without a token keeps working as before.
	if out, isErr := callTool(t, app, "execute_command", `{"host":"lab","command":"echo hi","agent_name":"other"}`); isErr || out["state"] != statePending {
		t.Fatalf("name without a token: %v", out)
	}
}

// N27: a bypassed run that outlives RequestTimeout answers state=running.
func TestDetachedBypassAnswersRunning(t *testing.T) {
	app := newTestApp(t)
	_, token := bypassAgent(t, app)
	app.mu.Lock()
	app.mcp.RequestTimeout = 1
	app.mu.Unlock()
	out := bypassExec(t, app, token, map[string]any{"command": "sleep 2.5"})
	if out["state"] != stateRunning || out["auto_executed"] != true {
		t.Fatalf("detached answer: %v", out)
	}
	id, _ := out["request_id"].(string)
	if got := statusEntries(t, app, token, id); got[0]["state"] != stateAutoRunning {
		t.Fatalf("status while running: %v", got[0])
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, _ := app.findRequest(id); got.Status == stateAutoSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached run did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// N28: a request authenticated with a token that was rotated before the
// bypass decision is not auto-executed.
func TestRotatedTokenLosesBypass(t *testing.T) {
	app := newTestApp(t)
	srv, _ := bypassAgent(t, app)
	conn, _ := app.findConnection("lab")
	before := agentCaller(app, "trusted")
	if app.needsHumanApproval(conn, "Low", "echo hi", before, "trusted", false) {
		t.Fatalf("current token should bypass")
	}
	issueAgentToken(t, app, "trusted") // rotate
	if !app.needsHumanApproval(conn, "Low", "echo hi", before, "trusted", false) {
		t.Fatalf("the rotated-out token still bypasses approval")
	}
	if app.needsHumanApproval(conn, "Low", "echo hi", agentCaller(app, "trusted"), "trusted", false) {
		t.Fatalf("the new token should bypass")
	}
	if !app.needsHumanApproval(conn, "Low", "echo hi", mcpCaller{Agent: "trusted"}, "trusted", false) {
		t.Fatalf("a caller without a token hash bypassed approval")
	}
	if len(srv.Execs()) != 0 {
		t.Fatalf("execs: %v", srv.Execs())
	}
}

// E4: a token is issued without bypass unless the human granted it; the GUI
// grant turns it on, and an explicitly bypass-less token never auto-runs.
func TestAgentTokenBypassFlag(t *testing.T) {
	app := newTestApp(t)
	enableBearer(t, app)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	tokenBypass := func() bool {
		app.mu.RLock()
		defer app.mu.RUnlock()
		return app.agentTokens["trusted"].Bypass
	}
	storedBypass := func() bool {
		tokens, err := app.store.loadAgentTokens()
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range tokens {
			if tok.AgentName == "trusted" {
				return tok.Bypass
			}
		}
		t.Fatalf("token row missing")
		return false
	}

	issueAgentToken(t, app, "trusted")
	if tokenBypass() || storedBypass() {
		t.Fatalf("a token issued without a grant has bypass on")
	}
	app.setAgentApprovalBypass("trusted", true)
	if !tokenBypass() || !storedBypass() {
		t.Fatalf("granting bypass did not enable the token's flag")
	}
	if app.needsHumanApproval(conn, "Low", "echo hi", agentCaller(app, "trusted"), "trusted", false) {
		t.Fatalf("granted agent should bypass")
	}

	if r := app.IssueAgentTokenWithBypass("trusted", false); !r.OK || r.Token == "" {
		t.Fatalf("issue without bypass: %+v", r)
	}
	if tokenBypass() || storedBypass() {
		t.Fatalf("IssueAgentTokenWithBypass(false) kept bypass on")
	}
	if !app.needsHumanApproval(conn, "Low", "echo hi", agentCaller(app, "trusted"), "trusted", false) {
		t.Fatalf("a bypass-less token auto-ran while the grant is active")
	}
	for _, p := range app.ListAgentApprovalBypasses() {
		if p.AgentName == "trusted" && p.Active {
			t.Fatalf("grant shown active for a bypass-less token: %+v", p)
		}
	}

	// Rotating while the grant is active (the "approve and bypass, then
	// issue" flow) gives the new token bypass.
	issueAgentToken(t, app, "trusted")
	if !tokenBypass() {
		t.Fatalf("token issued under an active grant has bypass off")
	}
	app.RevokeAgentApprovalBypass("trusted")
	if tokenBypass() || storedBypass() {
		t.Fatalf("revoking the grant left the token's bypass on")
	}
}

// N23: deleting a connection cancels its running commands, like a disconnect,
// and says how many it stopped.
func TestDeleteConnectionCancelsRunningExec(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("web-01"))
	req, done := sessStartApproved(t, app, conn, "sleep 5")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool {
		return sessSummary(t, app, conn.ID).Running == 1
	})
	result := app.DeleteConnection(conn.ID)
	if !result.OK || !strings.Contains(result.Message, "実行中コマンド 1 件") {
		t.Fatalf("delete: %+v", result)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the command kept running after its connection was deleted")
	}
	if got, _ := app.findRequest(req.ID); got.Status != stateFailed || got.ErrorKind != "canceled" {
		t.Fatalf("request status=%s errorKind=%s, want failed/canceled", got.Status, got.ErrorKind)
	}
	if n := app.runningCounts()[conn.ID]; n != 0 {
		t.Fatalf("still running: %d", n)
	}
}

// E2: when the row of an edited connection is not saved, the password typed
// for it does not stay in the keychain.
func TestFailedSaveRestoresPreviousPassword(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	saved := addTrustedConnection(t, app, srv.Connection("lab"))
	account := passwordAccount(saved.ID)
	keychain := func() string {
		t.Helper()
		v, err := app.secrets.Get(context.Background(), account)
		if err != nil {
			t.Fatalf("keychain: %v", err)
		}
		return v
	}

	// updateConnection fails.
	if _, err := app.store.db.Exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON connections BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	edit := saved
	edit.Credential = "unsaved-1"
	if r := app.SaveConnection(edit); r.OK {
		t.Fatalf("save should fail: %+v", r)
	}
	if got := keychain(); got != sshTestPassword {
		t.Fatalf("keychain after a failed save = %q, want the previous password", got)
	}
	if r := app.TestConnection(saved.ID); !r.OK {
		t.Fatalf("previous password must still work: %+v", r)
	}
	if _, err := app.store.db.Exec(`DROP TRIGGER fail_update`); err != nil {
		t.Fatal(err)
	}

	// updateConnection finds no row (deleted behind the app's back).
	if _, err := app.store.db.Exec(`DELETE FROM connections WHERE id = ?`, saved.ID); err != nil {
		t.Fatal(err)
	}
	edit.Credential = "unsaved-2"
	if r := app.SaveConnection(edit); r.OK {
		t.Fatalf("save of a vanished row should fail: %+v", r)
	}
	if got := keychain(); got != sshTestPassword {
		t.Fatalf("keychain after !updated = %q, want the previous password", got)
	}
}

// E2: with no previous keychain item, a failed save removes the new one.
func TestFailedSaveRemovesPasswordWithoutPrevious(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")
	conn.AuthMethod = "SSH鍵認証"
	conn.Credential = "/nonexistent/key"
	saved := addTestConnection(t, app, conn)
	if _, err := app.store.db.Exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON connections BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	edit := srv.Connection("lab")
	edit.ID = saved.ID
	if r := app.SaveConnection(edit); r.OK {
		t.Fatalf("save should fail: %+v", r)
	}
	if _, err := app.secrets.Get(context.Background(), passwordAccount(saved.ID)); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("unsaved password left in the keychain: %v", err)
	}
}

// E5: SSH validation failures name the field they are about.
func TestSaveConnectionValidationFields(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	addTestConnection(t, app, srv.Connection("lab"))
	cases := []struct {
		name  string
		edit  func(*Connection)
		field string
		code  string
	}{
		{"empty name", func(c *Connection) { c.Name = "" }, "name", "required"},
		{"empty host", func(c *Connection) { c.Name, c.Host = "x", "" }, "host", "required"},
		{"empty user", func(c *Connection) { c.Name, c.User = "x", "" }, "user", "required"},
		{"duplicate name", func(c *Connection) {}, "name", "duplicate_name"},
		{"duplicate name without password", func(c *Connection) { c.Credential = "" }, "name", "duplicate_name"},
	}
	for _, tc := range cases {
		conn := srv.Connection("lab")
		tc.edit(&conn)
		r := app.SaveConnection(conn)
		if r.OK || r.Field != tc.field || r.Code != tc.code {
			t.Fatalf("%s: %+v, want field=%s code=%s", tc.name, r, tc.field, tc.code)
		}
	}
}

// N21: a pending request carried over from v2 (no target_fp) gets its
// connection's target at the upgrade, so a later edit of the connection is
// caught; one that still has no target is refused instead of approved.
func TestV2PendingRequestGetsTargetFP(t *testing.T) {
	path := makeV2Database(t, time.Now().Format(time.RFC3339))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE command_requests DROP COLUMN target_fp`,
		`INSERT INTO connections (id, name, type, host, port, user, auth_method, credential, serial_port, baud_rate,
			connect_timeout, command_timeout, tags_json, description, status, last_checked, updated_at)
			VALUES ('c-ssh', 'web', 'SSH', 'Web01.example', 2222, 'deploy', 'SSH鍵認証', '', '', 0, 10, 30, '[]', '', '', '', '')`,
		`INSERT INTO command_requests (id, connection_id, host, user, command, reason, requested_by, requested_at, risk, status,
			stdout, stderr, duration, created_at, updated_at)
			VALUES ('REQ-1-1', 'c-ssh', 'web', 'deploy', 'ls', '', 'a', '2026-01-01T00:00:00Z', 'Low', 'pending', '', '', '-', 1, '')`,
		`INSERT INTO command_requests (id, connection_id, host, user, command, reason, requested_by, requested_at, risk, status,
			stdout, stderr, duration, created_at, updated_at)
			VALUES ('REQ-1-2', 'c-ssh', 'web', 'deploy', 'ls', '', 'a', '2026-01-01T00:00:00Z', 'Low', 'succeeded', '', '', '-', 1, '')`,
		`INSERT INTO command_requests (id, connection_id, host, user, command, reason, requested_by, requested_at, risk, status,
			stdout, stderr, duration, created_at, updated_at)
			VALUES ('REQ-1-3', 'gone', 'old', 'deploy', 'ls', '', 'a', '2026-01-01T00:00:00Z', 'Low', 'pending', '', '', '-', 1, '')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()

	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fp := func(id string) string {
		req, ok, err := store.loadCommandRequest(id)
		if err != nil || !ok {
			t.Fatalf("load %s: ok=%v err=%v", id, ok, err)
		}
		return req.TargetFP
	}
	if got := fp("REQ-1-1"); got != "deploy@web01.example:2222" {
		t.Fatalf("pending target_fp = %q", got)
	}
	if got := fp("REQ-1-2"); got != "" {
		t.Fatalf("finished request got a target_fp: %q", got)
	}
	if got := fp("REQ-1-3"); got != "" {
		t.Fatalf("request of a deleted connection got a target_fp: %q", got)
	}
}

func TestApproveRefusesRequestWithoutTarget(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo x", "agent")
	app.mu.Lock()
	for i := range app.requests {
		if app.requests[i].ID == req.ID {
			app.requests[i].TargetFP = "" // as stored before target_fp existed
		}
	}
	app.mu.Unlock()
	if target := app.GetApprovalTarget(req.ID); !target.TargetUnknown || !target.FPChanged {
		t.Fatalf("approval target: %+v", target)
	}
	for _, r := range []ActionResult{app.ApproveCommand(req.ID, ""), app.ApproveCommandAndBypassAgent(req.ID, "")} {
		if r.OK || r.Code != "target_unknown" {
			t.Fatalf("approve without target: %+v", r)
		}
	}
	if got, _ := app.findRequest(req.ID); got.Status != statePending || len(srv.Execs()) != 0 {
		t.Fatalf("request %s ran or changed: %v", got.Status, srv.Execs())
	}
}

// N41: the config dir, the database and every copy of its data next to it go
// through restrictToOwner, which on Windows sets a protected DACL
// (restrictACL); here the hook records the calls and the modes are checked.
func TestDatabaseFilesRestrictedToOwner(t *testing.T) {
	var mu sync.Mutex
	restricted := map[string]bool{}
	old := restrictACL
	restrictACL = func(path string, isDir bool) error {
		mu.Lock()
		restricted[path] = isDir
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { restrictACL = old })

	dir := filepath.Join(t.TempDir(), "cfg")
	path := filepath.Join(dir, "ssh-gete.sqlite")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	// Files an older version (or another tool) left readable.
	for _, p := range []string{path + ".bak-v2", path + "-wal"} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for p, wantDir := range map[string]bool{dir: true, path: false, path + ".bak-v2": false, path + "-wal": false, path + ".lock": false} {
		mu.Lock()
		isDir, ok := restricted[p]
		mu.Unlock()
		if !ok || isDir != wantDir {
			t.Errorf("%s: DACL hook called=%v dir=%v", p, ok, isDir)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if wantDir {
			want = 0o700
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", p, info.Mode().Perm(), want)
		}
	}
}

// N20: the GUI connection test trusts a new host key only after the
// password was accepted.
func TestConnectionTestRecordsHostKeyOnlyAfterAuth(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")
	conn.Credential = "wrong"
	saved := addTestConnection(t, app, conn)
	if r := app.TestConnection(saved.ID); r.OK {
		t.Fatalf("wrong password passed: %+v", r)
	}
	if info, err := app.GetHostKeyInfo(saved.ID); err != nil || info.Known {
		t.Fatalf("host key trusted after a failed authentication: %+v %v", info, err)
	}
	fixed := saved
	fixed.Credential = sshTestPassword
	if r := app.SaveConnection(fixed); !r.OK {
		t.Fatalf("save: %+v", r)
	}
	r := app.TestConnection(saved.ID)
	if !r.OK || !strings.Contains(r.Message, ssh.FingerprintSHA256(srv.HostKey())) {
		t.Fatalf("test: %+v", r)
	}
	if info, _ := app.GetHostKeyInfo(saved.ID); !info.Known {
		t.Fatalf("host key not trusted after a successful test")
	}
}

// N20: a terminal dial (openSSHClient) and the plain testSSHConnection trust
// an unknown host key only once authentication succeeded.
func TestTerminalDialRecordsHostKeyOnlyAfterAuth(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("term")
	conn.Credential = "wrong"
	saved := addTestConnection(t, app, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c, err := openSSHClient(ctx, conn, false); err == nil {
		_ = c.Close()
		t.Fatalf("wrong password authenticated")
	}
	if err := testSSHConnection(ctx, conn, false); err == nil {
		t.Fatalf("wrong password passed testSSHConnection")
	}
	if info, err := app.GetHostKeyInfo(saved.ID); err != nil || info.Known {
		t.Fatalf("host key trusted after a failed authentication: %+v %v", info, err)
	}
	conn.Credential = sshTestPassword
	c, err := openSSHClient(ctx, conn, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
	if info, _ := app.GetHostKeyInfo(saved.ID); !info.Known {
		t.Fatalf("host key not trusted after a successful terminal dial")
	}
}
