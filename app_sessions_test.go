package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sessEvent is one emitted Wails event, with the payload round-tripped
// through JSON so tests see exactly what the frontend would.
type sessEvent struct {
	name    string
	payload map[string]any
}

type sessRecorder struct {
	mu     sync.Mutex
	events []sessEvent
}

func (r *sessRecorder) emit(name string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	r.mu.Lock()
	r.events = append(r.events, sessEvent{name: name, payload: decoded})
	r.mu.Unlock()
}

func (r *sessRecorder) named(name string) []sessEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []sessEvent
	for _, ev := range r.events {
		if ev.name == name {
			out = append(out, ev)
		}
	}
	return out
}

// exitsFor returns the terminal:exit payloads of one session id.
func (r *sessRecorder) exitsFor(id string) []map[string]any {
	var out []map[string]any
	for _, ev := range r.named(terminalExitEvent) {
		if ev.payload["id"] == id {
			out = append(out, ev.payload)
		}
	}
	return out
}

// newSessTestApp is newTestApp without the seed connection, with emitted
// events recorded instead of going to the Wails runtime.
func newSessTestApp(t *testing.T) (*App, *sessRecorder) {
	t.Helper()
	isolateEnv(t)
	t.Setenv("SSH_GETE_NO_SEED", "1")
	app := NewApp()
	rec := &sessRecorder{}
	app.emit = rec.emit
	t.Cleanup(func() { app.shutdown(context.Background()) })
	return app, rec
}

func sessWaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sessSummary(t *testing.T, app *App, connID string) SessionSummary {
	t.Helper()
	for _, s := range app.sessionSummaries() {
		if s.ConnID == connID {
			return s
		}
	}
	t.Fatalf("no session summary for %s", connID)
	return SessionSummary{}
}

func sessOpenTab(t *testing.T, app *App, id string, conn Connection) {
	t.Helper()
	result := app.StartTerminal(id, conn.ID, 80, 24)
	if !result.OK {
		t.Fatalf("StartTerminal %s: %s (%s)", id, result.Message, result.Code)
	}
}

// sessStartApproved queues command on conn and approves it in the background.
// The returned channel yields the approval result.
func sessStartApproved(t *testing.T, app *App, conn Connection, command string) (CommandRequest, <-chan ActionResult) {
	t.Helper()
	req := queueOn(t, app, conn, command, "agent")
	done := make(chan ActionResult, 1)
	go func() { done <- app.ApproveCommand(req.ID, "") }()
	return req, done
}

func TestDashboardSessionsShape(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	a := addTestConnection(t, app, srv.Connection("web-01"))
	b := addTestConnection(t, app, srv.Connection("web-02"))
	serial := addTestConnection(t, app, Connection{Name: "router", Type: "Serial", SerialPort: "/dev/ttyFAKE0", BaudRate: 9600})

	data := app.GetInitialData()
	if len(data.Sessions) != 3 {
		t.Fatalf("sessions=%d, want 3", len(data.Sessions))
	}
	for i, want := range []Connection{a, b, serial} {
		s := data.Sessions[i]
		if s.ConnID != want.ID || s.ConnName != want.Name {
			t.Fatalf("sessions[%d]=%+v, want %s in a.conns order", i, s, want.Name)
		}
		// A serial row without a tab reports whether its device is there
		// (phase4-serial-patch §7.4); /dev/ttyFAKE0 does not exist.
		wantState := connStateDisconnected
		if want.Type == "Serial" {
			wantState = connStateMissing
		}
		if s.ConnState != wantState || s.Shells != 0 || s.Running != 0 {
			t.Fatalf("sessions[%d]=%+v, want idle %s", i, s, wantState)
		}
	}
	if data.Sessions[0].Kind != "ssh" || data.Sessions[2].Kind != "serial" {
		t.Fatalf("kinds: %q %q", data.Sessions[0].Kind, data.Sessions[2].Kind)
	}
	if data.Sessions[2].Label != "/dev/ttyFAKE0 @9600" || data.Sessions[0].Label != "" {
		t.Fatalf("labels: %q %q", data.Sessions[0].Label, data.Sessions[2].Label)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"terminals":[]`, `"runningTotal":0`, `"storageError":""`, `"connState":"disconnected"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("dashboard JSON lacks %s: %s", want, raw)
		}
	}
}

