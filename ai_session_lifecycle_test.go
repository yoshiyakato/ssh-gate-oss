package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAIHost drives the manager with a settable clock (no App).
type fakeAIHost struct {
	mu      sync.Mutex
	t       time.Time
	tokens  map[string]bool
	fps     map[string]string
	events  []string
	changes int
}

func newFakeAIHost() *fakeAIHost {
	return &fakeAIHost{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), tokens: map[string]bool{"a1": true, "a2": true},
		fps: map[string]string{"c1": "u@h:22"}}
}

func (h *fakeAIHost) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *fakeAIHost) advance(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}
func (h *fakeAIHost) emit(name string, payload any) {
	h.mu.Lock()
	h.events = append(h.events, name)
	h.mu.Unlock()
}
func (h *fakeAIHost) sessionsChanged() { h.mu.Lock(); h.changes++; h.mu.Unlock() }
func (h *fakeAIHost) agentHasToken(agent string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tokens[agent]
}
func (h *fakeAIHost) targetFP(connID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fp, ok := h.fps[connID]
	return fp, ok
}
func (h *fakeAIHost) settleLines(g *aiLineGate, reason string) { g.beginClose(reason) }
func (h *fakeAIHost) reconcileLine(g *aiLineGate)              {}

func newFakeManager(t *testing.T) (*aiSessionManager, *fakeAIHost, *fakeAIFactory) {
	t.Helper()
	h := newFakeAIHost()
	m := newAISessionManager(h, aiSessionHooks{})
	f := &fakeAIFactory{script: execLikeScript}
	m.transports[aiModeExec] = f.open
	s := defaultAISessionSettings()
	s.Enabled = true
	m.setSettings(s)
	return m, h, f
}

