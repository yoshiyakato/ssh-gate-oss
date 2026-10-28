package main

// Audit records for AI sessions (REQ-016, phase7-ai-session.md §6 and §7).
//
// Everything here is new tables only; command_requests is reused unchanged
// for the lines themselves (§7.1). The wiring into storage.go is two lines
// (§9.2, done by the integrator):
//
//	const schemaVersion = aiSessionSchemaVersion   // 4 -> 5
//	if err := migrateAISessionTables(tx); err != nil { return err }  // in migrate(), before PRAGMA user_version
//
// Rules this file keeps:
//   - Each audit step that belongs together is one transaction: a session row
//     and its "open" event, a line's command_requests row and its link, a
//     close and its "close" event, an output batch and its counters.
//   - Only already-redacted text reaches ai_session_output (the caller runs
//     redactSecrets first, §6.3). Environment overlay values whose names look
//     secret are masked here again before they are written, so a raw value
//     never reaches the DB file or the WAL even if a caller forgets.
//   - The store never holds an App lock; callers must not call it while
//     holding aiMu, s.mu or out.mu (§5 lock rules), because these calls do I/O.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// aiSessionSchemaVersion is the user_version that adds the ai_session_*
// tables. Main was at v4 when phase 7 merged (phase 6 headless added no
// migration), so v5 follows v4 directly (§7.3). The migration is CREATE-only.
const aiSessionSchemaVersion = 5

var (
	errAISessionNotFound  = errors.New("AI セッションの記録がありません")
	errAISessionRowClosed = errors.New("AI セッションは閉じています")
)

// Close reasons (§2.9) are defined in ai_session.go. Stored verbatim in
// ai_sessions.close_reason.
var aiSessionCloseReasons = map[string]bool{
	aiCloseAgent: true, aiCloseHuman: true, aiCloseIdle: true, aiCloseMaxLifetime: true,
	aiCloseDisconnected: true, aiCloseConnDeleted: true, aiCloseTargetChanged: true, aiCloseTokenRevoked: true,
	aiCloseDisabled: true, aiCloseRemoteExit: true, aiCloseConnLost: true, aiCloseAppShutdown: true,
	aiCloseAppRestart: true, aiCloseOpenFailed: true,
}

// Event kinds (§7.2 ai_session_events.kind).
const (
	aiEventOpen            = "open"
	aiEventLineRejected    = "line_rejected"
	aiEventInterrupt       = "interrupt"
	aiEventSecretRedacted  = "secret_redacted"
	aiEventOutputTruncated = "output_truncated"
	aiEventTainted         = "tainted"
	aiEventClose           = "close"
)

var aiSessionEventKinds = map[string]bool{
	aiEventOpen: true, aiEventLineRejected: true, aiEventInterrupt: true, aiEventSecretRedacted: true,
	aiEventOutputTruncated: true, aiEventTainted: true, aiEventClose: true,
}

// Line effects (§7.2 ai_session_lines.effect).
var aiSessionLineEffects = map[string]bool{"none": true, "cd": true, "env": true, "state_change": true}

// closed_by values ("agent:<name>", "gui", "system") are defined in ai_session.go.

func validAIClosedBy(by string) bool {
	if by == aiClosedByGUI || by == aiClosedBySystem {
		return true
	}
	return strings.HasPrefix(by, "agent:") && len(by) > len("agent:")
}

// aiSessionRow is one ai_sessions row. ClosedAt is zero while the session is
// open. The counters are maintained by the store (line inserts, output
// appends, secret_redacted events); values passed to insertAISession are
// ignored.
type aiSessionRow struct {
	ID                string    `json:"id"`
	AgentName         string    `json:"agentName"`
	ConnectionID      string    `json:"connectionId"`
	HostName          string    `json:"hostName"`
	TargetFP          string    `json:"targetFp"`
	Mode              string    `json:"mode"`
	Shell             string    `json:"shell"`
	Reason            string    `json:"reason"`
	ClientRequestID   string    `json:"clientRequestId"`
	OpenedAt          time.Time `json:"openedAt"`
	ClosedAt          time.Time `json:"closedAt"`
	CloseReason       string    `json:"closeReason"`
	ClosedBy          string    `json:"closedBy"`
	LineCount         int64     `json:"lineCount"`
	OutputBytes       int64     `json:"outputBytes"`
	StoredOutputBytes int64     `json:"storedOutputBytes"`
	Redactions        int64     `json:"redactions"`
}

