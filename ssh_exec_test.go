package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// stallServer is an SSH server that finishes key exchange and auth, then
// never answers a CHANNEL_OPEN (the case NewSession used to hang on). It
// accepts the test password and, with acceptPublicKey, any public key.
// Closed reports each server-side connection that saw the client go away.
type stallServer struct {
	listener net.Listener
	signer   ssh.Signer
	config   *ssh.ServerConfig
	closed   chan struct{}
	opens    chan struct{}

	mu    sync.Mutex
	conns []net.Conn
}

func newStallServer(t *testing.T, acceptPublicKey bool) *stallServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stallServer{
		listener: listener,
		signer:   newTestHostKey(t),
		closed:   make(chan struct{}, 16),
		opens:    make(chan struct{}, 16),
	}
	s.config = &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == sshTestUser && string(password) == sshTestPassword {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	if acceptPublicKey {
		s.config.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		}
	}
	s.config.AddHostKey(s.signer)
	go s.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.conns {
			_ = c.Close()
		}
	})
	return s
}

func (s *stallServer) serve() {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, nc)
		s.mu.Unlock()
		go s.handle(nc)
	}
}

func (s *stallServer) handle(nc net.Conn) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.config)
	if err != nil {
		_ = nc.Close()
		s.closed <- struct{}{}
		return
	}
	go ssh.DiscardRequests(reqs)
	// Hold every channel open request unanswered.
	var held []ssh.NewChannel
	for ch := range chans {
		held = append(held, ch)
		s.opens <- struct{}{}
	}
	_ = sc.Wait()
	_ = held
	s.closed <- struct{}{}
}