func fakeOpen(t *testing.T, m *aiSessionManager, owner string) *aiSession {
	t.Helper()
	s, _, err := m.open(context.Background(), aiOpenRequest{Owner: owner, Conn: Connection{ID: "c1", Name: "web", Type: "SSH", Host: "h", Port: 22, User: "u"}, Reason: "r"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func TestAISessionIdleAndMaxLifetime(t *testing.T) {
	m, h, _ := newFakeManager(t)
	s := fakeOpen(t, m, "a1")
	h.advance(9 * time.Minute)
	if m.sweep() != 0 {
		t.Fatal("closed before idle timeout")
	}
	s.touch()
	h.advance(9 * time.Minute)
	if m.sweep() != 0 {
		t.Fatal("touch did not restart the idle timer")
	}
	h.advance(2 * time.Minute)
	if m.sweep() != 1 || s.snap().CloseReason != aiCloseIdle {
		t.Fatalf("idle: %+v", s.snap())
	}

	// A pending line keeps it from idling out; the max lifetime still wins.
	s2 := fakeOpen(t, m, "a1")
	g := s2.Gate()
	if _, err := g.reserve("REQ-1", "ls", h.now(), time.Second); err != nil {
		t.Fatal(err)
	}
	g.markPending("REQ-1")
	h.advance(30 * time.Minute)
	if m.sweep() != 0 || s2.State() != aiStatePendingApproval {
		t.Fatalf("pending session idled out: %s", s2.State())
	}
	h.advance(31 * time.Minute)
	if m.sweep() != 1 || s2.snap().CloseReason != aiCloseMaxLifetime {
		t.Fatalf("max lifetime: %+v", s2.snap())
	}
}

func TestAISessionSweeperRevokedDeletedChanged(t *testing.T) {
	m, h, _ := newFakeManager(t)
	s1 := fakeOpen(t, m, "a1")
	h.mu.Lock()
	h.tokens["a1"] = false
	h.mu.Unlock()
	m.sweep()
	if s1.snap().CloseReason != aiCloseTokenRevoked {
		t.Fatalf("revoked: %+v", s1.snap())
	}
	s2 := fakeOpen(t, m, "a2")
	h.mu.Lock()
	h.fps["c1"] = "u@other:22"
	h.mu.Unlock()
	m.sweep()
	if s2.snap().CloseReason != aiCloseTargetChanged {
		t.Fatalf("changed: %+v", s2.snap())
	}
	h.mu.Lock()
	h.fps["c1"] = "u@h:22"
	h.mu.Unlock()
	s3 := fakeOpen(t, m, "a2")
	h.mu.Lock()
	delete(h.fps, "c1")
	h.mu.Unlock()
	m.sweep()
	if s3.snap().CloseReason != aiCloseConnDeleted {
		t.Fatalf("deleted: %+v", s3.snap())
	}
	// Tombstones last aiTombstoneTTL.
	if _, err := m.lookup(s3.id, "a2", false); aiSessErrorCode(err) != aiErrSessionClosed {
		t.Fatalf("tombstone lookup = %v", err)
	}
	h.advance(aiTombstoneTTL)
	m.sweep()
	if _, err := m.lookup(s3.id, "a2", false); aiSessErrorCode(err) != aiErrSessionNotFound {
		t.Fatalf("expired tombstone lookup = %v", err)
	}
}

func TestAISessionLimitsAndShutdown(t *testing.T) {
	m, _, f := newFakeManager(t)
	st := m.Settings()
	st.Total = 2
	m.setSettings(st)
	fakeOpen(t, m, "a1")
	fakeOpen(t, m, "a2")
	_, _, err := m.open(context.Background(), aiOpenRequest{Owner: "a1", Conn: Connection{ID: "c1", Type: "SSH"}, Reason: "r"})
	if se, ok := err.(*aiSessError); !ok || se.Code != aiErrSessionLimit || se.Extra["scope"] != "total" {
		t.Fatalf("total limit = %v", err)
	}
	if c := m.Counts(); c.Open != 2 {
		t.Fatalf("counts = %+v", c)
	}
	if n := m.shutdown(aiCloseAppShutdown, time.Second); n != 2 {
		t.Fatalf("shutdown closed %d", n)
	}
	for _, tr := range f.list {
		if tr.Ready() {
			t.Fatal("transport not closed")
		}
	}
	_, _, err = m.open(context.Background(), aiOpenRequest{Owner: "a1", Conn: Connection{ID: "c1", Type: "SSH"}, Reason: "r"})
	if aiSessErrorCode(err) != aiErrShuttingDown {
		t.Fatalf("open after shutdown = %v", err)
	}
	if list := m.list("", true); len(list) != 2 || list[0].CloseReason != aiCloseAppShutdown {
		t.Fatalf("list = %+v", list)
	}
}

func TestAISessionRemoteEndClosesSession(t *testing.T) {
	m, _, f := newFakeManager(t)
	s := fakeOpen(t, m, "a1")
	f.last().end(aiCloseRemoteExit)
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed")
	}
	if s.snap().CloseReason != aiCloseRemoteExit {
		t.Fatalf("reason = %s", s.snap().CloseReason)
	}
}

func TestAISessionSerialAndOwnerChecks(t *testing.T) {
	m, _, _ := newFakeManager(t)
	_, _, err := m.open(context.Background(), aiOpenRequest{Owner: "a1", Conn: Connection{ID: "s", Type: "Serial"}, Reason: "r"})
	if aiSessErrorCode(err) != aiErrSerialNotSupported {
		t.Fatalf("serial = %v", err)
	}
	s := fakeOpen(t, m, "a1")
	if _, err := m.lookup(s.id, "a2", false); aiSessErrorCode(err) != aiErrNotOwner {
		t.Fatalf("other owner = %v", err)
	}
	if _, err := m.lookup("TERM-abc", "a1", false); aiSessErrorCode(err) != aiErrSessionNotFound {
		t.Fatalf("TERM id = %v", err)
	}
	if !strings.HasPrefix(s.id, "AIS-") || !aiSessionIDPattern.MatchString(s.id) {
		t.Fatalf("id = %s", s.id)
	}
}

// Shutdown: the running line is recorded interrupted and the session row is
// closed (app_shutdown) before the store closes.
func TestAISessionShutdownInterruptsAndRecords(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	f := &fakeAIFactory{script: execLikeScript}
	app.aiMgr().transports[aiModeExec] = f.open
	conn := addTestConnection(t, app, Connection{Name: "web-01", Type: "SSH", Host: "10.0.0.9", Port: 22, User: "deploy",
		AuthMethod: "パスワード保管", Credential: "pw", ConnectTimeout: 5, CommandTimeout: 10, Tags: []string{}})
	issueAgentToken(t, app, aiTestAgent)
	settings := defaultAISessionSettings()
	settings.Enabled = true
	app.SaveAISessionSettings(settings)
	app.setAgentApprovalBypass(aiTestAgent, true)
	open, err := app.aiSessionMCPOpen(context.Background(), aiTestAgent, aiTestCaller(app), openSessionArgs{Host: conn.Name, Reason: "r", Mode: "exec", Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	wait := 0
	sent, err := app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller(app), sendLineArgs{SessionID: open.SessionID, Line: "tail -f x", ClientRequestID: "s", WaitSeconds: &wait})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", func() bool { return f.last() != nil && !f.last().Ready() })
	app.shutdown(context.Background())
	req, _ := app.findRequest(sent.RequestID)
	if req.Status != stateInterrupted {
		t.Fatalf("status = %s", req.Status)
	}
	store, err := openStore(app.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	row, _, _ := store.loadAISessionRow(open.SessionID)
	if row.open() || row.CloseReason != aiCloseAppShutdown {
		t.Fatalf("row = %+v", row)
	}
}

// The MCP layer keeps its own closed lists of states, close reasons and
// error codes; a code the backend returns that is missing there would reach
// the agent as "internal". Keep the two sides in step.
func TestAISessionEnumsMatchMCPLists(t *testing.T) {
	states := []string{aiStateOpening, aiStateIdle, aiStatePendingApproval, aiStateBusy, aiStateClosing, aiStateClosed}
	if !slices.Equal(states, mcpAISessionStates) {
		t.Fatalf("states: backend %v, mcp %v", states, mcpAISessionStates)
	}
	if !slices.Equal(aiCloseReasons, mcpAISessionCloseReasons) {
		t.Fatalf("close reasons: backend %v, mcp %v", aiCloseReasons, mcpAISessionCloseReasons)
	}
	// invalid_settings is GUI-only (SaveAISessionSettings).
	for _, code := range []string{
		aiErrSessionNotFound, aiErrSessionClosed, aiErrNotOwner, aiErrSessionBusy, aiErrSessionLimit,
		aiErrAgentTokenRequired, aiErrDisabled, aiErrShuttingDown, aiErrShellNotAllowed, aiErrModeNotAvailable,
		aiErrSerialNotSupported, aiErrHostNotFound, aiErrAmbiguousHost, aiErrConnectFailed, aiErrInterruptRateLimit,
		aiErrInvalidCursor, aiErrInvalidArgument, aiErrStorage, aiErrTargetChanged, aiErrIdempotencyConflict,
		"line_rejected", "server_busy",
	} {
		if !slices.Contains(mcpAISessionErrorCodes, code) {
			t.Errorf("backend error %q is not in mcpAISessionErrorCodes", code)
		}
	}
}
