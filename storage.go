package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is stored in PRAGMA user_version. v2 introduced immutable
// connection IDs, request state codes and approval/audit columns. v3 added
// agent_tokens, bypass expiry/scope on agent_policies and serial line
// parameters on connections. v4 added connections.credential_store /
// passphrase_saved and the secret_gc queue for the OS keychain
// (phase5-keychain.md); the passwords themselves move out of the credential
// column in migrateCredentialsToKeyring, which needs a SecretStore and so
// runs after openStore. v5 added the ai_session_* tables (REQ-016,
// migrateAISessionTables; CREATE only, so it runs on every open).
const schemaVersion = aiSessionSchemaVersion // 5: ai_session_* tables (REQ-016)

var errStoreUnavailable = errors.New("SQLiteストアが初期化されていないため保存できません")

// errStateConflict is returned when a request transition loses a race (the
// request is no longer in the expected state).
var errStateConflict = errors.New("request state changed")

type Store struct {
	db   *sql.DB
	path string
	// lock holds the single-instance lock (<path>.lock) until Close.
	lock *sql.DB
	// fresh reports whether the database held no schema at all before this
	// open (user_version 0 and an empty sqlite_master), so an empty file left
	// by an interrupted first start still counts as new (audit #34).
	fresh bool
	// prevVersion is the user_version found on open (before migration).
	prevVersion int
	// denyPlaintextPasswords makes insert/updateConnection refuse to write a
	// new password into the credential column (keychain-only mode). The App
	// turns it on after migrateCredentialsToKeyring.
	denyPlaintextPasswords atomic.Bool
}

// errPlaintextPassword is returned when a password would be written to
// SQLite while denyPlaintextPasswords is on.
var errPlaintextPassword = errors.New("パスワードを平文でデータベースに保存することはできません（キーチェーンに保存してください）")

// Values of connections.credential_store.
const (
	credentialStoreDB      = "db"      // credential column holds the value (legacy, key paths)
	credentialStoreKeyring = "keyring" // credential column is empty; value is in the SecretStore
)

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// errAnotherInstance is returned by openStore when another ssh-gete process
// (or another Store in this process) already has the database open. The
// second instance must not touch it: recoverInterruptedRequests would turn
// the first one's running commands into interrupted (audit #3).
var errAnotherInstance = errors.New("別の ssh-gete が同じデータを使用中のため、保存先を開けません。起動中の ssh-gete を使うか、終了してから起動し直してください / Another ssh-gete instance is using the same data; use it, or quit it and start again")

// errNoConfigDir is returned for an empty database path: appConfigDir found
// no per-user directory and there is no safe fallback (audit #23).
var errNoConfigDir = errors.New("設定の保存先（ユーザーの設定ディレクトリまたはホームディレクトリ）が見つかりません。SSH_GETE_CONFIG_DIR を指定してください / No per-user config directory or home directory was found; set SSH_GETE_CONFIG_DIR")

func openStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, errNoConfigDir
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// MkdirAll leaves an existing directory's mode alone; the database and
	// its backups hold credentials, so tighten it (audit #12). On Windows
	// this also sets a protected owner+SYSTEM DACL that files created in it
	// later (-wal, -shm, backups) inherit (N41).
	if err := restrictToOwner(dir); err != nil {
		log.Printf("restrict %s: %v", dir, err)
	}
	lock, err := acquireStoreLock(path)
	if err != nil {
		return nil, err
	}
	if !fileExists(path) {
		// Create the file with owner-only permissions before SQLite opens it.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			_ = lock.Close()
			return nil, err
		}
		_ = f.Close()
		if err := restrictToOwner(path); err != nil {
			log.Printf("restrict %s: %v", path, err)
		}
	}
	// secure_delete zero-fills freed content, so a password removed from the
	// credential column does not linger in free pages.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=secure_delete(on)")
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	// A single connection serializes writes so read-check-write sequences
	// (state transitions) cannot interleave inside SQLite.
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path, lock: lock}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		_ = lock.Close()
		return nil, err
	}
	restrictDBFilePermissions(path)
	return store, nil
}

// acquireStoreLock takes the single-instance lock of dbPath: a small
// SQLite file <dbPath>.lock held in EXCLUSIVE locking mode. After its first
// write the connection keeps the exclusive file lock until it is closed (or
// the process dies, when the OS drops it), so a second process, or a second
// Store in this one, gets SQLITE_BUSY at once and openStore fails with
// errAnotherInstance. It works the same on macOS, Linux and Windows.
func acquireStoreLock(dbPath string) (*sql.DB, error) {
	lockPath := dbPath + ".lock"
	if !fileExists(lockPath) {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	}
	db, err := sql.Open("sqlite", lockPath+"?_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		return nil, err
	}
	// Exactly one connection that is never recycled: closing it would drop
	// the lock.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS instance (id INTEGER PRIMARY KEY CHECK (id = 1), pid INTEGER NOT NULL, started_at TEXT NOT NULL)`,
		`INSERT INTO instance (id, pid, started_at) VALUES (1, ?, ?)
			ON CONFLICT(id) DO UPDATE SET pid = excluded.pid, started_at = excluded.started_at`,
	} {
		var args []any
		if strings.Contains(stmt, "INSERT") {
			args = []any{os.Getpid(), nowRFC3339()}
		}
		if _, err := db.Exec(stmt, args...); err != nil {
			_ = db.Close()
			if isSQLiteBusy(err) {
				return nil, errAnotherInstance
			}
			return nil, fmt.Errorf("instance lock: %w", err)
		}
	}
	return db, nil
}

func isSQLiteBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// restrictDBFilePermissions makes the database and every file next to it
// that holds the same data (WAL, shared memory, lock, pre-migration backups)
// owner-only. The shared MCP token and the bypass grants stay in the
// database, so on Windows, where 0600 means nothing, each also gets a
// protected owner+SYSTEM DACL instead of inheriting the parent's (N41).
func restrictDBFilePermissions(path string) {
	paths := []string{path, path + "-wal", path + "-shm", path + ".lock"}
	backups, _ := filepath.Glob(globEscape(path) + ".bak-v*")
	paths = append(paths, backups...)
	for _, p := range paths {
		if fileExists(p) {
			if err := restrictToOwner(p); err != nil {
				log.Printf("restrict %s: %v", p, err)
			}
		}
	}
}

// restrictACL sets a protected DACL granting only the current user and
// SYSTEM (directories: inherited by what is created in them). It is a no-op
// except on Windows (storage_windows.go), where mode bits do not restrict
// access.
var restrictACL = func(path string, isDir bool) error { return nil }

// restrictToOwner limits path to the current user: mode 0600 (0700 for a
// directory), and on Windows a protected DACL (restrictACL).
func restrictToOwner(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info.IsDir() {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return restrictACL(path, info.IsDir())
}

// Close closes the database, then releases the single-instance lock.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	if s.lock != nil {
		_ = s.lock.Close()
	}
	return err
}

// backupBeforeMigration writes a complete copy of the database as
// <path>.bak-v<N>. The copy goes to a temporary name first and is renamed only
// after VACUUM INTO succeeded, so a crash never leaves a partial file under the
// backup name. An existing .bak-v<N> (from an earlier upgrade, possibly of
// older data) is kept and this copy gets a timestamped name instead, so every
// migration has a copy taken right before it.
func (s *Store) backupBeforeMigration() (string, error) {
	backup := fmt.Sprintf("%s.bak-v%d", s.path, s.prevVersion)
	if fileExists(backup) {
		backup = fmt.Sprintf("%s.bak-v%d-%d", s.path, s.prevVersion, time.Now().UnixNano())
	}
	tmp := fmt.Sprintf("%s.tmp-%d", backup, time.Now().UnixNano())
	// The copy holds every credential. Create it empty and owner-only first
	// (O_EXCL: never reuse a file someone else made); VACUUM INTO accepts an
	// empty target and keeps its mode, so the copy is never readable by
	// others, not even between the write and a chmod (audit #12).
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	// umask cannot widen the mode, but be explicit; on Windows this is also
	// where the copy gets its owner-only DACL, before any data is in it (N41).
	if err := restrictToOwner(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, backup); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return backup, nil
}

