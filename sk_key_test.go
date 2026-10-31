package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func sshString(b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

// fakeSKECDSAKey builds an unencrypted openssh-key-v1 file whose key is an
// sk-ecdsa-sha2-nistp256@openssh.com (security key) identity, like the ones
// ssh-keygen -t ecdsa-sk writes for a FIDO key or the Secure Enclave. The
// private section only carries the key handle, so x/crypto/ssh cannot load it.
func fakeSKECDSAKey(t *testing.T) ([]byte, ssh.PublicKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point := elliptic.Marshal(elliptic.P256(), priv.X, priv.Y) //nolint:staticcheck // wire format of the key
	var pubBlob []byte
	pubBlob = append(pubBlob, sshString([]byte(ssh.KeyAlgoSKECDSA256))...)
	pubBlob = append(pubBlob, sshString([]byte("nistp256"))...)
	pubBlob = append(pubBlob, sshString(point)...)
	pubBlob = append(pubBlob, sshString([]byte("ssh:"))...)
	pub, err := ssh.ParsePublicKey(pubBlob)
	if err != nil {
		t.Fatalf("fixture public key: %v", err)
	}

	var private []byte
	private = append(private, 1, 2, 3, 4, 1, 2, 3, 4) // check ints
	private = append(private, pubBlob[:len(pubBlob)-len(sshString([]byte("ssh:")))]...)
	private = append(private, sshString([]byte("ssh:"))...)
	private = append(private, 0x01)                           // flags
	private = append(private, sshString([]byte("handle"))...) // key handle
	private = append(private, sshString(nil)...)              // reserved
	private = append(private, sshString([]byte("comment"))...)
	for i := byte(1); len(private)%8 != 0; i++ {
		private = append(private, i)
	}

	blob := []byte("openssh-key-v1\x00")
	blob = append(blob, sshString([]byte("none"))...)
	blob = append(blob, sshString([]byte("none"))...)
	blob = append(blob, sshString(nil)...)
	blob = binary.BigEndian.AppendUint32(blob, 1)
	blob = append(blob, sshString(pubBlob)...)
	blob = append(blob, sshString(private)...)
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: blob}), pub
}

func TestSecurityKeyPublicKey(t *testing.T) {
	pemBytes, want := fakeSKECDSAKey(t)
	if _, err := ssh.ParsePrivateKey(pemBytes); err == nil {
		t.Fatal("fixture broken: x/crypto/ssh loaded a security key")
	}
	got := securityKeyPublicKey(pemBytes, filepath.Join(t.TempDir(), "absent"))
	if got == nil || string(got.Marshal()) != string(want.Marshal()) {
		t.Fatalf("security key public key = %v, want %v", got, want)
	}

	// The .pub file is the fallback when the envelope cannot be read.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ecdsa_sk")
	if err := os.WriteFile(keyPath+".pub", ssh.MarshalAuthorizedKey(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := securityKeyPublicKey([]byte("garbage"), keyPath); got == nil || string(got.Marshal()) != string(want.Marshal()) {
		t.Fatalf("pub-file fallback = %v", got)
	}

	// Ordinary keys are not security keys.
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(edPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := securityKeyPublicKey(pem.EncodeToMemory(block), filepath.Join(dir, "absent")); got != nil {
		t.Fatalf("ed25519 key reported as a security key: %v", got.Type())
	}
}

// Without an ssh-agent holding it, a security key is refused with a clear
// error instead of "ssh: unhandled key type".
func TestSecurityKeyWithoutAgent(t *testing.T) {
	isolateEnv(t) // SSH_AUTH_SOCK is cleared
	pemBytes, _ := fakeSKECDSAKey(t)
	keyPath := filepath.Join(t.TempDir(), "id_ecdsa_sk_rk")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	conn := Connection{Name: "sk", Host: "127.0.0.1", Port: 22, User: "u", AuthMethod: "SSH鍵認証", Credential: keyPath}
	_, closeAgent, err := sshAuthMethods(context.Background(), conn, time.Now().Add(time.Second))
	closeAgent()
	var skErr *securityKeyNotInAgentError
	if !errors.As(err, &skErr) || skErr.keyPath != keyPath {
		t.Fatalf("sshAuthMethods error = %v, want securityKeyNotInAgentError", err)
	}
	if msg := explainSSHError(conn, err); !strings.Contains(msg, "ssh-add "+keyPath) || !strings.Contains(msg, "Secure Enclave") {
		t.Fatalf("message does not explain ssh-add: %s", msg)
	}
}

// A passing connection test clears a terminal error left by an earlier tab,
// so the sidebar stops showing "接続エラー" for a connection that works.
func TestPassingConnectionTestClearsTerminalError(t *testing.T) {
	app := newTestApp(t)
	conn := app.conns[0]
	app.termMu.Lock()
	app.ensureMaps()
	app.lastErr[conn.ID] = "connection lost"
	app.termMu.Unlock()
	notified := 0
	app.onSessionsChanged = func() { notified++ }

	app.recordConnectionTest(conn, "failed", "still down")
	if app.terminalRuntime()[conn.ID].LastError == "" {
		t.Fatal("a failed test must keep the terminal error")
	}
	app.recordConnectionTest(conn, "ok", "fine")
	if got := app.terminalRuntime()[conn.ID].LastError; got != "" {
		t.Fatalf("terminal error not cleared: %q", got)
	}
	if notified != 1 {
		t.Fatalf("sessions:changed sent %d times, want 1", notified)
	}
	app.recordConnectionTest(conn, "ok", "fine")
	if notified != 1 {
		t.Fatalf("nothing to clear, but sessions:changed sent again (%d)", notified)
	}
}

// Reaching the host by running a command or dialing an AI session also clears
// the terminal error; a run that never reached the host keeps it.
func TestSuccessfulSSHUseClearsTerminalError(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	setError := func() {
		app.termMu.Lock()
		app.ensureMaps()
		app.lastErr[conn.ID] = "connection lost"
		app.termMu.Unlock()
	}
	lastError := func() string { return app.terminalRuntime()[conn.ID].LastError }

	setError()
	if _, err := app.executeSSH(context.Background(), conn, "exit 3", 0, mcpCredentialTimeout); !isExitError(err) {
		t.Fatalf("expected an exit error, got %v", err)
	}
	if got := lastError(); got != "" {
		t.Fatalf("a non-zero exit still reached the host, error kept: %q", got)
	}

	setError()
	client, err := app.dialAISessionClient(context.Background(), conn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = client.Close()
	if got := lastError(); got != "" {
		t.Fatalf("AI session dial kept the error: %q", got)
	}

	setError()
	srv.Close()
	if _, err := app.executeSSH(context.Background(), conn, "echo hi", 0, mcpCredentialTimeout); err == nil {
		t.Fatal("expected a dial failure")
	}
	if lastError() == "" {
		t.Fatal("a failed dial must keep the terminal error")
	}
}

// The sidebar shows the build version from the dashboard data.
func TestDashboardDataCarriesVersion(t *testing.T) {
	if got := newTestApp(t).GetInitialData().Version; got != appVersion {
		t.Fatalf("version %q, want %q", got, appVersion)
	}
}