func TestConnStateFromTabs(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))

	sessOpenTab(t, app, "TERM-a", conn)
	s := sessSummary(t, app, conn.ID)
	if s.ConnState != connStateConnected || s.Shells != 1 || s.Connecting != 0 {
		t.Fatalf("after open: %+v", s)
	}
	if _, err := time.Parse(time.RFC3339, s.ConnectedSince); err != nil {
		t.Fatalf("connectedSince %q: %v", s.ConnectedSince, err)
	}
	if got := app.GetInitialData().Terminals; len(got) != 1 || got[0].ID != "TERM-a" {
		t.Fatalf("terminals=%+v", got)
	}

	app.CloseTerminal("TERM-a")
	sessWaitFor(t, 2*time.Second, "disconnected", func() bool {
		return sessSummary(t, app, conn.ID).ConnState == connStateDisconnected
	})
	if s := sessSummary(t, app, conn.ID); s.ConnectedSince != "" || s.LastError != "" {
		t.Fatalf("after user close: %+v", s)
	}
}

func TestConnStateErrorAfterConnLost(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))

	sessOpenTab(t, app, "TERM-a", conn)
	srv.KillAll()
	sessWaitFor(t, 3*time.Second, "error state", func() bool {
		return sessSummary(t, app, conn.ID).ConnState == connStateError
	})
	if s := sessSummary(t, app, conn.ID); s.LastError == "" || s.Shells != 0 {
		t.Fatalf("after conn lost: %+v", s)
	}

	sessOpenTab(t, app, "TERM-b", conn)
	if s := sessSummary(t, app, conn.ID); s.ConnState != connStateConnected {
		t.Fatalf("after reopen: %+v", s)
	}
}

func TestRunningCounter(t *testing.T) {
	app, rec := newSessTestApp(t)
	srv := newSSHTestServer(t)
	// Approved runs refuse an unrecorded host key (TOFU, phase 3).
	conn := addTrustedConnection(t, app, srv.Connection("web-01"))

	_, done := sessStartApproved(t, app, conn, "sleep 1")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool {
		return sessSummary(t, app, conn.ID).Running == 1
	})
	data := app.GetInitialData()
	if data.RunningTotal != 1 {
		t.Fatalf("runningTotal=%d, want 1", data.RunningTotal)
	}
	if s := sessSummary(t, app, conn.ID); s.ConnState != connStateDisconnected {
		t.Fatalf("running must not change connState: %+v", s)
	}

	if result := <-done; !result.OK {
		t.Fatalf("approve: %s", result.Message)
	}
	if s := sessSummary(t, app, conn.ID); s.Running != 0 || app.GetInitialData().RunningTotal != 0 {
		t.Fatalf("after finish: %+v", s)
	}
	// Start and end each published a snapshot.
	var sawOne, sawZero bool
	for _, ev := range rec.named(sessionsChangedEvent) {
		switch ev.payload["runningTotal"] {
		case float64(1):
			sawOne = true
		case float64(0):
			sawZero = sawOne
		}
	}
	if !sawOne || !sawZero {
		t.Fatalf("sessions:changed did not report running 1 then 0")
	}
}

func TestDisconnectClosesTabsAndCancelsExec(t *testing.T) {
	app, rec := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))

	sessOpenTab(t, app, "TERM-a", conn)
	sessOpenTab(t, app, "TERM-b", conn)
	req, done := sessStartApproved(t, app, conn, "sleep 5")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool {
		return sessSummary(t, app, conn.ID).Running == 1
	})

	start := time.Now()
	result := app.DisconnectConnection(conn.ID)
	if !result.OK || result.ID != conn.ID {
		t.Fatalf("disconnect: %+v", result)
	}
	if !strings.Contains(result.Message, "ターミナル 2 件") || !strings.Contains(result.Message, "実行中コマンド 1 件") {
		t.Fatalf("message lacks counts: %s", result.Message)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled command did not return within 1s")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("disconnect took %s", elapsed)
	}
	for _, id := range []string{"TERM-a", "TERM-b"} {
		sessWaitFor(t, 2*time.Second, "exit of "+id, func() bool { return len(rec.exitsFor(id)) > 0 })
		exits := rec.exitsFor(id)
		if len(exits) != 1 || exits[0]["reason"] != "user" {
			t.Fatalf("%s exits=%v, want one reason=user", id, exits)
		}
	}
	got, _ := app.findRequest(req.ID)
	if got.Status != stateFailed || got.ErrorKind != "canceled" {
		t.Fatalf("request status=%s errorKind=%s, want failed/canceled", got.Status, got.ErrorKind)
	}
	if s := sessSummary(t, app, conn.ID); s.ConnState != connStateDisconnected || s.Running != 0 || s.Shells != 0 {
		t.Fatalf("after disconnect: %+v", s)
	}

	if r := app.DisconnectConnection("missing"); r.OK || r.Code != "not_found" {
		t.Fatalf("unknown id: %+v", r)
	}
}

