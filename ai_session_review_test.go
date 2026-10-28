package main

// Regression tests for the REQ-016 adversarial review findings.

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 1. In shell mode an export / assignment the standalone parser cannot read
// (quotes, expansions) changes the shell's environment behind the gateway:
// it must taint the session, and a dangerous name must say dangerous_env.
func TestAISessionShellOpaqueEnvChangeTaints(t *testing.T) {
	shell := sessionRiskState{Mode: aiModeShell, Env: map[string]string{}}
	exec := sessionRiskState{Mode: aiModeExec, Env: map[string]string{}}
	cases := []struct {
		line      string
		st        sessionRiskState
		effect    string
		dangerous bool
	}{
		{`export LD_PRELOAD="/tmp/x.so"`, shell, lineEffectStateChange, true},
		{`export PATH=$HOME/bin:$PATH`, shell, lineEffectStateChange, true},
		{`export FOO="a b"`, shell, lineEffectStateChange, false},
		{`FOO="bar"`, shell, lineEffectStateChange, false},
		{`FOO=$(id -u)`, shell, lineEffectStateChange, false},
		{`ls && export "BASH_ENV=/tmp/x"`, shell, lineEffectStateChange, true},
		{`declare -x LD_LIBRARY_PATH="/tmp"`, shell, lineEffectStateChange, true},
		{`readonly PROMPT_COMMAND="x"`, shell, lineEffectStateChange, true},
		// A prefix assignment only reaches that one command.
		{`FOO="1" ls`, shell, lineEffectNone, false},
		{`env FOO="1" ls`, shell, lineEffectNone, false},
		// exec mode: nothing persists, but the reason is still shown.
		{`export LD_PRELOAD="/tmp/x.so"`, exec, "", true},
	}
	for _, c := range cases {
		got := classifySessionLine(c.line, c.st)
		if c.effect != "" && got.Effect != c.effect {
			t.Errorf("%q (%s): effect=%s reasons=%v, want %s", c.line, c.st.Mode, got.Effect, got.Reasons, c.effect)
		}
		if has := slices.Contains(got.Reasons, "dangerous_env"); has != c.dangerous {
			t.Errorf("%q (%s): dangerous_env=%v reasons=%v", c.line, c.st.Mode, has, got.Reasons)
		}
		if c.dangerous && got.Risk != "High" {
			t.Errorf("%q: risk=%s, want High", c.line, got.Risk)
		}
	}
	// The plain forms keep the gateway overlay path.
	if got := classifySessionLine("export LANG=C", shell); got.Effect != lineEffectEnv {
		t.Errorf("plain export = %+v", got)
	}
}

// 2. get_request_status and get_command_history must not show another
// agent's AI session line (output, session_id) — only read_output's owner
// check used to guard it.
func TestAISessionRequestStatusIsolation(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	issueAgentToken(t, e.app, "evil")
	e.app.mu.Lock()
	e.app.mcp.BearerEnabled = true
	e.app.mu.Unlock()

	id := e.open(t)
	sent := e.send(t, id, "echo TOPSECRET-OUTPUT")
	if !strings.Contains(sent.Output, "TOPSECRET-OUTPUT") {
		t.Fatalf("send = %+v", sent)
	}
	reqID := sent.RequestID

	status := func(caller mcpCaller) string {
		t.Helper()
		out, rpcErr := e.app.requestStatusTool(context.Background(), caller, requestStatusArgs{RequestIDs: []string{reqID}})
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		return mustJSON(t, out)
	}
	if got := status(aiTestCaller(e.app)); !strings.Contains(got, "TOPSECRET-OUTPUT") || !strings.Contains(got, id) {
		t.Fatalf("owner status = %s", got)
	}
	for name, caller := range map[string]mcpCaller{
		"other agent":  {Agent: "evil"},
		"shared token": {Shared: true},
		"anonymous":    {},
	} {
		got := status(caller)
		if strings.Contains(got, "TOPSECRET") || strings.Contains(got, id) || !strings.Contains(got, "not_found") {
			t.Errorf("%s reads the line: %s", name, got)
		}
	}

	history := func(caller mcpCaller) string {
		t.Helper()
		out, rpcErr := e.app.commandHistoryTool(caller, historyArgs{AllAgents: true})
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		return mustJSON(t, out)
	}
	if got := history(mcpCaller{Agent: "evil"}); strings.Contains(got, reqID) || strings.Contains(got, id) {
		t.Errorf("all_agents history shows another agent's session line: %s", got)
	}
	if got := history(mcpCaller{Shared: true}); strings.Contains(got, reqID) {
		t.Errorf("shared-token history shows a session line: %s", got)
	}
	if got := history(aiTestCaller(e.app)); !strings.Contains(got, reqID) {
		t.Errorf("owner history lacks its own line: %s", got)
	}
}

