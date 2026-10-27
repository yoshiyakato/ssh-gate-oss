package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is stored in PRAGMA user_version. v2 introduced immutable
// connection IDs, request state codes and approval/audit columns.
const schemaVersion = 2

var errStoreUnavailable = errors.New("SQLiteストアが初期化されていないため保存できません")

// errStateConflict is returned when a request transition loses a race (the
// request is no longer in the expected state).
var errStateConflict = errors.New("request state changed")

type Store struct {
	db   *sql.DB
	path string
	// fresh reports whether the database file did not exist before openStore.
	fresh bool
	// prevVersion is the user_version found on open (before migration).
	prevVersion int
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func openStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fresh := !fileExists(path)
	if fresh {
		// Create the file with owner-only permissions before SQLite opens it.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	// A single connection serializes writes so read-check-write sequences
	// (state transitions) cannot interleave inside SQLite.
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path, fresh: fresh}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	restrictDBFilePermissions(path)
	return store, nil
}

func restrictDBFilePermissions(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if fileExists(p) {
			_ = os.Chmod(p, 0o600)
		}
	}
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&s.prevVersion); err != nil {
		return err
	}
	if s.prevVersion > schemaVersion {
		return fmt.Errorf("このデータベースは新しいバージョン(v%d)の ssh-gete で作成されています。アプリを更新してください", s.prevVersion)
	}
	statements := []string{
		`PRAGMA journal_mode=WAL`,
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
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	if s.prevVersion < schemaVersion && !s.fresh {
		// Keep a copy of the pre-migration database for rollback.
		backup := fmt.Sprintf("%s.bak-v%d", s.path, s.prevVersion)
		if !fileExists(backup) {
			if _, err := s.db.Exec(`VACUUM INTO ?`, backup); err != nil {
				return fmt.Errorf("backup before migration: %w", err)
			}
			_ = os.Chmod(backup, 0o600)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
	for _, statement := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_connections_id ON connections(id)`,
		`CREATE INDEX IF NOT EXISTS idx_command_requests_created ON command_requests(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_command_requests_status ON command_requests(status)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// legacyStatusCodes maps the Japanese request statuses stored before v2 to
// the state codes used from v2 on.
var legacyStatusCodes = map[string]string{
	"承認待ち":   statePending,
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
	connect_timeout, command_timeout, tags_json, description, last_test_result, last_test_at, last_test_message, mcp_first_trust`

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
		if err := rows.Scan(&conn.ID, &conn.Name, &conn.Type, &conn.Host, &conn.Port, &conn.User, &conn.AuthMethod,
			&conn.Credential, &conn.SerialPort, &conn.BaudRate, &conn.ConnectTimeout, &conn.CommandTimeout, &tagsJSON,
			&conn.Description, &conn.LastTestResult, &conn.LastTestAt, &conn.LastTestMessage, &firstTrust); err != nil {
			return nil, err
		}
		conn.Type = conn.connectionType()
		conn.MCPFirstTrust = firstTrust != 0
		if err := json.Unmarshal([]byte(tagsJSON), &conn.Tags); err != nil {
			return nil, err
		}
		conns = append(conns, conn)
	}
	return conns, rows.Err()
}

// insertConnection adds a new connection. The caller must have assigned ID.
func (s *Store) insertConnection(conn Connection) error {
	tagsJSON, err := json.Marshal(conn.Tags)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO connections (
			id, name, type, host, port, user, auth_method, credential, serial_port, baud_rate,
			connect_timeout, command_timeout, tags_json, description, mcp_first_trust,
			status, last_checked, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?)`,
		conn.ID, conn.Name, conn.connectionType(), conn.Host, conn.Port, conn.User, conn.AuthMethod, conn.Credential,
		conn.SerialPort, conn.BaudRate, conn.ConnectTimeout, conn.CommandTimeout, string(tagsJSON), conn.Description,
		boolInt(conn.MCPFirstTrust), nowRFC3339(),
	)
	return err
}

// updateConnection updates the editable settings of an existing connection
// (including renames) without touching runtime/test columns. It reports
// whether a row was updated, so a concurrently deleted row is not resurrected.
func (s *Store) updateConnection(conn Connection) (bool, error) {
	tagsJSON, err := json.Marshal(conn.Tags)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(`
		UPDATE connections SET
			name = ?, type = ?, host = ?, port = ?, user = ?, auth_method = ?, credential = ?,
			serial_port = ?, baud_rate = ?, connect_timeout = ?, command_timeout = ?, tags_json = ?,
			description = ?, mcp_first_trust = ?, updated_at = ?
		WHERE id = ?`,
		conn.Name, conn.connectionType(), conn.Host, conn.Port, conn.User, conn.AuthMethod, conn.Credential,
		conn.SerialPort, conn.BaudRate, conn.ConnectTimeout, conn.CommandTimeout, string(tagsJSON),
		conn.Description, boolInt(conn.MCPFirstTrust), nowRFC3339(), conn.ID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) updateConnectionTest(id string, result string, at string, message string) error {
	_, err := s.db.Exec(`UPDATE connections SET last_test_result = ?, last_test_at = ?, last_test_message = ? WHERE id = ?`,
		result, at, message, id)
	return err
}

func (s *Store) deleteConnection(id string) error {
	_, err := s.db.Exec(`DELETE FROM connections WHERE id = ?`, id)
	return err
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
	settings.TokenInput = ""
	settings.TokenPreview = ""
	payload, err := json.Marshal(settings)
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
		string(payload), token, nowRFC3339(),
	)
	return err
}

const requestColumns = `id, connection_id, host, user, command, executed_command, reason, requested_by, requested_at,
	risk, approved_risk, status, stdout, stderr, duration, decided_by, decided_at, finished_at, exit_code, error_kind,
	client_request_id`

func scanRequest(scanner interface{ Scan(...any) error }) (CommandRequest, error) {
	var req CommandRequest
	var exitCode sql.NullInt64
	err := scanner.Scan(&req.ID, &req.ConnectionID, &req.Host, &req.User, &req.Command, &req.ExecutedCommand,
		&req.Reason, &req.RequestedBy, &req.RequestedAt, &req.Risk, &req.ApprovedRisk, &req.Status, &req.Stdout,
		&req.Stderr, &req.Duration, &req.DecidedBy, &req.DecidedAt, &req.FinishedAt, &exitCode, &req.ErrorKind,
		&req.ClientRequestID)
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
		WHERE status IN (?, ?, ?)
		   OR id IN (SELECT id FROM command_requests ORDER BY created_at DESC LIMIT ?)
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
	createdAt := time.Now().UnixMilli()
	if parsed, err := parseRequestIDMillis(req.ID); err == nil {
		createdAt = parsed
	}
	_, err := s.db.Exec(`
		INSERT INTO command_requests (
			id, connection_id, host, user, command, executed_command, reason, requested_by, requested_at,
			risk, approved_risk, status, stdout, stderr, duration, decided_by, decided_at, finished_at,
			exit_code, error_kind, client_request_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.ConnectionID, req.Host, req.User, req.Command, req.ExecutedCommand, req.Reason, req.RequestedBy,
		req.RequestedAt, req.Risk, req.ApprovedRisk, req.Status, req.Stdout, req.Stderr, req.Duration, req.DecidedBy,
		req.DecidedAt, req.FinishedAt, nullableInt(req.ExitCode), req.ErrorKind, req.ClientRequestID, createdAt,
		nowRFC3339(),
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
			error_kind = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		req.ConnectionID, req.Host, req.ExecutedCommand, req.ApprovedRisk, req.Status, req.Stdout, req.Stderr,
		req.Duration, req.DecidedBy, req.DecidedAt, req.FinishedAt, nullableInt(req.ExitCode), req.ErrorKind,
		nowRFC3339(), req.ID, from,
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

func (s *Store) deleteCommandRequest(id string) error {
	_, err := s.db.Exec(`DELETE FROM command_requests WHERE id = ?`, id)
	return err
}

func (s *Store) loadAgentPolicies() ([]AgentPolicy, error) {
	rows, err := s.db.Query(`SELECT agent_name, approval_bypass, updated_at FROM agent_policies ORDER BY agent_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []AgentPolicy
	for rows.Next() {
		var policy AgentPolicy
		var approvalBypass int
		if err := rows.Scan(&policy.AgentName, &approvalBypass, &policy.UpdatedAt); err != nil {
			return nil, err
		}
		policy.ApprovalBypass = approvalBypass != 0
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func (s *Store) saveAgentPolicy(policy AgentPolicy) error {
	if strings.TrimSpace(policy.UpdatedAt) == "" {
		policy.UpdatedAt = nowRFC3339()
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_policies (agent_name, approval_bypass, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(agent_name) DO UPDATE SET
			approval_bypass = excluded.approval_bypass,
			updated_at = excluded.updated_at`,
		policy.AgentName, boolInt(policy.ApprovalBypass), policy.UpdatedAt,
	)
	return err
}

// parseRequestIDMillis extracts the millisecond timestamp from a request ID.
// IDs are "REQ-<millis>-<seq>"; legacy IDs are "REQ-<millis>" (no seq).
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