func (r aiSessionRow) open() bool { return r.ClosedAt.IsZero() }

// aiSessionEnvEntry is one entry of the gateway's environment overlay as it
// was shown on the approval screen. Unset marks `unset NAME`.
type aiSessionEnvEntry struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	Unset bool   `json:"unset,omitempty"`
}

// aiSessionLineRow links a command_requests row to its session (§7.2
// ai_session_lines). HasCursorEnd is false until finishAISessionLine.
type aiSessionLineRow struct {
	RequestID    string              `json:"requestId"`
	SessionID    string              `json:"sessionId"`
	Seq          int                 `json:"seq"`
	Cwd          string              `json:"cwd"`
	Env          []aiSessionEnvEntry `json:"env"`
	Effect       string              `json:"effect"`
	RiskReasons  []string            `json:"riskReasons"`
	Tainted      bool                `json:"tainted"`
	CursorStart  uint64              `json:"cursorStart"`
	CursorEnd    uint64              `json:"cursorEnd"`
	HasCursorEnd bool                `json:"hasCursorEnd"`
}

// aiSessionEventRow is one ai_session_events row. Seq is assigned by the
// store (1, 2, ... per session).
type aiSessionEventRow struct {
	SessionID string         `json:"sessionId"`
	Seq       int            `json:"seq"`
	At        time.Time      `json:"at"`
	Kind      string         `json:"kind"`
	Detail    map[string]any `json:"detail"`
}

// aiSessionOutputChunk is one piece of redacted, ANSI-stripped text for the
// audit log. CursorStart is the text cursor of its first byte. ChunkSeq is
// assigned by the store.
type aiSessionOutputChunk struct {
	ChunkSeq    int       `json:"chunkSeq"`
	CursorStart uint64    `json:"cursorStart"`
	Text        string    `json:"text"`
	CreatedAt   time.Time `json:"createdAt"`
}

// aiSessionOutputAppend reports what appendAISessionOutput did.
type aiSessionOutputAppend struct {
	// Bytes is the size of all chunks passed (added to output_bytes).
	Bytes int64
	// Stored is how many of those bytes were written to ai_session_output.
	Stored int64
	// Truncated is true when this call hit the audit limit for the first
	// time and wrote the output_truncated event.
	Truncated bool
}

// migrateAISessionTables creates the AI session tables. It only uses
// CREATE ... IF NOT EXISTS, so it is safe to run on every open and leaves
// existing rows of other tables untouched.
func migrateAISessionTables(tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS ai_sessions (
			id TEXT PRIMARY KEY,
			agent_name TEXT NOT NULL,
			connection_id TEXT NOT NULL,
			host_name TEXT NOT NULL,
			target_fp TEXT NOT NULL,
			mode TEXT NOT NULL,
			shell TEXT NOT NULL,
			reason TEXT NOT NULL,
			client_request_id TEXT NOT NULL DEFAULT '',
			opened_at INTEGER NOT NULL,
			closed_at INTEGER,
			close_reason TEXT NOT NULL DEFAULT '',
			closed_by TEXT NOT NULL DEFAULT '',
			line_count INTEGER NOT NULL DEFAULT 0,
			output_bytes INTEGER NOT NULL DEFAULT 0,
			stored_output_bytes INTEGER NOT NULL DEFAULT 0,
			redactions INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_sessions_open ON ai_sessions(closed_at)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_sessions_agent ON ai_sessions(agent_name, opened_at DESC)`,
		`CREATE TABLE IF NOT EXISTS ai_session_lines (
			request_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			cwd TEXT NOT NULL,
			env_json TEXT NOT NULL,
			effect TEXT NOT NULL,
			risk_reasons TEXT NOT NULL,
			tainted INTEGER NOT NULL DEFAULT 0,
			cursor_start INTEGER NOT NULL,
			cursor_end INTEGER,
			UNIQUE (session_id, seq)
		)`,
		`CREATE TABLE IF NOT EXISTS ai_session_events (
			session_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			at INTEGER NOT NULL,
			kind TEXT NOT NULL,
			detail_json TEXT NOT NULL,
			PRIMARY KEY (session_id, seq)
		)`,
		`CREATE TABLE IF NOT EXISTS ai_session_output (
			session_id TEXT NOT NULL,
			chunk_seq INTEGER NOT NULL,
			cursor_start INTEGER NOT NULL,
			text TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, chunk_seq)
		)`,
		`CREATE TABLE IF NOT EXISTS ai_session_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			payload_json TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// aiSecretEnvName reports whether an overlay variable's value must be masked
// (§6.3: names containing TOKEN / SECRET / PASSWORD / KEY; PASSWD and
// CREDENTIAL are added because masking more only hides more).
func aiSecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, part := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "KEY", "CREDENTIAL"} {
		if strings.Contains(upper, part) {
			return true
		}
	}
	return false
}

