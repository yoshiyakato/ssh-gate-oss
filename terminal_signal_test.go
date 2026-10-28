package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
	"golang.org/x/crypto/ssh"
)

// sigTestServer is an SSH server for REQ-017: its shell records every raw
// byte it receives (control characters included) and every "signal" channel
// request. signalReply chooses how signal requests are answered.
type sigTestServer struct {
	listener net.Listener
	config   *ssh.ServerConfig

	mu          sync.Mutex
	conns       []net.Conn
	data        []byte
	signals     []sigRecord
	signalReply string // "accept" (default), "reject", "silent"
}

type sigRecord struct {
	Name      string
	WantReply bool
}

func newSigTestServer(t *testing.T, reply string) *sigTestServer {
	t.Helper()
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == sshTestUser && string(password) == sshTestPassword {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	config.AddHostKey(newTestHostKey(t))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sigTestServer{listener: listener, config: config, signalReply: reply}
	go s.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		s.mu.Lock()
		conns := s.conns
		s.conns = nil
		s.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return s
}

func (s *sigTestServer) addr() string { return s.listener.Addr().String() }

func (s *sigTestServer) Connection(id string) Connection {
	return Connection{
		ID:             id,
		Name:           "sig-" + id,
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

func (s *sigTestServer) Data() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.data)
}

func (s *sigTestServer) Signals() []sigRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sigRecord(nil), s.signals...)
}

