package main

// Fake transport for AI session tests (phase7 §9.1 aisession_fake_test.go):
// it records every line that reached it, so the tests can prove that a line
// that was not approved never gets a byte through (§3.5).

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAIScript is how the fake answers one line.
type fakeAIScript struct {
	Output string
	Exit   int
	NewCwd string
	// Block keeps the line running until Interrupt or release.
	Block bool
	Err   error
}

type fakeAITransport struct {
	out    io.Writer
	cwd    string
	script func(plan aiLinePlan) fakeAIScript

	mu         sync.Mutex
	lines      []aiLinePlan
	cur        *fakeAILine
	interrupts int
	closed     bool
	done       chan struct{}
	doneOnce   sync.Once
	endReason  string
}

type fakeAILine struct {
	done chan struct{}
	once sync.Once
	oc   aiTransportOutcome
}

func (l *fakeAILine) Done() <-chan struct{}       { return l.done }
func (l *fakeAILine) Outcome() aiTransportOutcome { <-l.done; return l.oc }
func (l *fakeAILine) finish(oc aiTransportOutcome) {
	l.once.Do(func() { l.oc = oc; close(l.done) })
}

func (t *fakeAITransport) StartLine(ctx context.Context, plan aiLinePlan) (aiLineHandle, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errAITransportClosed
	}
	if t.cur != nil {
		t.mu.Unlock()
		return nil, errAITransportBusy
	}
	t.lines = append(t.lines, plan)
	sc := fakeAIScript{Output: "ok\n"}
	if t.script != nil {
		sc = t.script(plan)
	}
	l := &fakeAILine{done: make(chan struct{})}
	if sc.Block {
		t.cur = l
	}
	t.mu.Unlock()
	if sc.Output != "" {
		_, _ = t.out.Write([]byte(sc.Output))
	}
	if !sc.Block {
		code := sc.Exit
		l.finish(aiTransportOutcome{ExitCode: &code, NewCwd: sc.NewCwd, Err: sc.Err})
	}
	return l, nil
}

func (t *fakeAITransport) Interrupt() error {
	t.mu.Lock()
	t.interrupts++
	l := t.cur
	t.cur = nil
	t.mu.Unlock()
	if l != nil {
		_, _ = t.out.Write([]byte("^C\n"))
		code := 130
		l.finish(aiTransportOutcome{ExitCode: &code})
	}
	return nil
}

func (t *fakeAITransport) Ready() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed && t.cur == nil
}

func (t *fakeAITransport) Cwd() string { return t.cwd }

func (t *fakeAITransport) Close() error {
	t.mu.Lock()
	t.closed = true
	l := t.cur
	t.cur = nil
	t.mu.Unlock()
	if l != nil {
		l.finish(aiTransportOutcome{Err: errAITransportClosed})
	}
	t.end(aiCloseConnLost)
	return nil
}

func (t *fakeAITransport) Done() <-chan struct{} { return t.done }

func (t *fakeAITransport) EndReason() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endReason
}

func (t *fakeAITransport) end(reason string) {
	t.doneOnce.Do(func() {
		t.mu.Lock()
		if t.endReason == "" {
			t.endReason = reason
		}
		t.mu.Unlock()
		close(t.done)
	})
}

// Delivered is every line that reached the transport.
func (t *fakeAITransport) Delivered() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.lines))
	for i, p := range t.lines {
		out[i] = p.Line
	}
	return out
}

func (t *fakeAITransport) Plans() []aiLinePlan {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]aiLinePlan(nil), t.lines...)
}

// fakeAIFactory hands out fake transports and remembers them.
type fakeAIFactory struct {
	mu     sync.Mutex
	script func(plan aiLinePlan) fakeAIScript
	list   []*fakeAITransport
	err    error
}

func (f *fakeAIFactory) open(ctx context.Context, conn Connection, opt aiTransportOptions) (aiTransport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	t := &fakeAITransport{out: opt.Out, cwd: "/home/tester", script: f.script, done: make(chan struct{})}
	f.list = append(f.list, t)
	return t, nil
}

func (f *fakeAIFactory) last() *fakeAITransport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.list) == 0 {
		return nil
	}
	return f.list[len(f.list)-1]
}

