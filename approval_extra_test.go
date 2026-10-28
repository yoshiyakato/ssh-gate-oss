package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newGapTestApp is newSessTestApp whose emit, the first time a
// sessions:changed reports runningTotal=1, calls onRunning synchronously.
// activate() emits that event right after the request became running and
// before the SSH start, so onRunning lands exactly in the N2 gap.
func newGapTestApp(t *testing.T, onRunning func(app *App)) (*App, *sessRecorder) {
	t.Helper()
	app, rec := newSessTestApp(t)
	var once sync.Once
	app.emit = func(name string, payload any) {
		rec.emit(name, payload)
		if p, ok := payload.(sessionsPayload); ok && name == sessionsChangedEvent && p.RunningTotal == 1 {
			once.Do(func() { onRunning(app) })
		}
	}
	return app, rec
}

// TestApproveDisconnectInGapDoesNotRun (N2): a disconnect between
// pending->running and the SSH start cancels the run; nothing reaches the
// host and the request ends failed/canceled.
func TestApproveDisconnectInGapDoesNotRun(t *testing.T) {
	var cancelled atomic.Int64
	var connID atomic.Value
	app, _ := newGapTestApp(t, func(app *App) {
		// DisconnectConnection's cancelling step; the full call would wait on
		// sessionsMu, which the emitting goroutine holds.
		cancelled.Store(int64(app.cancelExecsForConnection(connID.Load().(string), errDisconnected)))
	})
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("web-01"))
	connID.Store(conn.ID)
	req := queueOn(t, app, conn, "sleep 0.3", "agent")

	result := app.ApproveCommand(req.ID, "")
	if result.OK || result.Code != "canceled" {
		t.Fatalf("approve: %+v, want canceled", result)
	}
	if cancelled.Load() != 1 {
		t.Fatalf("disconnect cancelled %d runs in the gap, want 1", cancelled.Load())
	}
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("command reached the host after disconnect: %v", execs)
	}
	got, _ := app.findRequest(req.ID)
	if got.Status != stateFailed || got.ErrorKind != "canceled" {
		t.Fatalf("status=%s kind=%s, want failed/canceled", got.Status, got.ErrorKind)
	}
	if app.runningTotal.Load() != 0 || sessSummary(t, app, conn.ID).Running != 0 {
		t.Fatalf("still counted as running")
	}
}

// TestApproveDisconnectBeforeTransitionKeepsPending (N2): a run cancelled
// while only reserved never leaves pending.
func TestApproveDisconnectBeforeTransitionKeepsPending(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("web-01"))
	req := queueOn(t, app, conn, "echo hi", "agent")

	res, ok := app.reserveExec(app.appCtx, conn.ID)
	if !ok {
		t.Fatal("reserve refused")
	}
	if n := app.cancelExecsForConnection(conn.ID, errDisconnected); n != 1 {
		t.Fatalf("reservation not cancellable: %d", n)
	}
	if cause := stopCauseOf(res.ctx); cause != errDisconnected {
		t.Fatalf("cause=%v", cause)
	}
	res.release()
	res.release() // idempotent
	if got, _ := app.findRequest(req.ID); got.Status != statePending {
		t.Fatalf("status=%s", got.Status)
	}
}

// TestBypassDisconnectInGapDoesNotRun (N2): same gap on the
// approval-bypass path (auto_running recorded, SSH not yet started).
func TestBypassDisconnectInGapDoesNotRun(t *testing.T) {
	var connID atomic.Value
	app, _ := newGapTestApp(t, func(app *App) {
		app.cancelExecsForConnection(connID.Load().(string), errDisconnected)
	})
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	connID.Store(conn.ID)
	app.setAgentApprovalBypass("trusted", true)
	issueAgentToken(t, app, "trusted")

	out, _ := app.executeCommandTool(context.Background(), agentCaller(app, "trusted"),
		executeCommandArgs{Host: "lab", Command: "echo gap", AgentName: "trusted"})
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("command reached the host after disconnect: %v (%v)", execs, out)
	}
	got, _ := bypassedRequest(app, conn.ID, "echo gap")
	if got.Status != stateAutoFailed || got.ErrorKind != "canceled" {
		t.Fatalf("status=%s kind=%s, want auto_failed/canceled", got.Status, got.ErrorKind)
	}
	if app.runningTotal.Load() != 0 {
		t.Fatalf("runningTotal=%d", app.runningTotal.Load())
	}
}

