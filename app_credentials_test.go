package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bug.st/serial"
)

// App-level wiring of the keychain (phase5-keychain.md §5, §8, §9.1 "App のテスト")
// and of the phase 4 serial changes in app.go / approval.go
// (phase4-serial-patch §7.3–§7.5, §9.6).

// credentialRow reads what SQLite holds for a connection's password.
func appCredentialRow(t *testing.T, app *App, id string) (credential string, store string) {
	t.Helper()
	if err := app.store.db.QueryRow(`SELECT credential, credential_store FROM connections WHERE id = ?`, id).Scan(&credential, &store); err != nil {
		t.Fatal(err)
	}
	return credential, store
}

// swapSecrets replaces the app's keychain with a fault-injecting one over
// the current store, and drops cached secrets.
func swapSecrets(app *App) *faultSecretStore {
	fault := newFaultSecretStore(app.secrets)
	app.mu.Lock()
	app.secrets = fault
	app.secretCache = map[string]string{}
	app.mu.Unlock()
	return fault
}

func TestSavePasswordGoesToKeychain(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	saved := addTrustedConnection(t, app, srv.Connection("lab"))

	credential, store := appCredentialRow(t, app, saved.ID)
	if credential != "" || store != credentialStoreKeyring {
		t.Fatalf("row credential=%q store=%q, want empty / keyring", credential, store)
	}
	if got, err := app.secrets.Get(context.Background(), passwordAccount(saved.ID)); err != nil || got != sshTestPassword {
		t.Fatalf("keychain item = %q, %v", got, err)
	}
	var public Connection
	for _, c := range app.GetInitialData().Connections {
		if c.ID == saved.ID {
			public = c
		}
	}
	if public.Credential != "" || !public.CredentialSet {
		t.Fatalf("public connection credential=%q set=%v, want hidden and set", public.Credential, public.CredentialSet)
	}
	// The test result is recorded on the saved row even though the saved
	// side holds only a reference (recordConnectionTest / sameTarget).
	if conn, _ := app.connectionByID(saved.ID); conn.LastTestResult != "ok" {
		t.Fatalf("last test = %q, want ok", conn.LastTestResult)
	}

	// Saving again with the password left blank keeps the keychain item.
	again := saved
	again.Credential = ""
	again.Description = "edited"
	if r := app.SaveConnection(again); !r.OK {
		t.Fatalf("resave: %+v", r)
	}
	if _, store := appCredentialRow(t, app, saved.ID); store != credentialStoreKeyring {
		t.Fatalf("store after blank resave = %q", store)
	}
	if r := app.TestConnection(saved.ID); !r.OK {
		t.Fatalf("test after resave: %+v", r)
	}
}

func TestKeychainUnavailableRefusesSave(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	saved := addTrustedConnection(t, app, srv.Connection("lab"))
	fault := swapSecrets(app)
	fault.hook = func(op, account string, n int) error {
		if op == "set" {
			return ErrSecretUnavailable
		}
		return nil
	}

	before := len(app.GetInitialData().Connections)
	fresh := srv.Connection("lab-2")
	if r := app.SaveConnection(fresh); r.OK || r.Code != "keyring_unavailable" {
		t.Fatalf("new connection while keychain is down: %+v", r)
	}
	if n := len(app.GetInitialData().Connections); n != before {
		t.Fatalf("connections %d -> %d, want the failed one not added", before, n)
	}
	var plaintext int
	if err := app.store.db.QueryRow(`SELECT COUNT(*) FROM connections WHERE credential != '' AND auth_method LIKE '%パスワード%'`).Scan(&plaintext); err != nil {
		t.Fatal(err)
	}
	if plaintext != 0 {
		t.Fatalf("%d plaintext passwords written while the keychain was down", plaintext)
	}

	// A new password on an existing row is refused too; the old one stays.
	edit := saved
	edit.Credential = "another"
	if r := app.SaveConnection(edit); r.OK || r.Code != "keyring_unavailable" {
		t.Fatalf("password change while keychain is down: %+v", r)
	}
	if r := app.TestConnection(saved.ID); !r.OK {
		t.Fatalf("old password must still work: %+v", r)
	}
}