// execLikeScript models exec mode: cd reports its target through NewCwd.
func execLikeScript(plan aiLinePlan) fakeAIScript {
	switch {
	case plan.CdProbe:
		target := strings.TrimSpace(strings.TrimPrefix(plan.Line, "cd"))
		if !strings.HasPrefix(target, "/") {
			target = strings.TrimRight(plan.Cwd, "/") + "/" + target
		}
		return fakeAIScript{NewCwd: target}
	case plan.Line == "pwd":
		return fakeAIScript{Output: plan.Cwd + "\n"}
	case strings.HasPrefix(plan.Line, "sleep"), strings.HasPrefix(plan.Line, "tail -f"):
		return fakeAIScript{Block: true}
	case strings.HasPrefix(plan.Line, "false"):
		return fakeAIScript{Exit: 1}
	case strings.HasPrefix(plan.Line, "echo "):
		return fakeAIScript{Output: strings.TrimPrefix(plan.Line, "echo ") + "\n"}
	}
	return fakeAIScript{Output: "ok\n"}
}

const aiTestAgent = "claude-code"

// aiTestCaller is the mcpCaller authenticateMCP builds for aiTestAgent's
// current token: main binds bypass to the authenticating token (N28), so the
// caller carries its hash.
func aiTestCaller(app *App) mcpCaller { return agentCaller(app, aiTestAgent) }

type aiTestEnv struct {
	app     *App
	factory *fakeAIFactory
	conn    Connection
}

// newAITestApp is an App with AI sessions enabled, the fake transport for
// exec and shell mode, an agent token for aiTestAgent and one connection.
func newAITestApp(t *testing.T) *aiTestEnv {
	t.Helper()
	app := newTestApp(t)
	f := &fakeAIFactory{script: execLikeScript}
	m := app.aiMgr()
	m.transports[aiModeExec] = f.open
	m.transports[aiModeShell] = f.open
	conn := addTestConnection(t, app, Connection{Name: "web-01", Type: "SSH", Host: "10.0.0.9", Port: 22, User: "deploy",
		AuthMethod: "パスワード保管", Credential: "pw", ConnectTimeout: 5, CommandTimeout: 10, Tags: []string{}})
	issueAgentToken(t, app, aiTestAgent)
	settings := defaultAISessionSettings()
	settings.Enabled = true
	if r := app.SaveAISessionSettings(settings); !r.OK {
		t.Fatalf("enable: %+v", r)
	}
	return &aiTestEnv{app: app, factory: f, conn: conn}
}

func (e *aiTestEnv) open(t *testing.T) string {
	t.Helper()
	view, err := e.app.aiSessionMCPOpen(context.Background(), aiTestAgent, aiTestCaller(e.app),
		openSessionArgs{Host: e.conn.Name, Reason: "調査", Mode: aiModeExec, Shell: aiShellSh})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return view.SessionID
}

var aiTestCRID int64
var aiTestCRIDMu sync.Mutex

func nextCRID() string {
	aiTestCRIDMu.Lock()
	defer aiTestCRIDMu.Unlock()
	aiTestCRID++
	return fmt.Sprintf("crid-%d-%d", time.Now().UnixNano(), aiTestCRID)
}

func (e *aiTestEnv) send(t *testing.T, id, line string) mcpAISessionSendView {
	t.Helper()
	wait := 5
	view, err := e.app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller(e.app),
		sendLineArgs{SessionID: id, Line: line, ClientRequestID: nextCRID(), WaitSeconds: &wait})
	if err != nil {
		t.Fatalf("send %q: %v", line, err)
	}
	return view
}

func (e *aiTestEnv) sendErr(id, line string) error {
	wait := 5
	_, err := e.app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller(e.app),
		sendLineArgs{SessionID: id, Line: line, ClientRequestID: nextCRID(), WaitSeconds: &wait})
	return err
}

// grantBypass lets aiTestAgent skip approval (up to Medium, the default).
func (e *aiTestEnv) grantBypass(t *testing.T) {
	t.Helper()
	if r := e.app.setAgentApprovalBypass(aiTestAgent, true); !r.OK {
		t.Fatalf("bypass: %+v", r)
	}
}

func aiErrCode(err error) string {
	if e, ok := err.(*mcpAISessionErr); ok {
		return e.Code
	}
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *aiTestEnv) requestStatus(t *testing.T, id string) string {
	t.Helper()
	req, ok := e.app.findRequest(id)
	if !ok {
		t.Fatalf("request %s not found", id)
	}
	return req.Status
}
