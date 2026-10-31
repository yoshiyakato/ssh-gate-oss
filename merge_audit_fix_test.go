package main

// Regression tests for the post-merge audit findings (統合後の監査).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runHeadlessForExit starts headless on dir and returns its exit code and
// stderr. A start that gets as far as waiting for signals is stopped and
// reported as -1.
func runHeadlessForExit(t *testing.T, dir string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	deps := headlessHooks
	deps.stdout = &syncWriter{w: &out}
	deps.stderr = &syncWriter{w: &errOut}
	started := make(chan struct{})
	deps.signals = func() (<-chan os.Signal, func()) {
		close(started)
		return make(chan os.Signal), func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	// isolateEnv's MCP port was released too, so the kernel may hand it out again.
	listen := "127.0.0.1:" + strconv.Itoa(wireFreePort(t))
	for listen == os.Getenv("SSH_GETE_MCP_ADDR") {
		listen = "127.0.0.1:" + strconv.Itoa(wireFreePort(t))
	}
	opts := CLIOptions{Headless: true, DataDir: dir, Listen: listen}
	go func() { done <- runHeadlessWith(ctx, opts, embeddedDistForTest(t), deps) }()
	select {
	case code := <-done:
		return code, errOut.String()
	case <-started:
		cancel()
		<-done
		return -1, errOut.String()
	case <-time.After(30 * time.Second):
		t.Fatal("headless did not exit")
	}
	return 0, ""
}

// 1. Headless refuses to start when an MCP Bearer (the effective one or the
// saved one SSH_GETE_TOKEN hides) equals the admin password.
func TestHeadlessRefusesBearerEqualToAdminPassword(t *testing.T) {
	const pw = "shared-admin-password-0123456789"
	t.Run("SSH_GETE_TOKEN", func(t *testing.T) {
		dir := isolateEnv(t)
		if err := setAdminPasswordFile(filepath.Join(dir, adminPasswordFileName), pw, nil); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SSH_GETE_TOKEN", pw)
		code, stderr := runHeadlessForExit(t, dir)
		if code != exitUsage || !strings.Contains(stderr, "管理パスワード") {
			t.Fatalf("exit %d, want %d; stderr:\n%s", code, exitUsage, stderr)
		}
	})
	t.Run("saved by the GUI", func(t *testing.T) {
		dir := isolateEnv(t)
		gui := NewApp() // GUI mode: adminCredential is nil, so the save passes
		settings := gui.GetMCPSettings()
		settings.TokenInput = pw
		settings.BearerEnabled = true
		if r := gui.SaveMCPSettings(settings); !r.OK {
			t.Fatalf("GUI save: %+v", r)
		}
		gui.shutdown(context.Background())
		if err := setAdminPasswordFile(filepath.Join(dir, adminPasswordFileName), pw, nil); err != nil {
			t.Fatal(err)
		}
		// The saved Bearer is hidden by SSH_GETE_TOKEN, but unsetting it
		// would bring it back.
		t.Setenv("SSH_GETE_TOKEN", "another-mcp-bearer-0123456789abc")
		code, stderr := runHeadlessForExit(t, dir)
		if code != exitUsage || !strings.Contains(stderr, "管理パスワード") {
			t.Fatalf("exit %d, want %d; stderr:\n%s", code, exitUsage, stderr)
		}
	})
	t.Run("different values start", func(t *testing.T) {
		dir := isolateEnv(t)
		if err := setAdminPasswordFile(filepath.Join(dir, adminPasswordFileName), pw, nil); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SSH_GETE_TOKEN", "another-mcp-bearer-0123456789abc")
		if code, stderr := runHeadlessForExit(t, dir); code != -1 {
			t.Fatalf("exit %d, want a running server; stderr:\n%s", code, stderr)
		}
	})
}

// 2. SaveMCPSettings refuses an MCP listener that overlaps the Web admin's.
func TestSaveMCPSettingsRefusesWebListenOverlap(t *testing.T) {
	app := newTestApp(t)
	webPort := wireFreePort(t)
	app.mu.Lock()
	app.webListenOverlap = webListenOverlapCheck("0.0.0.0:" + strconv.Itoa(webPort))
	app.mu.Unlock()
	before := app.GetMCPSettings()

	settings := before
	settings.Enabled = true
	settings.ListenAddress = "127.0.0.1"
	settings.Port = webPort
	r := app.SaveMCPSettings(settings)
	if r.OK || r.Code != "invalid_listen" {
		t.Fatalf("overlapping save = %+v, want invalid_listen", r)
	}
	if got := app.GetMCPSettings(); got.Port != before.Port || got.ListenAddress != before.ListenAddress {
		t.Fatalf("refused settings applied: %+v", got)
	}
	if saved, _, loaded, err := app.store.loadMCPSettings(app.dbPath); err == nil && loaded && saved.Port == webPort {
		t.Fatal("refused settings stored")
	}

	// Positive control: another port saves.
	settings.Port = wireFreePort(t)
	if r := app.SaveMCPSettings(settings); !r.OK {
		t.Fatalf("non-overlapping save refused: %+v", r)
	}
}

// 3. A cookie-only page request (as a same-site page on another port can
// make) does not refresh the idle timer; an /api/* call with the key does.
func TestAdminSessionPageRequestDoesNotExtendIdle(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{IdleMinutes: 10, MaxHours: 12})
	c := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	page := func() int {
		h := f.auth.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(c) // cookie only: no session key (an <img> from elsewhere)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	f.clock.Add(9 * time.Minute)
	if page() != 200 {
		t.Fatal("page refused while the session is live")
	}
	f.clock.Add(9 * time.Minute) // 18 min after the last /api/* use
	if code := page(); code == 200 {
		t.Fatal("page request kept the idle session alive")
	}
	if f.protected("/api/x", c).Code != http.StatusUnauthorized {
		t.Fatal("idle expiry defeated by page requests")
	}

	// The admin's own /api/* use still extends it.
	c = sessionCookie(t, f.login(f.token, "127.0.0.1"))
	f.clock.Add(9 * time.Minute)
	if f.protected("/api/x", c).Code != 200 {
		t.Fatal("api refused")
	}
	f.clock.Add(9 * time.Minute)
	if f.protected("/api/x", c).Code != 200 {
		t.Fatal("api use did not extend idle")
	}
}

// 4-6. Shell-state changes through printf -v / read / mapfile, history
// re-execution, and NAME+= assignments of dangerous names.
func TestAISessionClassifiesHiddenStateChanges(t *testing.T) {
	shell := sessionRiskState{Mode: aiModeShell, Env: map[string]string{}}
	exec := sessionRiskState{Mode: aiModeExec, Env: map[string]string{}}
	cases := []struct {
		line   string
		st     sessionRiskState
		reason string
		state  bool // effect state_change
	}{
		// 4
		{"printf -v PATH /tmp/x:/usr/bin", shell, "dangerous_env", true},
		{"printf -vHOME /tmp/d", shell, "dangerous_env", true},
		{"printf -v FOO %s bar", shell, "shell_state_change", true},
		{"read PATH < /tmp/p", shell, "dangerous_env", true},
		{"read -r line < /tmp/p", shell, "shell_state_change", true},
		{"mapfile -t lines < /tmp/p", shell, "shell_state_change", true},
		{"readarray lines < /tmp/p", shell, "shell_state_change", true},
		{"getopts ab opt", shell, "shell_state_change", true},
		{"printf -v PATH /tmp/x; ls", exec, "dangerous_env", false},
		// 5
		{"fc -s", shell, "shell_state_change", true},
		{"fc -s report=wipe", shell, "shell_state_change", true},
		{"history", shell, "shell_state_change", true},
		{"^report^wipe", shell, "history_reexec", false},
		{"!!", shell, "history_reexec", false},
		{"echo !python", shell, "history_reexec", false},
		// 6
		{"PATH+=:/tmp/x id", exec, "dangerous_env", false},
		{"BASH_ENV+=/tmp/x id", exec, "dangerous_env", false},
		{"HOME+=/sub git log", shell, "dangerous_env", false},
		{"env PATH+=:/tmp/x id", exec, "dangerous_env", false},
		{"export PATH+=:/tmp/x", shell, "dangerous_env", true},
	}
	for _, c := range cases {
		got := classifySessionLine(c.line, c.st)
		if got.Risk != "High" || !slices.Contains(got.Reasons, c.reason) {
			t.Errorf("%q (%s): risk=%s reasons=%v, want High with %s", c.line, c.st.Mode, got.Risk, got.Reasons, c.reason)
		}
		if c.state && got.Effect != lineEffectStateChange {
			t.Errorf("%q (%s): effect=%s, want state_change", c.line, c.st.Mode, got.Effect)
		}
		if !sessionNeedsApproval(c.st, got, "Medium") {
			t.Errorf("%q (%s): runs without approval", c.line, c.st.Mode)
		}
	}
	// Plain uses keep their risk.
	for _, line := range []string{"printf '%s\\n' hi", "ls -la", `[ "$a" != b ]`, "echo hi!", "kill $!"} {
		got := classifySessionLine(line, shell)
		if slices.Contains(got.Reasons, "history_reexec") || got.Effect == lineEffectStateChange {
			t.Errorf("%q: reasons=%v effect=%s", line, got.Reasons, got.Effect)
		}
	}
}

// 5. The marker shell starts with history (and history expansion) off in
// bash, so neither fc nor history can re-run a line or show the nonce.
func TestAIShellInitLineDisablesHistory(t *testing.T) {
	line := string(aiShellInitLine("abc"))
	for _, want := range []string{"set +o history +H", "history -c", "BASH_VERSION"} {
		if !strings.Contains(line, want) {
			t.Fatalf("init line %q lacks %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("init line must be one line: %q", line)
	}
}

// 7. After an interrupt, a transport that keeps the line for its own grace
// is waited for, so the next line does not hit transport_busy.
type graceTransport struct {
	fakeAIBareTransport
	released chan struct{}
	grace    time.Duration
}

type fakeAIBareTransport struct{}

func (fakeAIBareTransport) StartLine(context.Context, aiLinePlan) (aiLineHandle, error) {
	return nil, errors.New("unused")
}
func (fakeAIBareTransport) Interrupt() error      { return nil }
func (fakeAIBareTransport) Ready() bool           { return true }
func (fakeAIBareTransport) Cwd() string           { return "" }
func (fakeAIBareTransport) Close() error          { return nil }
func (fakeAIBareTransport) Done() <-chan struct{} { return nil }
func (fakeAIBareTransport) EndReason() string     { return "" }

func (g *graceTransport) interruptGrace() time.Duration { return g.grace }
func (g *graceTransport) Ready() bool {
	select {
	case <-g.released:
		return true
	default:
		return false
	}
}

type chanHandle struct{ done chan struct{} }

func (h chanHandle) Done() <-chan struct{}       { return h.done }
func (h chanHandle) Outcome() aiTransportOutcome { return aiTransportOutcome{} }

func TestWaitTransportLineWaitsForTransportGrace(t *testing.T) {
	old := aiLineCancelGrace
	aiLineCancelGrace = 50 * time.Millisecond
	defer func() { aiLineCancelGrace = old }()

	done := make(chan struct{})
	tr := &graceTransport{released: done, grace: 2 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go func() {
		time.Sleep(300 * time.Millisecond) // the transport's own close timer
		close(done)
	}()
	waitTransportLine(ctx, tr, chanHandle{done: done})
	if !tr.Ready() {
		t.Fatal("waitTransportLine returned while the transport still held the line")
	}
	if g := (&aiExecTransport{}).interruptGrace(); g <= aiExecInterruptGrace || g <= old {
		t.Fatalf("exec interruptGrace %s must exceed aiExecInterruptGrace %s and aiLineCancelGrace %s", g, aiExecInterruptGrace, old)
	}
}

// 7, over SSH: the server ignores the signal; after the timed-out line the
// next line runs instead of failing with transport_busy.
func TestAISessionExecNextLineAfterIgnoredInterrupt(t *testing.T) {
	oldCancel, oldInt := aiLineCancelGrace, aiExecInterruptGrace
	aiLineCancelGrace, aiExecInterruptGrace = 100*time.Millisecond, 700*time.Millisecond
	defer func() { aiLineCancelGrace, aiExecInterruptGrace = oldCancel, oldInt }()

	srv := newSSHTestServer(t)
	app := newTestApp(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	issueAgentToken(t, app, aiTestAgent)
	settings := defaultAISessionSettings()
	settings.Enabled = true
	if r := app.SaveAISessionSettings(settings); !r.OK {
		t.Fatal(r)
	}
	if r := app.setAgentApprovalBypass(aiTestAgent, true); !r.OK {
		t.Fatal(r)
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(args)
		out, rpcErr, handled := app.callAISessionTool(context.Background(), aiTestCaller(app), toolCallParams{Name: name, Arguments: raw})
		if !handled || rpcErr != nil {
			t.Fatalf("%s: %v", name, rpcErr)
		}
		var res struct {
			StructuredContent map[string]any `json:"structuredContent"`
		}
		_ = json.Unmarshal([]byte(mustJSON(t, out)), &res)
		return res.StructuredContent
	}
	open := call("open_session", map[string]any{"host": conn.Name, "reason": "test"})
	id, _ := open["session_id"].(string)
	if id == "" {
		t.Fatalf("open = %v", open)
	}
	v := call("send_line", map[string]any{"session_id": id, "line": "sleep 600", "client_request_id": "h1", "timeout_seconds": 1, "wait_seconds": 10})
	if v["state"] != stateAutoFailed {
		t.Fatalf("sleep 600 = %v", v)
	}
	v = call("send_line", map[string]any{"session_id": id, "line": "pwd", "client_request_id": "h2", "wait_seconds": 10})
	if v["state"] != stateAutoSucceeded {
		t.Fatalf("line after the interrupted one = %v", v)
	}
}

// 8 / 9. execute_command's client_request_id dedupe never answers with an AI
// session line, so another caller cannot read it once the owner's token is
// gone (and the owner does not get it through this tool either).
func TestExecuteCommandDedupeDoesNotLeakAISessionLine(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	id := e.open(t)
	wait := 5
	view, err := e.app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller(e.app),
		sendLineArgs{SessionID: id, Line: "echo TOPSECRET", ClientRequestID: "line-1", WaitSeconds: &wait})
	if err != nil || view.State != stateAutoSucceeded {
		t.Fatalf("send: %+v %v", view, err)
	}
	ownerOut, _ := e.app.executeCommandTool(context.Background(), aiTestCaller(e.app),
		executeCommandArgs{Host: e.conn.Name, Command: "true", AgentName: aiTestAgent, ClientRequestID: "line-1"})
	if s := mustJSON(t, ownerOut); strings.Contains(s, "TOPSECRET") || strings.Contains(s, id) {
		t.Fatalf("owner got the AI line through execute_command: %s", s)
	}
	if r := e.app.RevokeAgentToken(aiTestAgent); !r.OK {
		t.Fatalf("revoke: %+v", r)
	}
	for _, caller := range []mcpCaller{{Shared: true}, {}} {
		out, rpcErr := e.app.executeCommandTool(context.Background(), caller,
			executeCommandArgs{Host: e.conn.Name, Command: "true", AgentName: aiTestAgent, ClientRequestID: "line-1"})
		if rpcErr != nil {
			t.Fatalf("execute_command: %v", rpcErr)
		}
		s := mustJSON(t, out)
		if strings.Contains(s, "TOPSECRET") || strings.Contains(s, id) || strings.Contains(s, `"deduplicated":true`) {
			t.Fatalf("AI session line leaked through the dedupe: %s", s)
		}
		if !strings.Contains(s, "idempotency_conflict") {
			t.Fatalf("want idempotency_conflict: %s", s)
		}
	}
}

// 10 / 12. A GUI holding only the database lock is "another process using
// the same data": headless exits 3, not 1.
func TestHeadlessExitsLockedWhenStoreLockHeld(t *testing.T) {
	dir := isolateEnv(t)
	lock, err := acquireStoreLock(filepath.Join(dir, "ssh-gete.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	code, stderr := runHeadlessForExit(t, dir)
	if code != exitLocked {
		t.Fatalf("exit %d, want %d; stderr:\n%s", code, exitLocked, stderr)
	}
}

// 11. The shipped unit lets Go read the interface addresses (netlink), and a
// failure to read them is logged instead of silently dropping origins.
func TestSystemdUnitAllowsNetlinkAndAddrFailureIsLogged(t *testing.T) {
	b, err := os.ReadFile("deploy/systemd/ssh-gete.service")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "RestrictAddressFamilies=") {
			found = true
			if !strings.Contains(line, "AF_NETLINK") {
				t.Fatalf("unit lacks AF_NETLINK: %s", line)
			}
		}
	}
	if !found {
		t.Fatal("no RestrictAddressFamilies line")
	}

	old := webInterfaceAddrs
	webInterfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("address family not supported") }
	defer func() { webInterfaceAddrs = old }()
	var buf syncWriter
	var logged bytes.Buffer
	buf.w = &logged
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	if got := webInterfaceIPs(true); got != nil {
		t.Fatalf("ips = %v", got)
	}
	if !strings.Contains(logged.String(), "web.publicURL") {
		t.Fatalf("no warning logged: %q", logged.String())
	}
}

// 13. The GUI release builds stamp the version like the headless ones.
func TestReleaseGUIBuildStampsVersion(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "wails build") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			n++
			if !strings.Contains(line, "-X main.appVersion=") {
				t.Fatalf("wails build without the version: %s", line)
			}
		}
	}
	if n == 0 {
		t.Fatal("no wails build step")
	}
	doc, err := os.ReadFile("docs/spec/phase6-headless.md")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(doc, []byte("main.version=")) {
		t.Fatal("phase6-headless.md still names main.version")
	}
}