func TestNeedsPasswordAndSessionOnlyPassword(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	saved := addTrustedConnection(t, app, srv.Connection("lab"))

	// The item disappears (deleted in Keychain Access, DB copied elsewhere).
	if err := app.secrets.Delete(context.Background(), passwordAccount(saved.ID)); err != nil {
		t.Fatal(err)
	}
	app.forgetSecrets(saved.ID)
	if r := app.TestConnection(saved.ID); r.OK || r.Code != "needs_password" {
		t.Fatalf("missing item: %+v, want needs_password", r)
	}
	id := "TERM-needs-password"
	if r := app.StartTerminal(id, saved.ID, 80, 24); r.OK || r.Code != "needs_password" {
		t.Fatalf("terminal with missing item: %+v, want needs_password", r)
	}

	if r := app.UseCredentialForSession(saved.ID, sshTestPassword); !r.OK {
		t.Fatalf("session-only password: %+v", r)
	}
	if r := app.TestConnection(saved.ID); !r.OK {
		t.Fatalf("test with the session-only password: %+v", r)
	}
	// Never persisted.
	if credential, _ := appCredentialRow(t, app, saved.ID); credential != "" {
		t.Fatalf("session-only password reached SQLite: %q", credential)
	}
	if _, err := app.secrets.Get(context.Background(), passwordAccount(saved.ID)); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("session-only password reached the keychain: %v", err)
	}
}

func TestMCPCredentialUnavailableFailsFast(t *testing.T) {
	app := newTestApp(t)
	enableBearer(t, app)
	srv := newSSHTestServer(t)
	addTrustedConnection(t, app, srv.Connection("lab"))
	app.setAgentApprovalBypass("trusted", true)
	token := issueAgentToken(t, app, "trusted")
	fault := swapSecrets(app)
	fault.hang.Store(true) // a locked keychain whose prompt nobody answers

	mcpCredentialTimeoutForTest(t, 200*time.Millisecond)
	start := time.Now()
	out, _ := callToolAuth(t, app, token, "execute_command", `{"host":"lab","command":"echo ok","agent_name":"trusted"}`)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("MCP run waited %s for the keychain", elapsed)
	}
	if out["state"] != stateAutoFailed || out["error_kind"] != "credential_unavailable" {
		t.Fatalf("out=%v, want auto_failed / credential_unavailable", out)
	}
	if stderr, _ := out["stderr"].(string); strings.Contains(stderr, "127.0.0.1") {
		t.Fatalf("agent stderr leaks the target: %q", stderr)
	}
	if len(srv.Execs()) != 0 {
		t.Fatalf("command ran without a password: %v", srv.Execs())
	}
}

// mcpCredentialTimeoutForTest shortens the MCP keychain wait.
func mcpCredentialTimeoutForTest(t *testing.T, d time.Duration) {
	t.Helper()
	old := mcpCredentialTimeout
	mcpCredentialTimeout = d
	t.Cleanup(func() { mcpCredentialTimeout = old })
}