func TestDisconnectDoesNotTouchOtherConnection(t *testing.T) {
	app, rec := newSessTestApp(t)
	srvA := newSSHTestServer(t)
	srvB := newSSHTestServer(t)
	connA := addTestConnection(t, app, srvA.Connection("web-a"))
	connB := addTestConnection(t, app, srvB.Connection("web-b"))

	sessOpenTab(t, app, "TERM-a", connA)
	sessOpenTab(t, app, "TERM-b", connB)
	reqB, doneB := sessStartApproved(t, app, connB, "sleep 1")
	sessWaitFor(t, 3*time.Second, "running on B", func() bool {
		return sessSummary(t, app, connB.ID).Running == 1
	})

	result := app.DisconnectConnection(connA.ID)
	if !strings.Contains(result.Message, "ターミナル 1 件") || !strings.Contains(result.Message, "実行中コマンド 0 件") {
		t.Fatalf("message: %s", result.Message)
	}
	infos := app.ListTerminalSessions()
	if len(infos) != 1 || infos[0].ID != "TERM-b" {
		t.Fatalf("remaining terminals=%+v", infos)
	}
	if len(rec.exitsFor("TERM-b")) != 0 {
		t.Fatal("TERM-b must not exit")
	}
	if s := sessSummary(t, app, connB.ID); s.Running != 1 || s.ConnState != connStateConnected {
		t.Fatalf("B after disconnecting A: %+v", s)
	}
	if r := <-doneB; !r.OK {
		t.Fatalf("B approve: %s", r.Message)
	}
	if got, _ := app.findRequest(reqB.ID); got.Status != stateSucceeded {
		t.Fatalf("B request status=%s", got.Status)
	}
}

func TestShutdownInterruptsRunning(t *testing.T) {
	old := shutdownExecGrace
	shutdownExecGrace = 2 * time.Second
	t.Cleanup(func() { shutdownExecGrace = old })

	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))
	sessOpenTab(t, app, "TERM-a", conn)
	req, done := sessStartApproved(t, app, conn, "sleep 30")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool {
		return sessSummary(t, app, conn.ID).Running == 1
	})

	start := time.Now()
	app.shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > shutdownExecGrace+3*time.Second {
		t.Fatalf("shutdown took %s", elapsed)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("approve did not return after shutdown")
	}
	if got, _ := app.findRequest(req.ID); got.Status != stateInterrupted {
		t.Fatalf("request status=%s, want interrupted", got.Status)
	}
	if infos := app.ListTerminalSessions(); len(infos) != 0 {
		t.Fatalf("terminals after shutdown=%+v", infos)
	}
	// Executions started after shutdown are cancelled at once and not counted.
	ctx, finish := app.trackExec(context.Background(), conn.ID)
	defer finish()
	if context.Cause(ctx) != errShuttingDown || app.runningTotal.Load() != 0 {
		t.Fatalf("post-shutdown exec: cause=%v running=%d", context.Cause(ctx), app.runningTotal.Load())
	}
}