func (s *stallServer) Connection(name string) Connection {
	return Connection{
		Name:           name,
		Type:           "SSH",
		Host:           "127.0.0.1",
		Port:           s.listener.Addr().(*net.TCPAddr).Port,
		User:           sshTestUser,
		AuthMethod:     "パスワード保管",
		Credential:     sshTestPassword,
		ConnectTimeout: 5,
		Tags:           []string{},
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitGoroutines fails unless the goroutine count drops back to baseline.
func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines: %d, want <= %d\n%s", n, baseline, buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runWithin runs f in a goroutine and fails if it does not return in limit.
func runWithin(t *testing.T, limit time.Duration, f func() error) (error, time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("still blocked after %v", limit)
		return nil, 0
	}
}

// trustStallServer records the stall server's host key (GUI TOFU; the
// handshake alone succeeds) so MCP-origin runs accept it.
func trustStallServer(t *testing.T, conn Connection) {
	t.Helper()
	if err := testSSHConnection(hostKeyTestCtx(t), conn, false); err != nil {
		t.Fatalf("record host key: %v", err)
	}
}

func TestRunSSHCommandSessionOpenHonorsCancel(t *testing.T) {
	isolateEnv(t)
	srv := newStallServer(t, false)
	conn := srv.Connection("stall")
	trustStallServer(t, conn)
	waitSignal(t, srv.closed, "probe connection close")
	conn.ConnectTimeout = 30
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err, took := runWithin(t, 5*time.Second, func() error {
		_, err := runSSHCommand(ctx, conn, "echo hi", 1024, false)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
	if took > 3*time.Second {
		t.Fatalf("took %v after a 300ms ctx", took)
	}
	waitSignal(t, srv.opens, "channel open")
	waitSignal(t, srv.closed, "client to close the connection")
	waitGoroutines(t, baseline)
}

func TestRunSSHCommandSessionOpenTimesOut(t *testing.T) {
	isolateEnv(t)
	srv := newStallServer(t, false)
	conn := srv.Connection("stall")
	trustStallServer(t, conn)
	waitSignal(t, srv.closed, "probe connection close")
	conn.ConnectTimeout = 1
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err, took := runWithin(t, 6*time.Second, func() error {
		_, err := runSSHCommand(ctx, conn, "echo hi", 1024, false)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "did not open a session") {
		t.Fatalf("err=%v, want session open timeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v came from ctx, want ConnectTimeout", err)
	}
	if took < 900*time.Millisecond || took > 4*time.Second {
		t.Fatalf("took %v, want about ConnectTimeout=1s", took)
	}
	waitSignal(t, srv.closed, "client to close the connection")
	waitGoroutines(t, baseline)
}

func TestNewSessionContextCanceledDoesNotRun(t *testing.T) {
	isolateEnv(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")
	client, err := openSSHClient(hostKeyTestCtx(t), conn, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Success path first.
	session, err := newSessionContext(hostKeyTestCtx(t), client, time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	out, err := session.Output("echo ok")
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("run: %q %v", out, err)
	}
	_ = session.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newSessionContext(ctx, client, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want canceled", err)
	}
	// The client is closed and unusable.
	waitDone := make(chan struct{})
	go func() { _ = client.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("client not closed after canceled open")
	}
	if got := srv.Execs(); len(got) != 1 {
		t.Fatalf("execs=%v, want only the first", got)
	}
}

// newStallAgent listens on a Unix socket that accepts connections and reads
// requests but never answers (an unanswered Touch ID / approval prompt or a
// wedged agent). Closed receives one value per client disconnect.
func newStallAgent(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-socket ssh-agent")
	}
	// Short base dir: sun_path is limited to ~104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "sga")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{}, 16)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						closed <- struct{}{}
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return sock, closed
}

func writeEncryptedKey(t *testing.T, dir string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("pass"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "id_enc")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStalledAgentIsBounded(t *testing.T) {
	keyDir := t.TempDir()
	cases := []struct {
		name       string
		credential func() string
		timeout    int
		ctx        time.Duration
		wantCtx    bool
	}{
		// Key file missing: agent-only auth during the handshake.
		{"agent-only/ConnectTimeout", func() string { return filepath.Join(keyDir, "missing") }, 1, time.Minute, false},
		{"agent-only/ctx", func() string { return filepath.Join(keyDir, "missing") }, 60, 300 * time.Millisecond, true},
		// Encrypted key with no passphrase: Signers() runs before dialing.
		{"encrypted/ConnectTimeout", func() string { return writeEncryptedKey(t, keyDir) }, 1, time.Minute, false},
		{"encrypted/ctx", func() string { return writeEncryptedKey(t, keyDir) }, 60, 300 * time.Millisecond, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			sock, agentClosed := newStallAgent(t)
			t.Setenv("SSH_AUTH_SOCK", sock)
			srv := newStallServer(t, true)
			conn := srv.Connection("agent")
			conn.AuthMethod = "SSH鍵認証"
			conn.Credential = tc.credential()
			conn.ConnectTimeout = tc.timeout
			baseline := runtime.NumGoroutine()

			ctx, cancel := context.WithTimeout(context.Background(), tc.ctx)
			defer cancel()
			err, took := runWithin(t, 6*time.Second, func() error {
				return testSSHConnection(ctx, conn, false)
			})
			if err == nil {
				t.Fatal("connected through a silent agent")
			}
			if tc.wantCtx && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v, want ctx deadline", err)
			}
			if took > 4*time.Second {
				t.Fatalf("took %v", took)
			}
			waitSignal(t, agentClosed, "agent connection close")
			waitGoroutines(t, baseline)
		})
	}
}

func TestExpandPathKeepsLiteralDollarAndPercent(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	literal := filepath.Join(dir, "id$prod")
	if err := os.WriteFile(literal, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	percent := filepath.Join(dir, "50%a%b")
	if err := os.WriteFile(percent, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("prod", "SHOULD_NOT_APPEAR")
	t.Setenv("a", "SHOULD_NOT_APPEAR")
	t.Setenv("SGA_KEYDIR", dir)
	t.Setenv("SGA_EMPTY", "")
	_ = os.Unsetenv("SGA_UNSET")

	cases := map[string]string{
		literal:                          literal, // existing file wins
		percent:                          percent,
		`"` + literal + `"`:              literal,
		"~":                              home,
		"~/.ssh/id_ed25519":              filepath.Join(home, ".ssh", "id_ed25519"),
		"$SGA_KEYDIR/id":                 dir + "/id",
		"${SGA_KEYDIR}/id":               dir + "/id",
		"%SGA_KEYDIR%/id":                dir + "/id",
		"/keys/id$SGA_UNSET":             "/keys/id$SGA_UNSET", // unset stays
		"/keys/id${SGA_UNSET}x":          "/keys/id${SGA_UNSET}x",
		"/keys/id$SGA_EMPTY":             "/keys/id$SGA_EMPTY",
		"/keys/50%SGA_UNSET%b":           "/keys/50%SGA_UNSET%b",
		"/keys/a$":                       "/keys/a$",
		"/keys/a$1":                      "/keys/a$1",
		"/keys/%/x/%":                    "/keys/%/x/%",
		"/keys/$SGA_KEYDIR$SGA_UNSET":    "/keys/" + dir + "$SGA_UNSET",
		"/keys/%SGA_KEYDIR%%SGA_UNSET%x": "/keys/" + dir + "%SGA_UNSET%x",
	}
	for in, want := range cases {
		if got := expandPath(in); got != want {
			t.Errorf("expandPath(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestNormalizeWindowsAgentPipe(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`\\.\pipe\openssh-ssh-agent`, `\\.\pipe\openssh-ssh-agent`, true},
		{`\\.\PIPE\my-agent`, `\\.\pipe\my-agent`, true},
		{`\\.\Pipe\my-agent`, `\\.\pipe\my-agent`, true},
		{`//./pipe/my-agent`, `\\.\pipe\my-agent`, true},
		{`\\?\pipe\my-agent`, `\\.\pipe\my-agent`, true},
		{` "\\.\pipe\my agent" `, `\\.\pipe\my agent`, true},
		{`\\.\pipe\`, "", false},
		{`\\.\pipe`, "", false},
		{"", "", false},
		{"/tmp/ssh-XXXX/agent.123", "", false},
		{`C:\Users\u\agent.sock`, "", false},
		{`\\server\pipe\x`, "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeWindowsAgentPipe(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("normalizeWindowsAgentPipe(%q)=(%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