// TestReservationIsSilentUntilActivated (merge audit #2): a reservation is
// neither counted in runningTotal nor announced; only activation is.
func TestReservationIsSilentUntilActivated(t *testing.T) {
	app, rec := newSessTestApp(t)
	before := len(rec.named(sessionsChangedEvent))
	res, ok := app.reserveExec(app.appCtx, "conn-x")
	if !ok {
		t.Fatal("reserve refused")
	}
	if app.runningTotal.Load() != 0 {
		t.Fatalf("reservation counted: %d", app.runningTotal.Load())
	}
	res.release()
	if n := len(rec.named(sessionsChangedEvent)); n != before {
		t.Fatalf("unactivated reservation emitted %d sessions:changed", n-before)
	}

	res, _ = app.reserveExec(app.appCtx, "conn-x")
	res.activate()
	res.activate()
	if app.runningTotal.Load() != 1 {
		t.Fatalf("activated runningTotal=%d", app.runningTotal.Load())
	}
	res.release()
	if app.runningTotal.Load() != 0 {
		t.Fatalf("released runningTotal=%d", app.runningTotal.Load())
	}
	if n := len(rec.named(sessionsChangedEvent)); n != before+2 {
		t.Fatalf("activate+release emitted %d events, want 2", n-before)
	}
}

// TestLosingApprovalsAreNeverCounted (merge audit #2, -race): of many
// concurrent approvals one wins; the losers never show up as running.
func TestLosingApprovalsAreNeverCounted(t *testing.T) {
	app, rec := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo once", "agent")
	before := len(rec.named(sessionsChangedEvent))

	var wg sync.WaitGroup
	var okCount atomic.Int64
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if app.ApproveCommand(req.ID, "").OK {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if okCount.Load() != 1 || len(srv.Execs()) != 1 {
		t.Fatalf("ok=%d execs=%d", okCount.Load(), len(srv.Execs()))
	}
	events := rec.named(sessionsChangedEvent)[before:]
	if len(events) != 2 {
		t.Fatalf("got %d sessions:changed, want 2 (start, end)", len(events))
	}
	for _, ev := range events {
		if total := ev.payload["runningTotal"].(float64); total > 1 {
			t.Fatalf("runningTotal=%v during approval race", total)
		}
	}
	if app.runningTotal.Load() != 0 {
		t.Fatalf("runningTotal=%d", app.runningTotal.Load())
	}
}

// TestQueuedTimeoutAppliesOnApproval (N11): timeout_seconds given to a
// queued execute_command bounds the approved run.
func TestQueuedTimeoutAppliesOnApproval(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))

	out, isErr := app.executeCommandTool(context.Background(), mcpCaller{},
		executeCommandArgs{Host: "lab", Command: "sleep 5", AgentName: "agent", TimeoutSeconds: 1})
	if isErr != nil {
		t.Fatalf("execute_command: %v", isErr)
	}
	req, ok := bypassedRequest(app, conn.ID, "sleep 5")
	if !ok || req.Status != statePending {
		t.Fatalf("not queued: %v", out)
	}
	if got := requestTimeoutOf(req); got != 1 {
		t.Fatalf("stored timeout=%d, want 1", got)
	}
	start := time.Now()
	result := app.ApproveCommand(req.ID, "")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("approved run ignored timeout_seconds=1: took %s", elapsed)
	}
	if result.OK || !strings.Contains(result.Message, req.ID) {
		t.Fatalf("approve: %+v, want a failed run", result)
	}
	got, _ := app.findRequest(req.ID)
	if got.ErrorKind != "timeout" {
		t.Fatalf("status=%s kind=%s, want timeout", got.Status, got.ErrorKind)
	}
}