// aiRedactionMark is the mark used for a masked value, in the same format as
// redactSecrets: [REDACTED:<kind>:<first 8 hex of sha256>].
func aiRedactionMark(kind string, value string) string {
	sum := sha256.Sum256([]byte(value))
	return "[REDACTED:" + kind + ":" + hex.EncodeToString(sum[:])[:8] + "]"
}

// maskAISessionEnv returns a copy of env with secret-looking values masked.
// Values that are already a redaction mark are kept.
func maskAISessionEnv(env []aiSessionEnvEntry) []aiSessionEnvEntry {
	out := make([]aiSessionEnvEntry, len(env))
	for i, e := range env {
		if !e.Unset && e.Value != "" && aiSecretEnvName(e.Name) && !strings.HasPrefix(e.Value, "[REDACTED:") {
			e.Value = aiRedactionMark("env", e.Value)
		}
		out[i] = e
	}
	return out
}

func aiStoreMsTime(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64)
}

func aiStoreTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func aiStoreDetailJSON(detail map[string]any) (string, error) {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type aiStoreQueryRower interface {
	QueryRow(string, ...any) *sql.Row
}

// aiSessionOpenState reads whether the session exists and is open.
func aiSessionOpenState(q aiStoreQueryRower, id string) (exists bool, open bool, err error) {
	var closed sql.NullInt64
	err = q.QueryRow(`SELECT closed_at FROM ai_sessions WHERE id = ?`, id).Scan(&closed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, !closed.Valid, nil
}

// insertAISessionEventTx appends an event with the next seq of the session
// and returns that seq. secret_redacted adds detail["count"] to
// ai_sessions.redactions in the same transaction.
func insertAISessionEventTx(tx *sql.Tx, sessionID string, at time.Time, kind string, detail map[string]any) (int, error) {
	if !aiSessionEventKinds[kind] {
		return 0, fmt.Errorf("unknown AI session event kind %q", kind)
	}
	payload, err := aiStoreDetailJSON(detail)
	if err != nil {
		return 0, err
	}
	var seq int
	if err := tx.QueryRow(`INSERT INTO ai_session_events (session_id, seq, at, kind, detail_json)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ? FROM ai_session_events WHERE session_id = ?
		RETURNING seq`, sessionID, aiStoreTime(at).UnixMilli(), kind, payload, sessionID).Scan(&seq); err != nil {
		return 0, err
	}
	if kind == aiEventSecretRedacted {
		if n := aiStoreDetailCount(detail); n > 0 {
			if _, err := tx.Exec(`UPDATE ai_sessions SET redactions = redactions + ? WHERE id = ?`, n, sessionID); err != nil {
				return 0, err
			}
		}
	}
	return seq, nil
}

func aiStoreDetailCount(detail map[string]any) int64 {
	switch v := detail["count"].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

// insertAISession writes a new session row and its "open" event. The
// session must be recorded before the transport carries any line (§7.2: no
// execution without an audit record).
func (s *Store) insertAISession(row aiSessionRow) error {
	if s == nil || s.db == nil {
		return errStoreUnavailable
	}
	if !strings.HasPrefix(row.ID, "AIS-") {
		return fmt.Errorf("invalid AI session id %q", row.ID)
	}
	opened := aiStoreTime(row.OpenedAt)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO ai_sessions (
			id, agent_name, connection_id, host_name, target_fp, mode, shell, reason, client_request_id, opened_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.AgentName, row.ConnectionID, row.HostName, row.TargetFP, row.Mode, row.Shell, row.Reason,
		row.ClientRequestID, opened.UnixMilli()); err != nil {
		return err
	}
	if _, err := insertAISessionEventTx(tx, row.ID, opened, aiEventOpen, map[string]any{
		"agent": row.AgentName, "connection_id": row.ConnectionID, "host": row.HostName,
		"target_fp": row.TargetFP, "mode": row.Mode, "shell": row.Shell,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// closeAISessionRow records the end of a session and its "close" event in
// one transaction. It returns false (and writes nothing) when the session is
// already closed, so a second close keeps the first reason. detail is merged
// into the close event (for example the withdrawn request id).
func (s *Store) closeAISessionRow(id string, reason string, by string, at time.Time, detail map[string]any) (bool, error) {
	if s == nil || s.db == nil {
		return false, errStoreUnavailable
	}
	if !aiSessionCloseReasons[reason] {
		return false, fmt.Errorf("unknown AI session close reason %q", reason)
	}
	if !validAIClosedBy(by) {
		return false, fmt.Errorf("invalid closed_by %q", by)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	closed, err := closeAISessionRowTx(tx, id, reason, by, aiStoreTime(at), detail)
	if err != nil || !closed {
		return closed, err
	}
	return true, tx.Commit()
}

func closeAISessionRowTx(tx *sql.Tx, id string, reason string, by string, at time.Time, detail map[string]any) (bool, error) {
	exists, open, err := aiSessionOpenState(tx, id)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, errAISessionNotFound
	}
	if !open {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE ai_sessions SET closed_at = ?, close_reason = ?, closed_by = ? WHERE id = ? AND closed_at IS NULL`,
		at.UnixMilli(), reason, by, id); err != nil {
		return false, err
	}
	var lines, outBytes, stored, redactions int64
	if err := tx.QueryRow(`SELECT line_count, output_bytes, stored_output_bytes, redactions FROM ai_sessions WHERE id = ?`, id).
		Scan(&lines, &outBytes, &stored, &redactions); err != nil {
		return false, err
	}
	ev := map[string]any{}
	for k, v := range detail {
		ev[k] = v
	}
	ev["reason"] = reason
	ev["by"] = by
	ev["lines"] = lines
	ev["output_bytes"] = outBytes
	ev["stored_output_bytes"] = stored
	ev["redactions"] = redactions
	if _, err := insertAISessionEventTx(tx, id, at, aiEventClose, ev); err != nil {
		return false, err
	}
	return true, nil
}

func validateAISessionLine(line aiSessionLineRow) error {
	if line.RequestID == "" || line.SessionID == "" {
		return errors.New("AI session line needs request_id and session_id")
	}
	if line.Seq <= 0 {
		return fmt.Errorf("invalid AI session line seq %d", line.Seq)
	}
	if !aiSessionLineEffects[line.Effect] {
		return fmt.Errorf("unknown AI session line effect %q", line.Effect)
	}
	return nil
}

func insertAISessionLineTx(tx *sql.Tx, line aiSessionLineRow) error {
	if err := validateAISessionLine(line); err != nil {
		return err
	}
	exists, open, err := aiSessionOpenState(tx, line.SessionID)
	if err != nil {
		return err
	}
	if !exists {
		return errAISessionNotFound
	}
	if !open {
		return errAISessionRowClosed
	}
	env := line.Env
	if env == nil {
		env = []aiSessionEnvEntry{}
	}
	envJSON, err := json.Marshal(maskAISessionEnv(env))
	if err != nil {
		return err
	}
	reasons := line.RiskReasons
	if reasons == nil {
		reasons = []string{}
	}
	reasonsJSON, err := json.Marshal(reasons)
	if err != nil {
		return err
	}
	var cursorEnd any
	if line.HasCursorEnd {
		cursorEnd = int64(line.CursorEnd)
	}
	if _, err := tx.Exec(`INSERT INTO ai_session_lines (
			request_id, session_id, seq, cwd, env_json, effect, risk_reasons, tainted, cursor_start, cursor_end
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		line.RequestID, line.SessionID, line.Seq, line.Cwd, string(envJSON), line.Effect, string(reasonsJSON),
		boolInt(line.Tainted), int64(line.CursorStart), cursorEnd); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE ai_sessions SET line_count = line_count + 1 WHERE id = ?`, line.SessionID)
	return err
}

// insertAISessionLine records the session link of an existing
// command_requests row. Prefer insertAISessionLineRequest, which writes both
// in one transaction.
func (s *Store) insertAISessionLine(line aiSessionLineRow) error {
	if s == nil || s.db == nil {
		return errStoreUnavailable
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAISessionLineTx(tx, line); err != nil {
		return err
	}
	return tx.Commit()
}

// insertAISessionLineRequest writes the command_requests row of a sent line
// and its ai_session_lines link in one transaction, so a line can never be
// queued or run without its session context, nor linked without a request.
// It fails with errAISessionRowClosed when the session was closed meanwhile.
func (s *Store) insertAISessionLineRequest(req CommandRequest, line aiSessionLineRow) error {
	if s == nil || s.db == nil {
		return errStoreUnavailable
	}
	if req.ID != line.RequestID {
		return fmt.Errorf("request id %q does not match line request id %q", req.ID, line.RequestID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAISessionLineTx(tx, line); err != nil {
		return err
	}
	if err := execInsertCommandRequest(tx, req); err != nil {
		return err
	}
	return tx.Commit()
}

// finishAISessionLine sets the text cursor where the line's output ended.
// It returns false when the line is unknown or already finished.
func (s *Store) finishAISessionLine(requestID string, cursorEnd uint64) (bool, error) {
	if s == nil || s.db == nil {
		return false, errStoreUnavailable
	}
	res, err := s.db.Exec(`UPDATE ai_session_lines SET cursor_end = ? WHERE request_id = ? AND cursor_end IS NULL`,
		int64(cursorEnd), requestID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// insertAISessionEvent appends an event and returns its seq. The session
// must exist (it may be closed: a late interrupt or truncation is still
// recorded).
func (s *Store) insertAISessionEvent(ev aiSessionEventRow) (int, error) {
	if s == nil || s.db == nil {
		return 0, errStoreUnavailable
	}
	if ev.Kind == aiEventOpen || ev.Kind == aiEventClose {
		return 0, fmt.Errorf("event %q is written by insertAISession / closeAISessionRow", ev.Kind)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	exists, _, err := aiSessionOpenState(tx, ev.SessionID)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, errAISessionNotFound
	}
	seq, err := insertAISessionEventTx(tx, ev.SessionID, ev.At, ev.Kind, ev.Detail)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// aiStoreTruncateUTF8 cuts s to at most n bytes without splitting a rune.
func aiStoreTruncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// appendAISessionOutput writes one batch of redacted output in a single
// transaction (§7.2: batched every 64 KiB or 2 s). auditLimit is AuditKB in
// bytes; once ai_session_output holds that much, the rest is only counted in
// output_bytes and the output_truncated event is written exactly once, with
// the text cursor where storing stopped. auditLimit <= 0 stores nothing.
func (s *Store) appendAISessionOutput(sessionID string, chunks []aiSessionOutputChunk, auditLimit int64) (aiSessionOutputAppend, error) {
	var result aiSessionOutputAppend
	if s == nil || s.db == nil {
		return result, errStoreUnavailable
	}
	if len(chunks) == 0 {
		return result, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var stored int64
	err = tx.QueryRow(`SELECT stored_output_bytes FROM ai_sessions WHERE id = ?`, sessionID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return result, errAISessionNotFound
	}
	if err != nil {
		return result, err
	}
	var truncatedBefore int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM ai_session_events WHERE session_id = ? AND kind = ?`,
		sessionID, aiEventOutputTruncated).Scan(&truncatedBefore); err != nil {
		return result, err
	}
	var nextSeq int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(chunk_seq), 0) + 1 FROM ai_session_output WHERE session_id = ?`, sessionID).Scan(&nextSeq); err != nil {
		return result, err
	}
	truncated := truncatedBefore > 0
	for _, c := range chunks {
		result.Bytes += int64(len(c.Text))
		if truncated {
			continue
		}
		text := c.Text
		room := auditLimit - stored
		if int64(len(text)) > room {
			text = aiStoreTruncateUTF8(text, int(max(room, 0)))
			truncated = true
		}
		if text != "" {
			if _, err := tx.Exec(`INSERT INTO ai_session_output (session_id, chunk_seq, cursor_start, text, created_at)
				VALUES (?, ?, ?, ?, ?)`, sessionID, nextSeq, int64(c.CursorStart), text, aiStoreTime(c.CreatedAt).UnixMilli()); err != nil {
				return result, err
			}
			nextSeq++
			stored += int64(len(text))
			result.Stored += int64(len(text))
		}
		if truncated {
			result.Truncated = true
			if _, err := insertAISessionEventTx(tx, sessionID, c.CreatedAt, aiEventOutputTruncated, map[string]any{
				"cursor":      c.CursorStart + uint64(len(text)),
				"audit_limit": auditLimit,
			}); err != nil {
				return result, err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE ai_sessions SET output_bytes = output_bytes + ?, stored_output_bytes = stored_output_bytes + ? WHERE id = ?`,
		result.Bytes, result.Stored, sessionID); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// recoverOpenAISessionRows runs once on startup, before any AI session can
// be opened (§5 起動時の回収). Every session left open by the previous process
// is closed as app_restart, and every pending request linked to a session
// line is withdrawn, so a line can never be approved without its session.
// Running requests are left to recoverInterruptedRequests. It returns the
// closed session ids and the withdrawn request ids.
func (s *Store) recoverOpenAISessionRows(at time.Time) (closed []string, withdrawn []string, err error) {
	if s == nil || s.db == nil {
		return nil, nil, errStoreUnavailable
	}
	at = aiStoreTime(at)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	type pendingLine struct{ requestID, sessionID string }
	rows, err := tx.Query(`SELECT l.request_id, l.session_id FROM ai_session_lines l
		JOIN command_requests r ON r.id = l.request_id
		WHERE r.status = ? ORDER BY l.session_id, l.seq`, statePending)
	if err != nil {
		return nil, nil, err
	}
	var pending []pendingLine
	for rows.Next() {
		var p pendingLine
		if err := rows.Scan(&p.requestID, &p.sessionID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	stamp := at.Format(time.RFC3339)
	bySession := map[string][]string{}
	for _, p := range pending {
		res, err := tx.Exec(`UPDATE command_requests SET status = ?, decided_by = ?, decided_at = ?, finished_at = ?, updated_at = ?
			WHERE id = ? AND status = ?`, stateWithdrawn, aiClosedBySystem, stamp, stamp, nowRFC3339(), p.requestID, statePending)
		if err != nil {
			return nil, nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			withdrawn = append(withdrawn, p.requestID)
			bySession[p.sessionID] = append(bySession[p.sessionID], p.requestID)
		}
	}

	ids, err := aiStoreQueryStrings(tx, `SELECT id FROM ai_sessions WHERE closed_at IS NULL ORDER BY opened_at, id`)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		detail := map[string]any{}
		if w := bySession[id]; len(w) > 0 {
			detail["withdrawn"] = w
		}
		ok, err := closeAISessionRowTx(tx, id, aiCloseAppRestart, aiClosedBySystem, at, detail)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			closed = append(closed, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return closed, withdrawn, nil
}

func aiStoreQueryStrings(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const aiSessionColumns = `id, agent_name, connection_id, host_name, target_fp, mode, shell, reason, client_request_id,
	opened_at, closed_at, close_reason, closed_by, line_count, output_bytes, stored_output_bytes, redactions`

type aiStoreScanner interface {
	Scan(dest ...any) error
}

func scanAISessionRow(sc aiStoreScanner) (aiSessionRow, error) {
	var r aiSessionRow
	var opened int64
	var closed sql.NullInt64
	err := sc.Scan(&r.ID, &r.AgentName, &r.ConnectionID, &r.HostName, &r.TargetFP, &r.Mode, &r.Shell, &r.Reason,
		&r.ClientRequestID, &opened, &closed, &r.CloseReason, &r.ClosedBy, &r.LineCount, &r.OutputBytes,
		&r.StoredOutputBytes, &r.Redactions)
	if err != nil {
		return r, err
	}
	r.OpenedAt = time.UnixMilli(opened)
	r.ClosedAt = aiStoreMsTime(closed)
	return r, nil
}

func (s *Store) queryAISessionRows(query string, args ...any) ([]aiSessionRow, error) {
	if s == nil || s.db == nil {
		return nil, errStoreUnavailable
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiSessionRow
	for rows.Next() {
		r, err := scanAISessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// openAISessionRows lists the sessions whose closed_at is NULL, oldest first.
func (s *Store) openAISessionRows() ([]aiSessionRow, error) {
	return s.queryAISessionRows(`SELECT ` + aiSessionColumns + ` FROM ai_sessions WHERE closed_at IS NULL ORDER BY opened_at, id`)
}

// loadAISessionRows lists sessions newest first (for the GUI history and the
// audit view). limit <= 0 means 200.
func (s *Store) loadAISessionRows(includeClosed bool, limit int) ([]aiSessionRow, error) {
	if limit <= 0 {
		limit = 200
	}
	where := ``
	if !includeClosed {
		where = ` WHERE closed_at IS NULL`
	}
	return s.queryAISessionRows(`SELECT `+aiSessionColumns+` FROM ai_sessions`+where+` ORDER BY opened_at DESC, id DESC LIMIT ?`, limit)
}

// loadAISessionRow reads one session row.
func (s *Store) loadAISessionRow(id string) (aiSessionRow, bool, error) {
	rows, err := s.queryAISessionRows(`SELECT `+aiSessionColumns+` FROM ai_sessions WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return aiSessionRow{}, false, err
	}
	return rows[0], true, nil
}

const aiSessionLineColumns = `request_id, session_id, seq, cwd, env_json, effect, risk_reasons, tainted, cursor_start, cursor_end`

func scanAISessionLineRow(sc aiStoreScanner) (aiSessionLineRow, error) {
	var l aiSessionLineRow
	var envJSON, reasonsJSON string
	var tainted int
	var start int64
	var end sql.NullInt64
	if err := sc.Scan(&l.RequestID, &l.SessionID, &l.Seq, &l.Cwd, &envJSON, &l.Effect, &reasonsJSON, &tainted, &start, &end); err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(envJSON), &l.Env); err != nil {
		return l, fmt.Errorf("ai_session_lines.env_json of %s: %w", l.RequestID, err)
	}
	if err := json.Unmarshal([]byte(reasonsJSON), &l.RiskReasons); err != nil {
		return l, fmt.Errorf("ai_session_lines.risk_reasons of %s: %w", l.RequestID, err)
	}
	l.Tainted = tainted != 0
	l.CursorStart = uint64(start)
	if end.Valid {
		l.CursorEnd = uint64(end.Int64)
		l.HasCursorEnd = true
	}
	return l, nil
}

func (s *Store) queryAISessionLines(query string, args ...any) ([]aiSessionLineRow, error) {
	if s == nil || s.db == nil {
		return nil, errStoreUnavailable
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiSessionLineRow
	for rows.Next() {
		l, err := scanAISessionLineRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// loadAISessionLinks maps command_requests ids to their session line, used
// on startup to fill CommandRequest.SessionID / SessionSeq (§7.1).
func (s *Store) loadAISessionLinks() (map[string]aiSessionLineRow, error) {
	lines, err := s.queryAISessionLines(`SELECT ` + aiSessionLineColumns + ` FROM ai_session_lines`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]aiSessionLineRow, len(lines))
	for _, l := range lines {
		out[l.RequestID] = l
	}
	return out, nil
}

// loadAISessionLine reads the link of one request (GetAISessionLineContext).
func (s *Store) loadAISessionLine(requestID string) (aiSessionLineRow, bool, error) {
	lines, err := s.queryAISessionLines(`SELECT `+aiSessionLineColumns+` FROM ai_session_lines WHERE request_id = ?`, requestID)
	if err != nil || len(lines) == 0 {
		return aiSessionLineRow{}, false, err
	}
	return lines[0], true, nil
}

// loadAISessionLines lists a session's lines in seq order.
func (s *Store) loadAISessionLines(sessionID string) ([]aiSessionLineRow, error) {
	return s.queryAISessionLines(`SELECT `+aiSessionLineColumns+` FROM ai_session_lines WHERE session_id = ? ORDER BY seq`, sessionID)
}

// loadAISessionEvents lists a session's events in seq order.
func (s *Store) loadAISessionEvents(sessionID string) ([]aiSessionEventRow, error) {
	if s == nil || s.db == nil {
		return nil, errStoreUnavailable
	}
	rows, err := s.db.Query(`SELECT seq, at, kind, detail_json FROM ai_session_events WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiSessionEventRow
	for rows.Next() {
		ev := aiSessionEventRow{SessionID: sessionID}
		var at int64
		var payload string
		if err := rows.Scan(&ev.Seq, &at, &ev.Kind, &payload); err != nil {
			return nil, err
		}
		ev.At = time.UnixMilli(at)
		if err := json.Unmarshal([]byte(payload), &ev.Detail); err != nil {
			return nil, fmt.Errorf("ai_session_events.detail_json of %s/%d: %w", sessionID, ev.Seq, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// loadAISessionOutput reads stored output chunks whose cursor_start is at or
// after sinceCursor, in order, until about maxBytes of text (at least one
// chunk). maxBytes <= 0 means no limit.
func (s *Store) loadAISessionOutput(sessionID string, sinceCursor uint64, maxBytes int) ([]aiSessionOutputChunk, error) {
	if s == nil || s.db == nil {
		return nil, errStoreUnavailable
	}
	rows, err := s.db.Query(`SELECT chunk_seq, cursor_start, text, created_at FROM ai_session_output
		WHERE session_id = ? AND cursor_start >= ? ORDER BY chunk_seq`, sessionID, int64(sinceCursor))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiSessionOutputChunk
	total := 0
	for rows.Next() {
		var c aiSessionOutputChunk
		var start, created int64
		if err := rows.Scan(&c.ChunkSeq, &start, &c.Text, &created); err != nil {
			return nil, err
		}
		if maxBytes > 0 && len(out) > 0 && total+len(c.Text) > maxBytes {
			break
		}
		c.CursorStart = uint64(start)
		c.CreatedAt = time.UnixMilli(created)
		out = append(out, c)
		total += len(c.Text)
	}
	return out, rows.Err()
}

// pruneAISessionOutput deletes the stored output of sessions closed before
// `before` (§7.3: 30 days). Sessions, lines and events are kept. It returns
// the number of chunks deleted. secure_delete is on, so the freed pages are
// zeroed.
func (s *Store) pruneAISessionOutput(before time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errStoreUnavailable
	}
	res, err := s.db.Exec(`DELETE FROM ai_session_output WHERE session_id IN (
		SELECT id FROM ai_sessions WHERE closed_at IS NOT NULL AND closed_at < ?)`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// loadAISessionSettings decodes the stored settings into dst (normally
// *AISessionSettings, owned by aisession.go). found is false when nothing is
// stored yet; dst is then left as the caller's defaults.
func (s *Store) loadAISessionSettings(dst any) (bool, error) {
	if s == nil || s.db == nil {
		return false, errStoreUnavailable
	}
	var payload string
	err := s.db.QueryRow(`SELECT payload_json FROM ai_session_settings WHERE id = 1`).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(payload), dst); err != nil {
		return false, fmt.Errorf("ai_session_settings: %w", err)
	}
	return true, nil
}

// saveAISessionSettings stores settings as JSON (kept out of MCPSettings,
// §7.2, so SaveMCPSettings cannot overwrite it).
func (s *Store) saveAISessionSettings(settings any, at time.Time) error {
	if s == nil || s.db == nil {
		return errStoreUnavailable
	}
	payload, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO ai_session_settings (id, payload_json, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET payload_json = excluded.payload_json, updated_at = excluded.updated_at`,
		string(payload), aiStoreTime(at).Format(time.RFC3339))
	return err
}
