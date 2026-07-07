package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

func openStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, path: path}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
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
	// Additive columns for the serial/SSH connection type. Older databases were
	// created before these existed, so add them when missing.
	for _, col := range []struct{ name, ddl string }{
		{"type", `ALTER TABLE connections ADD COLUMN type TEXT NOT NULL DEFAULT 'SSH'`},
		{"serial_port", `ALTER TABLE connections ADD COLUMN serial_port TEXT NOT NULL DEFAULT ''`},
		{"baud_rate", `ALTER TABLE connections ADD COLUMN baud_rate INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.addColumnIfMissing("connections", col.name, col.ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addColumnIfMissing(table string, column string, ddl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(ddl)
	return err
}

func (s *Store) loadConnections() ([]Connection, error) {
	rows, err := s.db.Query(`SELECT name, type, host, port, user, auth_method, credential, serial_port, baud_rate, connect_timeout, command_timeout, tags_json, description, status, last_checked FROM connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conns []Connection
	for rows.Next() {
		var conn Connection
		var tagsJSON string
		if err := rows.Scan(&conn.Name, &conn.Type, &conn.Host, &conn.Port, &conn.User, &conn.AuthMethod, &conn.Credential, &conn.SerialPort, &conn.BaudRate, &conn.ConnectTimeout, &conn.CommandTimeout, &tagsJSON, &conn.Description, &conn.Status, &conn.LastChecked); err != nil {
			return nil, err
		}
		conn.Type = conn.connectionType()
		if err := json.Unmarshal([]byte(tagsJSON), &conn.Tags); err != nil {
			return nil, err
		}
		conns = append(conns, conn)
	}
	return conns, rows.Err()
}

func (s *Store) saveConnection(conn Connection) error {
	tagsJSON, err := json.Marshal(conn.Tags)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO connections (
			name, type, host, port, user, auth_method, credential, serial_port, baud_rate,
			connect_timeout, command_timeout, tags_json, description, status, last_checked, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			type = excluded.type,
			host = excluded.host,
			port = excluded.port,
			user = excluded.user,
			auth_method = excluded.auth_method,
			credential = excluded.credential,
			serial_port = excluded.serial_port,
			baud_rate = excluded.baud_rate,
			connect_timeout = excluded.connect_timeout,
			command_timeout = excluded.command_timeout,
			tags_json = excluded.tags_json,
			description = excluded.description,
			status = excluded.status,
			last_checked = excluded.last_checked,
			updated_at = excluded.updated_at`,
		conn.Name, conn.connectionType(), conn.Host, conn.Port, conn.User, conn.AuthMethod, conn.Credential,
		conn.SerialPort, conn.BaudRate, conn.ConnectTimeout, conn.CommandTimeout, string(tagsJSON), conn.Description,
		conn.Status, conn.LastChecked, time.Now().Format(time.RFC3339),
	)
	return err
}

func (s *Store) deleteConnection(name string) error {
	_, err := s.db.Exec(`DELETE FROM connections WHERE name = ?`, name)
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
		string(payload), token, time.Now().Format(time.RFC3339),
	)
	return err
}

func (s *Store) deleteMCPSettings() error {
	_, err := s.db.Exec(`DELETE FROM mcp_settings WHERE id = 1`)
	return err
}

func (s *Store) loadCommandRequests(limit int) ([]CommandRequest, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
		SELECT id, host, user, command, reason, requested_by, requested_at, risk, status, stdout, stderr, duration
		FROM command_requests
		ORDER BY created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []CommandRequest
	for rows.Next() {
		var req CommandRequest
		if err := rows.Scan(&req.ID, &req.Host, &req.User, &req.Command, &req.Reason, &req.RequestedBy, &req.RequestedAt, &req.Risk, &req.Status, &req.Stdout, &req.Stderr, &req.Duration); err != nil {
			return nil, err
		}
		requests = append(requests, req)
	}
	return requests, rows.Err()
}

func (s *Store) saveCommandRequest(req CommandRequest) error {
	createdAt := time.Now().UnixMilli()
	if parsed, err := parseRequestIDMillis(req.ID); err == nil {
		createdAt = parsed
	}
	_, err := s.db.Exec(`
		INSERT INTO command_requests (
			id, host, user, command, reason, requested_by, requested_at, risk, status,
			stdout, stderr, duration, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			host = excluded.host,
			user = excluded.user,
			command = excluded.command,
			reason = excluded.reason,
			requested_by = excluded.requested_by,
			requested_at = excluded.requested_at,
			risk = excluded.risk,
			status = excluded.status,
			stdout = excluded.stdout,
			stderr = excluded.stderr,
			duration = excluded.duration,
			updated_at = excluded.updated_at`,
		req.ID, req.Host, req.User, req.Command, req.Reason, req.RequestedBy, req.RequestedAt,
		req.Risk, req.Status, req.Stdout, req.Stderr, req.Duration, createdAt, time.Now().Format(time.RFC3339),
	)
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
	approvalBypass := 0
	if policy.ApprovalBypass {
		approvalBypass = 1
	}
	if strings.TrimSpace(policy.UpdatedAt) == "" {
		policy.UpdatedAt = time.Now().Format(time.RFC3339)
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_policies (agent_name, approval_bypass, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(agent_name) DO UPDATE SET
			approval_bypass = excluded.approval_bypass,
			updated_at = excluded.updated_at`,
		policy.AgentName, approvalBypass, policy.UpdatedAt,
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
