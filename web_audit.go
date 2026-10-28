package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Admin audit log (phase6-headless.md §6.9): the admin_audit table, written
// by one goroutine from a buffered chan so a Web call never waits on the
// database, and read by GET /api/admin/audit.

const adminAuditSchema = `
CREATE TABLE IF NOT EXISTS admin_audit (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  at         TEXT NOT NULL,
  actor      TEXT NOT NULL,
  remote     TEXT NOT NULL,
  action     TEXT NOT NULL,
  target     TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '',
  ok         INTEGER NOT NULL,
  code       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS admin_audit_at ON admin_audit(at);`

const (
	adminAuditQueue     = 1024
	adminAuditKeep      = 100000
	adminAuditListMax   = 500
	adminAuditListDflt  = 100
	adminAuditPruneEach = 24 * time.Hour
)

// adminAuditEntry is one admin_audit row. It never carries a secret: the
// producers (webapi.go, webauth.go, config.go) already reduce arguments to
// IDs, field names, digests and byte counts.
type adminAuditEntry struct {
	ID     int64  `json:"id"`
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Remote string `json:"remote"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	OK     bool   `json:"ok"`
	Code   string `json:"code"`
}

// auditSink receives audit entries; record must not block.
type auditSink interface {
	record(e adminAuditEntry)
}

func ensureAdminAuditTable(s *Store) error {
	if s == nil || s.db == nil {
		return errStoreUnavailable
	}
	_, err := s.db.Exec(adminAuditSchema)
	return err
}

// adminAuditWriter is the single writer of admin_audit.
type adminAuditWriter struct {
	store *Store
	now   func() time.Time
	logf  func(format string, args ...any)

	mu      sync.Mutex
	closed  bool
	ch      chan adminAuditEntry
	done    chan struct{}
	dropped int
}

// newAdminAuditWriter creates the table, prunes it and starts the writer.
func newAdminAuditWriter(s *Store) (*adminAuditWriter, error) {
	if err := ensureAdminAuditTable(s); err != nil {
		return nil, err
	}
	w := &adminAuditWriter{
		store: s,
		now:   time.Now,
		logf:  log.Printf,
		ch:    make(chan adminAuditEntry, adminAuditQueue),
		done:  make(chan struct{}),
	}
	w.prune()
	go w.run()
	return w, nil
}

// record queues e (and logs it on stderr as one line). A full queue drops
// the entry and counts it; the caller is never delayed.
func (w *adminAuditWriter) record(e adminAuditEntry) {
	if e.At == "" {
		e.At = w.now().UTC().Format(time.RFC3339)
	}
	w.logf("audit: actor=%s remote=%s action=%s target=%q detail=%q ok=%t code=%s",
		e.Actor, e.Remote, e.Action, e.Target, e.Detail, e.OK, e.Code)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped++
		if w.dropped == 1 || w.dropped%100 == 0 {
			w.logf("audit: queue full, %d entries dropped", w.dropped)
		}
	}
}

func (w *adminAuditWriter) run() {
	defer close(w.done)
	tick := time.NewTicker(adminAuditPruneEach)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-w.ch:
			if !ok {
				return
			}
			w.insert(e)
		case <-tick.C:
			w.prune()
		}
	}
}

func (w *adminAuditWriter) insert(e adminAuditEntry) {
	ok := 0
	if e.OK {
		ok = 1
	}
	if _, err := w.store.db.Exec(`INSERT INTO admin_audit (at, actor, remote, action, target, detail, ok, code) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.At, e.Actor, e.Remote, e.Action, e.Target, e.Detail, ok, e.Code); err != nil {
		w.logf("audit: write failed: %v", err)
	}
}

func (w *adminAuditWriter) prune() {
	if _, err := w.store.db.Exec(`DELETE FROM admin_audit WHERE id <= (SELECT COALESCE(MAX(id), 0) - ? FROM admin_audit)`, adminAuditKeep); err != nil {
		w.logf("audit: prune failed: %v", err)
	}
}

// Close stops accepting entries and waits up to timeout for the queue to be
// written (§8 step 4, before store.Close).
func (w *adminAuditWriter) Close(timeout time.Duration) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.ch)
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(timeout):
		w.logf("audit: flush timed out after %s", timeout)
	}
}

// listAdminAudit returns the newest limit entries, newest first.
func listAdminAudit(s *Store, limit int) ([]adminAuditEntry, error) {
	if s == nil || s.db == nil {
		return nil, errStoreUnavailable
	}
	if limit <= 0 {
		limit = adminAuditListDflt
	}
	if limit > adminAuditListMax {
		limit = adminAuditListMax
	}
	rows, err := s.db.Query(`SELECT id, at, actor, remote, action, target, detail, ok, code FROM admin_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []adminAuditEntry{}
	for rows.Next() {
		var e adminAuditEntry
		var ok int
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Remote, &e.Action, &e.Target, &e.Detail, &ok, &e.Code); err != nil {
			return nil, err
		}
		e.OK = ok != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// adminAuditListHandler serves GET /api/admin/audit?limit=N (session
// required by the server's mux).
func adminAuditListHandler(s *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			webauthError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		limit := adminAuditListDflt
		if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				webauthError(w, http.StatusBadRequest, "invalid limit")
				return
			}
			limit = n
		}
		entries, err := listAdminAudit(s, limit)
		if err != nil {
			webauthError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
	})
}

// fromWebAudit converts webauth.go / webapi.go events into audit entries.
func fromWebAudit(e webAuditEvent) adminAuditEntry {
	return adminAuditEntry{Actor: e.Actor, Remote: e.Remote, Action: e.Action, Target: e.Target, Detail: e.Detail, OK: e.OK, Code: e.Code}
}

// configAuditTo turns a sink into config.go's configAudit (actor "config").
func configAuditTo(s auditSink) configAudit {
	return func(action, target, detail string) {
		s.record(adminAuditEntry{Actor: "config", Remote: "", Action: action, Target: target, Detail: detail, OK: true})
	}
}