// TestShutdownInterruptsMCPExec is the MCP-path variant: an auto-run
// execute_command parents on r.Context(), so shutdown must cancel it with
// errShuttingDown before the HTTP server closes that request context.
func TestShutdownInterruptsMCPExec(t *testing.T) {
	old := shutdownExecGrace
	shutdownExecGrace = 2 * time.Second
	t.Cleanup(func() { shutdownExecGrace = old })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	app, _ := newSessTestApp(t)
	// Set the free port first: GenerateMCPToken (enableBearer) restarts the
	// listener, which must not bind the default 8787.
	app.mu.Lock()
	app.mcp.ListenAddress = "127.0.0.1"
	app.mcp.Port = port
	app.mcp.Enabled = true
	app.mu.Unlock()
	enableBearer(t, app)
	srv := newSSHTestServer(t)
	// Since phase 3 a bypass needs the agent's own token and MCP runs need a
	// recorded host key.
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	app.setAgentApprovalBypass("trusted", true)
	token := issueAgentToken(t, app, "trusted")
	if err := app.restartMCPServer(); err != nil {
		t.Fatal(err)
	}

	body := toolCall("execute_command", `{"host":"lab","command":"sleep 30","agent_name":"trusted"}`)
	httpReq, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	go func() {
		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	sessWaitFor(t, 3*time.Second, "running=1", func() bool {
		return app.runningTotal.Load() == 1
	})

	// §5.0: shutdown waits for the grace period, then cancels what is left.
	oldGrace := shutdownExecGrace
	shutdownExecGrace = 500 * time.Millisecond
	t.Cleanup(func() { shutdownExecGrace = oldGrace })
	start := time.Now()
	app.shutdown(context.Background())
	elapsed := time.Since(start)
	if elapsed > shutdownExecGrace+1500*time.Millisecond {
		t.Fatalf("shutdown took %s (stalled on the MCP server deadline)", elapsed)
	}
	if elapsed < shutdownExecGrace {
		t.Fatalf("shutdown took %s, want at least the %s grace period", elapsed, shutdownExecGrace)
	}
	var got CommandRequest
	app.mu.RLock()
	for _, r := range app.requests {
		if r.ConnectionID == conn.ID && r.Command == "sleep 30" {
			got = r
		}
	}
	app.mu.RUnlock()
	if got.Status != stateInterrupted {
		t.Fatalf("request status=%s kind=%s, want interrupted", got.Status, got.ErrorKind)
	}
}

func TestTrackExecCancelCause(t *testing.T) {
	app, _ := newSessTestApp(t)
	ctxA, doneA := app.trackExec(context.Background(), "conn-a")
	ctxB, doneB := app.trackExec(context.Background(), "conn-b")
	defer doneB()
	if got := app.runningCounts(); got["conn-a"] != 1 || got["conn-b"] != 1 {
		t.Fatalf("runningCounts=%v", got)
	}
	if n := app.cancelExecsForConnection("conn-a", errDisconnected); n != 1 {
		t.Fatalf("cancelled %d, want 1", n)
	}
	if context.Cause(ctxA) != errDisconnected {
		t.Fatalf("cause=%v", context.Cause(ctxA))
	}
	if ctxB.Err() != nil {
		t.Fatal("other connection's exec was cancelled")
	}
	doneA()
	doneA() // idempotent
	if app.runningTotal.Load() != 1 {
		t.Fatalf("runningTotal=%d, want 1", app.runningTotal.Load())
	}
}

func TestSessionsSeqMonotonic(t *testing.T) {
	app, rec := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))

	before := len(rec.named(sessionsChangedEvent)) // SaveConnection already published one
	first := app.GetInitialData().SessionsSeq
	sessOpenTab(t, app, "TERM-a", conn)
	_, finish := app.trackExec(context.Background(), conn.ID)
	finish()
	app.CloseTerminal("TERM-a")
	sessWaitFor(t, 2*time.Second, "exit", func() bool { return len(rec.exitsFor("TERM-a")) > 0 })
	last := app.GetInitialData().SessionsSeq

	events := rec.named(sessionsChangedEvent)[before:]
	if len(events) < 4 {
		t.Fatalf("only %d sessions:changed events", len(events))
	}
	prev := first
	for _, ev := range events {
		seq := uint64(ev.payload["sessionsSeq"].(float64))
		if seq <= prev {
			t.Fatalf("seq %d after %d is not increasing", seq, prev)
		}
		prev = seq
	}
	if last <= prev {
		t.Fatalf("GetInitialData seq %d not after last event %d", last, prev)
	}
}

func TestSaveConnectionKeepsTabs(t *testing.T) {
	app, rec := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))
	sessOpenTab(t, app, "TERM-a", conn)

	conn.Name = "web-01-renamed"
	conn.Credential = "" // password is write-only; blank keeps it
	if result := app.SaveConnection(conn); !result.OK || result.ID != conn.ID {
		t.Fatalf("save: %+v", result)
	}
	infos := app.ListTerminalSessions()
	if len(infos) != 1 || infos[0].ConnName != "web-01-renamed" {
		t.Fatalf("terminals after rename=%+v", infos)
	}
	if len(rec.exitsFor("TERM-a")) != 0 {
		t.Fatal("saving must not close the tab")
	}
	data := app.GetInitialData()
	if len(data.Sessions) != 1 || data.Sessions[0].ConnName != "web-01-renamed" || data.Sessions[0].Shells != 1 {
		t.Fatalf("sessions after rename=%+v", data.Sessions)
	}
}