// fakeOpenAsync starts an open whose onOpen blocks until release.
func blockingOnOpen(m *aiSessionManager) (entered chan struct{}, release chan struct{}, inserted *atomic.Int32) {
	entered, release = make(chan struct{}), make(chan struct{})
	inserted = &atomic.Int32{}
	m.hooks.onOpen = func(AISessionInfo, string) error {
		close(entered)
		<-release
		inserted.Add(1)
		return nil
	}
	return entered, release, inserted
}

type closeRecorder struct {
	mu    sync.Mutex
	infos []AISessionInfo
}

func (r *closeRecorder) hook(info AISessionInfo) {
	r.mu.Lock()
	r.infos = append(r.infos, info)
	r.mu.Unlock()
}

func (r *closeRecorder) list() []AISessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AISessionInfo(nil), r.infos...)
}

// 3 and 5. A close that gives up waiting on the open (aiCloseGrace) must not
// let the opener revive the session; the opener closes its transport, and
// the inserted row still gets its close recorded (once, real reason).
func TestAISessionCloseTimeoutDuringOnOpen(t *testing.T) {
	old := aiCloseGrace
	aiCloseGrace = 50 * time.Millisecond
	t.Cleanup(func() { aiCloseGrace = old })

	m, _, f := newFakeManager(t)
	entered, release, inserted := blockingOnOpen(m)
	rec := &closeRecorder{}
	m.hooks.onClose = rec.hook

	type result struct {
		s   *aiSession
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, _, err := m.open(context.Background(), aiOpenRequest{Owner: "a1", Conn: Connection{ID: "c1", Type: "SSH"}, Reason: "r"})
		done <- result{s, err}
	}()
	<-entered
	var id string
	m.mu.Lock()
	for sid := range m.sessions {
		id = sid
	}
	m.mu.Unlock()
	if !m.closeSession(id, aiCloseHuman, aiClosedByGUI) {
		t.Fatal("close refused")
	}
	// close() finished after its grace; now onOpen returns.
	close(release)
	r := <-done
	if r.err == nil || aiSessErrorCode(r.err) != aiErrSessionClosed {
		t.Fatalf("open after close = %v (session %v)", r.err, r.s)
	}
	if tr := f.last(); tr == nil || tr.Ready() {
		t.Fatal("the opener did not close its transport")
	}
	if _, err := m.lookup(id, "a1", false); aiSessErrorCode(err) != aiErrSessionClosed {
		t.Fatalf("lookup = %v", err)
	}
	if c := m.Counts(); c.Open != 0 {
		t.Fatalf("counts = %+v", c)
	}
	got := rec.list()
	if inserted.Load() != 1 || len(got) != 1 || got[0].CloseReason != aiCloseHuman {
		t.Fatalf("inserted=%d onClose=%+v", inserted.Load(), got)
	}
}

