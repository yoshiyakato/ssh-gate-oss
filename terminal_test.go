package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// termShellServer is an in-process SSH server that serves interactive shells:
// it records pty-req / window-change sizes, greets with "welcome <user>" (a
// stand-in for the MOTD), then answers one command per input line:
//
//	echo <text>   prints text
//	exit <n>      exits with status n
//	yes           writes output until the channel closes
//
// Every input line is appended to lines. KillAll drops every client
// connection but keeps listening.
type termShellServer struct {
	listener net.Listener
	config   *ssh.ServerConfig

	mu    sync.Mutex
	conns []net.Conn
	sizes []string // "pty 132x40", "win 100x30"
	lines []string
}

func newTermShellServer(t *testing.T) *termShellServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == sshTestUser && string(password) == sshTestPassword {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &termShellServer{listener: listener, config: config}
	go s.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		s.KillAll()
	})
	return s
}

func (s *termShellServer) Connection(id, name string) Connection {
	return Connection{
		ID:             id,
		Name:           name,
		Type:           "SSH",
		Host:           "127.0.0.1",
		Port:           s.listener.Addr().(*net.TCPAddr).Port,
		User:           sshTestUser,
		AuthMethod:     "パスワード保管",
		Credential:     sshTestPassword,
		ConnectTimeout: 5,
		CommandTimeout: 10,
		Tags:           []string{},
	}
}

func (s *termShellServer) KillAll() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *termShellServer) PtySizes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sizes...)
}

func (s *termShellServer) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *termShellServer) serve() {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, nc)
		s.mu.Unlock()
		go s.handleConn(nc)
	}
}

func (s *termShellServer) handleConn(nc net.Conn) {
	_, chans, reqs, err := ssh.NewServerConn(nc, s.config)
	if err != nil {
		_ = nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *termShellServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	shell := make(chan struct{})
	go func() {
		started := false
		for req := range reqs {
			ok := s.handleRequest(req)
			if req.WantReply {
				_ = req.Reply(ok, nil)
			}
			if ok && req.Type == "shell" && !started {
				started = true
				close(shell)
			}
		}
	}()
	<-shell
	sendExitStatus(ch, s.shell(ch))
}

// handleRequest records pty sizes and accepts pty-req, window-change and shell.
func (s *termShellServer) handleRequest(req *ssh.Request) bool {
	if req.Type == "shell" {
		return true
	}
	size, ok := parsePtySize(req)
	if ok {
		s.mu.Lock()
		s.sizes = append(s.sizes, size)
		s.mu.Unlock()
	}
	return ok
}

func (s *termShellServer) shell(ch ssh.Channel) int {
	return runTestShell(ch, func(command string) {
		s.mu.Lock()
		s.lines = append(s.lines, command)
		s.mu.Unlock()
	})
}

// eventRecorder is the fake a.emit: it keeps every (name, payload).
type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
	gate   chan struct{} // when non-nil, terminal:data blocks until it is closed
}

type recordedEvent struct {
	name    string
	payload any
}

func (r *eventRecorder) emit(name string, payload any) {
	r.mu.Lock()
	gate := r.gate
	r.mu.Unlock()
	if gate != nil && name == terminalDataEvent {
		<-gate
	}
	r.mu.Lock()
	r.events = append(r.events, recordedEvent{name, payload})
	r.mu.Unlock()
}

func (r *eventRecorder) data(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.events {
		if p, ok := e.payload.(terminalDataPayload); ok && e.name == terminalDataEvent && p.ID == id {
			raw, _ := base64.StdEncoding.DecodeString(p.Data)
			b.Write(raw)
		}
	}
	return b.String()
}

func (r *eventRecorder) dataPayloads() []terminalDataPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []terminalDataPayload
	for _, e := range r.events {
		if p, ok := e.payload.(terminalDataPayload); ok && e.name == terminalDataEvent {
			out = append(out, p)
		}
	}
	return out
}

func (r *eventRecorder) exits(id string) []terminalExitPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []terminalExitPayload
	for _, e := range r.events {
		if p, ok := e.payload.(terminalExitPayload); ok && e.name == terminalExitEvent && p.ID == id {
			out = append(out, p)
		}
	}
	return out
}

