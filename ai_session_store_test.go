package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ensureAISessionTables runs the AI session migration on a store. Until the
// integrator wires migrateAISessionTables into migrate() (§9.2) openStore does
// not create the tables; afterwards this is a no-op, which also exercises
// idempotence.
func ensureAISessionTables(t *testing.T, store *Store) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := migrateAISessionTables(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func newAISessionTestStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(t)
	ensureAISessionTables(t, store)
	return store
}

var aiTestT0 = time.UnixMilli(1_790_000_000_000)

func testAISessionRow(id string) aiSessionRow {
	return aiSessionRow{
		ID: id, AgentName: "codex", ConnectionID: "c1", HostName: "web", TargetFP: "u@10.0.0.1:22",
		Mode: "exec", Shell: "sh", Reason: "deploy check", OpenedAt: aiTestT0,
		// Counters passed in are ignored.
		LineCount: 99, OutputBytes: 99,
	}
}

func testAILineRequest(sessionID string, seq int, status string) (CommandRequest, aiSessionLineRow) {
	at := aiTestT0.Add(time.Duration(seq) * time.Second)
	req := testRequest(fmt.Sprintf("%s-%d", sessionID, seq), "codex", "", at)
	req.Command = fmt.Sprintf("echo %d", seq)
	req.Status = status
	line := aiSessionLineRow{
		RequestID: req.ID, SessionID: sessionID, Seq: seq, Cwd: "/srv/app", Effect: "none",
		RiskReasons: []string{"read_only"}, CursorStart: uint64(seq * 100),
	}
	return req, line
}

func requestStatus(t *testing.T, store *Store, id string) string {
	t.Helper()
	req, ok, err := store.loadCommandRequest(id)
	if err != nil || !ok {
		t.Fatalf("load %s: ok=%v err=%v", id, ok, err)
	}
	return req.Status
}