// 5. A close that arrives while onOpen runs (within the grace) records the
// close of the inserted row with its real reason.
func TestAISessionCloseDuringOnOpenRecordsClose(t *testing.T) {
	m, _, f := newFakeManager(t)
	entered, release, inserted := blockingOnOpen(m)
	rec := &closeRecorder{}
	m.hooks.onClose = rec.hook

	done := make(chan error, 1)
	go func() {
		_, _, err := m.open(context.Background(), aiOpenRequest{Owner: "a1", Conn: Connection{ID: "c1", Type: "SSH"}, Reason: "r"})
		done <- err
	}()
	<-entered
	var id string
	m.mu.Lock()
	for sid := range m.sessions {
		id = sid
	}
	m.mu.Unlock()
	closed := make(chan bool, 1)
	go func() { closed <- m.closeSession(id, aiCloseHuman, aiClosedByGUI) }()
	waitFor(t, "closing", func() bool {
		m.mu.Lock()
		s := m.sessions[id]
		m.mu.Unlock()
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.state == aiStateClosing
	})
	close(release)
	if !<-closed {
		t.Fatal("close refused")
	}
	if err := <-done; aiSessErrorCode(err) != aiErrSessionClosed {
		t.Fatalf("open = %v", err)
	}
	if tr := f.last(); tr == nil || tr.Ready() {
		t.Fatal("transport left open")
	}
	got := rec.list()
	if inserted.Load() != 1 || len(got) != 1 || got[0].CloseReason != aiCloseHuman {
		t.Fatalf("inserted=%d onClose=%+v", inserted.Load(), got)
	}
}

// 4. Opening and closing in a loop must not pile up full-size rings in the
// tombstones.
func TestAISessionTombstonesAreBounded(t *testing.T) {
	m, _, _ := newFakeManager(t)
	var last *aiSession
	for i := 0; i < aiTombstonesPerOwner+10; i++ {
		s := fakeOpen(t, m, "a1")
		_, _ = s.Output().Write([]byte(strings.Repeat("x", 100*1024) + "\nTAIL\n"))
		if !m.closeSession(s.id, aiCloseAgent, aiClosedByAgent("a")) {
			t.Fatal("close refused")
		}
		last = s
	}
	m.mu.Lock()
	n := len(m.tombstones)
	_, kept := m.tombstones[last.id]
	m.mu.Unlock()
	if n != aiTombstonesPerOwner || !kept {
		t.Fatalf("tombstones = %d (latest kept %v), want %d", n, kept, aiTombstonesPerOwner)
	}
	o := last.Output()
	o.mu.Lock()
	rawSize, textSize := len(o.raw.data), len(o.text.data)
	o.mu.Unlock()
	if rawSize > aiTombstoneRingBytes || textSize > aiTombstoneRingBytes {
		t.Fatalf("tombstone rings = %d/%d bytes", rawSize, textSize)
	}
	// read_output still gets the tail, with the dropped head as missed bytes.
	r, err := o.ReadText(0, 1<<20)
	if err != nil || !strings.HasSuffix(r.Text, "TAIL\n") || r.MissedBytes == 0 || r.EndCursor != uint64(100*1024+6) {
		t.Fatalf("tombstone read = missed %d end %d err %v", r.MissedBytes, r.EndCursor, err)
	}

	// The total cap holds across owners.
	oldTotal := aiTombstonesTotal
	aiTombstonesTotal = aiTombstonesPerOwner + 3
	t.Cleanup(func() { aiTombstonesTotal = oldTotal })
	for i := 0; i < 5; i++ {
		s := fakeOpen(t, m, "a2")
		m.closeSession(s.id, aiCloseAgent, aiClosedByAgent("a"))
	}
	m.mu.Lock()
	n = len(m.tombstones)
	m.mu.Unlock()
	if n != aiTombstonesTotal {
		t.Fatalf("total tombstones = %d, want %d", n, aiTombstonesTotal)
	}
}

func TestAIRingShrinkKeepsCursors(t *testing.T) {
	r := newAIRing(16)
	r.write([]byte("0123456789abcdefghij")) // wraps
	r.shrink(4)
	if len(r.data) != 4 || r.end != 20 || r.start() != 16 || string(r.read(16, 10)) != "ghij" {
		t.Fatalf("shrunk = %+v %q", r, r.read(r.start(), 10))
	}
	r.write([]byte("kl"))
	if string(r.read(r.start(), 10)) != "ijkl" {
		t.Fatalf("after write = %q", r.read(r.start(), 10))
	}
	small := newAIRing(16)
	small.write([]byte("ab"))
	small.shrink(8)
	if string(small.read(0, 10)) != "ab" || small.start() != 0 {
		t.Fatalf("small = %q", small.read(0, 10))
	}
}