func (r *eventRecorder) allExits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.name == terminalExitEvent {
			n++
		}
	}
	return n
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newTerminalTestApp(t *testing.T, conns ...Connection) (*App, *eventRecorder) {
	t.Helper()
	// A known_hosts per test: the OS reuses loopback ports, so a key one test
	// recorded for 127.0.0.1:<port> would otherwise mismatch the next test's
	// server on the same port.
	isolateEnv(t)
	rec := &eventRecorder{}
	a := &App{
		conns:          conns,
		sshPassphrases: map[string]string{},
		agentPolicies:  map[string]AgentPolicy{},
	}
	a.emit = rec.emit
	t.Cleanup(func() {
		rec.mu.Lock()
		if rec.gate != nil {
			select {
			case <-rec.gate:
			default:
				close(rec.gate)
			}
		}
		rec.mu.Unlock()
		a.CloseAllTerminals()
	})
	return a, rec
}

func mustStart(t *testing.T, a *App, id, connID string, cols, rows int) {
	t.Helper()
	res := a.StartTerminal(id, connID, cols, rows)
	if !res.OK {
		t.Fatalf("StartTerminal(%s): %+v", id, res)
	}
	if res.SessionID != id {
		t.Fatalf("SessionID = %q, want %q", res.SessionID, id)
	}
}

func mustSend(t *testing.T, a *App, id, data string) {
	t.Helper()
	if res := a.SendTerminalInput(id, data); !res.OK {
		t.Fatalf("SendTerminalInput(%s): %+v", id, res)
	}
}

func TestTerminalOutputIsolatedByID(t *testing.T) {
	srvA, srvB := newTermShellServer(t), newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srvA.Connection("ca", "web-a"), srvB.Connection("cb", "web-b"))
	mustStart(t, a, "TERM-a1", "ca", 80, 24)
	mustStart(t, a, "TERM-a2", "ca", 80, 24)
	mustStart(t, a, "TERM-b1", "cb", 80, 24)
	mustSend(t, a, "TERM-a1", "echo AAA\r")
	mustSend(t, a, "TERM-a2", "echo BBB\r")
	mustSend(t, a, "TERM-b1", "echo CCC\r")
	want := map[string]string{"TERM-a1": "AAA", "TERM-a2": "BBB", "TERM-b1": "CCC"}
	for id, s := range want {
		waitUntil(t, id+" output", func() bool { return strings.Contains(rec.data(id), s) })
	}
	for id, s := range want {
		for other, o := range want {
			if other != id && strings.Contains(rec.data(id), o) {
				t.Fatalf("%s output %q contains %s's %q", id, rec.data(id), other, s)
			}
		}
	}
	if got := len(a.ListTerminalSessions()); got != 3 {
		t.Fatalf("ListTerminalSessions = %d, want 3", got)
	}
}

func TestTerminalMOTDNotLost(t *testing.T) {
	srv := newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-motd", "c1", 80, 24)
	waitUntil(t, "welcome", func() bool { return strings.Contains(rec.data("TERM-motd"), "welcome "+sshTestUser) })
}

func TestTerminalInitialPtySize(t *testing.T) {
	srv := newTermShellServer(t)
	a, _ := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-size", "c1", 132, 40)
	if sizes := srv.PtySizes(); len(sizes) == 0 || sizes[0] != "pty 132x40" {
		t.Fatalf("pty sizes = %v, want first pty 132x40", sizes)
	}
	if res := a.ResizeTerminal("TERM-size", 100, 30); !res.OK {
		t.Fatalf("ResizeTerminal: %+v", res)
	}
	waitUntil(t, "window-change", func() bool {
		sizes := srv.PtySizes()
		return len(sizes) > 0 && sizes[len(sizes)-1] == "win 100x30"
	})
	if res := a.ResizeTerminal("TERM-size", 0, 30); res.Code != "invalid_size" {
		t.Fatalf("zero cols: %+v", res)
	}
	if res := a.ResizeTerminal("TERM-none", 80, 24); res.Code != "no_session" {
		t.Fatalf("unknown id: %+v", res)
	}
}