func TestGetInitialDataNoDeadlock(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = app.GetInitialData()
				_ = app.activeWork()
			}
		}()
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < 5; i++ {
			id := fmt.Sprintf("TERM-loop-%d", i)
			if r := app.StartTerminal(id, conn.ID, 80, 24); !r.OK {
				t.Errorf("start %s: %s", id, r.Message)
				return
			}
			_, done := app.trackExec(context.Background(), conn.ID)
			done()
			app.CloseTerminal(id)
		}
		app.DisconnectConnection(conn.ID)
	}()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("deadlock: tab open/close did not finish")
	}
	close(stop)
	wg.Wait()
}

func TestActiveWork(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("web-01"))
	if w := app.activeWork(); w.any() {
		t.Fatalf("idle app reports work: %+v", w)
	}
	queueOn(t, app, conn, "uptime", "agent")
	sessOpenTab(t, app, "TERM-a", conn)
	_, done := app.trackExec(context.Background(), conn.ID)
	defer done()
	if w := app.activeWork(); w.Terminals != 1 || w.Running != 1 || w.Pending != 1 || !w.any() {
		t.Fatalf("activeWork=%+v", w)
	}
	if s := sessSummary(t, app, conn.ID); s.Pending != 1 {
		t.Fatalf("pending=%d", s.Pending)
	}
}

// bypassedRequest returns the newest request of agent on conn.
func bypassedRequest(app *App, connID string, command string) (CommandRequest, bool) {
	app.mu.RLock()
	defer app.mu.RUnlock()
	for _, r := range app.requests {
		if r.ConnectionID == connID && r.Command == command {
			return r, true
		}
	}
	return CommandRequest{}, false
}

// TestDisconnectCancelsBypassedMCPRun: an approval-bypassed run is tracked
// like a GUI-approved one, so DisconnectConnection cancels it; it is recorded
// as auto_failed / canceled (a disconnect, not an interruption).
func TestDisconnectCancelsBypassedMCPRun(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	app.setAgentApprovalBypass("trusted", true)
	issueAgentToken(t, app, "trusted")

	answer := make(chan any, 1)
	go func() {
		out, _ := app.executeCommandTool(context.Background(), agentCaller(app, "trusted"),
			executeCommandArgs{Host: "lab", Command: "sleep 30", AgentName: "trusted"})
		answer <- out
	}()
	sessWaitFor(t, 3*time.Second, "running=1", func() bool { return sessSummary(t, app, conn.ID).Running == 1 })
	if r := app.DisconnectConnection(conn.ID); !strings.Contains(r.Message, "実行中コマンド 1 件") {
		t.Fatalf("disconnect: %+v", r)
	}
	select {
	case <-answer:
	case <-time.After(2 * time.Second):
		t.Fatal("bypassed run was not cancelled by DisconnectConnection")
	}
	got, _ := bypassedRequest(app, conn.ID, "sleep 30")
	if got.Status != stateAutoFailed || got.ErrorKind != "canceled" {
		t.Fatalf("request status=%s kind=%s, want auto_failed/canceled", got.Status, got.ErrorKind)
	}
	if app.runningTotal.Load() != 0 {
		t.Fatalf("runningTotal=%d after cancel", app.runningTotal.Load())
	}
}

// TestClientGoneInterruptsBypassedRun: while the agent waits synchronously,
// an HTTP disconnect cancels the run (errClientGone) and it is recorded as
// interrupted.
func TestClientGoneInterruptsBypassedRun(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	app.setAgentApprovalBypass("trusted", true)
	issueAgentToken(t, app, "trusted")

	httpCtx, hangUp := context.WithCancel(context.Background())
	answer := make(chan any, 1)
	go func() {
		out, _ := app.executeCommandTool(httpCtx, agentCaller(app, "trusted"),
			executeCommandArgs{Host: "lab", Command: "sleep 30", AgentName: "trusted"})
		answer <- out
	}()
	sessWaitFor(t, 3*time.Second, "running=1", func() bool { return app.runningTotal.Load() == 1 })
	hangUp()
	<-answer
	sessWaitFor(t, 2*time.Second, "interrupted", func() bool {
		got, _ := bypassedRequest(app, conn.ID, "sleep 30")
		return got.Status == stateInterrupted && got.ErrorKind == "canceled"
	})
	sessWaitFor(t, time.Second, "running=0", func() bool { return app.runningTotal.Load() == 0 })
}