func (s *Store) migrate() error {
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&s.prevVersion); err != nil {
		return err
	}
	if s.prevVersion > schemaVersion {
		return fmt.Errorf("このデータベースは新しいバージョン(v%d)の ssh-gete で作成されています。アプリを更新してください", s.prevVersion)
	}
	var objects int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&objects); err != nil {
		return err
	}
	s.fresh = s.prevVersion == 0 && objects == 0
	if _, err := s.db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return err
	}
	if s.prevVersion < schemaVersion && !s.fresh {
		// Keep a copy of the pre-migration database for rollback. This runs
		// before any DDL so the copy is exactly what the older version wrote.
		if _, err := s.backupBeforeMigration(); err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS connections (
			name TEXT PRIMARY KEY,
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			user TEXT NOT NULL,
			auth_method TEXT NOT NULL,
			credential TEXT NOT NULL,
			connect_timeout INTEGER NOT NULL,
			command_timeout INTEGER NOT NULL,
			tags_json TEXT NOT NULL,
			description TEXT NOT NULL,
			status TEXT NOT NULL,
			last_checked TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mcp_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			payload_json TEXT NOT NULL,
			token TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS command_requests (
			id TEXT PRIMARY KEY,
			host TEXT NOT NULL,
			user TEXT NOT NULL,
			command TEXT NOT NULL,
			reason TEXT NOT NULL,
			requested_by TEXT NOT NULL,
			requested_at TEXT NOT NULL,
			risk TEXT NOT NULL,
			status TEXT NOT NULL,
			stdout TEXT NOT NULL,
			stderr TEXT NOT NULL,
			duration TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS agent_policies (
			agent_name TEXT PRIMARY KEY,
			approval_bypass INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		// agent_tokens binds an agent name to its own Bearer token. Only the
		// SHA-256 of the token is stored; the plaintext is shown once at issue.
		`CREATE TABLE IF NOT EXISTS agent_tokens (
			name TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL UNIQUE,
			bypass INTEGER NOT NULL DEFAULT 0,
			preview TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		// secret_gc lists keychain items that are no longer referenced and
		// still have to be deleted (a Delete failed, or has not run yet).
		`CREATE TABLE IF NOT EXISTS secret_gc (
			account TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL
		)`,
		// schema_meta holds one-off migration flags (§5.5), such as
		// legacy_migrated for the pre-SQLite JSON import.
		`CREATE TABLE IF NOT EXISTS schema_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	// The tables are created inside the migration transaction, so a first
	// start that dies before the commit leaves no schema behind and the next
	// start is still fresh (audit #34).
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	// A new database imports the legacy JSON files (pending); one from before
	// v2 imported them long ago and only retires them (rename); later
	// databases are done. An existing row is never overwritten.
	legacy := legacyMigrationDone
	switch {
	case s.fresh:
		legacy = legacyMigrationPending
	case s.prevVersion < 2:
		legacy = legacyMigrationRename
	}
	if _, err := tx.Exec(`INSERT INTO schema_meta (key, value) VALUES ('legacy_migrated', ?) ON CONFLICT(key) DO NOTHING`, legacy); err != nil {
		return err
	}

	columns := []struct{ table, name, ddl string }{
		{"connections", "type", `ALTER TABLE connections ADD COLUMN type TEXT NOT NULL DEFAULT 'SSH'`},
		{"connections", "serial_port", `ALTER TABLE connections ADD COLUMN serial_port TEXT NOT NULL DEFAULT ''`},
		{"connections", "baud_rate", `ALTER TABLE connections ADD COLUMN baud_rate INTEGER NOT NULL DEFAULT 0`},
		{"connections", "id", `ALTER TABLE connections ADD COLUMN id TEXT NOT NULL DEFAULT ''`},
		{"connections", "last_test_result", `ALTER TABLE connections ADD COLUMN last_test_result TEXT NOT NULL DEFAULT ''`},
		{"connections", "last_test_at", `ALTER TABLE connections ADD COLUMN last_test_at TEXT NOT NULL DEFAULT ''`},
		{"connections", "last_test_message", `ALTER TABLE connections ADD COLUMN last_test_message TEXT NOT NULL DEFAULT ''`},
		{"connections", "mcp_first_trust", `ALTER TABLE connections ADD COLUMN mcp_first_trust INTEGER NOT NULL DEFAULT 0`},
		{"command_requests", "connection_id", `ALTER TABLE command_requests ADD COLUMN connection_id TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "executed_command", `ALTER TABLE command_requests ADD COLUMN executed_command TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "approved_risk", `ALTER TABLE command_requests ADD COLUMN approved_risk TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "decided_by", `ALTER TABLE command_requests ADD COLUMN decided_by TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "decided_at", `ALTER TABLE command_requests ADD COLUMN decided_at TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "finished_at", `ALTER TABLE command_requests ADD COLUMN finished_at TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "exit_code", `ALTER TABLE command_requests ADD COLUMN exit_code INTEGER`},
		{"command_requests", "error_kind", `ALTER TABLE command_requests ADD COLUMN error_kind TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "client_request_id", `ALTER TABLE command_requests ADD COLUMN client_request_id TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "expires_at", `ALTER TABLE command_requests ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''`},
		{"command_requests", "target_fp", `ALTER TABLE command_requests ADD COLUMN target_fp TEXT NOT NULL DEFAULT ''`},
		{"connections", "data_bits", `ALTER TABLE connections ADD COLUMN data_bits INTEGER NOT NULL DEFAULT 8`},
		{"connections", "parity", `ALTER TABLE connections ADD COLUMN parity TEXT NOT NULL DEFAULT 'none'`},
		{"connections", "stop_bits", `ALTER TABLE connections ADD COLUMN stop_bits TEXT NOT NULL DEFAULT '1'`},
		{"agent_policies", "max_risk", `ALTER TABLE agent_policies ADD COLUMN max_risk TEXT NOT NULL DEFAULT 'Medium'`},
		{"agent_policies", "scope_conn_ids", `ALTER TABLE agent_policies ADD COLUMN scope_conn_ids TEXT NOT NULL DEFAULT '[]'`},
		{"agent_policies", "granted_at", `ALTER TABLE agent_policies ADD COLUMN granted_at INTEGER NOT NULL DEFAULT 0`},
		{"agent_policies", "granted_by", `ALTER TABLE agent_policies ADD COLUMN granted_by TEXT NOT NULL DEFAULT ''`},
		{"agent_policies", "expires_at", `ALTER TABLE agent_policies ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`},
		{"connections", "credential_store", `ALTER TABLE connections ADD COLUMN credential_store TEXT NOT NULL DEFAULT 'db'`},
		{"connections", "passphrase_saved", `ALTER TABLE connections ADD COLUMN passphrase_saved INTEGER NOT NULL DEFAULT 0`},
		// deleted_at (ms, 0 = live): deleting from the history hides the row
		// but keeps it as the audit record (§5.5, audit #4).
		{"command_requests", "deleted_at", `ALTER TABLE command_requests ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0`},
		// timeout_seconds is the agent's timeout_seconds for a queued run
		// (0 = the connection's command timeout), so an approval after a
		// restart still honours it (N11).
		{"command_requests", "timeout_seconds", `ALTER TABLE command_requests ADD COLUMN timeout_seconds INTEGER NOT NULL DEFAULT 0`},
	}
	for _, col := range columns {
		if err := addColumnIfMissing(tx, col.table, col.name, col.ddl); err != nil {
			return err
		}
	}

	if s.prevVersion < 2 {
		if err := migrateToV2(tx); err != nil {
			return err
		}
	}
	if s.prevVersion < 3 {
		if err := migrateToV3(tx, time.Now()); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_connections_id ON connections(id)`,
		`CREATE INDEX IF NOT EXISTS idx_command_requests_created ON command_requests(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_command_requests_status ON command_requests(status)`,
		`CREATE INDEX IF NOT EXISTS idx_command_requests_client ON command_requests(requested_by, client_request_id)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if err := migrateAISessionTables(tx); err != nil { // v5: AI sessions (REQ-016)
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// legacyStatusCodes maps the Japanese request statuses stored before v2 to
// the state codes used from v2 on.
//
// "承認待ち" is not in the map: before v2, ApproveCommand ran the command while
// the row still said 承認待ち, so such a row may have run (or be mid-run when
// the old version died). migrateToV2 never turns it back into an approvable
// pending request (audit #47).
var legacyStatusCodes = map[string]string{
	"実行済み":   stateSucceeded,
	"実行失敗":   stateFailed,
	"拒否":     stateRejected,
	"自動実行中":  stateInterrupted,
	"自動実行済み": stateAutoSucceeded,
	"自動実行失敗": stateAutoFailed,
}

func migrateToV2(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT name FROM connections WHERE id = ''`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := tx.Exec(`UPDATE connections SET id = ? WHERE name = ?`, newID(), name); err != nil {
			return err
		}
	}
	// A 承認待ち row with any trace of a run (output, a duration) is
	// interrupted: outcome unknown, never re-approvable. The rest expire, so
	// the agent has to ask again instead of a stale approval running twice.
	if _, err := tx.Exec(`UPDATE command_requests SET status = ?, error_kind = 'canceled'
		WHERE status = '承認待ち' AND (stdout != '' OR stderr != '' OR (duration != '' AND duration != '-'))`,
		stateInterrupted); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE command_requests SET status = ? WHERE status = '承認待ち'`, stateExpired); err != nil {
		return err
	}
	for legacy, code := range legacyStatusCodes {
		if _, err := tx.Exec(`UPDATE command_requests SET status = ? WHERE status = ?`, code, legacy); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE command_requests SET executed_command = command
		WHERE executed_command = '' AND status IN (?, ?, ?, ?)`,
		stateSucceeded, stateFailed, stateAutoSucceeded, stateAutoFailed); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE command_requests SET connection_id = COALESCE(
			(SELECT c.id FROM connections c WHERE c.name = command_requests.host), '')
		WHERE connection_id = ''`); err != nil {
		return err
	}
	// requested_at used to be "15:04:05" only; rebuild it from created_at.
	type stamp struct {
		id      string
		created int64
	}
	var stamps []stamp
	rows, err = tx.Query(`SELECT id, created_at FROM command_requests WHERE requested_at NOT LIKE '%T%'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var st stamp
		if err := rows.Scan(&st.id, &st.created); err != nil {
			rows.Close()
			return err
		}
		stamps = append(stamps, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, st := range stamps {
		ts := time.UnixMilli(st.created).Format(time.RFC3339)
		if _, err := tx.Exec(`UPDATE command_requests SET requested_at = ? WHERE id = ?`, ts, st.id); err != nil {
			return err
		}
	}
	return nil
}

// migrateToV3 gives bypass grants written before v3 (which never expired)
// the rule AgentPolicy documents for legacy rows: they started at updated_at
// and end defaultBypassTTL later. A grant older than that is therefore
// already expired after the upgrade; one with an unreadable updated_at
// starts at the migration.
func migrateToV3(tx *sql.Tx, now time.Time) error {
	if err := fillPendingTargetFP(tx); err != nil {
		return err
	}
	type grant struct {
		name    string
		updated string
	}
	var grants []grant
	rows, err := tx.Query(`SELECT agent_name, updated_at FROM agent_policies WHERE approval_bypass = 1 AND expires_at = 0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.name, &g.updated); err != nil {
			rows.Close()
			return err
		}
		grants = append(grants, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range grants {
		start := rfc3339Millis(g.updated)
		if start == 0 {
			start = now.UnixMilli()
		}
		if _, err := tx.Exec(`UPDATE agent_policies SET granted_at = ?, expires_at = ? WHERE agent_name = ?`,
			start, start+defaultBypassTTL.Milliseconds(), g.name); err != nil {
			return err
		}
	}
	return nil
}

// fillPendingTargetFP gives pending requests written before target_fp existed
// (v2) the user@host:port of their connection as it is at the upgrade, which
// is what the agent asked about: the older version could not change it
// afterwards without the app running. approve() then refuses them if the
// connection is edited later, like any newer request (N21). A row whose
// connection is gone or is serial keeps "", which approve() refuses.
func fillPendingTargetFP(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT r.id, c.type, c.host, c.port, c.user FROM command_requests r
		JOIN connections c ON c.id = r.connection_id
		WHERE r.status = ? AND r.target_fp = '' AND r.connection_id != ''`, statePending)
	if err != nil {
		return err
	}
	fps := map[string]string{}
	for rows.Next() {
		var id string
		var conn Connection
		if err := rows.Scan(&id, &conn.Type, &conn.Host, &conn.Port, &conn.User); err != nil {
			rows.Close()
			return err
		}
		if fp := targetFingerprint(conn); fp != "" {
			fps[id] = fp
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, fp := range fps {
		if _, err := tx.Exec(`UPDATE command_requests SET target_fp = ? WHERE id = ? AND target_fp = ''`, fp, id); err != nil {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(tx *sql.Tx, table string, column string, ddl string) error {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = tx.Exec(ddl)
	return err
}

// recoverInterruptedRequests marks requests that were mid-execution when the
// previous process exited. Their outcome is unknown and they must not be
// re-approved.
func (s *Store) recoverInterruptedRequests() error {
	_, err := s.db.Exec(`UPDATE command_requests SET status = ?, error_kind = 'canceled', updated_at = ?
		WHERE status IN (?, ?)`, stateInterrupted, nowRFC3339(), stateRunning, stateAutoRunning)
	return err
}

const connectionColumns = `id, name, type, host, port, user, auth_method, credential, serial_port, baud_rate,
	data_bits, parity, stop_bits, connect_timeout, command_timeout, tags_json, description, last_test_result, last_test_at, last_test_message, mcp_first_trust,
	credential_store`

// credentialInKeyring reports whether conn's password lives in the
// SecretStore. In memory this is encoded without a new Connection field: a
// password connection with an empty Credential and CredentialSet = true
// (normalizeConnection clears CredentialSet, so the UI cannot set it).
func credentialInKeyring(conn Connection) bool {
	return conn.connectionType() != "Serial" && conn.usesPassword() && conn.Credential == "" && conn.CredentialSet
}

func credentialStoreOf(conn Connection) string {
	if credentialInKeyring(conn) {
		return credentialStoreKeyring
	}
	return credentialStoreDB
}

// keyPathOf is the key path whose passphrase may be saved, or "" when conn
// does not use key auth.
func keyPathOf(conn Connection) string {
	if conn.connectionType() == "Serial" || conn.usesPassword() {
		return ""
	}
	return conn.Credential
}

func (s *Store) loadConnections() ([]Connection, error) {
	rows, err := s.db.Query(`SELECT ` + connectionColumns + ` FROM connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conns []Connection
	for rows.Next() {
		var conn Connection
		var tagsJSON string
		var firstTrust int
		var credStore string
		if err := rows.Scan(&conn.ID, &conn.Name, &conn.Type, &conn.Host, &conn.Port, &conn.User, &conn.AuthMethod,
			&conn.Credential, &conn.SerialPort, &conn.BaudRate, &conn.DataBits, &conn.Parity, &conn.StopBits, &conn.ConnectTimeout, &conn.CommandTimeout, &tagsJSON,
			&conn.Description, &conn.LastTestResult, &conn.LastTestAt, &conn.LastTestMessage, &firstTrust, &credStore); err != nil {
			return nil, err
		}
		conn.Type = conn.connectionType()
		conn.MCPFirstTrust = firstTrust != 0
		liftLegacySerialPort(&conn)
		if credStore == credentialStoreKeyring && conn.usesPassword() && conn.Type != "Serial" {
			conn.Credential = ""
			conn.CredentialSet = true
		}
		if err := json.Unmarshal([]byte(tagsJSON), &conn.Tags); err != nil {
			return nil, err
		}
		conns = append(conns, conn)
	}
	return conns, rows.Err()
}

// insertConnection adds a new connection. The caller must have assigned ID.
// A keychain-backed password (credentialInKeyring) is recorded as
// credential_store='keyring' with an empty credential column; the caller
// writes the SecretStore item first.
func (s *Store) insertConnection(conn Connection) error {
	tagsJSON, err := json.Marshal(conn.Tags)
	if err != nil {
		return err
	}
	credStore := credentialStoreOf(conn)
	if s.denyPlaintextPasswords.Load() && conn.usesPassword() && conn.connectionType() != "Serial" && conn.Credential != "" {
		return errPlaintextPassword
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertConnectionTx(tx, conn, string(tagsJSON), credStore); err != nil {
		return err
	}
	return tx.Commit()
}

func insertConnectionTx(tx *sql.Tx, conn Connection, tagsJSON string, credStore string) error {
	if _, err := tx.Exec(`
		INSERT INTO connections (
			id, name, type, host, port, user, auth_method, credential, serial_port, baud_rate,
			data_bits, parity, stop_bits, connect_timeout, command_timeout, tags_json, description, mcp_first_trust,
			credential_store, status, last_checked, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?)`,
		conn.ID, conn.Name, conn.connectionType(), conn.Host, conn.Port, conn.User, conn.AuthMethod, conn.Credential,
		conn.SerialPort, conn.BaudRate, conn.DataBits, conn.Parity, conn.StopBits, conn.ConnectTimeout, conn.CommandTimeout, tagsJSON, conn.Description,
		boolInt(conn.MCPFirstTrust), credStore, nowRFC3339(),
	); err != nil {
		return err
	}
	if credStore == credentialStoreKeyring {
		if _, err := tx.Exec(`DELETE FROM secret_gc WHERE account = ?`, passwordAccount(conn.ID)); err != nil {
			return err
		}
	}
	return nil
}

// storedSecretState is what a connections row says about its secrets.
type storedSecretState struct {
	authMethod      string
	connType        string
	credential      string
	credentialStore string
	passphraseSaved bool
}

func loadSecretState(tx *sql.Tx, id string) (storedSecretState, bool, error) {
	var st storedSecretState
	var saved int
	err := tx.QueryRow(`SELECT auth_method, type, credential, credential_store, passphrase_saved FROM connections WHERE id = ?`, id).
		Scan(&st.authMethod, &st.connType, &st.credential, &st.credentialStore, &saved)
	if errors.Is(err, sql.ErrNoRows) {
		return st, false, nil
	}
	st.passphraseSaved = saved != 0
	return st, err == nil, err
}

// obsoleteAccounts lists the SecretStore items of row id that are no longer
// referenced once the row becomes next (nil next: the row is deleted).
func (st storedSecretState) obsoleteAccounts(id string, next *Connection) []string {
	var out []string
	if st.credentialStore == credentialStoreKeyring && (next == nil || !credentialInKeyring(*next)) {
		out = append(out, passwordAccount(id))
	}
	if st.passphraseSaved {
		prevPath := ""
		if st.connType != "Serial" && !strings.Contains(st.authMethod, "パスワード") {
			prevPath = st.credential
		}
		if next == nil || prevPath == "" || keyPathOf(*next) != prevPath {
			out = append(out, passphraseAccount(id, prevPath))
		}
	}
	return out
}

func enqueueSecretGCTx(tx *sql.Tx, accounts []string) error {
	now := time.Now().UnixMilli()
	for _, account := range accounts {
		if _, err := tx.Exec(`INSERT INTO secret_gc (account, created_at) VALUES (?, ?) ON CONFLICT(account) DO NOTHING`, account, now); err != nil {
			return err
		}
	}
	return nil
}

// updateConnection updates the editable settings of an existing connection
// (including renames) without touching runtime/test columns. It reports
// whether a row was updated, so a concurrently deleted row is not resurrected.
//
// Secrets: credential_store follows credentialInKeyring(conn). Keychain items
// the new settings no longer use (password after switching to key/serial, the
// passphrase of a previous key path) are queued in secret_gc in the same
// transaction; the caller then runs runSecretGC outside a.mu. A saved
// passphrase stops being "saved" when the key path or auth method changes.
func (s *Store) updateConnection(conn Connection) (bool, error) {
	tagsJSON, err := json.Marshal(conn.Tags)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	prev, ok, err := loadSecretState(tx, conn.ID)
	if err != nil || !ok {
		return false, err
	}
	credStore := credentialStoreOf(conn)
	if s.denyPlaintextPasswords.Load() && conn.usesPassword() && conn.connectionType() != "Serial" && conn.Credential != "" &&
		!(prev.credentialStore == credentialStoreDB && prev.credential == conn.Credential) {
		// Only an unchanged legacy value (not yet migrated) may stay.
		return false, errPlaintextPassword
	}
	obsolete := prev.obsoleteAccounts(conn.ID, &conn)
	keepPassphrase := prev.passphraseSaved
	for _, account := range obsolete {
		if strings.Contains(account, "/passphrase/") {
			keepPassphrase = false
		}
	}
	res, err := tx.Exec(`
		UPDATE connections SET
			name = ?, type = ?, host = ?, port = ?, user = ?, auth_method = ?, credential = ?,
			serial_port = ?, baud_rate = ?, data_bits = ?, parity = ?, stop_bits = ?, connect_timeout = ?, command_timeout = ?, tags_json = ?,
			description = ?, mcp_first_trust = ?, credential_store = ?, passphrase_saved = ?, updated_at = ?
		WHERE id = ?`,
		conn.Name, conn.connectionType(), conn.Host, conn.Port, conn.User, conn.AuthMethod, conn.Credential,
		conn.SerialPort, conn.BaudRate, conn.DataBits, conn.Parity, conn.StopBits, conn.ConnectTimeout,
		conn.CommandTimeout, string(tagsJSON), conn.Description, boolInt(conn.MCPFirstTrust), credStore, boolInt(keepPassphrase),
		nowRFC3339(), conn.ID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if err := enqueueSecretGCTx(tx, obsolete); err != nil {
		return false, err
	}
	if credStore == credentialStoreKeyring {
		// The account is live again (e.g. key -> password -> key -> password
		// before the queue ran): it must not be collected.
		if _, err := tx.Exec(`DELETE FROM secret_gc WHERE account = ?`, passwordAccount(conn.ID)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *Store) updateConnectionTest(id string, result string, at string, message string) error {
	_, err := s.db.Exec(`UPDATE connections SET last_test_result = ?, last_test_at = ?, last_test_message = ? WHERE id = ?`,
		result, at, message, id)
	return err
}

// deleteConnection removes the row and, in the same transaction, queues its
// keychain items in secret_gc (run runSecretGC afterwards, outside a.mu), so
// a crash between the two never leaves an unreferenced item behind.
func (s *Store) deleteConnection(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	prev, ok, err := loadSecretState(tx, id)
	if err != nil {
		return err
	}
	if ok {
		if err := enqueueSecretGCTx(tx, prev.obsoleteAccounts(id, nil)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM connections WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) loadMCPSettings(path string) (MCPSettings, string, bool, error) {
	settings := defaultMCPSettings(path)
	var payload string
	var token string
	err := s.db.QueryRow(`SELECT payload_json, token FROM mcp_settings WHERE id = 1`).Scan(&payload, &token)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, "", false, nil
	}
	if err != nil {
		return settings, "", false, err
	}
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return settings, "", false, err
	}
	settings.ConfigPath = path
	return settings, token, true, nil
}

func (s *Store) saveMCPSettings(settings MCPSettings, token string) error {
	payload, err := mcpSettingsPayload(settings)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO mcp_settings (id, payload_json, token, updated_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			payload_json = excluded.payload_json,
			token = excluded.token,
			updated_at = excluded.updated_at`,
		payload, token, nowRFC3339(),
	)
	return err
}

func mcpSettingsPayload(settings MCPSettings) (string, error) {
	settings.TokenInput = ""
	settings.TokenPreview = ""
	payload, err := json.Marshal(settings)
	return string(payload), err
}

// Values of schema_meta.legacy_migrated.
const (
	legacyMigrationPending = "pending" // import the JSON files, then rename them
	legacyMigrationRename  = "rename"  // imported (or pre-v2): only rename them
	legacyMigrationDone    = "done"
)

func (s *Store) legacyMigrationState() (string, error) {
	var state string
	err := s.db.QueryRow(`SELECT value FROM schema_meta WHERE key = 'legacy_migrated'`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return legacyMigrationDone, nil
	}
	return state, err
}

func (s *Store) setLegacyMigrationState(state string) error {
	_, err := s.db.Exec(`INSERT INTO schema_meta (key, value) VALUES ('legacy_migrated', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, state)
	return err
}

// importLegacyData writes the pre-SQLite connections and MCP settings and
// moves legacy_migrated to "rename" in one transaction, so a failure (or a
// crash) imports nothing and the next start tries again (audit #10). Names
// that collide (with each other after trimming, or with rows made since a
// failed attempt) get a " (2)", " (3)" suffix instead of aborting. MCP
// settings are written only when none are stored yet. It returns the names
// actually used.
func (s *Store) importLegacyData(conns []Connection, settings *MCPSettings, token string) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	used := map[string]bool{}
	rows, err := tx.Query(`SELECT name FROM connections`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		used[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var names []string
	for _, conn := range conns {
		base := conn.Name
		if base == "" {
			base = conn.Host
		}
		if base == "" {
			base = "imported"
		}
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		used[name] = true
		conn.Name = name
		tagsJSON, err := json.Marshal(conn.Tags)
		if err != nil {
			return nil, err
		}
		if err := insertConnectionTx(tx, conn, string(tagsJSON), credentialStoreOf(conn)); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if settings != nil {
		payload, err := mcpSettingsPayload(*settings)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO mcp_settings (id, payload_json, token, updated_at) VALUES (1, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`, payload, token, nowRFC3339()); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE schema_meta SET value = ? WHERE key = 'legacy_migrated'`, legacyMigrationRename); err != nil {
		return nil, err
	}
	return names, tx.Commit()
}

const requestColumns = `id, connection_id, host, user, command, executed_command, reason, requested_by, requested_at,
	risk, approved_risk, status, stdout, stderr, duration, decided_by, decided_at, finished_at, exit_code, error_kind,
	client_request_id, expires_at, target_fp, timeout_seconds`

func scanRequest(scanner interface{ Scan(...any) error }) (CommandRequest, error) {
	var req CommandRequest
	var exitCode sql.NullInt64
	err := scanner.Scan(&req.ID, &req.ConnectionID, &req.Host, &req.User, &req.Command, &req.ExecutedCommand,
		&req.Reason, &req.RequestedBy, &req.RequestedAt, &req.Risk, &req.ApprovedRisk, &req.Status, &req.Stdout,
		&req.Stderr, &req.Duration, &req.DecidedBy, &req.DecidedAt, &req.FinishedAt, &exitCode, &req.ErrorKind,
		&req.ClientRequestID, &req.ExpiresAt, &req.TargetFP, &req.TimeoutSeconds)
	if exitCode.Valid {
		code := int(exitCode.Int64)
		req.ExitCode = &code
	}
	return req, err
}

// loadCommandRequests returns every open (pending/running) request plus the
// most recent `limit` closed ones, newest first.
func (s *Store) loadCommandRequests(limit int) ([]CommandRequest, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
		SELECT `+requestColumns+` FROM command_requests
		WHERE deleted_at = 0 AND (status IN (?, ?, ?)
		   OR id IN (SELECT id FROM command_requests WHERE deleted_at = 0 ORDER BY created_at DESC LIMIT ?))
		ORDER BY created_at DESC`, statePending, stateRunning, stateAutoRunning, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []CommandRequest
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, req)
	}
	return requests, rows.Err()
}

func (s *Store) insertCommandRequest(req CommandRequest) error {
	return execInsertCommandRequest(s.db, req)
}

// execInsertCommandRequest runs the INSERT on a DB or a transaction.
func execInsertCommandRequest(db interface {
	Exec(string, ...any) (sql.Result, error)
}, req CommandRequest) error {
	createdAt := time.Now().UnixMilli()
	if parsed, err := parseRequestIDMillis(req.ID); err == nil {
		createdAt = parsed
	}
	_, err := db.Exec(`
		INSERT INTO command_requests (
			id, connection_id, host, user, command, executed_command, reason, requested_by, requested_at,
			risk, approved_risk, status, stdout, stderr, duration, decided_by, decided_at, finished_at,
			exit_code, error_kind, client_request_id, expires_at, target_fp, timeout_seconds, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.ConnectionID, req.Host, req.User, req.Command, req.ExecutedCommand, req.Reason, req.RequestedBy,
		req.RequestedAt, req.Risk, req.ApprovedRisk, req.Status, req.Stdout, req.Stderr, req.Duration, req.DecidedBy,
		req.DecidedAt, req.FinishedAt, nullableInt(req.ExitCode), req.ErrorKind, req.ClientRequestID, req.ExpiresAt, req.TargetFP,
		req.TimeoutSeconds, createdAt, nowRFC3339(),
	)
	return err
}

// updateCommandRequestFrom writes req only if the stored status is still
// `from` (compare-and-swap). It returns errStateConflict when it is not.
func (s *Store) updateCommandRequestFrom(req CommandRequest, from string) error {
	res, err := s.db.Exec(`
		UPDATE command_requests SET
			connection_id = ?, host = ?, executed_command = ?, approved_risk = ?, status = ?, stdout = ?,
			stderr = ?, duration = ?, decided_by = ?, decided_at = ?, finished_at = ?, exit_code = ?,
			error_kind = ?, expires_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		req.ConnectionID, req.Host, req.ExecutedCommand, req.ApprovedRisk, req.Status, req.Stdout, req.Stderr,
		req.Duration, req.DecidedBy, req.DecidedAt, req.FinishedAt, nullableInt(req.ExitCode), req.ErrorKind,
		req.ExpiresAt, nowRFC3339(), req.ID, from,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errStateConflict
	}
	return nil
}

func (s *Store) updatePendingRequestsConnection(oldName string, connID string, newName string) error {
	_, err := s.db.Exec(`UPDATE command_requests SET host = ? WHERE connection_id = ? AND status = ? AND host = ?`,
		newName, connID, statePending, oldName)
	return err
}

// deleteCommandRequestUnlessRunning removes the request from the history
// unless it is running. The row is not deleted: deleted_at is set, so it
// drops out of the GUI list, get_request_status and get_command_history but
// stays in SQLite as the audit record (audit #4), and a retry with the same
// client_request_id still finds it instead of running the command again.
// errStateConflict means the row is running (or already gone or deleted),
// so nothing changed.
func (s *Store) deleteCommandRequestUnlessRunning(id string) error {
	res, err := s.db.Exec(`UPDATE command_requests SET deleted_at = ?, updated_at = ?
		WHERE id = ? AND deleted_at = 0 AND status NOT IN (?, ?)`,
		time.Now().UnixMilli(), nowRFC3339(), id, stateRunning, stateAutoRunning)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errStateConflict
	}
	return nil
}

// policyActiveAt reports whether p lets the agent skip approval at now.
// Expiry is read from ExpiresAt; an unparsable or empty value is inactive.
func policyActiveAt(p AgentPolicy, now time.Time) bool {
	return p.ApprovalBypass && rfc3339Millis(p.ExpiresAt) > now.UnixMilli()
}

// policyAllows reports whether an active grant covers risk on connID. An
// empty scope means every connection. High is covered only by a forced grant
// (max_risk High, raised by the human in the GUI).
func policyAllows(p AgentPolicy, risk string, connID string, now time.Time) bool {
	if !policyActiveAt(p, now) {
		return false
	}
	rank, ok := riskRank[risk]
	if !ok || rank > riskRank[normalizeBypassMaxRisk(p.MaxRisk)] {
		return false
	}
	if len(p.ScopeConnIDs) == 0 {
		return true
	}
	for _, id := range p.ScopeConnIDs {
		if id == connID {
			return true
		}
	}
	return false
}

// normalizeBypassMaxRisk clamps max_risk to Low, Medium or High (forced).
func normalizeBypassMaxRisk(risk string) string {
	if risk == "Low" || risk == forcedBypassMaxRisk {
		return risk
	}
	return defaultBypassMaxRisk
}

// fillPolicyDefaults applies the §5.5 defaults the way AgentPolicy documents
// them: an enabled grant without GrantedAt starts at UpdatedAt (or now), and
// one without ExpiresAt ends defaultBypassTTL after it starts.
func fillPolicyDefaults(p AgentPolicy, now time.Time) AgentPolicy {
	p.AgentName = strings.TrimSpace(p.AgentName)
	p.MaxRisk = normalizeBypassMaxRisk(p.MaxRisk)
	if p.ScopeConnIDs == nil {
		p.ScopeConnIDs = []string{}
	}
	if strings.TrimSpace(p.UpdatedAt) == "" {
		p.UpdatedAt = now.Format(time.RFC3339)
	}
	if p.ApprovalBypass {
		if rfc3339Millis(p.GrantedAt) == 0 {
			p.GrantedAt = p.UpdatedAt
		}
		if rfc3339Millis(p.ExpiresAt) == 0 {
			start := rfc3339Millis(p.GrantedAt)
			if start == 0 {
				start = now.UnixMilli()
			}
			p.ExpiresAt = millisRFC3339(start + defaultBypassTTL.Milliseconds())
		}
	}
	return p
}

const agentPolicyColumns = `p.agent_name, p.approval_bypass, p.max_risk, p.scope_conn_ids, p.granted_at, p.granted_by,
	p.expires_at, p.updated_at, EXISTS (SELECT 1 FROM agent_tokens t WHERE t.name = p.agent_name)`

func scanAgentPolicy(scanner interface{ Scan(...any) error }, now time.Time) (AgentPolicy, error) {
	var p AgentPolicy
	var bypass, hasToken int
	var scopeJSON string
	var grantedAt, expiresAt int64
	if err := scanner.Scan(&p.AgentName, &bypass, &p.MaxRisk, &scopeJSON, &grantedAt, &p.GrantedBy,
		&expiresAt, &p.UpdatedAt, &hasToken); err != nil {
		return p, err
	}
	p.ApprovalBypass = bypass != 0
	p.MaxRisk = normalizeBypassMaxRisk(p.MaxRisk)
	p.ScopeConnIDs = []string{}
	if scopeJSON != "" {
		if err := json.Unmarshal([]byte(scopeJSON), &p.ScopeConnIDs); err != nil {
			return p, err
		}
	}
	p.GrantedAt = millisRFC3339(grantedAt)
	p.ExpiresAt = millisRFC3339(expiresAt)
	p.Active = policyActiveAt(p, now)
	p.HasToken = hasToken != 0
	return p, nil
}

func (s *Store) queryAgentPolicies(now time.Time, where string, args ...any) ([]AgentPolicy, error) {
	rows, err := s.db.Query(`SELECT `+agentPolicyColumns+` FROM agent_policies p `+where+` ORDER BY p.agent_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := []AgentPolicy{}
	for rows.Next() {
		p, err := scanAgentPolicy(rows, now)
		if err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

// loadAgentPolicies returns every agent_policies row with Active and
// HasToken derived. Expired grants keep ApprovalBypass as stored but load
// with Active=false.
func (s *Store) loadAgentPolicies() ([]AgentPolicy, error) {
	return s.queryAgentPolicies(time.Now(), ``)
}

// loadActiveAgentPolicies returns the grants that let an agent skip approval
// at now (the "承認省略の一覧").
func (s *Store) loadActiveAgentPolicies(now time.Time) ([]AgentPolicy, error) {
	return s.queryAgentPolicies(now, `WHERE p.approval_bypass = 1 AND p.expires_at > ?`, now.UnixMilli())
}

// loadAgentPolicy returns one agent's row; ok is false when none exists.
func (s *Store) loadAgentPolicy(agentName string) (AgentPolicy, bool, error) {
	policies, err := s.queryAgentPolicies(time.Now(), `WHERE p.agent_name = ?`, agentName)
	if err != nil || len(policies) == 0 {
		return AgentPolicy{}, false, err
	}
	return policies[0], true, nil
}

// saveAgentPolicy upserts every persisted field of policy after applying
// fillPolicyDefaults. Turning bypass off clears expires_at.
func (s *Store) saveAgentPolicy(policy AgentPolicy) error {
	policy = fillPolicyDefaults(policy, time.Now())
	if policy.AgentName == "" {
		return errors.New("エージェント名が不足しています")
	}
	scopeJSON, err := json.Marshal(policy.ScopeConnIDs)
	if err != nil {
		return err
	}
	expiresAt := rfc3339Millis(policy.ExpiresAt)
	if !policy.ApprovalBypass {
		expiresAt = 0
	}
	_, err = s.db.Exec(`
		INSERT INTO agent_policies (agent_name, approval_bypass, max_risk, scope_conn_ids, granted_at, granted_by,
			expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_name) DO UPDATE SET
			approval_bypass = excluded.approval_bypass,
			max_risk = excluded.max_risk,
			scope_conn_ids = excluded.scope_conn_ids,
			granted_at = excluded.granted_at,
			granted_by = excluded.granted_by,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at`,
		policy.AgentName, boolInt(policy.ApprovalBypass), policy.MaxRisk, string(scopeJSON),
		rfc3339Millis(policy.GrantedAt), policy.GrantedBy, expiresAt, policy.UpdatedAt,
	)
	return err
}

// revokeAgentPolicy turns bypass off for agentName. It reports whether an
// active grant was actually revoked (false for unknown or already expired
// agents, whose row is still switched off).
func (s *Store) revokeAgentPolicy(agentName string, now time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var active int
	err = tx.QueryRow(`SELECT COUNT(*) FROM agent_policies WHERE agent_name = ? AND approval_bypass = 1 AND expires_at > ?`,
		agentName, now.UnixMilli()).Scan(&active)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE agent_policies SET approval_bypass = 0, expires_at = 0, updated_at = ?
		WHERE agent_name = ? AND approval_bypass = 1`, now.Format(time.RFC3339), agentName); err != nil {
		return false, err
	}
	return active == 1, tx.Commit()
}

// expireAgentPolicies switches off every grant whose expires_at has passed
// and returns the affected agent names, so the caller can update memory and
// notify the UI.
func (s *Store) expireAgentPolicies(now time.Time) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT agent_name FROM agent_policies WHERE approval_bypass = 1 AND expires_at <= ? ORDER BY agent_name`,
		now.UnixMilli())
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(`UPDATE agent_policies SET approval_bypass = 0, updated_at = ?
		WHERE approval_bypass = 1 AND expires_at <= ?`, now.Format(time.RFC3339), now.UnixMilli()); err != nil {
		return nil, err
	}
	return names, tx.Commit()
}

func hashAgentToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// agentTokenPreview is the non-secret hint shown in the UI.
func agentTokenPreview(token string) string {
	if len(token) <= len(agentTokenPrefix)+4 {
		return agentTokenPrefix + "…"
	}
	return agentTokenPrefix + "…" + token[len(token)-4:]
}

// newAgentToken generates a token for agentName. The plaintext is returned
// once; only the AgentToken (hash and preview) is meant to be stored.
func newAgentToken(agentName string, bypass bool) (string, AgentToken, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return "", AgentToken{}, errors.New("エージェント名が不足しています")
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", AgentToken{}, err
	}
	token := agentTokenPrefix + hex.EncodeToString(b[:])
	return token, AgentToken{
		AgentName: agentName,
		TokenHash: hashAgentToken(token),
		Bypass:    bypass,
		CreatedAt: nowRFC3339(),
		Preview:   agentTokenPreview(token),
	}, nil
}

// saveAgentToken creates or rotates the token row for t.AgentName.
// Rotating replaces the hash, so the old token stops working.
func (s *Store) saveAgentToken(t AgentToken) error {
	t.AgentName = strings.TrimSpace(t.AgentName)
	if t.AgentName == "" || t.TokenHash == "" {
		return errors.New("エージェント名またはトークンが不足しています")
	}
	if rfc3339Millis(t.CreatedAt) == 0 {
		t.CreatedAt = nowRFC3339()
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_tokens (name, token_hash, bypass, preview, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			token_hash = excluded.token_hash,
			bypass = excluded.bypass,
			preview = excluded.preview,
			created_at = excluded.created_at`,
		t.AgentName, t.TokenHash, boolInt(t.Bypass), t.Preview, rfc3339Millis(t.CreatedAt),
	)
	return err
}

// loadAgentTokens returns every token row including its hash (for the
// in-memory lookup; the hash is not serialized to the UI).
func (s *Store) loadAgentTokens() ([]AgentToken, error) {
	rows, err := s.db.Query(`SELECT name, token_hash, bypass, preview, created_at FROM agent_tokens ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []AgentToken{}
	for rows.Next() {
		var t AgentToken
		var bypass int
		var created int64
		if err := rows.Scan(&t.AgentName, &t.TokenHash, &bypass, &t.Preview, &created); err != nil {
			return nil, err
		}
		t.Bypass = bypass != 0
		t.CreatedAt = millisRFC3339(created)
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// loadAgentTokenByHash resolves a presented token's hash to its row.
func (s *Store) loadAgentTokenByHash(hash string) (AgentToken, bool, error) {
	var t AgentToken
	var bypass int
	var created int64
	err := s.db.QueryRow(`SELECT name, token_hash, bypass, preview, created_at FROM agent_tokens WHERE token_hash = ?`, hash).
		Scan(&t.AgentName, &t.TokenHash, &bypass, &t.Preview, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentToken{}, false, nil
	}
	if err != nil {
		return AgentToken{}, false, err
	}
	t.Bypass = bypass != 0
	t.CreatedAt = millisRFC3339(created)
	return t, true, nil
}

// setAgentTokenBypass changes whether the token may use the agent's grant.
// It reports whether the token exists.
func (s *Store) setAgentTokenBypass(agentName string, bypass bool) (bool, error) {
	res, err := s.db.Exec(`UPDATE agent_tokens SET bypass = ? WHERE name = ?`, boolInt(bypass), agentName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// deleteAgentToken revokes the token. It reports whether one existed.
func (s *Store) deleteAgentToken(agentName string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM agent_tokens WHERE name = ?`, agentName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// clientRequestIDWindow is how long (agent_name, client_request_id) stays
// deduplicated (§5.3).
const clientRequestIDWindow = 24 * time.Hour

const requestByClientIDQuery = `SELECT ` + requestColumns + ` FROM command_requests
	WHERE requested_by = ? AND client_request_id = ? AND created_at >= ?
	ORDER BY created_at DESC LIMIT 1`

// loadRequestByClientID returns the newest request from agentName carrying
// clientRequestID created within 24 hours before now.
func (s *Store) loadRequestByClientID(agentName string, clientRequestID string, now time.Time) (CommandRequest, bool, error) {
	if clientRequestID == "" {
		return CommandRequest{}, false, nil
	}
	req, err := scanRequest(s.db.QueryRow(requestByClientIDQuery,
		agentName, clientRequestID, now.Add(-clientRequestIDWindow).UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return CommandRequest{}, false, nil
	}
	if err != nil {
		return CommandRequest{}, false, err
	}
	return req, true, nil
}

// loadCommandRequest returns one request by ID, including its output. A
// request deleted from the history is not found.
func (s *Store) loadCommandRequest(id string) (CommandRequest, bool, error) {
	req, err := scanRequest(s.db.QueryRow(`SELECT `+requestColumns+` FROM command_requests WHERE id = ? AND deleted_at = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CommandRequest{}, false, nil
	}
	if err != nil {
		return CommandRequest{}, false, err
	}
	return req, true, nil
}

// historyFilter is the get_command_history query. Empty fields do not
// filter. ConnectionID and HostName match either column, so renamed or
// deleted connections still find their history.
type historyFilter struct {
	ConnectionID string
	HostName     string
	State        string
	RequestedBy  string
	Limit        int
	// SessionViewer hides the lines of AI sessions (ai_session_lines) that
	// another agent sent: only their owner may see them (REQ-016 isolation).
	// HideSessionLines turns that on; SessionViewer "" then hides them all.
	HideSessionLines bool
	SessionViewer    string
}

// historyColumns is requestColumns with stdout/stderr blanked: the history
// is a summary and must not drag megabytes of output through the query.
var historyColumns = strings.NewReplacer("stdout,", "'' AS stdout,", "stderr,", "'' AS stderr,").Replace(requestColumns)

// loadCommandHistory returns matching requests newest first, without output.
// Limit follows get_command_history: default defaultHistoryLimit, capped at
// maxHistoryLimit (both in mcp_server.go).
func (s *Store) loadCommandHistory(f historyFilter) ([]CommandRequest, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	limit = min(limit, maxHistoryLimit)
	where := []string{`deleted_at = 0`}
	var args []any
	switch {
	case f.ConnectionID != "" && f.HostName != "":
		where = append(where, `(connection_id = ? OR host = ?)`)
		args = append(args, f.ConnectionID, f.HostName)
	case f.ConnectionID != "":
		where = append(where, `connection_id = ?`)
		args = append(args, f.ConnectionID)
	case f.HostName != "":
		where = append(where, `host = ?`)
		args = append(args, f.HostName)
	}
	if f.State != "" {
		where = append(where, `status = ?`)
		args = append(args, f.State)
	}
	if f.RequestedBy != "" {
		where = append(where, `requested_by = ?`)
		args = append(args, f.RequestedBy)
	}
	if f.HideSessionLines {
		where = append(where, `(requested_by = ? OR id NOT IN (SELECT request_id FROM ai_session_lines))`)
		args = append(args, f.SessionViewer)
	}
	query := `SELECT ` + historyColumns + ` FROM command_requests WHERE ` + strings.Join(where, " AND ")
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := []CommandRequest{}
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, req)
	}
	return requests, rows.Err()
}

func millisRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}

// rfc3339Millis parses an RFC3339 timestamp; empty or invalid gives 0.
func rfc3339Millis(ts string) int64 {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts))
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// parseRequestIDMillis extracts the millisecond timestamp from a request ID.
// IDs are "REQ-<millis>-<seq>-<random hex>"; older ones "REQ-<millis>-<seq>"
// and legacy ones "REQ-<millis>" (no seq).
func parseRequestIDMillis(id string) (int64, error) {
	rest := strings.TrimPrefix(id, "REQ-")
	if idx := strings.IndexByte(rest, '-'); idx >= 0 {
		rest = rest[:idx]
	}
	return strconv.ParseInt(rest, 10, 64)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nowRFC3339() string {
	return time.Now().Format(time.RFC3339)
}

// --- OS keychain (phase5-keychain.md) -------------------------------------

// setPassphraseSaved records whether the passphrase of key keyPath is kept in
// the SecretStore (opt-in, default off). It only applies while the row still
// uses that key path, and reports whether it did. Turning it off queues the
// item in secret_gc; turning it on takes it out of the queue. The caller
// writes the item (storeSecretVerified) before turning it on.
func (s *Store) setPassphraseSaved(id string, keyPath string, saved bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	st, ok, err := loadSecretState(tx, id)
	if err != nil || !ok {
		return false, err
	}
	if st.connType == "Serial" || strings.Contains(st.authMethod, "パスワード") || st.credential != keyPath || keyPath == "" {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE connections SET passphrase_saved = ? WHERE id = ?`, boolInt(saved), id); err != nil {
		return false, err
	}
	account := passphraseAccount(id, keyPath)
	if saved {
		_, err = tx.Exec(`DELETE FROM secret_gc WHERE account = ?`, account)
	} else {
		err = enqueueSecretGCTx(tx, []string{account})
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// passphraseSavedIDs returns the IDs of connections whose key passphrase is
// saved in the SecretStore.
func (s *Store) passphraseSavedIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM connections WHERE passphrase_saved = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// enqueueSecretGC queues accounts for deletion from the SecretStore, e.g.
// the item written for a new connection whose INSERT then failed.
func (s *Store) enqueueSecretGC(accounts ...string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := enqueueSecretGCTx(tx, accounts); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) loadSecretGC() ([]string, error) {
	rows, err := s.db.Query(`SELECT account FROM secret_gc ORDER BY created_at, account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return nil, err
		}
		out = append(out, account)
	}
	return out, rows.Err()
}

// secretAccountReferenced reports whether a connections row currently uses
// account, so a queued item that became live again is never deleted.
func (s *Store) secretAccountReferenced(account string) (bool, error) {
	rest, ok := strings.CutPrefix(account, "conn/")
	if !ok {
		return false, nil
	}
	id, _, ok := strings.Cut(rest, "/")
	if !ok || id == "" {
		return false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	st, found, err := loadSecretState(tx, id)
	if err != nil || !found {
		return false, err
	}
	if st.credentialStore == credentialStoreKeyring && account == passwordAccount(id) {
		return true, nil
	}
	if st.passphraseSaved && st.connType != "Serial" && !strings.Contains(st.authMethod, "パスワード") &&
		account == passphraseAccount(id, st.credential) {
		return true, nil
	}
	return false, nil
}

// runSecretGC deletes the queued SecretStore items. Call it without holding
// a.mu (at startup, after DeleteConnection / SaveConnection, and hourly).
// Items still referenced by a row are dropped from the queue without being
// deleted. A failed Delete stays queued; the first error is returned.
func (s *Store) runSecretGC(ctx context.Context, secrets SecretStore) (int, error) {
	if secrets == nil {
		return 0, ErrSecretUnavailable
	}
	accounts, err := s.loadSecretGC()
	if err != nil {
		return 0, err
	}
	deleted := 0
	var firstErr error
	for _, account := range accounts {
		live, err := s.secretAccountReferenced(account)
		if err != nil {
			return deleted, err
		}
		if !live {
			if err := secrets.Delete(ctx, account); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				if errors.Is(err, ErrSecretUnavailable) {
					break // the store is down; the rest would fail the same way
				}
				continue
			}
			deleted++
		}
		if _, err := s.db.Exec(`DELETE FROM secret_gc WHERE account = ?`, account); err != nil {
			return deleted, err
		}
	}
	return deleted, firstErr
}

// CredentialMigration is the outcome of migrateCredentialsToKeyring.
type CredentialMigration struct {
	Moved int
	// Remaining counts password rows still stored in the credential column
	// (failed now, or not attempted because the store was unavailable).
	Remaining int
	// Err is the first SecretStore error (nil when Remaining is 0).
	Err error
}

// migrateCredentialsToKeyring moves plaintext passwords from the credential
// column into secrets (phase5-keychain.md §7). Run it at startup after
// openStore / migrateLegacyData and before loadConnections and the MCP
// server, without holding a.mu. For each row: Set, read back and compare,
// then clear the column only if it still holds the same value. A row is
// cleared only after its item is verified, so a crash or failure at any point
// leaves the plaintext in place and the next start retries. perItem bounds
// each keychain round trip; the first ErrSecretUnavailable stops the run.
// Passwords left on Serial rows are cleared, not moved. When anything moved
// or was cleared, and on the first start after the upgrade to v4, freed pages
// and the WAL are scrubbed (secure_delete, checkpoint, VACUUM). The returned
// error is for database failures only.
func (s *Store) migrateCredentialsToKeyring(ctx context.Context, secrets SecretStore, perItem time.Duration) (CredentialMigration, error) {
	var result CredentialMigration
	// Serial rows never use the credential column (saveConnection clears it),
	// but a v0.1.0 row switched from password SSH to Serial kept the old
	// password there. Drop it instead of moving it.
	res, err := s.db.Exec(`UPDATE connections SET credential = '' WHERE type = 'Serial' AND credential != ''`)
	if err != nil {
		return result, err
	}
	serialCleared, _ := res.RowsAffected()
	type row struct{ id, credential string }
	rows, err := s.db.Query(`SELECT id, credential FROM connections
		WHERE credential_store = 'db' AND credential != '' AND type != 'Serial' AND auth_method LIKE '%パスワード%'
		ORDER BY name`)
	if err != nil {
		return result, err
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.credential); err != nil {
			rows.Close()
			return result, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	if perItem <= 0 {
		perItem = 10 * time.Second
	}
	for i, r := range pending {
		if secrets == nil {
			result.Err = ErrSecretUnavailable
			result.Remaining = len(pending) - i
			break
		}
		itemCtx, cancel := context.WithTimeout(ctx, perItem)
		err := storeSecretVerified(itemCtx, secrets, passwordAccount(r.id), r.credential)
		cancel()
		if err != nil {
			if result.Err == nil {
				result.Err = err
			}
			if errors.Is(err, ErrSecretUnavailable) {
				result.Remaining += len(pending) - i
				break
			}
			result.Remaining++
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return result, err
		}
		res, err := tx.Exec(`UPDATE connections SET credential = '', credential_store = 'keyring'
			WHERE id = ? AND credential = ? AND credential_store = 'db'`, r.id, r.credential)
		if err != nil {
			tx.Rollback()
			return result, err
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			if _, err := tx.Exec(`DELETE FROM secret_gc WHERE account = ?`, passwordAccount(r.id)); err != nil {
				tx.Rollback()
				return result, err
			}
		}
		if err := tx.Commit(); err != nil {
			return result, err
		}
		if n == 1 {
			result.Moved++
		}
		// n == 0: the row changed meanwhile (edited or deleted). Leave it;
		// a still-plaintext row is picked up next time.
	}
	// The first start after the upgrade also scrubs when nothing moved: before
	// v4 secure_delete was off, so values deleted or overwritten then (a
	// removed password connection, one switched to key auth) can still sit in
	// free pages.
	if result.Moved > 0 || serialCleared > 0 || (s.prevVersion < 4 && !s.fresh) {
		if err := s.scrubFreedPages(); err != nil {
			return result, err
		}
	}
	return result, nil
}

// scrubFreedPages removes old copies of overwritten values: secure_delete
// (set on every connection in openStore) zero-fills cells freed from now on,
// the checkpoint folds and truncates the WAL, and VACUUM rewrites the file
// without free pages.
func (s *Store) scrubFreedPages() error {
	for _, stmt := range []string{
		`PRAGMA secure_delete = ON`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`VACUUM`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	restrictDBFilePermissions(s.path)
	return nil
}

// PlaintextBackup is a file next to the database that still holds
// plaintext passwords: a pre-migration backup (<db>.bak-v<N>[-<ns>]) or the
// renamed pre-SQLite connections.json.migrated.
type PlaintextBackup struct {
	Path      string `json:"path"`
	Passwords int    `json:"passwords"`
	// Unreadable: the file could not be inspected (partial copy, other
	// format); it may contain passwords.
	Unreadable bool `json:"unreadable"`
}

// findPlaintextBackups lists the backups of dbPath, and the given legacy JSON
// files, that still contain passwords. They are never deleted automatically
// (they are the rollback path); the UI offers removePlaintextBackups.
func findPlaintextBackups(dbPath string, legacyJSON ...string) ([]PlaintextBackup, error) {
	matches, err := filepath.Glob(globEscape(dbPath) + ".bak-v*")
	if err != nil {
		return nil, err
	}
	var out []PlaintextBackup
	for _, path := range matches {
		if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") || strings.HasSuffix(path, "-journal") {
			continue
		}
		if strings.Contains(filepath.Base(path), ".tmp-") {
			// Backups are written only inside openStore, so a .tmp- file seen
			// while the app runs is a VACUUM INTO cut off by a crash or power
			// loss. It may hold the plaintext passwords: report it so the
			// user can delete it.
			out = append(out, PlaintextBackup{Path: path, Unreadable: true})
			continue
		}
		n, err := countBackupPasswords(path)
		switch {
		case err != nil:
			out = append(out, PlaintextBackup{Path: path, Unreadable: true})
		case n > 0:
			out = append(out, PlaintextBackup{Path: path, Passwords: n})
		}
	}
	for _, path := range legacyJSON {
		if !fileExists(path) {
			continue
		}
		conns, err := loadConnections(path)
		if err != nil {
			out = append(out, PlaintextBackup{Path: path, Unreadable: true})
			continue
		}
		n := 0
		for _, c := range conns {
			if c.usesPassword() && c.Credential != "" {
				n++
			}
		}
		if n > 0 {
			out = append(out, PlaintextBackup{Path: path, Passwords: n})
		}
	}
	return out, nil
}

func globEscape(path string) string {
	r := strings.NewReplacer(`*`, `\*`, `?`, `\?`, `[`, `\[`)
	if filepath.Separator == '\\' {
		return path // '\' is the separator on Windows and cannot escape
	}
	return r.Replace(path)
}

func countBackupPasswords(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	header := make([]byte, 16)
	_, err = io.ReadFull(f, header)
	f.Close()
	if err != nil || string(header) != "SQLite format 3\x00" {
		return 0, fmt.Errorf("not a SQLite database: %s", path)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM connections WHERE credential != '' AND auth_method LIKE '%パスワード%'`).Scan(&n)
	return n, err
}

// removePlaintextBackups deletes the given files, but only those that
// findPlaintextBackups reports right now for the same dbPath / legacy
// files, so the UI cannot be used to delete arbitrary paths. It returns the
// paths it removed. The removal is a plain unlink: copies in Time Machine,
// cloud sync or SSD spare blocks are out of reach.
func removePlaintextBackups(dbPath string, paths []string, legacyJSON ...string) ([]string, error) {
	found, err := findPlaintextBackups(dbPath, legacyJSON...)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, b := range found {
		allowed[b.Path] = true
	}
	var removed []string
	var firstErr error
	for _, path := range paths {
		if !allowed[path] {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed = append(removed, path)
	}
	return removed, firstErr
}