func TestKillAllExitsOnlyThatServer(t *testing.T) {
	srvA, srvB := newTermShellServer(t), newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srvA.Connection("ca", "web-a"), srvB.Connection("cb", "web-b"))
	mustStart(t, a, "TERM-a1", "ca", 80, 24)
	mustStart(t, a, "TERM-a2", "ca", 80, 24)
	mustStart(t, a, "TERM-b1", "cb", 80, 24)
	srvA.KillAll()
	for _, id := range []string{"TERM-a1", "TERM-a2"} {
		waitUntil(t, id+" exit", func() bool { return len(rec.exits(id)) > 0 })
	}
	time.Sleep(50 * time.Millisecond)
	for _, id := range []string{"TERM-a1", "TERM-a2"} {
		exits := rec.exits(id)
		if len(exits) != 1 || exits[0].Reason != exitReasonConnLost || exits[0].ExitCode != nil {
			t.Fatalf("%s exits = %+v, want one conn_lost", id, exits)
		}
	}
	if exits := rec.exits("TERM-b1"); len(exits) != 0 {
		t.Fatalf("TERM-b1 got exit %+v", exits)
	}
	infos := a.ListTerminalSessions()
	if len(infos) != 1 || infos[0].ID != "TERM-b1" || infos[0].Status != terminalStatusOpen {
		t.Fatalf("ListTerminalSessions = %+v, want only TERM-b1", infos)
	}
	rt := a.terminalRuntime()
	if rt["ca"].Open != 0 || rt["ca"].LastError == "" {
		t.Fatalf("runtime[ca] = %+v, want lastError and no tabs", rt["ca"])
	}
	if rt["cb"].Open != 1 || rt["cb"].LastError != "" || rt["cb"].Since.IsZero() {
		t.Fatalf("runtime[cb] = %+v", rt["cb"])
	}
	if counts := a.terminalCounts(); counts["ca"] != 0 || counts["cb"] != 1 {
		t.Fatalf("terminalCounts = %v", counts)
	}
}

func TestCloseOneTabKeepsOthers(t *testing.T) {
	srv := newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-one", "c1", 80, 24)
	mustStart(t, a, "TERM-two", "c1", 80, 24)
	if res := a.CloseTerminal("TERM-one"); !res.OK {
		t.Fatalf("CloseTerminal: %+v", res)
	}
	if res := a.CloseTerminal("TERM-one"); !res.OK {
		t.Fatalf("second CloseTerminal: %+v", res)
	}
	exits := rec.exits("TERM-one")
	if len(exits) != 1 || exits[0].Reason != exitReasonUser {
		t.Fatalf("exits = %+v, want one user", exits)
	}
	if res := a.SendTerminalInput("TERM-one", "echo x\r"); res.Code != "no_session" {
		t.Fatalf("input to closed tab: %+v", res)
	}
	mustSend(t, a, "TERM-two", "echo still-here\r")
	waitUntil(t, "TERM-two output", func() bool { return strings.Contains(rec.data("TERM-two"), "still-here") })
	if exits := rec.exits("TERM-two"); len(exits) != 0 {
		t.Fatalf("TERM-two exits = %+v", exits)
	}
}

func TestRemoteExitCode(t *testing.T) {
	srv := newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-exit", "c1", 80, 24)
	mustSend(t, a, "TERM-exit", "exit 3\r")
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-exit")) > 0 })
	exits := rec.exits("TERM-exit")
	if len(exits) != 1 || exits[0].Reason != exitReasonRemoteExit || exits[0].ExitCode == nil || *exits[0].ExitCode != 3 {
		t.Fatalf("exits = %+v, want remote_exit 3", exits)
	}
	if len(a.ListTerminalSessions()) != 0 {
		t.Fatal("exited tab still listed")
	}
}

