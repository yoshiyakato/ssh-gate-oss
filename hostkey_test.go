package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func hostKeyTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func readKnownHosts(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(knownHostsPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

func TestHostKeyGUIRecordsThenMCPAccepts(t *testing.T) {
	isolateEnv(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")

	if err := testSSHConnection(hostKeyTestCtx(t), conn, false); err != nil {
		t.Fatalf("GUI first connect: %v", err)
	}
	info, err := os.Stat(knownHostsPath())
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && info.Mode().Perm() != 0o600 {
		t.Fatalf("known_hosts mode %v, want 0600", info.Mode().Perm())
	}
	want := knownhosts.Line([]string{knownhosts.Normalize(hostKeyAddress(conn))}, srv.HostKey()) + "\n"
	if got := readKnownHosts(t); got != want {
		t.Fatalf("known_hosts=%q, want %q", got, want)
	}
	result, err := runSSHCommand(hostKeyTestCtx(t), conn, "echo ok", 1024, false)
	if err != nil || strings.TrimSpace(result.Stdout) != "ok" {
		t.Fatalf("MCP run on recorded key: %+v %v", result, err)
	}
	// A second GUI connect does not append a duplicate line.
	if err := testSSHConnection(hostKeyTestCtx(t), conn, false); err != nil {
		t.Fatal(err)
	}
	if got := readKnownHosts(t); got != want {
		t.Fatalf("known_hosts grew: %q", got)
	}
}

func TestHostKeyMCPUnknownIsNotRecorded(t *testing.T) {
	isolateEnv(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")

	for _, strict := range []bool{false, true} {
		_, err := runSSHCommand(hostKeyTestCtx(t), conn, "echo x", 1024, strict)
		var unknown *hostKeyUnknownError
		if !errors.As(err, &unknown) || hostKeyErrorKind(err) != "hostkey_unknown" {
			t.Fatalf("strict=%v: want hostkey_unknown, got %v", strict, err)
		}
		if unknown.Fingerprint != ssh.FingerprintSHA256(srv.HostKey()) {
			t.Fatalf("fingerprint %s", unknown.Fingerprint)
		}
	}
	if got := readKnownHosts(t); got != "" {
		t.Fatalf("MCP recorded a key: %q", got)
	}
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("command reached an untrusted host: %v", execs)
	}
}

func TestHostKeyMismatchRejectedForEveryOrigin(t *testing.T) {
	isolateEnv(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")
	if err := testSSHConnection(hostKeyTestCtx(t), conn, false); err != nil {
		t.Fatal(err)
	}
	before := readKnownHosts(t)
	srv.RotateHostKey(t)

	for _, strict := range []bool{false, true} {
		err := testSSHConnection(hostKeyTestCtx(t), conn, strict)
		if hostKeyErrorKind(err) != "hostkey" {
			t.Fatalf("GUI strict=%v: want hostkey, got %v", strict, err)
		}
		_, err = runSSHCommand(hostKeyTestCtx(t), conn, "echo spoofed", 1024, strict)
		var mismatch *hostKeyMismatchError
		if !errors.As(err, &mismatch) || len(mismatch.Known) != 1 {
			t.Fatalf("MCP strict=%v: want mismatch with one known key, got %v", strict, err)
		}
		if _, err := openSSHClient(hostKeyTestCtx(t), conn, strict); hostKeyErrorKind(err) != "hostkey" {
			t.Fatalf("terminal strict=%v: want hostkey, got %v", strict, err)
		}
	}
	if got := readKnownHosts(t); got != before {
		t.Fatalf("mismatch rewrote known_hosts: %q", got)
	}
	if execs := srv.Execs(); len(execs) != 0 {
		t.Fatalf("command reached a host with a changed key: %v", execs)
	}
}

// TestHostKeyAlgorithmsFollowWant: the server offers ECDSA and Ed25519 but
// only Ed25519 is recorded. Restricting HostKeyAlgorithms to the recorded
// type keeps the client from negotiating ECDSA and reporting a mismatch.
func TestHostKeyAlgorithmsFollowWant(t *testing.T) {
	isolateEnv(t)
	edSigner := newTestHostKey(t)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecSigner, err := ssh.NewSignerFromKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	port := newHandshakeOnlyServer(t, ecSigner, edSigner)
	conn := Connection{Name: "multi", Type: "SSH", Host: "127.0.0.1", Port: port, User: sshTestUser,
		AuthMethod: "パスワード保管", Credential: sshTestPassword, ConnectTimeout: 5}
	addr := hostKeyAddress(conn)
	if err := recordHostKey(addr, edSigner.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := hostKeyFiles(false); err != nil {
		t.Fatal(err)
	}
	check, err := knownhosts.New(knownHostsPath())
	if err != nil {
		t.Fatal(err)
	}
	if algos := hostKeyAlgorithmsFor(knownKeysFor(check, addr)); len(algos) != 1 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms=%v", algos)
	}
	if err := testSSHConnection(hostKeyTestCtx(t), conn, false); err != nil {
		t.Fatalf("connect with recorded ed25519 key: %v", err)
	}
	if _, err := runSSHCommand(hostKeyTestCtx(t), conn, "true", 1024, false); hostKeyErrorKind(err) != "" {
		t.Fatalf("MCP misread the second key type as a host key failure: %v", err)
	}
}

func TestHostKeyAlgorithmsForRSA(t *testing.T) {
	if got := hostKeyAlgorithmsFor(nil); got != nil {
		t.Fatalf("unknown host must use library defaults, got %v", got)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	got := hostKeyAlgorithmsFor([]knownhosts.KnownKey{{Key: pub}, {Key: pub}})
	want := []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// newHandshakeOnlyServer accepts password auth with the given host keys and
// rejects every channel; enough for testSSHConnection.
func newHandshakeOnlyServer(t *testing.T, signers ...ssh.Signer) int {
	t.Helper()
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == sshTestUser && string(password) == sshTestPassword {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	for _, s := range signers {
		config.AddHostKey(s)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			nc, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer nc.Close()
				_, chans, reqs, err := ssh.NewServerConn(nc, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "handshake only")
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestHostKeyConcurrentFirstGUIConnectRecordsOnce(t *testing.T) {
	isolateEnv(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- testSSHConnection(hostKeyTestCtx(t), conn, false)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(readKnownHosts(t), "\n"); n != 1 {
		t.Fatalf("recorded %d lines, want 1:\n%s", n, readKnownHosts(t))
	}
}

func TestHostKeyFingerprintAndReset(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))

	info, err := app.hostKeyFingerprint(conn.ID)
	if err != nil || info.Known || len(info.Keys) != 0 {
		t.Fatalf("before trust: %+v %v", info, err)
	}
	if r := app.TestConnection(conn.ID); !r.OK {
		t.Fatalf("GUI test: %s", r.Message)
	}
	info, err = app.hostKeyFingerprint(conn.ID)
	if err != nil || !info.Known || len(info.Keys) != 1 || info.Keys[0].Fingerprint != ssh.FingerprintSHA256(srv.HostKey()) {
		t.Fatalf("after trust: %+v %v", info, err)
	}
	if info.Address != knownhosts.Normalize(hostKeyAddress(conn)) || info.Keys[0].Type != ssh.KeyAlgoED25519 {
		t.Fatalf("info=%+v", info)
	}

	gen := hostKeyGeneration()
	newKey := srv.RotateHostKey(t)
	if r := app.TestConnection(conn.ID); r.OK {
		t.Fatalf("GUI accepted a changed key")
	}
	removed, err := app.resetHostKey(conn.ID)
	if err != nil || removed != 1 {
		t.Fatalf("reset removed=%d err=%v", removed, err)
	}
	if hostKeyGeneration() == gen {
		t.Fatalf("reset did not bump the known_hosts generation")
	}
	if info, _ := app.hostKeyFingerprint(conn.ID); info.Known {
		t.Fatalf("still known after reset: %+v", info)
	}
	if r := app.TestConnection(conn.ID); !r.OK {
		t.Fatalf("GUI test after reset: %s", r.Message)
	}
	if info, _ := app.hostKeyFingerprint(conn.ID); len(info.Keys) != 1 || info.Keys[0].Fingerprint != ssh.FingerprintSHA256(newKey) {
		t.Fatalf("new key not recorded: %+v", info)
	}
	if removed, err := app.resetHostKey(conn.ID); err != nil || removed != 1 {
		t.Fatalf("second reset removed=%d err=%v", removed, err)
	}
	if removed, err := app.resetHostKey(conn.ID); err != nil || removed != 0 {
		t.Fatalf("reset of unknown host removed=%d err=%v", removed, err)
	}
	if _, err := app.hostKeyFingerprint("no-such-id"); err == nil {
		t.Fatalf("unknown connID must fail")
	}
}

func TestRemoveHostKeysKeepsOtherLines(t *testing.T) {
	isolateEnv(t)
	a, b := newTestHostKey(t).PublicKey(), newTestHostKey(t).PublicKey()
	if _, err := hostKeyFiles(false); err != nil {
		t.Fatal(err)
	}
	content := "# comment\n\n" +
		knownhosts.Line([]string{"example.com"}, a) + "\n" +
		knownhosts.Line([]string{"[example.com]:2222"}, b) + "\n" +
		knownhosts.Line([]string{"other.test"}, a) + "\n"
	if err := os.WriteFile(knownHostsPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Port 22 normalizes to the bare host, so only the first key line goes.
	removed, err := removeHostKeys("example.com:22")
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	want := "# comment\n\n" +
		knownhosts.Line([]string{"[example.com]:2222"}, b) + "\n" +
		knownhosts.Line([]string{"other.test"}, a) + "\n"
	if got := readKnownHosts(t); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if info, err := os.Stat(knownHostsPath()); err == nil && os.PathSeparator == '/' && info.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten mode %v", info.Mode().Perm())
	}
	keys, err := recordedHostKeys("example.com:2222")
	if err != nil || len(keys) != 1 || keys[0].Fingerprint != ssh.FingerprintSHA256(b) {
		t.Fatalf("keys=%+v err=%v", keys, err)
	}
}

// TestHostKeyStrictGUIUnknownIsNotRecorded: strict mode never records, so a
// host in neither known_hosts file fails on a GUI dial too, as it did before
// TOFU, and nothing is written.
func TestHostKeyStrictGUIUnknownIsNotRecorded(t *testing.T) {
	isolateEnv(t)
	srv := newSSHTestServer(t)
	conn := srv.Connection("lab")
	if err := testSSHConnection(hostKeyTestCtx(t), conn, true); hostKeyErrorKind(err) != "hostkey_unknown" {
		t.Fatalf("strict GUI dial to an unknown host: want hostkey_unknown, got %v", err)
	}
	if _, err := openSSHClient(hostKeyTestCtx(t), conn, true); hostKeyErrorKind(err) != "hostkey_unknown" {
		t.Fatalf("strict terminal dial to an unknown host: want hostkey_unknown, got %v", err)
	}
	t.Setenv("SSH_GETE_STRICT_HOSTKEY", "1")
	if err := testSSHConnection(hostKeyTestCtx(t), conn, false); hostKeyErrorKind(err) != "hostkey_unknown" {
		t.Fatalf("SSH_GETE_STRICT_HOSTKEY=1 GUI dial: want hostkey_unknown, got %v", err)
	}
	if got := readKnownHosts(t); got != "" {
		t.Fatalf("strict mode recorded a key: %q", got)
	}
	if _, err := runSSHCommand(hostKeyTestCtx(t), conn, "echo x", 1024, false); hostKeyErrorKind(err) != "hostkey_unknown" {
		t.Fatalf("MCP run after strict GUI dial: %v", err)
	}
}

// TestHostKeyHostIsCaseInsensitive: "LocalHost" and "localhost" are one
// TOFU identity for recording, lookup and reset, and a lowercase line that
// OpenSSH wrote in ~/.ssh/known_hosts matches in strict mode.
func TestHostKeyHostIsCaseInsensitive(t *testing.T) {
	isolateEnv(t)
	srv := newSSHTestServer(t)
	upper := srv.Connection("upper")
	upper.Host = "LocalHost"
	lower := srv.Connection("lower")
	lower.Host = "localhost"

	if err := testSSHConnection(hostKeyTestCtx(t), upper, false); err != nil {
		t.Fatalf("GUI record via LocalHost: %v", err)
	}
	if _, err := runSSHCommand(hostKeyTestCtx(t), lower, "echo ok", 1024, false); err != nil {
		t.Fatalf("MCP via localhost after recording LocalHost: %v", err)
	}
	if hostKeyAddress(upper) != hostKeyAddress(lower) {
		t.Fatalf("addresses differ: %s / %s", hostKeyAddress(upper), hostKeyAddress(lower))
	}
	if n, err := removeHostKeys(hostKeyAddress(upper)); err != nil || n != 1 {
		t.Fatalf("reset via LocalHost: removed=%d err=%v", n, err)
	}
	if _, err := runSSHCommand(hostKeyTestCtx(t), lower, "echo x", 1024, false); hostKeyErrorKind(err) != "hostkey_unknown" {
		t.Fatalf("after reset: %v", err)
	}

	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(home+"/.ssh", 0o700); err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(hostKeyAddress(lower))}, srv.HostKey()) + "\n"
	if err := os.WriteFile(home+"/.ssh/known_hosts", []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testSSHConnection(hostKeyTestCtx(t), upper, true); err != nil {
		t.Fatalf("strict LocalHost against a lowercase user entry: %v", err)
	}
}

// TestHostKeyCertAuthorityInStrictMode: a host covered by an @cert-authority
// line in ~/.ssh/known_hosts presents its certificate and is accepted; the
// CA's plain key type must not restrict HostKeyAlgorithms.
func TestHostKeyCertAuthorityInStrictMode(t *testing.T) {
	isolateEnv(t)
	ca := newTestHostKey(t)
	hostSigner := newTestHostKey(t)
	cert := &ssh.Certificate{
		Key:             hostSigner.PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{"127.0.0.1"},
		ValidAfter:      0,
		ValidBefore:     ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	certSigner, err := ssh.NewCertSigner(cert, hostSigner)
	if err != nil {
		t.Fatal(err)
	}
	port := newHandshakeOnlyServer(t, certSigner)

	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(home+"/.ssh", 0o700); err != nil {
		t.Fatal(err)
	}
	conn := Connection{Name: "cert", Type: "SSH", Host: "127.0.0.1", Port: port, User: sshTestUser,
		AuthMethod: "パスワード保管", Credential: sshTestPassword, ConnectTimeout: 5}
	line := "@cert-authority " + knownhosts.Line([]string{knownhosts.Normalize(hostKeyAddress(conn))}, ca.PublicKey()) + "\n"
	if err := os.WriteFile(home+"/.ssh/known_hosts", []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testSSHConnection(hostKeyTestCtx(t), conn, true); err != nil {
		t.Fatalf("strict dial to a CA-certified host: %v", err)
	}
	if got := readKnownHosts(t); got != "" {
		t.Fatalf("a certified host was recorded: %q", got)
	}
}