// TestQueuedTimeoutOverridesShortConnectionTimeout (N11): a longer
// timeout_seconds wins over the connection's command timeout.
func TestQueuedTimeoutOverridesShortConnectionTimeout(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	c := srv.Connection("lab")
	c.CommandTimeout = 1
	conn := addTrustedConnection(t, app, c)

	if _, rpcErr := app.executeCommandTool(context.Background(), mcpCaller{},
		executeCommandArgs{Host: "lab", Command: "sleep 1.5", AgentName: "agent", TimeoutSeconds: 10}); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	req, ok := bypassedRequest(app, conn.ID, "sleep 1.5")
	if !ok {
		t.Fatal("not queued")
	}
	if result := app.ApproveCommand(req.ID, ""); !result.OK {
		t.Fatalf("approve: %+v", result)
	}
	if got, _ := app.findRequest(req.ID); got.Status != stateSucceeded {
		t.Fatalf("status=%s kind=%s, want succeeded", got.Status, got.ErrorKind)
	}
}

// TestQueuedTimeoutPersisted (N11): timeout_seconds is stored in
// command_requests.timeout_seconds (capped at 3600), so an approval after a
// restart still uses it.
func TestQueuedTimeoutPersisted(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))

	a, err := app.queueCommandWithTimeout(conn, "echo a", "", "agent", "Low", "", 7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := app.queueCommandWithTimeout(conn, "echo b", "", "agent", "Low", "", 9999)
	if err != nil {
		t.Fatal(err)
	}
	c, err := app.queueCommandWithTimeout(conn, "echo c", "", "agent", "Low", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want int
	}{{a.ID, 7}, {b.ID, 3600}, {c.ID, 0}} {
		row, found, err := app.store.loadCommandRequest(tc.id)
		if err != nil || !found {
			t.Fatalf("load %s: found=%v err=%v", tc.id, found, err)
		}
		if requestTimeoutOf(row) != tc.want {
			t.Fatalf("%s stored timeout=%d, want %d", tc.id, requestTimeoutOf(row), tc.want)
		}
	}
}

// TestApproveReportsUnsavedResult (audit #37): when SQLite refuses the final
// state of an approved run, the GUI gets a storage error (not "approved and
// ran"), and the outcome is still shown in memory.
func TestApproveReportsUnsavedResult(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req, done := sessStartApproved(t, app, conn, "sleep 1")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool { return app.runningTotal.Load() == 1 })
	if err := app.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	var result ActionResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("approve did not return")
	}
	if result.OK || result.Code != "storage" {
		t.Fatalf("approve: %+v, want a storage error", result)
	}
	if got, _ := app.findRequest(req.ID); got.Status != stateSucceeded {
		t.Fatalf("memory state=%s, want succeeded", got.Status)
	}
	if app.GetInitialData().StorageError == "" {
		t.Fatal("storageError not set")
	}
}

// TestBypassReportsUnsavedResult (audit #37): execute_command answers
// isError with error_kind "storage" when the outcome could not be saved.
func TestBypassReportsUnsavedResult(t *testing.T) {
	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	app.setAgentApprovalBypass("trusted", true)
	issueAgentToken(t, app, "trusted")
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for app.runningTotal.Load() != 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		_ = app.store.db.Close()
	}()
	out, rpcErr := app.executeCommandTool(context.Background(), agentCaller(app, "trusted"),
		executeCommandArgs{Host: "lab", Command: "sleep 1", AgentName: "trusted"})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"isError":true`) || !strings.Contains(string(raw), `storage`) {
		t.Fatalf("execute_command: %s, want isError storage", raw)
	}
	// The run is released after the response; let it finish so it does not
	// touch globals the next test swaps.
	app.execWG.Wait()
}