func TestKeychainCallsHoldNoAppLock(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	fault := swapSecrets(app)
	var heldDuringCall atomic.Int32
	fault.hook = func(op, account string, n int) error {
		// Another goroutine (the expiry sweeper) holding a.mu for a moment
		// is not what this checks: retry briefly before calling it held.
		for i := 0; i < 50; i++ {
			if app.mu.TryLock() {
				app.mu.Unlock()
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		heldDuringCall.Add(1)
		return nil
	}
	saved := addTestConnection(t, app, srv.Connection("lab"))
	app.forgetSecrets(saved.ID)
	if r := app.TestConnection(saved.ID); !r.OK {
		t.Fatalf("test: %+v", r)
	}
	req := queueOn(t, app, saved, "echo x", "agent")
	app.forgetSecrets(saved.ID)
	if r := app.ApproveCommand(req.ID, ""); !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	if r := app.DeleteConnection(saved.ID); !r.OK {
		t.Fatalf("delete: %+v", r)
	}
	if fault.count("set") == 0 || fault.count("get") == 0 || fault.count("delete") == 0 {
		t.Fatalf("keychain calls set=%d get=%d delete=%d", fault.count("set"), fault.count("get"), fault.count("delete"))
	}
	if n := heldDuringCall.Load(); n != 0 {
		t.Fatalf("%d keychain calls made while a.mu was held", n)
	}
}

func TestDeleteConnectionRemovesKeychainItem(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	saved := addTestConnection(t, app, srv.Connection("lab"))
	if r := app.UseCredentialForSession(saved.ID, "temp"); !r.OK {
		t.Fatalf("session password: %+v", r)
	}
	if r := app.DeleteConnection(saved.ID); !r.OK {
		t.Fatalf("delete: %+v", r)
	}
	if _, err := app.secrets.Get(context.Background(), passwordAccount(saved.ID)); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("keychain item survived delete: %v", err)
	}
	if _, err := app.sessionSecrets.Get(context.Background(), passwordAccount(saved.ID)); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("session password survived delete: %v", err)
	}
	queued, err := app.store.loadSecretGC()
	if err != nil || len(queued) != 0 {
		t.Fatalf("secret_gc after delete = %v, %v", queued, err)
	}
}

func TestSwitchAuthToKeyCollectsPassword(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	saved := addTestConnection(t, app, srv.Connection("lab"))
	key := saved
	key.AuthMethod = "SSH鍵認証"
	key.Credential = "~/.ssh/id_ed25519"
	if r := app.SaveConnection(key); !r.OK {
		t.Fatalf("switch to key auth: %+v", r)
	}
	if _, err := app.secrets.Get(context.Background(), passwordAccount(saved.ID)); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("password item kept after switching to key auth: %v", err)
	}
	if _, store := appCredentialRow(t, app, saved.ID); store != credentialStoreDB {
		t.Fatalf("store after switch = %q, want db (key path)", store)
	}
}