func TestDuplicateAndInvalidID(t *testing.T) {
	srv := newTermShellServer(t)
	a, _ := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-dup", "c1", 80, 24)
	if res := a.StartTerminal("TERM-dup", "c1", 80, 24); res.Code != "duplicate_id" || res.SessionID != "TERM-dup" {
		t.Fatalf("duplicate: %+v", res)
	}
	for _, id := range []string{"foo", "TERM-", "TERM-a b", "TERM-" + strings.Repeat("x", 65)} {
		if res := a.StartTerminal(id, "c1", 80, 24); res.Code != "invalid_id" {
			t.Fatalf("%q: %+v", id, res)
		}
	}
	if res := a.StartTerminal("TERM-nf", "web-01", 80, 24); res.Code != "not_found" {
		t.Fatalf("lookup by name must fail: %+v", res)
	}
	if n := len(a.ListTerminalSessions()); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestCloseWhileConnecting(t *testing.T) {
	port := newSilentServer(t)
	conn := Connection{ID: "c1", Name: "silent", Type: "SSH", Host: "127.0.0.1", Port: port, User: sshTestUser,
		AuthMethod: "パスワード保管", Credential: sshTestPassword, ConnectTimeout: 10, Tags: []string{}}
	a, rec := newTerminalTestApp(t, conn)
	done := make(chan ActionResult, 1)
	go func() { done <- a.StartTerminal("TERM-slow", "c1", 80, 24) }()
	waitUntil(t, "connecting placeholder", func() bool {
		infos := a.ListTerminalSessions()
		return len(infos) == 1 && infos[0].Status == terminalStatusConnecting
	})
	if res := a.ResizeTerminal("TERM-slow", 90, 30); !res.OK {
		t.Fatalf("resize while connecting: %+v", res)
	}
	if rt := a.terminalRuntime()["c1"]; rt.Connecting != 1 || rt.Open != 0 {
		t.Fatalf("runtime = %+v", rt)
	}
	start := time.Now()
	a.CloseTerminal("TERM-slow")
	select {
	case res := <-done:
		if res.OK || res.Code != "canceled" || res.SessionID != "TERM-slow" {
			t.Fatalf("StartTerminal = %+v, want canceled", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StartTerminal did not return after CloseTerminal")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancel took %v", time.Since(start))
	}
	if n := rec.allExits(); n != 0 {
		t.Fatalf("got %d exit events, want none", n)
	}
	if len(a.ListTerminalSessions()) != 0 {
		t.Fatal("placeholder left behind")
	}
	if rt := a.terminalRuntime()["c1"]; rt.LastError != "" {
		t.Fatalf("canceled start recorded lastError %q", rt.LastError)
	}
}

func TestOutputCoalescing(t *testing.T) {
	old := terminalFlushInterval
	terminalFlushInterval = 200 * time.Millisecond
	t.Cleanup(func() { terminalFlushInterval = old })

	srv := newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-yes", "c1", 80, 24)
	mustSend(t, a, "TERM-yes", "yes\r")
	waitUntil(t, "1MiB of output", func() bool { return len(rec.data("TERM-yes")) > 1<<20 })
	a.CloseTerminal("TERM-yes")
	for _, p := range rec.dataPayloads() {
		raw, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > terminalFlushBytes {
			t.Fatalf("data event of %d bytes exceeds %d", len(raw), terminalFlushBytes)
		}
	}
}

func TestOutputPendingCap(t *testing.T) {
	oldCap := terminalPendingCap
	terminalPendingCap = 256 * 1024
	t.Cleanup(func() { terminalPendingCap = oldCap })

	srv := newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	gate := make(chan struct{})
	rec.mu.Lock()
	rec.gate = gate
	rec.mu.Unlock()
	mustStart(t, a, "TERM-cap", "c1", 80, 24)
	mustSend(t, a, "TERM-cap", "yes\r")

	a.termMu.Lock()
	ts := a.terms["TERM-cap"]
	a.termMu.Unlock()
	deadline := time.Now().Add(500 * time.Millisecond)
	sawFull := false
	for time.Now().Before(deadline) {
		n := ts.out.pending()
		if n > terminalPendingCap {
			t.Fatalf("pending %d exceeds cap %d", n, terminalPendingCap)
		}
		if n == terminalPendingCap {
			sawFull = true
		}
		time.Sleep(time.Millisecond)
	}
	if !sawFull {
		t.Fatal("buffer never reached the cap while emit was blocked")
	}
	rec.mu.Lock()
	rec.gate = nil
	rec.mu.Unlock()
	close(gate)
	waitUntil(t, "dropped report", func() bool {
		for _, p := range rec.dataPayloads() {
			if p.ID == "TERM-cap" && p.Dropped > 0 {
				return true
			}
		}
		return false
	})
}

func TestInputOrder(t *testing.T) {
	srv := newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-order", "c1", 80, 24)
	for i := range 1000 {
		mustSend(t, a, "TERM-order", fmt.Sprintf("seq %d\r", i))
	}
	mustSend(t, a, "TERM-order", "echo end\r")
	waitUntil(t, "end", func() bool { return strings.Contains(rec.data("TERM-order"), "end") })
	lines := srv.Lines()
	if len(lines) != 1001 {
		t.Fatalf("got %d lines, want 1001", len(lines))
	}
	for i := range 1000 {
		if want := fmt.Sprintf("seq %d", i); lines[i] != want {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want)
		}
	}
}

func TestInputTooLarge(t *testing.T) {
	srv := newTermShellServer(t)
	a, _ := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	mustStart(t, a, "TERM-big", "c1", 80, 24)
	if res := a.SendTerminalInput("TERM-big", strings.Repeat("x", terminalInputMax+1)); res.Code != "input_too_large" {
		t.Fatalf("oversized input: %+v", res)
	}
	if res := a.SendTerminalInput("TERM-none", "x"); res.Code != "no_session" {
		t.Fatalf("unknown id: %+v", res)
	}
}

func TestExitOnceUnderRace(t *testing.T) {
	for i := range 10 {
		srv := newTermShellServer(t)
		a, rec := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
		id := fmt.Sprintf("TERM-race-%d", i)
		mustStart(t, a, id, "c1", 80, 24)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); srv.KillAll() }()
		go func() { defer wg.Done(); a.CloseTerminal(id) }()
		go func() { defer wg.Done(); a.CloseTerminalsByConnection("c1") }()
		wg.Wait()
		waitUntil(t, "exit", func() bool { return len(rec.exits(id)) > 0 })
		time.Sleep(20 * time.Millisecond)
		if exits := rec.exits(id); len(exits) != 1 {
			t.Fatalf("exits = %+v, want exactly one", exits)
		}
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// TestInputEOFLeavesExitToSession: when stdin fails because the remote just
// closed, the pump must not take the exit Once; session.Wait's remote_exit
// (with its code) wins. A writer that fails while the session stays up still
// ends the tab with io_error after the grace.
func TestInputEOFLeavesExitToSession(t *testing.T) {
	old := terminalInputFailGrace
	terminalInputFailGrace = 300 * time.Millisecond
	t.Cleanup(func() { terminalInputFailGrace = old })

	newTS := func(a *App, id string) *terminalSession {
		ts := &terminalSession{
			id: id, connID: "c1", connName: "web-01", kind: "ssh",
			cancel:      func() {},
			input:       make(chan []byte, terminalInputQueue),
			out:         newOutputBuffer(func([]byte, int) {}),
			done:        make(chan struct{}),
			stdinWriter: failingWriter{io.EOF},
			status:      terminalStatusOpen,
		}
		a.termMu.Lock()
		a.ensureMaps()
		a.terms[id] = ts
		a.termMu.Unlock()
		return ts
	}

	a, rec := newTerminalTestApp(t)
	// pump runs the input pump and returns a channel closed when it returns,
	// so the grace variable is not restored while a pump still reads it.
	pump := func(ts *terminalSession) <-chan struct{} {
		exited := make(chan struct{})
		go func() { a.pumpTerminalInput(ts); close(exited) }()
		return exited
	}
	ts := newTS(a, "TERM-eof")
	eofPump := pump(ts)
	ts.input <- []byte("x")
	time.Sleep(50 * time.Millisecond)
	code := 0
	a.finishTerminal(ts, exitReasonRemoteExit, &code, "web-01 のセッションが終了しました")
	exits := rec.exits("TERM-eof")
	if len(exits) != 1 || exits[0].Reason != exitReasonRemoteExit || exits[0].ExitCode == nil || *exits[0].ExitCode != 0 {
		t.Fatalf("exits = %+v, want one remote_exit(0)", exits)
	}
	<-eofPump
	a.termMu.Lock()
	lastErr := a.lastErr["c1"]
	a.termMu.Unlock()
	if lastErr != "" {
		t.Fatalf("lastErr = %q after a clean exit", lastErr)
	}

	stuck := newTS(a, "TERM-stuck")
	stuckPump := pump(stuck)
	stuck.input <- []byte("x")
	<-stuckPump
	if got := rec.exits("TERM-stuck"); len(got) != 1 || got[0].Reason != exitReasonIOError {
		t.Fatalf("stuck exits = %+v, want one io_error", got)
	}
}

func TestCloseTerminalsByConnection(t *testing.T) {
	srvA, srvB := newTermShellServer(t), newTermShellServer(t)
	a, rec := newTerminalTestApp(t, srvA.Connection("ca", "web-a"), srvB.Connection("cb", "web-b"))
	mustStart(t, a, "TERM-a1", "ca", 80, 24)
	mustStart(t, a, "TERM-a2", "ca", 80, 24)
	mustStart(t, a, "TERM-b1", "cb", 80, 24)
	res := a.CloseTerminalsByConnection("ca")
	if !res.OK || res.ID != "ca" || res.Message != "2 件のターミナルを閉じました" {
		t.Fatalf("CloseTerminalsByConnection = %+v", res)
	}
	for _, id := range []string{"TERM-a1", "TERM-a2"} {
		if exits := rec.exits(id); len(exits) != 1 || exits[0].Reason != exitReasonUser {
			t.Fatalf("%s exits = %+v", id, exits)
		}
	}
	if infos := a.ListTerminalSessions(); len(infos) != 1 || infos[0].ID != "TERM-b1" {
		t.Fatalf("ListTerminalSessions = %+v", infos)
	}
	if n := a.closeTerminalsForConnection("ca"); n != 0 {
		t.Fatalf("second close = %d", n)
	}
}

func TestListTerminalSessionsOrderAndRename(t *testing.T) {
	a, _ := newTerminalTestApp(t, Connection{ID: "c1", Name: "renamed", Type: "SSH"})
	if infos := a.ListTerminalSessions(); infos == nil || len(infos) != 0 {
		t.Fatalf("empty list = %#v, want non-nil empty", infos)
	}
	base := time.Now()
	a.termMu.Lock()
	a.ensureMaps()
	for _, id := range []string{"TERM-b", "TERM-a", "TERM-c"} {
		started := base
		if id == "TERM-c" {
			started = base.Add(-time.Second)
		}
		a.terms[id] = &terminalSession{id: id, connID: "c1", connName: "old", kind: "ssh", startedAt: started,
			status: terminalStatusOpen, done: make(chan struct{}), input: make(chan []byte, 1)}
	}
	a.termMu.Unlock()
	infos := a.listTerminalInfos()
	got := []string{infos[0].ID, infos[1].ID, infos[2].ID}
	if strings.Join(got, ",") != "TERM-c,TERM-a,TERM-b" {
		t.Fatalf("order = %v", got)
	}
	if infos[0].ConnName != "renamed" {
		t.Fatalf("ConnName = %q, want current name", infos[0].ConnName)
	}
	a.termMu.Lock()
	a.terms = nil
	a.termMu.Unlock()
}

func TestNormalizeSerialPort(t *testing.T) {
	cases := []struct{ goos, in, want string }{
		{"darwin", "/dev/tty.usbserial-10", "/dev/cu.usbserial-10"},
		{"darwin", "/dev/cu.usbserial-10", "/dev/cu.usbserial-10"},
		{"windows", `\\.\com3`, "COM3"},
		{"windows", "com3", "COM3"},
		{"linux", "/dev/ttyUSB0", "/dev/ttyUSB0"},
	}
	for _, c := range cases {
		if got := normalizeSerialPort(c.goos, c.in); got != c.want {
			t.Errorf("normalizeSerialPort(%s, %q) = %q, want %q", c.goos, c.in, got, c.want)
		}
	}
}

func TestSerialReservation(t *testing.T) {
	a, _ := newTerminalTestApp(t,
		Connection{ID: "s1", Name: "board-1", Type: "Serial", SerialPort: "/dev/tty.usbserial-10", BaudRate: 115200},
		Connection{ID: "s2", Name: "board-2", Type: "Serial", SerialPort: "/dev/cu.usbserial-10", BaudRate: 9600},
	)
	norm := normalizeSerialPort("darwin", "/dev/tty.usbserial-10")
	a.termMu.Lock()
	a.ensureMaps()
	a.terms["TERM-s1"] = &terminalSession{id: "TERM-s1", connID: "s1", connName: "board-1", kind: "serial",
		port: "/dev/tty.usbserial-10", normPort: norm, status: terminalStatusOpen, done: make(chan struct{})}
	a.serialOwners[norm] = "TERM-s1"
	a.termMu.Unlock()
	if !a.serialPortInUse("s1") || a.serialPortInUse("s2") {
		t.Fatal("serialPortInUse mismatch")
	}
	if normalizeSerialPort(runtime.GOOS, "/dev/cu.usbserial-10") == norm {
		res := a.StartTerminal("TERM-s2", "s2", 80, 24)
		if res.Code != "port_in_use" || !strings.Contains(res.Message, "board-1") {
			t.Fatalf("second open of the same port: %+v", res)
		}
	}
	a.termMu.Lock()
	a.terms, a.serialOwners = nil, nil
	a.termMu.Unlock()
}

func TestSessionsChangedHook(t *testing.T) {
	srv := newTermShellServer(t)
	a, _ := newTerminalTestApp(t, srv.Connection("c1", "web-01"))
	var mu sync.Mutex
	calls := 0
	a.onSessionsChanged = func() {
		// Must be called with no terminal lock held.
		if !a.termMu.TryLock() {
			t.Error("sessionsChanged called while termMu is held")
		} else {
			a.termMu.Unlock()
		}
		mu.Lock()
		calls++
		mu.Unlock()
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	mustStart(t, a, "TERM-hook", "c1", 80, 24)
	afterOpen := count()
	if afterOpen < 2 {
		t.Fatalf("calls after open = %d, want placeholder + open", afterOpen)
	}
	a.CloseTerminal("TERM-hook")
	if count() <= afterOpen {
		t.Fatal("close did not call sessionsChanged")
	}
}

// #38 / N22: concurrent ResizeTerminal calls (Wails runs each on its own
// goroutine) must leave the PTY at the size stored last, not at whichever
// window change happened to run last.
func TestResizeAppliesLatestUnderRace(t *testing.T) {
	a, _ := newTerminalTestApp(t)
	var mu sync.Mutex
	var applied [][2]int
	ts := &terminalSession{
		id: "TERM-rs", connID: "c1", kind: "ssh", status: terminalStatusOpen,
		input: make(chan []byte, 1), done: make(chan struct{}), cancel: func() {},
		out: newOutputBuffer(func([]byte, int) {}),
		resize: func(cols, rows int) error {
			// Widen the window between reading the size and applying it.
			time.Sleep(time.Duration(cols%3) * time.Millisecond)
			mu.Lock()
			applied = append(applied, [2]int{cols, rows})
			mu.Unlock()
			return nil
		},
	}
	a.termMu.Lock()
	a.ensureMaps()
	a.terms[ts.id] = ts
	a.termMu.Unlock()
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for i := 1; i <= 16; i++ {
			wg.Add(1)
			go func(cols int) {
				defer wg.Done()
				if r := a.ResizeTerminal(ts.id, cols, 24); !r.OK {
					t.Errorf("resize: %+v", r)
				}
			}(80 + round*16 + i)
		}
		wg.Wait()
		a.termMu.Lock()
		want := [2]int{ts.cols, ts.rows}
		a.termMu.Unlock()
		mu.Lock()
		last := applied[len(applied)-1]
		mu.Unlock()
		if last != want {
			t.Fatalf("round %d: last applied %v, stored %v", round, last, want)
		}
	}
}

// A closing tab (reason set, closer not finished) refuses new input instead
// of queueing it for a writer that may already be closed (N1).
func TestSendInputRefusedWhileClosing(t *testing.T) {
	a, _ := newTerminalTestApp(t)
	ts := &terminalSession{
		id: "TERM-cl", connID: "c1", kind: "ssh", status: terminalStatusOpen,
		input: make(chan []byte, 1), done: make(chan struct{}), cancel: func() {},
		out: newOutputBuffer(func([]byte, int) {}),
	}
	a.termMu.Lock()
	a.ensureMaps()
	a.terms[ts.id] = ts
	a.termMu.Unlock()
	if r := a.SendTerminalInput(ts.id, "a"); !r.OK {
		t.Fatalf("open tab: %+v", r)
	}
	a.termMu.Lock()
	ts.reason = exitReasonUser
	a.termMu.Unlock()
	if r := a.SendTerminalInput(ts.id, "rm -rf ~\n"); r.OK || r.Code != "no_session" {
		t.Fatalf("closing tab: %+v", r)
	}
	if len(ts.input) != 1 {
		t.Fatalf("queued %d, want only the first", len(ts.input))
	}
}