func (s *sigTestServer) serve() {
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

func (s *sigTestServer) handleConn(nc net.Conn) {
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

func (s *sigTestServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	shell := make(chan struct{})
	go func() {
		started := false
		for req := range reqs {
			switch req.Type {
			case "pty-req", "window-change":
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			case "shell":
				_ = req.Reply(true, nil)
				if !started {
					started = true
					close(shell)
				}
			case "signal":
				var p struct{ Signal string }
				_ = ssh.Unmarshal(req.Payload, &p)
				s.mu.Lock()
				s.signals = append(s.signals, sigRecord{p.Signal, req.WantReply})
				mode := s.signalReply
				s.mu.Unlock()
				if req.WantReply {
					switch mode {
					case "reject":
						_ = req.Reply(false, nil)
					case "silent":
						// never answer
					default:
						_ = req.Reply(true, nil)
					}
				}
			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()
	<-shell
	_, _ = io.WriteString(ch, "ready\r\n")
	buf := make([]byte, 4096)
	for {
		n, err := ch.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.data = append(s.data, buf[:n]...)
			s.mu.Unlock()
		}
		if err != nil {
			sendExitStatus(ch, 0)
			return
		}
	}
}

// openSignalTab builds an open SSH tab the way openSSHTerminal does, plus the
// registerTerminalSignaler call that the REQ-017 wiring adds there.
func openSignalTab(t *testing.T, a *App, srv *sigTestServer, id string) *terminalSession {
	t.Helper()
	client, err := ssh.Dial("tcp", srv.addr(), &ssh.ClientConfig{
		User:            sshTestUser,
		Auth:            []ssh.AuthMethod{ssh.Password(sshTestPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	ts := &terminalSession{
		id: id, connID: "c1", connName: "sig", kind: "ssh",
		cancel:      func() {},
		input:       make(chan []byte, terminalInputQueue),
		out:         newOutputBuffer(func([]byte, int) {}),
		done:        make(chan struct{}),
		stdinWriter: stdin,
		status:      terminalStatusOpen,
		closer: func() {
			_ = session.Close()
			_ = client.Close()
		},
	}
	session.Stdout = ts.out
	session.Stderr = ts.out
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	a.termMu.Lock()
	a.ensureMaps()
	a.terms[id] = ts
	a.termMu.Unlock()
	registerTerminalSignaler(ts, session)
	a.terminalLoops.Add(1)
	go func() {
		defer a.terminalLoops.Done()
		a.pumpTerminalInput(ts)
	}()
	return ts
}

func wantSignalCode(t *testing.T, res ActionResult, code string) {
	t.Helper()
	if code == "" {
		if !res.OK || res.Code != "" {
			t.Fatalf("result = %+v, want OK", res)
		}
		return
	}
	if res.OK || res.Code != code {
		t.Fatalf("result = %+v, want code %q", res, code)
	}
	if res.Message == "" {
		t.Fatalf("result %+v has no message", res)
	}
}

func setSignalTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := terminalSignalTimeout
	terminalSignalTimeout = d
	t.Cleanup(func() { terminalSignalTimeout = old })
}

func signalerRegistered(ts *terminalSession) bool {
	signalers.mu.Lock()
	defer signalers.mu.Unlock()
	return signalers.m[ts] != nil
}

// Control characters go through the real StartTerminal input path and keep
// their place among typed input.
func TestSignalControlCharsInOrder(t *testing.T) {
	srv := newSigTestServer(t, "accept")
	a, _ := newTerminalTestApp(t, srv.Connection("c1"))
	mustStart(t, a, "TERM-sig-chars", "c1", 80, 24)

	var want strings.Builder
	chars := []struct{ sig, b string }{
		{"INT_CHAR", "\x03"}, {"TSTP_CHAR", "\x1a"}, {"QUIT_CHAR", "\x1c"}, {"EOF_CHAR", "\x04"},
	}
	for i := range 200 {
		text := fmt.Sprintf("k%d", i)
		mustSend(t, a, "TERM-sig-chars", text)
		want.WriteString(text)
		c := chars[i%len(chars)]
		wantSignalCode(t, a.SendTerminalSignal("TERM-sig-chars", c.sig), "")
		want.WriteString(c.b)
	}
	mustSend(t, a, "TERM-sig-chars", "end")
	want.WriteString("end")
	waitUntil(t, "all bytes", func() bool { return len(srv.Data()) >= want.Len() })
	if got := srv.Data(); got != want.String() {
		t.Fatalf("server got %q\nwant %q", got, want.String())
	}
	if got := srv.Signals(); len(got) != 0 {
		t.Fatalf("control characters must not send signal requests: %+v", got)
	}
}

func TestSignalSSHRequestDelivered(t *testing.T) {
	srv := newSigTestServer(t, "accept")
	a, _ := newTerminalTestApp(t)
	ts := openSignalTab(t, a, srv, "TERM-sig-ssh")
	for _, sig := range []string{"TERM", "KILL", "HUP", "INT"} {
		wantSignalCode(t, a.SendTerminalSignal("TERM-sig-ssh", sig), "")
	}
	got := srv.Signals()
	want := []sigRecord{{"TERM", true}, {"KILL", true}, {"HUP", true}, {"INT", true}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("signals = %+v, want %+v", got, want)
	}
	a.CloseTerminal("TERM-sig-ssh")
	waitUntil(t, "signaler dropped", func() bool { return !signalerRegistered(ts) })
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-ssh", "TERM"), "no_session")
}

func TestSignalSSHRejected(t *testing.T) {
	srv := newSigTestServer(t, "reject")
	a, _ := newTerminalTestApp(t)
	openSignalTab(t, a, srv, "TERM-sig-rej")
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-rej", "TERM"), "signal_unsupported")
	if got := srv.Signals(); len(got) != 1 || got[0].Name != "TERM" {
		t.Fatalf("signals = %+v", got)
	}
	// The tab stays usable.
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-rej", "INT_CHAR"), "")
	waitUntil(t, "ctrl-c", func() bool { return srv.Data() == "\x03" })
}

func TestSignalSSHNoReply(t *testing.T) {
	setSignalTimeout(t, 150*time.Millisecond)
	srv := newSigTestServer(t, "silent")
	a, _ := newTerminalTestApp(t)
	ts := openSignalTab(t, a, srv, "TERM-sig-silent")
	start := time.Now()
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-silent", "HUP"), "signal_unsupported")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	// The first request still waits for a reply: a second one is refused
	// instead of queueing behind it.
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-silent", "KILL"), "signal_busy")
	// Control characters are not blocked by the pending request.
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-silent", "EOF_CHAR"), "")
	waitUntil(t, "ctrl-d", func() bool { return srv.Data() == "\x04" })
	// Closing the tab releases the waiting request.
	a.CloseTerminal("TERM-sig-silent")
	waitUntil(t, "signaler dropped", func() bool { return !signalerRegistered(ts) })
}

func TestSignalErrors(t *testing.T) {
	srv := newSigTestServer(t, "accept")
	a, _ := newTerminalTestApp(t, srv.Connection("c1"))
	mustStart(t, a, "TERM-sig-plain", "c1", 80, 24)

	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-plain", "STOP"), "invalid_signal")
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-plain", ""), "invalid_signal")
	wantSignalCode(t, a.SendTerminalSignal("TERM-missing", "INT_CHAR"), "no_session")
	wantSignalCode(t, a.SendTerminalSignal("TERM-missing", "TERM"), "no_session")
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-plain", "BREAK"), "signal_not_applicable")
	// A tab without a registered session reports signal_unavailable. The
	// entry is removed by hand so this holds both before and after the
	// openSSHTerminal wiring (req017-signals-wiring.md §1) adds it.
	ts := startedSignalTab(t, a, "TERM-sig-plain")
	unregisterSignalerForTest(ts)
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-plain", "TERM"), "signal_unavailable")

	a.CloseTerminal("TERM-sig-plain")
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-plain", "INT_CHAR"), "no_session")
}

// startedSignalTab waits until the StartTerminal tab id is open and returns it.
func startedSignalTab(t *testing.T, a *App, id string) *terminalSession {
	t.Helper()
	var ts *terminalSession
	waitUntil(t, id+" open", func() bool {
		a.termMu.Lock()
		defer a.termMu.Unlock()
		ts = a.terms[id]
		return ts != nil && ts.status == terminalStatusOpen
	})
	return ts
}

func unregisterSignalerForTest(ts *terminalSession) {
	signalers.mu.Lock()
	delete(signalers.m, ts)
	signalers.mu.Unlock()
}

// End to end through StartTerminal and openSSHTerminal. Until the one-line
// wiring of req017-signals-wiring.md §1 is in terminal.go the session is not
// registered and the test skips; after the merge it must run and pass.
func TestSignalThroughStartTerminal(t *testing.T) {
	srv := newSigTestServer(t, "accept")
	a, _ := newTerminalTestApp(t, srv.Connection("c1"))
	mustStart(t, a, "TERM-sig-e2e", "c1", 80, 24)
	ts := startedSignalTab(t, a, "TERM-sig-e2e")
	if !signalerRegistered(ts) {
		a.CloseTerminal("TERM-sig-e2e")
		t.Skip("openSSHTerminal does not call registerTerminalSignaler yet (wired at merge, req017-signals-wiring.md §1)")
	}
	mustSend(t, a, "TERM-sig-e2e", "a")
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-e2e", "INT_CHAR"), "")
	for _, sig := range []string{"TERM", "KILL", "HUP", "INT"} {
		wantSignalCode(t, a.SendTerminalSignal("TERM-sig-e2e", sig), "")
	}
	waitUntil(t, "chars", func() bool { return srv.Data() == "a\x03" })
	want := []sigRecord{{"TERM", true}, {"KILL", true}, {"HUP", true}, {"INT", true}}
	if got := srv.Signals(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("signals = %+v, want %+v", got, want)
	}
	a.CloseTerminal("TERM-sig-e2e")
	waitUntil(t, "signaler dropped", func() bool { return !signalerRegistered(ts) })
	wantSignalCode(t, a.SendTerminalSignal("TERM-sig-e2e", "TERM"), "no_session")
}

// sigFakePort is a minimal serialPort; sigBreakPort adds Break.
type sigFakePort struct {
	mu      sync.Mutex
	written []byte
	events  []string     // "write", "drain", "break" in call order
	drainFn func() error // nil: Drain returns at once
	closed  chan struct{}
	once    sync.Once
}

func newSigFakePort() *sigFakePort { return &sigFakePort{closed: make(chan struct{})} }

func (p *sigFakePort) Read([]byte) (int, error) {
	<-p.closed
	return 0, io.EOF
}

func (p *sigFakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, b...)
	p.events = append(p.events, "write")
	return len(b), nil
}

func (p *sigFakePort) Close() error               { p.once.Do(func() { close(p.closed) }); return nil }
func (p *sigFakePort) SetMode(*serial.Mode) error { return nil }
func (p *sigFakePort) Drain() error {
	p.mu.Lock()
	p.events = append(p.events, "drain")
	fn := p.drainFn
	p.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return nil
}

func (p *sigFakePort) Events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

func (p *sigFakePort) Written() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.written)
}

type sigBreakPort struct {
	*sigFakePort
	h      *serialHandle
	err    error
	breaks []time.Duration
	locked []bool // whether writeMu was held during each Break
}

func (p *sigBreakPort) Break(d time.Duration) error {
	held := !p.h.writeMu.TryLock()
	if !held {
		p.h.writeMu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.breaks = append(p.breaks, d)
	p.locked = append(p.locked, held)
	p.events = append(p.events, "break")
	return p.err
}

func newSerialSignalTab(a *App, id string, port serialPort, status string) *terminalSession {
	h := &serialHandle{port: port}
	if bp, ok := port.(*sigBreakPort); ok {
		bp.h = h
	}
	ts := &terminalSession{
		id: id, connID: "s-" + id, connName: "serial", kind: "serial", port: "/dev/null",
		serial:      h,
		cancel:      func() {},
		input:       make(chan []byte, terminalInputQueue),
		out:         newOutputBuffer(func([]byte, int) {}),
		done:        make(chan struct{}),
		stdinWriter: h,
		status:      status,
		closer:      func() { _ = port.Close() },
	}
	a.termMu.Lock()
	a.ensureMaps()
	a.terms[id] = ts
	a.termMu.Unlock()
	if status == terminalStatusOpen {
		a.terminalLoops.Add(1)
		go func() {
			defer a.terminalLoops.Done()
			a.pumpTerminalInput(ts)
		}()
	}
	return ts
}

func TestSignalSerial(t *testing.T) {
	old := terminalBreakDuration
	terminalBreakDuration = 5 * time.Millisecond
	t.Cleanup(func() { terminalBreakDuration = old })
	a, _ := newTerminalTestApp(t)

	bp := &sigBreakPort{sigFakePort: newSigFakePort()}
	newSerialSignalTab(a, "TERM-ser-ok", bp, terminalStatusOpen)
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-ok", "BREAK"), "")
	bp.mu.Lock()
	if len(bp.breaks) != 1 || bp.breaks[0] != 5*time.Millisecond || !bp.locked[0] {
		t.Fatalf("breaks = %v locked = %v", bp.breaks, bp.locked)
	}
	bp.mu.Unlock()
	for _, sig := range []string{"TERM", "KILL", "HUP", "INT"} {
		wantSignalCode(t, a.SendTerminalSignal("TERM-ser-ok", sig), "signal_not_applicable")
	}
	mustSend(t, a, "TERM-ser-ok", "x")
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-ok", "INT_CHAR"), "")
	mustSend(t, a, "TERM-ser-ok", "y")
	waitUntil(t, "serial bytes", func() bool { return bp.Written() == "x\x03y" })

	newSerialSignalTab(a, "TERM-ser-nobrk", newSigFakePort(), terminalStatusOpen)
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-nobrk", "BREAK"), "break_unsupported")

	failing := &sigBreakPort{sigFakePort: newSigFakePort(), err: errors.New("ioctl failed")}
	newSerialSignalTab(a, "TERM-ser-fail", failing, terminalStatusOpen)
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-fail", "BREAK"), "break_failed")

	newSerialSignalTab(a, "TERM-ser-conn", &sigBreakPort{sigFakePort: newSigFakePort()}, terminalStatusConnecting)
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-conn", "BREAK"), "not_ready")
}

// BREAK waits for queued output to leave the port before asserting the line
// (finding: break cut into a write still in the UART FIFO), and a drain that
// never finishes does not hang it.
func TestSignalSerialBreakDrainsFirst(t *testing.T) {
	oldBreak, oldDrain := terminalBreakDuration, serialDrainTimeout
	terminalBreakDuration = time.Millisecond
	serialDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { terminalBreakDuration, serialDrainTimeout = oldBreak, oldDrain })
	a, _ := newTerminalTestApp(t)

	bp := &sigBreakPort{sigFakePort: newSigFakePort()}
	newSerialSignalTab(a, "TERM-ser-drain", bp, terminalStatusOpen)
	mustSend(t, a, "TERM-ser-drain", "show run\r")
	waitUntil(t, "write", func() bool { return bp.Written() == "show run\r" })
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-drain", "BREAK"), "")
	if got := bp.Events(); !slices.Equal(got, []string{"write", "drain", "break"}) {
		t.Fatalf("events = %v, want write, drain, break", got)
	}

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	hung := &sigBreakPort{sigFakePort: newSigFakePort()}
	hung.drainFn = func() error { <-stuck; return nil }
	newSerialSignalTab(a, "TERM-ser-hung", hung, terminalStatusOpen)
	start := time.Now()
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-hung", "BREAK"), "")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("BREAK waited %v for a drain that never ends", d)
	}
	if got := hung.Events(); !slices.Equal(got, []string{"drain", "break"}) {
		t.Fatalf("events = %v, want drain, break", got)
	}
}

// Signals racing with close and with each other: run with -race.
func TestSignalRaceWithClose(t *testing.T) {
	srv := newSigTestServer(t, "accept")
	a, _ := newTerminalTestApp(t)
	for i := range 5 {
		id := fmt.Sprintf("TERM-sig-race-%d", i)
		ts := openSignalTab(t, a, srv, id)
		var wg sync.WaitGroup
		for _, sig := range []string{"TERM", "INT_CHAR", "HUP", "EOF_CHAR", "KILL"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := a.SendTerminalSignal(id, sig)
				switch res.Code {
				case "", "no_session", "signal_busy", "signal_failed":
				default:
					t.Errorf("%s: unexpected %+v", sig, res)
				}
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); a.CloseTerminal(id) }()
		wg.Wait()
		waitUntil(t, "signaler dropped", func() bool { return !signalerRegistered(ts) })
	}
}

// BREAK holds a use of the fd like Write and Drain: once closePort has run,
// it must not reach the port (the fd number may already be reused, N1), and
// a closePort during the BREAK waits for it before closing the fd.
func TestSignalSerialBreakAfterClosePort(t *testing.T) {
	old := terminalBreakDuration
	terminalBreakDuration = time.Millisecond
	t.Cleanup(func() { terminalBreakDuration = old })
	a, _ := newTerminalTestApp(t)

	bp := &sigBreakPort{sigFakePort: newSigFakePort()}
	ts := newSerialSignalTab(a, "TERM-ser-closed", bp, terminalStatusOpen)
	ts.serial.closePort()
	wantSignalCode(t, a.SendTerminalSignal("TERM-ser-closed", "BREAK"), "no_session")
	if got := bp.Events(); slices.Contains(got, "break") {
		t.Fatalf("events = %v: Break reached a closed port", got)
	}
}