func eventKinds(t *testing.T, store *Store, sessionID string) []string {
	t.Helper()
	events, err := store.loadAISessionEvents(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i, ev := range events {
		if ev.Seq != i+1 {
			t.Fatalf("event seq = %d at index %d, want contiguous from 1", ev.Seq, i)
		}
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

func TestMigrateAISessionTablesIdempotentAndKeepsRows(t *testing.T) {
	store := newTestStore(t)
	if err := store.insertConnection(Connection{ID: "c1", Name: "web", Type: "SSH", Host: "10.0.0.1", Port: 22, User: "u",
		AuthMethod: authKey, Credential: "~/.ssh/id_ed25519", ConnectTimeout: 10, CommandTimeout: 30}); err != nil {
		t.Fatal(err)
	}
	req := testRequest("a", "codex", "", aiTestT0)
	if err := store.insertCommandRequest(req); err != nil {
		t.Fatal(err)
	}
	before, _, err := store.loadCommandRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ensureAISessionTables(t, store)
	}
	for _, table := range []string{"ai_sessions", "ai_session_lines", "ai_session_events", "ai_session_output", "ai_session_settings"} {
		if !tableExists(t, store.path, table) {
			t.Fatalf("table %s missing", table)
		}
	}
	after, _, err := store.loadCommandRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", before) != fmt.Sprintf("%+v", after) {
		t.Fatalf("command request changed by migration:\n%+v\n%+v", before, after)
	}
	conns, err := store.loadConnections()
	if err != nil || len(conns) != 1 || conns[0].ID != "c1" {
		t.Fatalf("connections after migration = %+v, %v", conns, err)
	}
}

// TestMigrationV4ToAISessionVersion runs once the integrator has raised
// schemaVersion (§7.3); before that it is skipped.
func TestMigrationV4ToAISessionVersion(t *testing.T) {
	if schemaVersion < aiSessionSchemaVersion {
		t.Skipf("schemaVersion is %d; wiring to v%d not done yet", schemaVersion, aiSessionSchemaVersion)
	}
	path := filepath.Join(t.TempDir(), "ssh-gete.sqlite")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	req := testRequest("keep", "codex", "", aiTestT0)
	if err := store.insertCommandRequest(req); err != nil {
		t.Fatal(err)
	}
	store.Close()
	rawExec(t, path,
		`DROP TABLE ai_sessions`, `DROP TABLE ai_session_lines`, `DROP TABLE ai_session_events`,
		`DROP TABLE ai_session_output`, `DROP TABLE ai_session_settings`, `PRAGMA user_version = 4`)

	for i := 0; i < 2; i++ {
		store, err = openStore(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := store.loadCommandRequest(req.ID)
		if err != nil || !ok || got.Command != req.Command {
			t.Fatalf("existing request after migration: %+v ok=%v err=%v", got, ok, err)
		}
		if err := store.insertAISession(testAISessionRow(fmt.Sprintf("AIS-mig%d", i))); err != nil {
			t.Fatalf("insert after migration: %v", err)
		}
		store.Close()
	}
	if v := userVersion(t, path); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	backup := path + ".bak-v4"
	if !fileExists(backup) {
		t.Fatalf("no backup before v4 -> v%d", schemaVersion)
	}
	if userVersion(t, backup) != 4 || tableExists(t, backup, "ai_sessions") {
		t.Fatalf("backup is not the untouched v4 database")
	}
}

func TestAISessionLifecycleAudit(t *testing.T) {
	store := newAISessionTestStore(t)
	const id = "AIS-0001"
	if err := store.insertAISession(aiSessionRow{ID: "TERM-1"}); err == nil {
		t.Fatalf("non AIS- id accepted")
	}
	if err := store.insertAISession(testAISessionRow(id)); err != nil {
		t.Fatal(err)
	}
	if err := store.insertAISession(testAISessionRow(id)); err == nil {
		t.Fatalf("duplicate session id accepted")
	}
	open, err := store.openAISessionRows()
	if err != nil || len(open) != 1 || !open[0].open() || open[0].LineCount != 0 || open[0].OutputBytes != 0 {
		t.Fatalf("open rows = %+v, %v", open, err)
	}
	if !open[0].OpenedAt.Equal(aiTestT0) {
		t.Fatalf("opened_at = %v, want %v", open[0].OpenedAt, aiTestT0)
	}

	req1, line1 := testAILineRequest(id, 1, stateAutoRunning)
	line1.Effect = "cd"
	line1.Env = []aiSessionEnvEntry{{Name: "LANG", Value: "C"}, {Name: "FOO", Unset: true}}
	if err := store.insertAISessionLineRequest(req1, line1); err != nil {
		t.Fatal(err)
	}
	// Same seq again: the link fails and the request row must not be left behind.
	req1b, line1b := testAILineRequest(id, 1, statePending)
	req1b.ID += "-dup"
	line1b.RequestID = req1b.ID
	if err := store.insertAISessionLineRequest(req1b, line1b); err == nil {
		t.Fatalf("duplicate (session, seq) accepted")
	}
	if _, ok, _ := store.loadCommandRequest(req1b.ID); ok {
		t.Fatalf("command request written without its session link")
	}
	// Mismatched ids and bad effect are refused before any write.
	req2, line2 := testAILineRequest(id, 2, statePending)
	line2.RequestID = "other"
	if err := store.insertAISessionLineRequest(req2, line2); err == nil {
		t.Fatalf("mismatched request id accepted")
	}
	line2.RequestID = req2.ID
	line2.Effect = "magic"
	if err := store.insertAISessionLineRequest(req2, line2); err == nil {
		t.Fatalf("unknown effect accepted")
	}

	if ok, err := store.finishAISessionLine(req1.ID, 180); err != nil || !ok {
		t.Fatalf("finish = %v, %v", ok, err)
	}
	if ok, _ := store.finishAISessionLine(req1.ID, 999); ok {
		t.Fatalf("second finish overwrote cursor_end")
	}
	got, ok, err := store.loadAISessionLine(req1.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got.Seq != 1 || got.Effect != "cd" || got.Cwd != "/srv/app" || !got.HasCursorEnd || got.CursorEnd != 180 ||
		got.CursorStart != 100 || len(got.Env) != 2 || !got.Env[1].Unset || got.Env[0].Value != "C" ||
		len(got.RiskReasons) != 1 {
		t.Fatalf("line = %+v", got)
	}

	if _, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: id, Kind: "bogus"}); err == nil {
		t.Fatalf("unknown event kind accepted")
	}
	if _, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: id, Kind: aiEventClose}); err == nil {
		t.Fatalf("close event accepted outside closeAISessionRow")
	}
	if _, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: "AIS-none", Kind: aiEventInterrupt}); !errors.Is(err, errAISessionNotFound) {
		t.Fatalf("event for unknown session: %v", err)
	}
	if seq, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: id, Kind: aiEventSecretRedacted,
		Detail: map[string]any{"kind": "github", "seq": 1, "count": 2}}); err != nil || seq != 2 {
		t.Fatalf("secret_redacted seq = %d, %v", seq, err)
	}
	if _, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: id, Kind: aiEventInterrupt,
		Detail: map[string]any{"via": "signal"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.closeAISessionRow(id, "bored", aiClosedByGUI, aiTestT0, nil); err == nil {
		t.Fatalf("unknown close reason accepted")
	}
	if _, err := store.closeAISessionRow(id, aiCloseHuman, "agent:", aiTestT0, nil); err == nil {
		t.Fatalf("empty agent closed_by accepted")
	}
	if _, err := store.closeAISessionRow("AIS-none", aiCloseHuman, aiClosedByGUI, aiTestT0, nil); !errors.Is(err, errAISessionNotFound) {
		t.Fatalf("close unknown: %v", err)
	}
	closedAt := aiTestT0.Add(time.Minute)
	if ok, err := store.closeAISessionRow(id, aiCloseAgent, aiClosedByAgent("codex"), closedAt, map[string]any{"pending": ""}); err != nil || !ok {
		t.Fatalf("close = %v, %v", ok, err)
	}
	if ok, err := store.closeAISessionRow(id, aiCloseIdle, aiClosedBySystem, closedAt.Add(time.Minute), nil); err != nil || ok {
		t.Fatalf("second close = %v, %v (want false, nil)", ok, err)
	}
	if err := store.insertAISessionLineRequest(req2, func() aiSessionLineRow { line2.Effect = "none"; return line2 }()); !errors.Is(err, errAISessionRowClosed) {
		t.Fatalf("line into closed session: %v", err)
	}
	if _, ok, _ := store.loadCommandRequest(req2.ID); ok {
		t.Fatalf("request of a closed session was written")
	}
	// A late interrupt after close is still recorded.
	if _, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: id, Kind: aiEventInterrupt}); err != nil {
		t.Fatal(err)
	}

	row, ok, err := store.loadAISessionRow(id)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if row.open() || !row.ClosedAt.Equal(closedAt) || row.CloseReason != aiCloseAgent || row.ClosedBy != "agent:codex" ||
		row.LineCount != 1 || row.Redactions != 2 {
		t.Fatalf("closed row = %+v", row)
	}
	if open, _ := store.openAISessionRows(); len(open) != 0 {
		t.Fatalf("closed session still listed as open: %+v", open)
	}
	if all, _ := store.loadAISessionRows(true, 0); len(all) != 1 {
		t.Fatalf("history = %+v", all)
	}
	if kinds := strings.Join(eventKinds(t, store, id), ","); kinds != "open,secret_redacted,interrupt,close,interrupt" {
		t.Fatalf("events = %s", kinds)
	}
	events, _ := store.loadAISessionEvents(id)
	closeEv := events[3].Detail
	if closeEv["reason"] != aiCloseAgent || closeEv["by"] != "agent:codex" || closeEv["lines"] != float64(1) || closeEv["redactions"] != float64(2) {
		t.Fatalf("close detail = %+v", closeEv)
	}
	if events[0].Detail["target_fp"] != "u@10.0.0.1:22" || events[0].Detail["mode"] != "exec" {
		t.Fatalf("open detail = %+v", events[0].Detail)
	}

	links, err := store.loadAISessionLinks()
	if err != nil || len(links) != 1 || links[req1.ID].SessionID != id || links[req1.ID].Seq != 1 {
		t.Fatalf("links = %+v, %v", links, err)
	}
	if lines, _ := store.loadAISessionLines(id); len(lines) != 1 {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestAISessionLineLinkOnly(t *testing.T) {
	store := newAISessionTestStore(t)
	if err := store.insertAISession(testAISessionRow("AIS-link")); err != nil {
		t.Fatal(err)
	}
	req, line := testAILineRequest("AIS-link", 1, statePending)
	if err := store.insertCommandRequest(req); err != nil {
		t.Fatal(err)
	}
	if err := store.insertAISessionLine(line); err != nil {
		t.Fatal(err)
	}
	if err := store.insertAISessionLine(aiSessionLineRow{RequestID: "x", SessionID: "AIS-none", Seq: 1, Effect: "none"}); !errors.Is(err, errAISessionNotFound) {
		t.Fatalf("line for unknown session: %v", err)
	}
	if err := store.insertAISessionLine(aiSessionLineRow{RequestID: "x", SessionID: "AIS-link", Seq: 0, Effect: "none"}); err == nil {
		t.Fatalf("seq 0 accepted")
	}
}

func TestAISessionOutputAuditLimit(t *testing.T) {
	store := newAISessionTestStore(t)
	const id = "AIS-out"
	if err := store.insertAISession(testAISessionRow(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendAISessionOutput("AIS-none", []aiSessionOutputChunk{{Text: "x"}}, 100); !errors.Is(err, errAISessionNotFound) {
		t.Fatalf("output for unknown session: %v", err)
	}
	if res, err := store.appendAISessionOutput(id, nil, 10); err != nil || res != (aiSessionOutputAppend{}) {
		t.Fatalf("empty append = %+v, %v", res, err)
	}
	// limit 10: "abcd" (4) + "日本語" (9 bytes) -> only "日本" (6) fits.
	res, err := store.appendAISessionOutput(id, []aiSessionOutputChunk{
		{CursorStart: 0, Text: "abcd", CreatedAt: aiTestT0},
		{CursorStart: 4, Text: "日本語", CreatedAt: aiTestT0},
		{CursorStart: 13, Text: "tail", CreatedAt: aiTestT0},
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != 17 || res.Stored != 10 || !res.Truncated {
		t.Fatalf("append result = %+v", res)
	}
	res, err = store.appendAISessionOutput(id, []aiSessionOutputChunk{{CursorStart: 17, Text: "more"}}, 10)
	if err != nil || res.Bytes != 4 || res.Stored != 0 || res.Truncated {
		t.Fatalf("append after limit = %+v, %v", res, err)
	}
	chunks, err := store.loadAISessionOutput(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for i, c := range chunks {
		if c.ChunkSeq != i+1 {
			t.Fatalf("chunk seq %d at %d", c.ChunkSeq, i)
		}
		texts = append(texts, c.Text)
	}
	if strings.Join(texts, "|") != "abcd|日本" {
		t.Fatalf("stored chunks = %q", texts)
	}
	if got, _ := store.loadAISessionOutput(id, 4, 0); len(got) != 1 || got[0].Text != "日本" {
		t.Fatalf("since cursor 4 = %+v", got)
	}
	if got, _ := store.loadAISessionOutput(id, 0, 1); len(got) != 1 {
		t.Fatalf("maxBytes still returns one chunk: %+v", got)
	}
	row, _, _ := store.loadAISessionRow(id)
	if row.OutputBytes != 21 || row.StoredOutputBytes != 10 {
		t.Fatalf("counters = %+v", row)
	}
	kinds := eventKinds(t, store, id)
	if strings.Join(kinds, ",") != "open,output_truncated" {
		t.Fatalf("events = %v", kinds)
	}
	events, _ := store.loadAISessionEvents(id)
	if events[1].Detail["cursor"] != float64(10) {
		t.Fatalf("truncated at cursor %v, want 10", events[1].Detail["cursor"])
	}

	// Limit 0 stores nothing but still counts and marks truncation once.
	if err := store.insertAISession(testAISessionRow("AIS-zero")); err != nil {
		t.Fatal(err)
	}
	res, err = store.appendAISessionOutput("AIS-zero", []aiSessionOutputChunk{{Text: "a"}, {Text: "b"}}, 0)
	if err != nil || res.Stored != 0 || res.Bytes != 2 || !res.Truncated {
		t.Fatalf("limit 0 = %+v, %v", res, err)
	}
	if kinds := eventKinds(t, store, "AIS-zero"); len(kinds) != 2 {
		t.Fatalf("limit 0 events = %v", kinds)
	}
}

func TestRecoverOpenAISessionRows(t *testing.T) {
	store := newAISessionTestStore(t)
	for _, id := range []string{"AIS-a", "AIS-b", "AIS-done"} {
		if err := store.insertAISession(testAISessionRow(id)); err != nil {
			t.Fatal(err)
		}
	}
	reqA1, lineA1 := testAILineRequest("AIS-a", 1, stateSucceeded)
	reqA2, lineA2 := testAILineRequest("AIS-a", 2, statePending)
	reqB1, lineB1 := testAILineRequest("AIS-b", 1, stateRunning)
	for _, p := range []struct {
		req  CommandRequest
		line aiSessionLineRow
	}{{reqA1, lineA1}, {reqA2, lineA2}, {reqB1, lineB1}} {
		if err := store.insertAISessionLineRequest(p.req, p.line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.closeAISessionRow("AIS-done", aiCloseAgent, aiClosedByAgent("codex"), aiTestT0, nil); err != nil {
		t.Fatal(err)
	}
	plain := testRequest("plain", "codex", "", aiTestT0)
	if err := store.insertCommandRequest(plain); err != nil {
		t.Fatal(err)
	}

	at := aiTestT0.Add(time.Hour)
	closed, withdrawn, err := store.recoverOpenAISessionRows(at)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(closed, ",") != "AIS-a,AIS-b" || strings.Join(withdrawn, ",") != reqA2.ID {
		t.Fatalf("closed=%v withdrawn=%v", closed, withdrawn)
	}
	if s := requestStatus(t, store, reqA2.ID); s != stateWithdrawn {
		t.Fatalf("pending line status = %s", s)
	}
	if r, _, _ := store.loadCommandRequest(reqA2.ID); r.DecidedBy != aiClosedBySystem || r.FinishedAt == "" {
		t.Fatalf("withdrawn request = %+v", r)
	}
	if s := requestStatus(t, store, reqB1.ID); s != stateRunning {
		t.Fatalf("running line touched: %s (recoverInterruptedRequests owns it)", s)
	}
	if s := requestStatus(t, store, reqA1.ID); s != stateSucceeded {
		t.Fatalf("finished line touched: %s", s)
	}
	if s := requestStatus(t, store, plain.ID); s != statePending {
		t.Fatalf("non-session pending request touched: %s", s)
	}
	row, _, _ := store.loadAISessionRow("AIS-a")
	if row.CloseReason != aiCloseAppRestart || row.ClosedBy != aiClosedBySystem || !row.ClosedAt.Equal(at) {
		t.Fatalf("recovered row = %+v", row)
	}
	done, _, _ := store.loadAISessionRow("AIS-done")
	if done.CloseReason != aiCloseAgent {
		t.Fatalf("already closed session changed: %+v", done)
	}
	events, _ := store.loadAISessionEvents("AIS-a")
	last := events[len(events)-1]
	if last.Kind != aiEventClose || fmt.Sprint(last.Detail["withdrawn"]) != fmt.Sprint([]any{reqA2.ID}) {
		t.Fatalf("close event = %+v", last)
	}
	// Second run finds nothing.
	closed, withdrawn, err = store.recoverOpenAISessionRows(at)
	if err != nil || len(closed) != 0 || len(withdrawn) != 0 {
		t.Fatalf("second recovery = %v %v %v", closed, withdrawn, err)
	}
}

func TestPruneAISessionOutput(t *testing.T) {
	store := newAISessionTestStore(t)
	for _, id := range []string{"AIS-old", "AIS-new", "AIS-open"} {
		if err := store.insertAISession(testAISessionRow(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.appendAISessionOutput(id, []aiSessionOutputChunk{{Text: "x"}, {Text: "y"}}, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.closeAISessionRow("AIS-old", aiCloseIdle, aiClosedBySystem, aiTestT0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.closeAISessionRow("AIS-new", aiCloseIdle, aiClosedBySystem, aiTestT0.Add(40*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	n, err := store.pruneAISessionOutput(aiTestT0.Add(30 * 24 * time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned = %d, %v", n, err)
	}
	for id, want := range map[string]int{"AIS-old": 0, "AIS-new": 2, "AIS-open": 2} {
		if got, _ := store.loadAISessionOutput(id, 0, 0); len(got) != want {
			t.Fatalf("%s chunks = %d, want %d", id, len(got), want)
		}
	}
	if row, ok, _ := store.loadAISessionRow("AIS-old"); !ok || row.StoredOutputBytes != 2 {
		t.Fatalf("session row must stay after prune: %+v", row)
	}
	if kinds := eventKinds(t, store, "AIS-old"); len(kinds) != 2 {
		t.Fatalf("events must stay after prune: %v", kinds)
	}
}

func TestAISessionSettingsRoundTrip(t *testing.T) {
	store := newAISessionTestStore(t)
	type settings struct {
		Enabled      bool     `json:"enabled"`
		IdleMinutes  int      `json:"idleMinutes"`
		ShellConnIDs []string `json:"shellConnIds"`
	}
	got := settings{IdleMinutes: 10}
	if found, err := store.loadAISessionSettings(&got); err != nil || found || got.IdleMinutes != 10 {
		t.Fatalf("empty load = %v %v %+v", found, err, got)
	}
	want := settings{Enabled: true, IdleMinutes: 15, ShellConnIDs: []string{"c1"}}
	for i := 0; i < 2; i++ {
		if err := store.saveAISessionSettings(want, aiTestT0); err != nil {
			t.Fatal(err)
		}
	}
	got = settings{}
	if found, err := store.loadAISessionSettings(&got); err != nil || !found || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("load = %v %v %+v", found, err, got)
	}
}

// Secret-looking env values never reach the DB file or the WAL in plain text
// (same check as the credential tests in storage_test.go).
func TestAISessionEnvSecretsNotStoredInPlaintext(t *testing.T) {
	store := newAISessionTestStore(t)
	if err := store.insertAISession(testAISessionRow("AIS-env")); err != nil {
		t.Fatal(err)
	}
	secrets := []string{"ghp_plaintextTokenValue123", "hunter2-plaintext-pw", "aws-secret-plaintext-xyz"}
	req, line := testAILineRequest("AIS-env", 1, statePending)
	line.Env = []aiSessionEnvEntry{
		{Name: "GITHUB_TOKEN", Value: secrets[0]},
		{Name: "db_password", Value: secrets[1]},
		{Name: "AWS_SECRET_ACCESS_KEY", Value: secrets[2]},
		{Name: "API_KEY", Value: "[REDACTED:env:deadbeef]"},
		{Name: "LANG", Value: "C.UTF-8"},
	}
	if err := store.insertAISessionLineRequest(req, line); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.loadAISessionLine(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Env[0].Value != aiRedactionMark("env", secrets[0]) || !strings.HasPrefix(got.Env[1].Value, "[REDACTED:env:") ||
		got.Env[3].Value != "[REDACTED:env:deadbeef]" || got.Env[4].Value != "C.UTF-8" {
		t.Fatalf("stored env = %+v", got.Env)
	}
	if aiRedactionMark("env", "a") != aiRedactionMark("env", "a") || aiRedactionMark("env", "a") == aiRedactionMark("env", "b") {
		t.Fatalf("redaction mark must be stable per value")
	}
	// The caller's slice is not modified.
	if line.Env[0].Value != secrets[0] {
		t.Fatalf("maskAISessionEnv modified its input")
	}
	for _, p := range []string{store.path, store.path + "-wal"} {
		if hit := fileContainsAny(t, p, secrets); hit != "" {
			t.Fatalf("%s contains plaintext %q", filepath.Base(p), hit)
		}
	}
	store.Close()
	if hit := fileContainsAny(t, store.path, secrets); hit != "" {
		t.Fatalf("db contains plaintext %q after close", hit)
	}
	if _, err := os.Stat(store.path); err != nil {
		t.Fatal(err)
	}
}

// Concurrent writers (sweeper, output flusher, tool calls) get contiguous,
// unique event seqs.
func TestAISessionEventSeqConcurrent(t *testing.T) {
	store := newAISessionTestStore(t)
	if err := store.insertAISession(testAISessionRow("AIS-conc")); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: "AIS-conc", Kind: aiEventInterrupt, Detail: map[string]any{"i": i}})
			errs <- err
		}(i)
		go func(i int) {
			defer wg.Done()
			_, err := store.appendAISessionOutput("AIS-conc", []aiSessionOutputChunk{{CursorStart: uint64(i), Text: "z"}}, 1<<20)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if kinds := eventKinds(t, store, "AIS-conc"); len(kinds) != n+1 {
		t.Fatalf("events = %d, want %d", len(kinds), n+1)
	}
	chunks, _ := store.loadAISessionOutput("AIS-conc", 0, 0)
	if len(chunks) != n {
		t.Fatalf("chunks = %d", len(chunks))
	}
	row, _, _ := store.loadAISessionRow("AIS-conc")
	if row.OutputBytes != n || row.StoredOutputBytes != n {
		t.Fatalf("counters = %+v", row)
	}
}

func TestAIStoreTruncateUTF8(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"}, {"abc", 2, "ab"}, {"日本", 4, "日"}, {"日本", 3, "日"}, {"日本", 2, ""}, {"x", 0, ""}, {"x", -1, ""},
	} {
		if got := aiStoreTruncateUTF8(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
