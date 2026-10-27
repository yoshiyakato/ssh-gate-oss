package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Acceptance tests for design-v1.md §7 phase 5 (ssh_pool.go). Every test runs
// with keepaliveInterval=50ms and maxChannels=3, an injected dial and an
// injected clock. Run with: go test -race -count=20 -run 'Pool' .

const (
	poolTestKeepalive   = 50 * time.Millisecond
	poolTestMaxChannels = 3
	poolTestAltPassword = "secret2" // accepted too, so fp can change and auth still works
)

// ---------------------------------------------------------------------------
// Adapter: the only place that names the pool's API. Adjust here when the
// implementation's names differ; the tests below go through these helpers.
// ---------------------------------------------------------------------------

type poolHarness struct {
	t     *testing.T
	pool  *SSHPool
	clock *poolFakeClock
	dials atomic.Int32
}

func newPoolHarness(t *testing.T, dial PoolDialFunc, tweak ...func(*PoolOptions)) *poolHarness {
	t.Helper()
	// Isolate from the developer's ssh setup (as mcp_server_test.go does):
	// openSSHClient and poolFingerprint read these.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("SSH_GETE_STRICT_HOSTKEY", "")
	h := &poolHarness{t: t, clock: newPoolFakeClock()}
	if dial == nil {
		dial = func(ctx context.Context, conn Connection) (*ssh.Client, error) {
			return openSSHClient(ctx, conn, false)
		}
	}
	opts := PoolOptions{
		Dial: func(ctx context.Context, conn Connection) (*ssh.Client, error) {
			h.dials.Add(1)
			return dial(ctx, conn)
		},
		KeepaliveInterval: poolTestKeepalive,
		KeepaliveTimeout:  4 * poolTestKeepalive,
		MaxChannels:       poolTestMaxChannels,
		AutoIdle:          5 * time.Minute,
		Now:               h.clock.Now,
	}
	for _, f := range tweak {
		f(&opts)
	}
	h.pool = NewSSHPool(opts)
	t.Cleanup(h.pool.CloseAll)
	return h
}

// openTab borrows a channel the way a terminal tab does and returns its
// release func.
func (h *poolHarness) openTab(ctx context.Context, conn Connection) (*ssh.Session, func(), error) {
	lease, err := h.pool.Acquire(ctx, conn, PoolChannelTerm, PoolOriginManual)
	if err != nil {
		return nil, nil, err
	}
	return lease.Session(), lease.Release, nil
}

// run executes one MCP command through the pool (agent-opened → auto).
func (h *poolHarness) run(ctx context.Context, conn Connection, command string) (commandResult, error) {
	return h.exec(ctx, conn, PoolOriginAuto, command)
}

// runManual is run with a human-opened connection.
func (h *poolHarness) runManual(ctx context.Context, conn Connection, command string) (commandResult, error) {
	return h.exec(ctx, conn, PoolOriginManual, command)
}

