package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the app.go / storage.go findings of audit-a6f2706 (#1 #3 #4 #10
// #14 #21 #23 #34 #36 #37) and merge audit 52d876a finding 1 (shutdown order).

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// #1: invalid paths are refused before they are saved.
func TestSaveMCPSettingsRejectsInvalidPaths(t *testing.T) {
	app := newTestApp(t)
	port := freePort(t)
	for name, mutate := range map[string]func(*MCPSettings){
		"no slash":       func(s *MCPSettings) { s.MCPPath = "mcp" },
		"trailing space": func(s *MCPSettings) { s.MCPPath = "/mcp " },
		"duplicate":      func(s *MCPSettings) { s.HealthPath = s.MCPPath },
		"wildcard":       func(s *MCPSettings) { s.AuditPath = "/a/{x" },
		"method":         func(s *MCPSettings) { s.MCPPath = "/GET /x" },
	} {
		settings := app.GetMCPSettings()
		settings.Enabled, settings.ListenAddress, settings.Port = true, "127.0.0.1", port
		mutate(&settings)
		if r := app.SaveMCPSettings(settings); r.OK || r.Code != "invalid_mcp_path" {
			t.Fatalf("%s: saved: %+v", name, r)
		}
		stored, _, loaded, err := app.store.loadMCPSettings(app.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if loaded && validateMCPPaths(stored) != nil {
			t.Fatalf("%s: invalid paths reached SQLite: %+v", name, stored)
		}
	}
	settings := app.GetMCPSettings()
	settings.Enabled, settings.ListenAddress, settings.Port = true, "127.0.0.1", port
	settings.MCPPath = "/v1/mcp"
	if r := app.SaveMCPSettings(settings); !r.OK {
		t.Fatalf("valid path refused: %+v", r)
	}
}

// #1: paths an older version stored do not crash startup; the listener stays
// off with an error the user can fix by saving.
func TestStartupSurvivesStoredInvalidMCPPath(t *testing.T) {
	isolateEnv(t)
	store, err := openStore(defaultDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	bad := defaultMCPSettings(defaultDatabasePath())
	bad.Enabled, bad.ListenAddress, bad.Port = true, "127.0.0.1", freePort(t)
	bad.MCPPath = "mcp"
	bad.HealthPath = "/mcp"
	if err := store.saveMCPSettings(bad, ""); err != nil {
		t.Fatal(err)
	}
	store.Close()

	app := NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	app.startup(context.Background()) // must not panic
	got := app.GetMCPSettings()
	if got.Status != "エラー" || !strings.Contains(got.LastError, "mcp") {
		t.Fatalf("status=%q lastError=%q", got.Status, got.LastError)
	}
	app.lifecycleMu.Lock()
	running := app.mcpServer != nil
	app.lifecycleMu.Unlock()
	if running {
		t.Fatal("listener started with invalid paths")
	}
	// GenerateMCPToken restarts the listener: it must not panic either.
	if r := app.GenerateMCPToken(); r.OK || r.Code != "invalid_mcp_path" {
		t.Fatalf("generate with invalid paths: %+v", r)
	}
	// Fixing the paths from the UI works.
	fixed := app.GetMCPSettings()
	fixed.MCPPath, fixed.HealthPath = "/mcp", "/healthz"
	if r := app.SaveMCPSettings(fixed); !r.OK {
		t.Fatalf("fix refused: %+v", r)
	}
}

func TestGuardMCPStartRecoversPanic(t *testing.T) {
	app := newTestApp(t)
	err := app.guardMCPStart(func() error { panic("pattern conflicts") })
	if err == nil || !strings.Contains(err.Error(), "pattern conflicts") {
		t.Fatalf("panic not turned into an error: %v", err)
	}
}

// #3: a second instance on the same data does not open the store, so it
// cannot rewrite the first one's running request to interrupted.
func TestSecondInstanceIsRefused(t *testing.T) {
	app := newTestApp(t)
	req := queueOn(t, app, app.conns[0], "echo x", "agent")
	if _, err := app.transitionRequest(req.ID, []string{statePending}, func(r *CommandRequest) { r.Status = stateRunning }); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(app.dbPath); !errors.Is(err, errAnotherInstance) {
		t.Fatalf("second openStore: %v", err)
	}
	second := NewApp()
	if second.store != nil || !strings.Contains(second.storageError, "ssh-gete") {
		t.Fatalf("second instance: store=%v storageError=%q", second.store, second.storageError)
	}
	second.shutdown(context.Background())
	row, found, err := app.store.loadCommandRequest(req.ID)
	if err != nil || !found || row.Status != stateRunning {
		t.Fatalf("first instance's running row: %+v found=%v err=%v", row, found, err)
	}
	// Once the first one has closed, the data opens again.
	app.shutdown(context.Background())
	store, err := openStore(app.dbPath)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	store.Close()
}

// #4: deleting from the history keeps the row as the audit record.
func TestDeleteCommandRequestIsLogical(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	req, err := app.queueCommand(app.conns[0], "echo x", "test", "agent", "Low", "cid-1")
	if err != nil {
		t.Fatal(err)
	}
	if r := app.RejectCommand(req.ID); !r.OK {
		t.Fatal(r.Message)
	}
	if r := app.DeleteCommandRequest(req.ID); !r.OK {
		t.Fatalf("delete: %+v", r)
	}
	if _, found, _ := app.store.loadCommandRequest(req.ID); found {
		t.Fatal("deleted request still visible")
	}
	var deletedAt int64
	if err := app.store.db.QueryRow(`SELECT deleted_at FROM command_requests WHERE id = ?`, req.ID).Scan(&deletedAt); err != nil || deletedAt == 0 {
		t.Fatalf("row physically deleted or not marked: deleted_at=%d err=%v", deletedAt, err)
	}
	if history, _ := app.store.loadCommandHistory(historyFilter{}); len(history) != 0 {
		t.Fatalf("deleted request in history: %+v", history)
	}
	// A retry with the same client_request_id still finds it (no re-run).
	if _, found, _ := app.store.loadRequestByClientID("agent", "cid-1", time.Now()); !found {
		t.Fatal("dedup lost the deleted request")
	}
	if err := app.store.deleteCommandRequestUnlessRunning(req.ID); !errors.Is(err, errStateConflict) {
		t.Fatalf("second delete: %v", err)
	}
	app.shutdown(context.Background())
	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if _, ok := reloaded.findRequest(req.ID); ok {
		t.Fatal("deleted request came back after restart")
	}
}

// #10: an import that fails imports nothing, reports storageError, keeps
// the files and is retried at the next start.
func TestLegacyImportFailureIsRetried(t *testing.T) {
	dir := isolateEnv(t)
	connsPath := filepath.Join(dir, "connections.json")
	if err := os.WriteFile(connsPath, []byte(`[{"name":"web","host":"10.0.0.1","user":"u"},`), 0o600); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	if app.storageError == "" {
		t.Fatal("import failure not reported")
	}
	if len(app.conns) != 0 {
		t.Fatalf("seeded or partially imported: %+v", app.conns)
	}
	if !fileExists(connsPath) {
		t.Fatal("legacy file retired although nothing was imported")
	}
	app.shutdown(context.Background())

	fixed := `[{"name":"web","host":"10.0.0.1","user":"u"},{"name":"web ","host":"10.0.0.2","user":"u"},{"name":"db","host":"10.0.0.3","user":"u"}]`
	if err := os.WriteFile(connsPath, []byte(fixed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-settings.json"), []byte(`{"enabled":false,"token":"SECRET-TOKEN"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	if reloaded.storageError != "" {
		t.Fatalf("retry failed: %s", reloaded.storageError)
	}
	names := map[string]bool{}
	for _, c := range reloaded.conns {
		names[c.Name] = true
	}
	if len(reloaded.conns) != 3 || !names["web"] || !names["web (2)"] || !names["db"] {
		t.Fatalf("imported: %+v", names)
	}
	if reloaded.mcpToken != "SECRET-TOKEN" {
		t.Fatal("legacy token not imported")
	}
	if fileExists(connsPath) || !fileExists(connsPath+".migrated") {
		t.Fatal("legacy file not retired after the import")
	}
	if state, _ := reloaded.store.legacyMigrationState(); state != legacyMigrationDone {
		t.Fatalf("legacy_migrated=%q", state)
	}
}

// #34: an empty database file left by an interrupted first start still
// imports the legacy JSON instead of renaming it away.
func TestLegacyImportWithEmptyDatabaseFile(t *testing.T) {
	dir := isolateEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "ssh-gete.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"name":"legacy","host":"10.1.1.1","port":22,"user":"u","authMethod":"パスワード保管","credential":"pw"}]`
	if err := os.WriteFile(filepath.Join(dir, "connections.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	defer app.shutdown(context.Background())
	if len(app.conns) != 1 || app.conns[0].Name != "legacy" {
		t.Fatalf("legacy connection not imported: %+v", app.conns)
	}
}

// #34/#10: a start that committed the schema but died before the import
// imports at the next start (legacy_migrated is still pending).
func TestLegacyImportAfterCrashBeforeImport(t *testing.T) {
	dir := isolateEnv(t)
	store, err := openStore(defaultDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	store.Close() // "crash": schema committed, no import
	legacy := `[{"name":"legacy","host":"10.1.1.1","user":"u"}]`
	if err := os.WriteFile(filepath.Join(dir, "connections.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	defer app.shutdown(context.Background())
	if len(app.conns) != 1 || app.conns[0].Name != "legacy" {
		t.Fatalf("legacy connection not imported: %+v", app.conns)
	}
}

// #14
func TestMaskTokenHidesShortTokens(t *testing.T) {
	for _, tc := range []struct{ token, want string }{
		{"", "未設定"},
		{"hunter2pw", "********"},
		{strings.Repeat("a", 31), "********"},
		{strings.Repeat("a", 60) + "wxyz", "********wxyz"},
	} {
		if got := maskToken(tc.token); got != tc.want {
			t.Fatalf("maskToken(%q)=%q want %q", tc.token, got, tc.want)
		}
	}
}

// #21: Delete serializes with Save's restart, so the stored setting, memory
// and the running listener always agree afterwards.
func TestDeleteMCPSettingsSerializesWithRestart(t *testing.T) {
	app := newTestApp(t)
	port := freePort(t)
	for i := 0; i < 10; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			s := app.GetMCPSettings()
			s.Enabled, s.ListenAddress, s.Port = true, "127.0.0.1", port
			app.SaveMCPSettings(s)
		}()
		go func() {
			defer wg.Done()
			app.DeleteMCPSettings()
		}()
		wg.Wait()
		app.lifecycleMu.Lock()
		running := app.mcpServer != nil
		app.mu.RLock()
		enabled := app.mcp.Enabled
		app.mu.RUnlock()
		app.lifecycleMu.Unlock()
		stored, _, _, _ := app.store.loadMCPSettings(app.dbPath)
		if running != enabled || stored.Enabled != enabled {
			t.Fatalf("round %d: listener=%v memory=%v sqlite=%v", i, running, enabled, stored.Enabled)
		}
	}
}

// #23: no per-user directory means no store, never /tmp.
func TestAppConfigDirHasNoTempFallback(t *testing.T) {
	for _, key := range []string{"SSH_GETE_CONFIG_DIR", "HOME", "XDG_CONFIG_HOME", "APPDATA", "USERPROFILE", "home"} {
		t.Setenv(key, "")
	}
	if dir := appConfigDir(); dir != "" {
		t.Skipf("this platform still resolves a per-user dir: %s", dir)
	}
	if path := defaultDatabasePath(); path != "" {
		t.Fatalf("database path %q", path)
	}
	if _, err := openStore(""); !errors.Is(err, errNoConfigDir) {
		t.Fatalf("openStore(\"\"): %v", err)
	}
	if _, err := openStore("relative.sqlite"); !errors.Is(err, errNoConfigDir) {
		t.Fatalf("openStore(relative): %v", err)
	}
}

// #36: a stored password is not used for a changed host, port or user.
func TestStoredPasswordNotReusedForNewEndpoint(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	saved := addTestConnection(t, app, srv.Connection("prod"))

	edit := saved
	edit.Credential = ""
	edit.Host = "127.0.0.2"
	if r := app.TestConnectionConfig(edit, ""); r.OK || r.Code != "password_required" {
		t.Fatalf("test with changed host: %+v", r)
	}
	if n := len(srv.Execs()); n != 0 {
		t.Fatalf("server contacted: %d", n)
	}
	for name, mutate := range map[string]func(*Connection){
		"host": func(c *Connection) { c.Host = "127.0.0.2" },
		"port": func(c *Connection) { c.Port = saved.Port + 1 },
		"user": func(c *Connection) { c.User = "someone-else" },
	} {
		edit := saved
		edit.Credential = ""
		mutate(&edit)
		if r := app.SaveConnection(edit); r.OK || r.Code != "password_required" {
			t.Fatalf("save with changed %s: %+v", name, r)
		}
	}
	if got, _ := app.connectionByID(saved.ID); got.Host != saved.Host || got.Port != saved.Port || got.User != saved.User {
		t.Fatalf("connection changed: %+v", got)
	}
	// Same endpoint, blank password: kept (rename, description).
	edit = saved
	edit.Credential = ""
	edit.Description = "renamed"
	if r := app.SaveConnection(edit); !r.OK {
		t.Fatalf("same endpoint save: %+v", r)
	}
	if r := app.TestConnectionConfig(edit, ""); !r.OK {
		t.Fatalf("same endpoint test: %+v", r)
	}
	// A new endpoint with the password typed again is fine.
	edit.Port = saved.Port + 1
	edit.Credential = "another"
	if r := app.SaveConnection(edit); !r.OK {
		t.Fatalf("new endpoint with password: %+v", r)
	}
}

// #37: a final state that cannot be stored is still published, reported as
// a storage error, and returned as such.
func TestFinalizeRequestPublishesOnStoreFailure(t *testing.T) {
	app := newTestApp(t)
	req := queueOn(t, app, app.conns[0], "echo x", "agent")
	if _, err := app.transitionRequest(req.ID, []string{statePending}, func(r *CommandRequest) { r.Status = stateRunning }); err != nil {
		t.Fatal(err)
	}
	if err := app.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := app.finalizeRequest(req.ID, stateRunning, func(r *CommandRequest) {
		r.Status = stateSucceeded
		r.Stdout = "ok\n"
	})
	var notSaved *errFinalStateNotSaved
	if !errors.As(err, &notSaved) {
		t.Fatalf("err=%v", err)
	}
	if got.Status != stateSucceeded {
		t.Fatalf("returned %+v", got)
	}
	if mem, _ := app.findRequest(req.ID); mem.Status != stateSucceeded || mem.Stdout != "ok\n" {
		t.Fatalf("memory left at %+v", mem)
	}
	if app.GetInitialData().StorageError == "" {
		t.Fatal("storageError not set")
	}
}

// #3 / #37: losing the CAS to a row another process changed adopts that row
// instead of leaving the request running in memory.
func TestFinalizeRequestAdoptsChangedRow(t *testing.T) {
	app := newTestApp(t)
	req := queueOn(t, app, app.conns[0], "echo x", "agent")
	if _, err := app.transitionRequest(req.ID, []string{statePending}, func(r *CommandRequest) { r.Status = stateRunning }); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.db.Exec(`UPDATE command_requests SET status = ?, error_kind = 'canceled' WHERE id = ?`, stateInterrupted, req.ID); err != nil {
		t.Fatal(err)
	}
	got, err := app.finalizeRequest(req.ID, stateRunning, func(r *CommandRequest) { r.Status = stateSucceeded })
	if !errors.Is(err, errStateConflict) || got.Status != stateInterrupted {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if mem, _ := app.findRequest(req.ID); mem.Status != stateInterrupted {
		t.Fatalf("memory left at %s", mem.Status)
	}
}

// Merge audit 1 / §5.0 step 2: shutdown waits the grace, so a command that
// ends within it is recorded as completed, with its output.
func TestShutdownLetsCommandFinishWithinGrace(t *testing.T) {
	old := shutdownExecGrace
	shutdownExecGrace = 5 * time.Second
	t.Cleanup(func() { shutdownExecGrace = old })

	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("web-01"))
	req, done := sessStartApproved(t, app, conn, "sleep 1")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool { return app.runningTotal.Load() == 1 })

	start := time.Now()
	app.shutdown(context.Background())
	if elapsed := time.Since(start); elapsed >= shutdownExecGrace {
		t.Fatalf("shutdown waited the whole grace (%s) for a 1s command", elapsed)
	}
	if r := <-done; !r.OK {
		t.Fatalf("approve: %+v", r)
	}
	got, _ := app.findRequest(req.ID)
	if got.Status != stateSucceeded {
		t.Fatalf("status=%s kind=%s, want succeeded", got.Status, got.ErrorKind)
	}
	// Persisted before the store closed.
	db, err := sql.Open("sqlite", app.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status string
	if err := db.QueryRow(`SELECT status FROM command_requests WHERE id = ?`, req.ID).Scan(&status); err != nil || status != stateSucceeded {
		t.Fatalf("stored status=%q err=%v", status, err)
	}
}

// Merge audit 1 / §5.0 step 2: a command still running after the grace is
// cancelled and recorded as interrupted; new executions are refused at once.
func TestShutdownInterruptsAfterGrace(t *testing.T) {
	old := shutdownExecGrace
	shutdownExecGrace = 700 * time.Millisecond
	t.Cleanup(func() { shutdownExecGrace = old })

	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("web-01"))
	req, done := sessStartApproved(t, app, conn, "sleep 30")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool { return app.runningTotal.Load() == 1 })

	start := time.Now()
	app.shutdown(context.Background())
	elapsed := time.Since(start)
	if elapsed < shutdownExecGrace {
		t.Fatalf("shutdown cancelled before the grace: %s", elapsed)
	}
	if elapsed > 2*shutdownExecGrace+3*time.Second {
		t.Fatalf("shutdown took %s", elapsed)
	}
	<-done
	got, _ := app.findRequest(req.ID)
	if got.Status != stateInterrupted || got.ErrorKind != "canceled" {
		t.Fatalf("status=%s kind=%s, want interrupted/canceled", got.Status, got.ErrorKind)
	}
	ctx, finish := app.trackExec(context.Background(), conn.ID)
	defer finish()
	if context.Cause(ctx) != errShuttingDown {
		t.Fatalf("post-shutdown exec cause=%v", context.Cause(ctx))
	}
}

// Intake closes at the start of shutdown, before the grace: an approval made
// while draining is refused rather than run.
func TestShutdownRefusesNewExecutionsDuringGrace(t *testing.T) {
	old := shutdownExecGrace
	shutdownExecGrace = 1500 * time.Millisecond
	t.Cleanup(func() { shutdownExecGrace = old })

	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("web-01"))
	_, done := sessStartApproved(t, app, conn, "sleep 30")
	sessWaitFor(t, 3*time.Second, "running=1", func() bool { return app.runningTotal.Load() == 1 })
	late := queueOn(t, app, conn, "echo late", "agent")

	finished := make(chan struct{})
	go func() {
		app.shutdown(context.Background())
		close(finished)
	}()
	sessWaitFor(t, 2*time.Second, "intake closed", func() bool {
		app.execMu.Lock()
		defer app.execMu.Unlock()
		return app.execsClosed
	})
	if r := app.ApproveCommand(late.ID, ""); r.OK {
		t.Fatalf("approval during shutdown ran: %+v", r)
	}
	<-finished
	<-done
	for _, cmd := range srv.Execs() {
		if strings.Contains(cmd, "late") {
			t.Fatal("late command executed")
		}
	}
}

// K3 follow-up: an old serial row kept its port in Host. The import and the
// load move it to SerialPort before Host is lowercased, so the port opens
// (/dev/ttyUSB0, not /dev/ttyusb0) and a later save does not clear it.
func TestLegacySerialPortInHostIsLifted(t *testing.T) {
	dir := isolateEnv(t)
	connsPath := filepath.Join(dir, "connections.json")
	if err := os.WriteFile(connsPath, []byte(`[{"name":"board","type":"Serial","host":" /dev/ttyUSB0 ","baudRate":9600}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	if app.storageError != "" {
		t.Fatalf("import: %s", app.storageError)
	}
	if len(app.conns) != 1 || app.conns[0].SerialPort != "/dev/ttyUSB0" || app.conns[0].Host != "" {
		t.Fatalf("imported: %+v", app.conns)
	}
	id := app.conns[0].ID
	// A row stored by an older build: port in host, serial_port empty.
	if _, err := app.store.db.Exec(`UPDATE connections SET host = '/dev/ttyACM1', serial_port = '' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	app.shutdown(context.Background())

	reloaded := NewApp()
	defer reloaded.shutdown(context.Background())
	got, ok := reloaded.connectionByID(id)
	if !ok || got.SerialPort != "/dev/ttyACM1" || got.Host != "" {
		t.Fatalf("loaded: %+v", got)
	}
	if n := normalizeConnection(Connection{Type: "Serial", Host: "COM3"}); n.SerialPort != "COM3" || n.Host != "" {
		t.Fatalf("normalize: %+v", n)
	}
	if n := normalizeConnection(Connection{Type: "SSH", Host: "Web01"}); n.SerialPort != "" || n.Host != "web01" {
		t.Fatalf("normalize ssh: %+v", n)
	}
}