func TestStartupMigratesPlaintextPasswords(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SSH_GETE_NO_SEED", "1")
	store, err := openStore(defaultDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	legacy := Connection{ID: newID(), Name: "legacy", Type: "SSH", Host: "10.0.0.9", Port: 22, User: "u",
		AuthMethod: "パスワード保管", Credential: "old-plaintext", Tags: []string{}}
	if err := store.insertConnection(legacy); err != nil {
		t.Fatal(err)
	}
	store.Close()

	app := NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	if app.keychainPending != 0 || app.GetInitialData().KeychainPending != 0 {
		t.Fatalf("keychainPending=%d, want 0", app.keychainPending)
	}
	if credential, store := appCredentialRow(t, app, legacy.ID); credential != "" || store != credentialStoreKeyring {
		t.Fatalf("after startup credential=%q store=%q", credential, store)
	}
	if got, err := app.secrets.Get(context.Background(), passwordAccount(legacy.ID)); err != nil || got != "old-plaintext" {
		t.Fatalf("migrated item = %q, %v", got, err)
	}
	conn, _ := app.connectionByID(legacy.ID)
	if !credentialInKeyring(conn) {
		t.Fatalf("loaded connection is not a keychain reference: %+v", conn)
	}
	// New plaintext is refused from now on (no fallback to SQLite).
	if !app.store.denyPlaintextPasswords.Load() {
		t.Fatal("denyPlaintextPasswords is off after startup")
	}
}

func TestPlaintextBackupsAPI(t *testing.T) {
	app := newTestApp(t)
	if got := app.PlaintextBackups(); len(got) != 0 {
		t.Fatalf("fresh install backups = %v", got)
	}
	if r := app.DeletePlaintextBackups([]string{app.dbPath}); !r.OK {
		t.Fatalf("delete with nothing listed: %+v", r)
	}
	if !fileExists(app.dbPath) {
		t.Fatal("DeletePlaintextBackups removed a path it did not list")
	}
}

// ---- phase 4 serial (app.go / approval.go) ----

func TestSaveConnectionRejectsInvalidSerialMode(t *testing.T) {
	withFakeSerial(t, "darwin", openFake(newFakeSerialPort()), nil)
	app := newTestApp(t)
	before := len(app.GetInitialData().Connections)
	r := app.SaveConnection(Connection{Name: "uart", Type: "Serial", SerialPort: "/dev/cu.usbserial-1", BaudRate: 9600, DataBits: 8, Parity: "mark", StopBits: "1"})
	if r.OK || r.Code != "invalid_serial_mode" || r.Field != "parity" {
		t.Fatalf("mark parity on darwin: %+v", r)
	}
	if after := len(app.GetInitialData().Connections); after != before {
		t.Fatalf("connections %d -> %d after a refused save", before, after)
	}
}

func TestSaveConnectionAppliesToOpenSerialTab(t *testing.T) {
	port := newFakeSerialPort()
	withFakeSerial(t, "linux", openFake(port), nil)
	app := newTestApp(t)
	conn := addTestConnection(t, app, Connection{Name: "uart", Type: "Serial", SerialPort: "/dev/ttyUSB7", BaudRate: 9600})
	if r := app.StartTerminal("TERM-serial-apply", conn.ID, 80, 24); !r.OK {
		t.Fatalf("open: %+v", r)
	}
	edit := conn
	edit.DataBits, edit.Parity = 7, "even"
	r := app.SaveConnection(edit)
	if !r.OK || r.Code != "serial_mode_applied" {
		t.Fatalf("save with an open tab: %+v", r)
	}
	modes := port.Modes()
	if len(modes) == 0 || modes[len(modes)-1].Parity != serial.EvenParity || modes[len(modes)-1].DataBits != 7 {
		t.Fatalf("modes=%v, want the last one 7E", modes)
	}
	moved := edit
	moved.SerialPort = "/dev/ttyUSB8"
	if r := app.SaveConnection(moved); !r.OK || r.Code != "reconnect_required" {
		t.Fatalf("port change with an open tab: %+v", r)
	}
	if n := app.terminalTabCountFor(conn.ID); n != 1 {
		t.Fatalf("tabs=%d, want the open tab kept", n)
	}
}

// terminalTabCountFor counts the open or connecting tabs of a connection.
func (a *App) terminalTabCountFor(connID string) int {
	rt := a.terminalRuntime()[connID]
	return rt.Open + rt.Connecting
}

func TestApproveRejectsSerial(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTestConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "echo x", "agent")
	serialConn := conn
	serialConn.Type, serialConn.SerialPort, serialConn.BaudRate = "Serial", "/dev/ttyUSB9", 9600
	if r := app.SaveConnection(serialConn); !r.OK {
		t.Fatalf("switch to serial: %+v", r)
	}
	for name, approve := range map[string]func(string, string) ActionResult{
		"ApproveCommand":               app.ApproveCommand,
		"ApproveCommandConfirmed":      app.ApproveCommandConfirmed,
		"ApproveCommandAndBypassAgent": app.ApproveCommandAndBypassAgent,
	} {
		if r := approve(req.ID, ""); r.OK || r.Code != "serial_not_supported" {
			t.Fatalf("%s on a serial target: %+v", name, r)
		}
	}
	if got, _ := app.findRequest(req.ID); got.Status != statePending {
		t.Fatalf("status=%s, want pending", got.Status)
	}
	if len(srv.Execs()) != 0 {
		t.Fatalf("executed: %v", srv.Execs())
	}
}