func (h *poolHarness) exec(ctx context.Context, conn Connection, origin PoolOrigin, command string) (commandResult, error) {
	stdout := &cappedBuffer{max: 4096}
	stderr := &cappedBuffer{max: 4096}
	err := h.pool.ExecOnce(ctx, conn, origin, command, stdout, stderr)
	return commandResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

func (h *poolHarness) reap() { h.pool.reapIdle(h.clock.Now()) }

func (h *poolHarness) clients(connID string) int {
	for _, st := range h.pool.Stats() {
		if st.ConnID == connID {
			return st.Clients
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Fault-injecting SSH server. It reuses sshTestServer.handleSession (and so
// its exec commands and execLog) but owns the listener so it can cap
// channels per TCP connection, count client-side closes and kill every
// connection at once (sleep/resume).
// ---------------------------------------------------------------------------

type poolTestServer struct {
	base     *sshTestServer // only handleSession/execLog are used
	listener net.Listener
	config   *ssh.ServerConfig
	maxChans int // per TCP connection; 0 = unlimited

	mu         sync.Mutex
	live       map[*poolServerConn]struct{}
	accepted   int
	peerClosed int // connections the client closed (not killed by us)
	rejected   int // channel opens refused with ResourceShortage
	stallNext  int // the next N channel opens (any conn) get no answer
}

type poolServerConn struct {
	nc     net.Conn
	open   int // guarded by poolTestServer.mu
	killed bool
	// Guarded by poolTestServer.mu. A stalled conn stays up (no FIN, so
	// client.Wait does not return) but never answers: a half-open link.
	stallOpens bool // channel opens get no reply
	stallReqs  bool // global requests (keepalive) get no reply
}

// StallExisting black-holes channel opens and/or global requests on every
// connection open now; later connections are unaffected.
func (s *poolTestServer) StallExisting(opens, reqs bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.live {
		c.stallOpens = c.stallOpens || opens
		c.stallReqs = c.stallReqs || reqs
	}
}

// StallNextOpens leaves the next n channel opens unanswered.
func (s *poolTestServer) StallNextOpens(n int) {
	s.mu.Lock()
	s.stallNext = n
	s.mu.Unlock()
}

func (s *poolTestServer) SetMaxChans(n int) {
	s.mu.Lock()
	s.maxChans = n
	s.mu.Unlock()
}

func newPoolTestServer(t *testing.T, maxChans int) *poolTestServer {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			p := string(password)
			if meta.User() == sshTestUser && (p == sshTestPassword || p == poolTestAltPassword) {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	cfg.AddHostKey(poolTestHostKey(t))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &poolTestServer{
		base:     &sshTestServer{},
		listener: listener,
		config:   cfg,
		maxChans: maxChans,
		live:     map[*poolServerConn]struct{}{},
	}
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

func (s *poolTestServer) Close() {
	_ = s.listener.Close()
	s.KillAll()
}

// KillAll drops every TCP connection without an SSH disconnect, like a NAT
// table lost across sleep.
func (s *poolTestServer) KillAll() {
	s.mu.Lock()
	conns := make([]*poolServerConn, 0, len(s.live))
	for c := range s.live {
		c.killed = true
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.nc.Close()
	}
}

func (s *poolTestServer) Connection(id string) Connection {
	return Connection{
		ID:             id,
		Name:           id,
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

func (s *poolTestServer) Execs() []string { return s.base.Execs() }

func (s *poolTestServer) stats() (live, accepted, peerClosed, rejected int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live), s.accepted, s.peerClosed, s.rejected
}

func (s *poolTestServer) serve() {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		c := &poolServerConn{nc: nc}
		s.mu.Lock()
		s.live[c] = struct{}{}
		s.accepted++
		s.mu.Unlock()
		go s.handleConn(c)
	}
}

func (s *poolTestServer) handleConn(c *poolServerConn) {
	defer func() {
		_ = c.nc.Close()
		s.mu.Lock()
		delete(s.live, c)
		if !c.killed {
			s.peerClosed++
		}
		s.mu.Unlock()
	}()
	sc, chans, reqs, err := ssh.NewServerConn(c.nc, s.config)
	if err != nil {
		return
	}
	defer sc.Close()
	go func() {
		for req := range reqs { // keepalive@openssh.com gets a (false) reply
			s.mu.Lock()
			stalled := c.stallReqs
			s.mu.Unlock()
			if req.WantReply && !stalled {
				_ = req.Reply(false, nil)
			}
		}
	}()
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		s.mu.Lock()
		stall := c.stallOpens || s.stallNext > 0
		if !c.stallOpens && s.stallNext > 0 {
			s.stallNext--
		}
		s.mu.Unlock()
		if stall {
			continue // never answered; the client's open hangs
		}
		s.mu.Lock()
		full := s.maxChans > 0 && c.open >= s.maxChans
		if full {
			s.rejected++
		} else {
			c.open++
		}
		s.mu.Unlock()
		if full {
			_ = newCh.Reject(ssh.ResourceShortage, "MaxSessions")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			s.mu.Lock()
			c.open--
			s.mu.Unlock()
			continue
		}
		go func() {
			s.base.handleSession(ch, chReqs)
			s.mu.Lock()
			c.open--
			s.mu.Unlock()
		}()
	}
}

func poolTestHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// ---------------------------------------------------------------------------
// Fake clock and small helpers
// ---------------------------------------------------------------------------

type poolFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newPoolFakeClock() *poolFakeClock {
	return &poolFakeClock{now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
}

func (c *poolFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *poolFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// waitFor polls cond until it holds or d elapses.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func poolCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// Acceptance: 7 tabs + 4 MCP at once, no healthy client is closed
// ---------------------------------------------------------------------------

func TestPoolSevenTabsFourExecsKeepHealthyClients(t *testing.T) {
	cases := []struct {
		name     string
		maxChans int // server-side MaxSessions per TCP connection
	}{
		{"pool semaphore only", 0},
		// Lower than the pool's 3, so NewSession hits *ssh.OpenChannelError on
		// a live client: the pool must wait or overflow, never evict.
		{"server MaxSessions below pool cap", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newPoolTestServer(t, tc.maxChans)
			h := newPoolHarness(t, nil)
			conn := srv.Connection("CONN-web")
			ctx := poolCtx(t, 20*time.Second)

			const tabs, execs = 7, 4
			var wg sync.WaitGroup
			errs := make(chan error, tabs+execs)
			start := make(chan struct{})
			for i := 0; i < tabs; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					sess, release, err := h.openTab(ctx, conn)
					if err != nil {
						errs <- fmt.Errorf("tab %d: %w", i, err)
						return
					}
					if sess == nil {
						errs <- fmt.Errorf("tab %d: nil session", i)
						release()
						return
					}
					time.Sleep(300 * time.Millisecond) // tab stays open while execs run
					_ = sess.Close()
					release()
				}(i)
			}
			for i := 0; i < execs; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					want := fmt.Sprintf("mcp-%d", i)
					res, err := h.run(ctx, conn, "echo "+want)
					if err != nil {
						errs <- fmt.Errorf("exec %d: %w", i, err)
						return
					}
					if strings.TrimSpace(res.Stdout) != want {
						errs <- fmt.Errorf("exec %d: stdout %q, want %q", i, res.Stdout, want)
					}
				}(i)
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}

			live, accepted, peerClosed, rejected := srv.stats()
			if peerClosed != 0 {
				t.Errorf("pool closed %d healthy client(s) (accepted=%d live=%d rejected=%d)", peerClosed, accepted, live, rejected)
			}
			if live != accepted {
				t.Errorf("live=%d accepted=%d: a client was dropped", live, accepted)
			}
			if got := int(h.dials.Load()); got != accepted {
				t.Errorf("dials=%d but server accepted %d", got, accepted)
			}
			// 11 concurrent channels at 3 per client needs overflow clients,
			// but not one per channel.
			if accepted > tabs+execs-1 {
				t.Errorf("accepted=%d: pool is not sharing clients", accepted)
			}
			if got := len(srv.Execs()); got != execs {
				t.Errorf("execLog=%d, want %d", got, execs)
			}
		})
	}
}

// The pool must never put more than maxChannels channels on one client.
func TestPoolRespectsMaxChannelsPerClient(t *testing.T) {
	srv := newPoolTestServer(t, poolTestMaxChannels) // server rejects the 4th
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-cap")
	ctx := poolCtx(t, 10*time.Second)

	var releases []func()
	for i := 0; i < poolTestMaxChannels+1; i++ {
		_, release, err := h.openTab(ctx, conn)
		if err != nil {
			t.Fatalf("tab %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	_, _, _, rejected := srv.stats()
	if rejected != 0 {
		t.Errorf("server rejected %d channel(s): pool exceeded maxChannels=%d on one client", rejected, poolTestMaxChannels)
	}
	if got := h.clients(conn.ID); got != 2 {
		t.Errorf("clients=%d, want 2 (one overflow)", got)
	}
	for _, r := range releases {
		r()
	}
}

// ---------------------------------------------------------------------------
// Acceptance: no retry after Run (execLog stays at 1)
// ---------------------------------------------------------------------------

func TestPoolNoRetryAfterRun(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-norun")
	ctx := poolCtx(t, 10*time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := h.run(ctx, conn, "sleep 1")
		done <- err
	}()
	waitFor(t, 2*time.Second, "exec to reach the server", func() bool { return len(srv.Execs()) == 1 })
	srv.KillAll() // connection dies after Run started

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run succeeded although the connection died mid-command")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the connection died")
	}
	time.Sleep(300 * time.Millisecond) // give a wrong retry time to land
	if got := srv.Execs(); len(got) != 1 {
		t.Fatalf("execLog=%v: command was re-run after Run", got)
	}

	// The pool recovers for the next, separate command.
	res, err := h.run(ctx, conn, "echo again")
	if err != nil {
		t.Fatalf("next run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "again" {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	if got := len(srv.Execs()); got != 2 {
		t.Fatalf("execLog=%d, want 2", got)
	}
}

// Retrying before Run is allowed: a client that died while idle is evicted
// and redialed, and the command still runs exactly once. The link is
// half-open (no FIN), so client.Wait does not fire and the dead client is
// still in the pool when run picks it: NewSession must fail, evict, retry.
func TestPoolRetriesDeadIdleClientBeforeRun(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil, func(o *PoolOptions) {
		o.KeepaliveInterval = time.Hour // keepalive must not evict it first
	})
	conn := srv.Connection("CONN-retry")
	ctx := poolCtx(t, 10*time.Second)

	if _, err := h.run(ctx, conn, "echo one"); err != nil {
		t.Fatal(err)
	}
	srv.StallExisting(true, true)
	if got := h.clients(conn.ID); got != 1 {
		t.Fatalf("clients=%d: dead client should still be pooled", got)
	}
	res, err := h.run(ctx, conn, "echo two")
	if err != nil {
		t.Fatalf("run after idle death: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "two" {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	if got := srv.Execs(); len(got) != 2 || got[1] != "echo two" {
		t.Fatalf("execLog=%v", got)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d, want 2", got)
	}
	waitFor(t, time.Second, "dead client closed by the pool", func() bool {
		_, _, peerClosed, _ := srv.stats()
		return peerClosed == 1
	})
}

// ---------------------------------------------------------------------------
// Liveness: sync ping on lease, keepalive eviction, busy clients survive a
// single timeout (§5.1)
// ---------------------------------------------------------------------------

// A client idle past AliveCheckAfter is pinged before it is leased. Here only
// keepalive goes unanswered (channel opens still work), so without the ping
// the run would reuse the half-dead client instead of redialing.
func TestPoolPingsBeforeLeaseAfterAliveCheck(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil, func(o *PoolOptions) {
		o.KeepaliveInterval = time.Hour
	})
	conn := srv.Connection("CONN-ping")
	ctx := poolCtx(t, 10*time.Second)

	if _, err := h.run(ctx, conn, "echo one"); err != nil {
		t.Fatal(err)
	}
	srv.StallExisting(false, true)
	h.clock.Advance(defaultPoolAliveCheckAfter + time.Second)
	if _, err := h.run(ctx, conn, "echo two"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d, want 2 (ping should have rejected the old client)", got)
	}
	waitFor(t, time.Second, "old client closed", func() bool {
		_, _, peerClosed, _ := srv.stats()
		return peerClosed == 1
	})
}

// Before AliveCheckAfter no ping is sent, so the same stall goes unnoticed.
func TestPoolNoPingBeforeAliveCheck(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil, func(o *PoolOptions) {
		o.KeepaliveInterval = time.Hour
	})
	conn := srv.Connection("CONN-noping")
	ctx := poolCtx(t, 10*time.Second)

	if _, err := h.run(ctx, conn, "echo one"); err != nil {
		t.Fatal(err)
	}
	srv.StallExisting(false, true)
	if _, err := h.run(ctx, conn, "echo two"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.dials.Load(); got != 1 {
		t.Fatalf("dials=%d, want 1", got)
	}
}

// A half-open link (no FIN) is found by keepalive alone: client.Wait never
// returns, so watch cannot catch it.
func TestPoolKeepaliveEvictsHalfOpenClient(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-ka")
	ctx := poolCtx(t, 10*time.Second)

	if _, err := h.runManual(ctx, conn, "echo one"); err != nil {
		t.Fatal(err)
	}
	srv.StallExisting(true, true)
	waitFor(t, 3*time.Second, "keepalive to evict the half-open client", func() bool {
		return h.clients(conn.ID) == 0
	})
	waitFor(t, time.Second, "pool to close the half-open client", func() bool {
		_, _, peerClosed, _ := srv.stats()
		return peerClosed == 1
	})
}

// One NewSession timeout on a client that still carries leases must not
// close it (that would kill the open tabs); new work goes elsewhere.
func TestPoolSessionTimeoutKeepsBusyClient(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-busy-timeout")
	ctx := poolCtx(t, 10*time.Second)

	tab, release, err := h.openTab(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	srv.StallNextOpens(1) // the exec's open on the busy client hangs
	if _, err := h.run(ctx, conn, "echo exec"); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The exec landed either on an overflow client or, once keepalive
	// cleared the suspect mark, back on A. Either way A must still be open.
	if err := tab.Run("echo tab-alive"); err != nil {
		t.Fatalf("open tab broken after one NewSession timeout: %v", err)
	}
	if _, _, peerClosed, _ := srv.stats(); peerClosed != 0 {
		t.Fatalf("busy client closed after one timeout (peerClosed=%d)", peerClosed)
	}
	if got := srv.Execs(); len(got) != 2 {
		t.Fatalf("execLog=%v, want exec + tab", got)
	}
}

// Same for a failed lease-time ping: the busy client is kept for keepalive
// to judge (here keepalive is off, so it simply stays).
func TestPoolPingTimeoutKeepsBusyClient(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil, func(o *PoolOptions) {
		o.KeepaliveInterval = time.Hour
	})
	conn := srv.Connection("CONN-busy-ping")
	ctx := poolCtx(t, 10*time.Second)

	tab, release, err := h.openTab(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	srv.StallExisting(false, true)
	h.clock.Advance(defaultPoolAliveCheckAfter + time.Second)
	if _, err := h.run(ctx, conn, "echo exec"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d, want 2", got)
	}
	if _, _, peerClosed, _ := srv.stats(); peerClosed != 0 {
		t.Fatalf("busy client closed after one ping timeout")
	}
	if err := tab.Run("echo tab-alive"); err != nil {
		t.Fatalf("open tab broken: %v", err)
	}
}

// A cap learned from a refusal (possibly close lag) expires, so the client
// can use more channels once the server has room again.
func TestPoolLearnedServerLimitExpires(t *testing.T) {
	srv := newPoolTestServer(t, 1)
	h := newPoolHarness(t, nil, func(o *PoolOptions) {
		o.ServerLimitTTL = 30 * time.Second
	})
	conn := srv.Connection("CONN-limit")
	ctx := poolCtx(t, 10*time.Second)

	var releases []func()
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	for i := 0; i < 2; i++ { // client A, then overflow B after A's refusal
		_, release, err := h.openTab(ctx, conn)
		if err != nil {
			t.Fatalf("tab %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d, want 2", got)
	}
	// Third tab: B is refused too, both caps are 1, MaxClients reached: wait.
	third := make(chan error, 1)
	go func() {
		_, release, err := h.openTab(ctx, conn)
		if err == nil {
			release()
		}
		third <- err
	}()
	waitFor(t, 2*time.Second, "second refusal", func() bool {
		_, _, _, rejected := srv.stats()
		return rejected >= 2
	})
	select {
	case err := <-third:
		t.Fatalf("third tab returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	srv.SetMaxChans(3)
	h.clock.Advance(31 * time.Second) // housekeeping expires the caps
	select {
	case err := <-third:
		if err != nil {
			t.Fatalf("third tab: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("learned cap never expired")
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: fp change drains (in-use client closes after release)
// ---------------------------------------------------------------------------

func TestPoolFingerprintChangeDrains(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	ctx := poolCtx(t, 10*time.Second)

	oldConn := srv.Connection("CONN-fp")
	newConn := oldConn
	newConn.Credential = poolTestAltPassword // credential is part of fp

	sessOld, releaseOld, err := h.openTab(ctx, oldConn)
	if err != nil {
		t.Fatal(err)
	}
	if h.dials.Load() != 1 {
		t.Fatalf("dials=%d", h.dials.Load())
	}

	// New fp: must not reuse the old client.
	_, releaseNew, err := h.openTab(ctx, newConn)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d after fp change, want 2", got)
	}

	// The stale client is still in use, so it must stay open.
	time.Sleep(200 * time.Millisecond)
	if live, _, peerClosed, _ := srv.stats(); peerClosed != 0 || live != 2 {
		t.Fatalf("stale in-use client closed early: live=%d peerClosed=%d", live, peerClosed)
	}

	// New work lands on the fresh client, never on the stale one.
	if _, err := h.run(ctx, newConn, "echo fresh"); err != nil {
		t.Fatal(err)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d: fresh fp should reuse the new client", got)
	}

	_ = sessOld.Close()
	releaseOld()
	waitFor(t, time.Second, "stale client to close after release", func() bool {
		live, _, peerClosed, _ := srv.stats()
		return peerClosed == 1 && live == 1
	})
	if got := h.clients(oldConn.ID); got != 1 {
		t.Errorf("clients=%d after drain, want 1", got)
	}
	releaseNew()
}

// ---------------------------------------------------------------------------
// Acceptance: auto connections are reclaimed after 5 idle minutes (now
// injected); manual and in-use ones are not.
// ---------------------------------------------------------------------------

func TestPoolReclaimsIdleAutoAfterFiveMinutes(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-auto")
	ctx := poolCtx(t, 10*time.Second)

	if _, err := h.run(ctx, conn, "echo a"); err != nil {
		t.Fatal(err)
	}
	if got := h.clients(conn.ID); got != 1 {
		t.Fatalf("clients=%d", got)
	}

	h.clock.Advance(5*time.Minute - time.Second)
	h.reap()
	if got := h.clients(conn.ID); got != 1 {
		t.Fatalf("reclaimed before 5 minutes: clients=%d", got)
	}

	h.clock.Advance(2 * time.Second)
	h.reap()
	if got := h.clients(conn.ID); got != 0 {
		t.Fatalf("not reclaimed after 5 minutes: clients=%d", got)
	}
	waitFor(t, time.Second, "server to see the close", func() bool {
		_, _, peerClosed, _ := srv.stats()
		return peerClosed == 1
	})

	// Next use dials again.
	if _, err := h.run(ctx, conn, "echo b"); err != nil {
		t.Fatal(err)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d, want 2", got)
	}
}

func TestPoolKeepsManualIdleClient(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-manual")
	ctx := poolCtx(t, 10*time.Second)

	if _, err := h.runManual(ctx, conn, "echo m"); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(30 * time.Minute)
	h.reap()
	if got := h.clients(conn.ID); got != 1 {
		t.Fatalf("manual client reclaimed: clients=%d", got)
	}
}

func TestPoolKeepsInUseAutoClient(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-busy")
	ctx := poolCtx(t, 10*time.Second)

	lease, err := h.pool.Acquire(ctx, conn, PoolChannelExec, PoolOriginAuto)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(30 * time.Minute)
	h.reap()
	if got := h.clients(conn.ID); got != 1 {
		t.Fatalf("in-use auto client reclaimed: clients=%d", got)
	}
	if _, _, peerClosed, _ := srv.stats(); peerClosed != 0 {
		t.Fatalf("in-use client closed")
	}
	lease.Release()
	// Idle time counts from release, not from the original dial.
	h.reap()
	if got := h.clients(conn.ID); got != 1 {
		t.Fatalf("reclaimed right after release: clients=%d", got)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: KillAll (sleep/resume) is detected within 1 second
// ---------------------------------------------------------------------------

func TestPoolDetectsKillAllWithinOneSecond(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	connA := srv.Connection("CONN-a")
	connB := srv.Connection("CONN-b")
	ctx := poolCtx(t, 10*time.Second)

	for _, c := range []Connection{connA, connB} {
		if _, err := h.run(ctx, c, "echo up"); err != nil {
			t.Fatal(err)
		}
	}
	if h.clients(connA.ID) != 1 || h.clients(connB.ID) != 1 {
		t.Fatalf("clients a=%d b=%d", h.clients(connA.ID), h.clients(connB.ID))
	}

	start := time.Now()
	srv.KillAll()
	waitFor(t, time.Second, "pool to drop killed clients", func() bool {
		return h.clients(connA.ID) == 0 && h.clients(connB.ID) == 0
	})
	t.Logf("detected in %v", time.Since(start))

	if _, err := h.run(ctx, connA, "echo back"); err != nil {
		t.Fatalf("run after resume: %v", err)
	}
}

// A tab holding a channel when the link dies must also be released by the
// pool (client gone), not left as a zombie.
func TestPoolDetectsKillAllWithTabOpen(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("CONN-tab")
	ctx := poolCtx(t, 10*time.Second)

	sess, release, err := h.openTab(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("sleep 2"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "command to reach the server", func() bool { return len(srv.Execs()) == 1 })
	waitDone := make(chan error, 1)
	go func() { waitDone <- sess.Wait() }()

	srv.KillAll()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("tab session did not end within 1s of KillAll")
	}
	waitFor(t, time.Second, "pool to drop the dead client", func() bool { return h.clients(conn.ID) == 0 })
	release() // releasing onto a dead client must be harmless
}

// ---------------------------------------------------------------------------
// dial: singleflight, waiter ctx, no error caching
// ---------------------------------------------------------------------------

func TestPoolConcurrentAcquireDialsOnce(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, func(ctx context.Context, conn Connection) (*ssh.Client, error) {
		time.Sleep(100 * time.Millisecond) // let all waiters pile up
		return openSSHClient(ctx, conn, false)
	})
	conn := srv.Connection("CONN-sf")
	ctx := poolCtx(t, 10*time.Second)

	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < poolTestMaxChannels; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := h.run(ctx, conn, fmt.Sprintf("echo %d", i)); err != nil {
				t.Error(err)
				failed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := h.dials.Load(); got != 1 {
		t.Fatalf("dials=%d for %d concurrent first uses, want 1", got, poolTestMaxChannels)
	}
}

func TestPoolWaiterHonorsOwnContext(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	unblock := make(chan struct{})
	h := newPoolHarness(t, func(ctx context.Context, conn Connection) (*ssh.Client, error) {
		select {
		case <-unblock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return openSSHClient(ctx, conn, false)
	})
	conn := srv.Connection("CONN-ctx")

	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.run(short, conn, "echo never")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waiter returned after %v", d)
	}
	if got := srv.Execs(); len(got) != 0 {
		t.Fatalf("execLog=%v", got)
	}

	close(unblock) // the dial belongs to the pool's ctx and finishes
	ctx := poolCtx(t, 10*time.Second)
	if _, err := h.run(ctx, conn, "echo later"); err != nil {
		t.Fatal(err)
	}
	if got := h.dials.Load(); got != 1 {
		t.Fatalf("dials=%d: the in-flight dial should have been reused", got)
	}
}

func TestPoolDoesNotCacheDialErrors(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	var calls atomic.Int32
	h := newPoolHarness(t, func(ctx context.Context, conn Connection) (*ssh.Client, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("injected dial failure")
		}
		return openSSHClient(ctx, conn, false)
	})
	conn := srv.Connection("CONN-err")
	ctx := poolCtx(t, 10*time.Second)

	if _, err := h.run(ctx, conn, "echo x"); err == nil {
		t.Fatal("first run should fail")
	}
	if _, err := h.run(ctx, conn, "echo y"); err != nil {
		t.Fatalf("second run reused a cached error: %v", err)
	}
	if got := srv.Execs(); len(got) != 1 || got[0] != "echo y" {
		t.Fatalf("execLog=%v", got)
	}
}

// A human disconnect during an in-flight dial bumps gen: the late client is
// closed, not stored, and the waiter is told the connection went away.
func TestPoolDisconnectDiscardsLateDial(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	unblock := make(chan struct{})
	dialed := make(chan struct{})
	var once sync.Once
	h := newPoolHarness(t, func(ctx context.Context, conn Connection) (*ssh.Client, error) {
		c, err := openSSHClient(ctx, conn, false)
		once.Do(func() { close(dialed) })
		<-unblock
		return c, err
	})
	conn := srv.Connection("CONN-late")
	ctx := poolCtx(t, 10*time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := h.run(ctx, conn, "echo never")
		done <- err
	}()
	<-dialed
	h.pool.Disconnect(conn.ID)
	select {
	case err := <-done:
		if !errors.Is(err, ErrPoolConnClosed) {
			t.Fatalf("waiter err=%v, want ErrPoolConnClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter not released by Disconnect")
	}
	close(unblock)

	waitFor(t, time.Second, "late client to be closed", func() bool {
		live, accepted, _, _ := srv.stats()
		return accepted == 1 && live == 0
	})
	if got := h.clients(conn.ID); got != 0 {
		t.Fatalf("clients=%d after Disconnect", got)
	}
	if got := srv.Execs(); len(got) != 0 {
		t.Fatalf("execLog=%v", got)
	}
}

// Disconnect cancels an in-flight dial, so a reconnect right after does not
// run two dials for one connID at once.
func TestPoolDisconnectCancelsInflightDial(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	var active, maxActive atomic.Int32
	cancelled := make(chan struct{}, 4)
	var first atomic.Bool
	h := newPoolHarness(t, func(ctx context.Context, conn Connection) (*ssh.Client, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		if first.CompareAndSwap(false, true) {
			<-ctx.Done() // a slow host: only cancellation ends it
			cancelled <- struct{}{}
			return nil, ctx.Err()
		}
		return openSSHClient(ctx, conn, false)
	})
	conn := srv.Connection("CONN-cancel")
	ctx := poolCtx(t, 10*time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := h.run(ctx, conn, "echo never")
		done <- err
	}()
	waitFor(t, 2*time.Second, "dial to start", func() bool { return active.Load() == 1 })
	h.pool.Disconnect(conn.ID)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not cancel the in-flight dial")
	}
	if err := <-done; !errors.Is(err, ErrPoolConnClosed) {
		t.Fatalf("waiter err=%v, want ErrPoolConnClosed", err)
	}
	if _, err := h.run(ctx, conn, "echo again"); err != nil {
		t.Fatal(err)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent dials=%d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// §5.1: only Delete and a human Disconnect close a busy client immediately;
// Drain (saved config changed) waits for the last lease.
// ---------------------------------------------------------------------------

func TestPoolDisconnectAndDeleteCloseBusyClientNow(t *testing.T) {
	for _, op := range []string{"Disconnect", "Delete"} {
		t.Run(op, func(t *testing.T) {
			srv := newPoolTestServer(t, 0)
			h := newPoolHarness(t, nil)
			conn := srv.Connection("conn-" + op)
			ctx := poolCtx(t, 10*time.Second)

			session, release, err := h.openTab(ctx, conn)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if op == "Disconnect" {
				h.pool.Disconnect(conn.ID)
			} else {
				h.pool.Delete(conn.ID)
			}
			waitFor(t, time.Second, "busy client closed", func() bool {
				live, _, _, _ := srv.stats()
				return live == 0
			})
			if err := session.Run("echo after"); err == nil {
				t.Fatal("session still usable after " + op)
			}
			if got := h.clients(conn.ID); got != 0 {
				t.Fatalf("clients=%d after %s", got, op)
			}
			release() // late release of an evicted client must be harmless
			release()

			// The connection can be used again afterwards (new dial).
			if _, err := h.run(ctx, conn, "echo again"); err != nil {
				t.Fatalf("run after %s: %v", op, err)
			}
			if got := h.dials.Load(); got != 2 {
				t.Fatalf("dials=%d, want 2", got)
			}
		})
	}
}

func TestPoolDrainKeepsBusyClientUntilRelease(t *testing.T) {
	srv := newPoolTestServer(t, 0)
	h := newPoolHarness(t, nil)
	conn := srv.Connection("conn-drain")
	ctx := poolCtx(t, 10*time.Second)

	session, release, err := h.openTab(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	h.pool.Drain(conn.ID)

	// The busy client survives; a new lease goes to a fresh client.
	if _, err := h.run(ctx, conn, "echo fresh"); err != nil {
		t.Fatal(err)
	}
	if got := h.dials.Load(); got != 2 {
		t.Fatalf("dials=%d, want 2 (drained client must not be reused)", got)
	}
	if live, _, _, _ := srv.stats(); live != 2 {
		t.Fatalf("live=%d, want 2 while the drained client is busy", live)
	}
	if err := session.Run("echo still-works"); err != nil {
		t.Fatalf("drained busy session broken: %v", err)
	}
	release()
	waitFor(t, time.Second, "drained client closed after release", func() bool {
		live, _, peerClosed, _ := srv.stats()
		return live == 1 && peerClosed == 1
	})
}

// known_hosts only enters the fp when host keys are checked, so an unrelated
// `ssh newhost` elsewhere does not drain every pooled client.
func TestPoolFingerprintKnownHostsOnlyWhenStrict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("SSH_GETE_STRICT_HOSTKEY", "")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(home, ".ssh", "known_hosts")
	if err := os.WriteFile(kh, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := Connection{ID: "x", Type: "SSH", Host: "h", User: "u", AuthMethod: "パスワード保管", Credential: "p"}
	loose, strict := poolFingerprint(conn, false), poolFingerprint(conn, true)

	if err := os.WriteFile(kh, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := poolFingerprint(conn, false); got != loose {
		t.Error("non-strict fp changed when known_hosts changed")
	}
	if got := poolFingerprint(conn, true); got == strict {
		t.Error("strict fp did not change when known_hosts changed")
	}
}
