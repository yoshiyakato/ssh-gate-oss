package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "ssh-gete.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, path string, table string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// makeV2Database writes a database shaped like the v2 release: no
// agent_tokens, no serial/bypass-scope columns, one never-expiring grant.
func makeV2Database(t *testing.T, legacyUpdated string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh-gete.sqlite")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE agent_tokens`,
		`DROP INDEX idx_command_requests_client`,
		`ALTER TABLE command_requests DROP COLUMN expires_at`,
		`ALTER TABLE connections DROP COLUMN data_bits`,
		`ALTER TABLE connections DROP COLUMN parity`,
		`ALTER TABLE connections DROP COLUMN stop_bits`,
		`ALTER TABLE agent_policies DROP COLUMN max_risk`,
		`ALTER TABLE agent_policies DROP COLUMN scope_conn_ids`,
		`ALTER TABLE agent_policies DROP COLUMN granted_at`,
		`ALTER TABLE agent_policies DROP COLUMN granted_by`,
		`ALTER TABLE agent_policies DROP COLUMN expires_at`,
		fmt.Sprintf(`INSERT INTO agent_policies (agent_name, approval_bypass, updated_at) VALUES ('legacy', 1, '%s')`, legacyUpdated),
		`INSERT INTO agent_policies (agent_name, approval_bypass, updated_at) VALUES ('stale', 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO agent_policies (agent_name, approval_bypass, updated_at) VALUES ('off', 0, '2026-01-01T00:00:00Z')`,
		`INSERT INTO connections (id, name, type, host, port, user, auth_method, credential, serial_port, baud_rate,
			connect_timeout, command_timeout, tags_json, description, status, last_checked, updated_at)
			VALUES ('c-serial', 'console', 'Serial', '', 0, '', '', '', '/dev/cu.usbserial', 115200, 10, 30, '[]', '', '', '', '')`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	store.Close()
	return path
}

func TestMigrationV2ToV3(t *testing.T) {
	legacyUpdated := time.Now().Add(-time.Hour).Truncate(time.Second)
	path := makeV2Database(t, legacyUpdated.Format(time.RFC3339))
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if store.prevVersion != 2 {
		t.Fatalf("prevVersion = %d", store.prevVersion)
	}
	if v := userVersion(t, path); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	backup := path + ".bak-v2"
	if !fileExists(backup) {
		t.Fatalf("pre-migration backup missing")
	}
	if userVersion(t, backup) != 2 || tableExists(t, backup, "agent_tokens") {
		t.Fatalf("backup is not the untouched v2 database")
	}

	// A legacy grant runs defaultBypassTTL from its updated_at.
	legacy, ok, err := store.loadAgentPolicy("legacy")
	if err != nil || !ok {
		t.Fatalf("legacy grant: ok=%v err=%v", ok, err)
	}
	wantExpiry := legacyUpdated.Add(defaultBypassTTL).Format(time.RFC3339)
	if !legacy.Active || legacy.ExpiresAt != wantExpiry || legacy.GrantedAt != legacyUpdated.Format(time.RFC3339) {
		t.Fatalf("legacy grant lifetime: %+v (want expiry %s)", legacy, wantExpiry)
	}
	if legacy.MaxRisk != defaultBypassMaxRisk || len(legacy.ScopeConnIDs) != 0 || legacy.HasToken {
		t.Fatalf("legacy grant defaults: %+v", legacy)
	}
	// A grant older than the TTL does not survive the upgrade as active.
	if stale, _, _ := store.loadAgentPolicy("stale"); stale.Active {
		t.Fatalf("stale legacy grant is active after upgrade: %+v", stale)
	}
	if off, _, _ := store.loadAgentPolicy("off"); off.ApprovalBypass || off.Active || off.ExpiresAt != "" {
		t.Fatalf("disabled grant changed: %+v", off)
	}

	conns, err := store.loadConnections()
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections: %+v err=%v", conns, err)
	}
	if c := conns[0]; c.DataBits != 8 || c.Parity != "none" || c.StopBits != "1" {
		t.Fatalf("serial defaults: %+v", c)
	}

	// Reopening a v3 database neither migrates nor backs up again.
	store.Close()
	store2, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	if store2.prevVersion != schemaVersion || fileExists(fmt.Sprintf("%s.bak-v%d", path, schemaVersion)) {
		t.Fatalf("v%d reopen migrated again", schemaVersion)
	}
}

// A stale or partial .bak-v2 left from an earlier run must not stand in for
// the backup of this migration: it is kept, and a fresh complete copy is
// written next to it. No temporary file is left behind.
func TestMigrationBackupNotSkippedByExistingFile(t *testing.T) {
	path := makeV2Database(t, time.Now().Format(time.RFC3339))
	stale := path + ".bak-v2"
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if data, _ := os.ReadFile(stale); string(data) != "partial" {
		t.Fatalf("existing backup was overwritten")
	}
	matches, err := filepath.Glob(path + ".bak-v2-*")
	if err != nil {
		t.Fatal(err)
	}
	var backups []string
	for _, m := range matches {
		if strings.Contains(m, ".tmp-") {
			t.Fatalf("temporary backup left behind: %s", m)
		}
		backups = append(backups, m)
	}
	if len(backups) != 1 {
		t.Fatalf("timestamped backups = %v, want one", backups)
	}
	if userVersion(t, backups[0]) != 2 || tableExists(t, backups[0], "agent_tokens") {
		t.Fatalf("new backup is not the untouched v2 database")
	}
}

func TestOpenStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-gete.sqlite")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := openStore(path); err == nil {
		t.Fatalf("newer schema was opened for writing")
	}
}

func TestAgentPolicyLifecycle(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	// Enabling with no times fills GrantedAt from UpdatedAt and the 8h expiry.
	if err := store.saveAgentPolicy(AgentPolicy{AgentName: " codex ", ApprovalBypass: true, GrantedBy: "gui",
		UpdatedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	codex, ok, err := store.loadAgentPolicy("codex")
	if err != nil || !ok {
		t.Fatalf("load codex: %v %v", ok, err)
	}
	if !codex.Active || codex.MaxRisk != "Medium" || codex.GrantedBy != "gui" ||
		codex.ExpiresAt != now.Add(defaultBypassTTL).Format(time.RFC3339) || codex.ScopeConnIDs == nil {
		t.Fatalf("defaults not applied: %+v", codex)
	}
	// Explicit fields are kept as given (High is clamped to Medium).
	scoped := AgentPolicy{AgentName: "scoped", ApprovalBypass: true, MaxRisk: "Low", ScopeConnIDs: []string{"c1"},
		GrantedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	if err := store.saveAgentPolicy(scoped); err != nil {
		t.Fatal(err)
	}
	if err := store.saveAgentPolicy(AgentPolicy{AgentName: "greedy", ApprovalBypass: true, MaxRisk: "High"}); err != nil {
		t.Fatal(err)
	}
	if g, _, _ := store.loadAgentPolicy("greedy"); g.MaxRisk != "Medium" {
		t.Fatalf("High max_risk stored: %+v", g)
	}
	if got, _, _ := store.loadAgentPolicy("scoped"); got.MaxRisk != "Low" || len(got.ScopeConnIDs) != 1 || got.ScopeConnIDs[0] != "c1" {
		t.Fatalf("scoped round trip: %+v", got)
	}
	if err := store.saveAgentPolicy(AgentPolicy{AgentName: "  ", ApprovalBypass: true}); err == nil {
		t.Fatalf("empty agent name accepted")
	}

	active, err := store.loadActiveAgentPolicies(now)
	if err != nil || len(active) != 3 {
		t.Fatalf("active grants: %+v err=%v", active, err)
	}
	// After an hour the scoped grant has lapsed but codex has not.
	later := now.Add(time.Hour + time.Second)
	active, _ = store.loadActiveAgentPolicies(later)
	if len(active) != 2 || active[0].AgentName != "codex" {
		t.Fatalf("expiry not applied to the list: %+v", active)
	}
	names, err := store.expireAgentPolicies(later)
	if err != nil || len(names) != 1 || names[0] != "scoped" {
		t.Fatalf("expireAgentPolicies = %v, %v", names, err)
	}
	if names, _ := store.expireAgentPolicies(later); len(names) != 0 {
		t.Fatalf("second expire returned %v", names)
	}

	revoked, err := store.revokeAgentPolicy("codex", later)
	if err != nil || !revoked {
		t.Fatalf("revoke codex: %v %v", revoked, err)
	}
	if revoked, _ := store.revokeAgentPolicy("codex", later); revoked {
		t.Fatalf("double revoke reported success")
	}
	if revoked, _ := store.revokeAgentPolicy("nobody", later); revoked {
		t.Fatalf("revoking an unknown agent reported success")
	}
	if active, _ := store.loadActiveAgentPolicies(later); len(active) != 1 || active[0].AgentName != "greedy" {
		t.Fatalf("revoked grant still listed: %+v", active)
	}
	// Disabling through saveAgentPolicy clears the expiry; the row stays.
	if err := store.saveAgentPolicy(AgentPolicy{AgentName: "greedy", ApprovalBypass: false}); err != nil {
		t.Fatal(err)
	}
	all, _ := store.loadAgentPolicies()
	if len(all) != 3 {
		t.Fatalf("rows lost: %+v", all)
	}
	for _, p := range all {
		if p.Active {
			t.Fatalf("%s still active: %+v", p.AgentName, p)
		}
	}
}

func TestPolicyAllows(t *testing.T) {
	now := time.Now()
	live := now.Add(time.Hour).Format(time.RFC3339)
	past := now.Add(-time.Second).Format(time.RFC3339)
	on := func(maxRisk string, scope ...string) AgentPolicy {
		return AgentPolicy{ApprovalBypass: true, MaxRisk: maxRisk, ScopeConnIDs: scope, ExpiresAt: live}
	}
	cases := []struct {
		name   string
		policy AgentPolicy
		risk   string
		connID string
		want   bool
	}{
		{"medium allows low", on("Medium"), "Low", "c1", true},
		{"medium allows medium", on("Medium"), "Medium", "c1", true},
		{"empty max risk is medium", on(""), "Medium", "c1", true},
		{"high never", on("High"), "High", "c1", false},
		{"low cap rejects medium", on("Low"), "Medium", "c1", false},
		{"unknown risk", on("Medium"), "", "c1", false},
		{"expired", AgentPolicy{ApprovalBypass: true, ExpiresAt: past}, "Low", "c1", false},
		{"no expiry", AgentPolicy{ApprovalBypass: true}, "Low", "c1", false},
		{"off", AgentPolicy{ApprovalBypass: false, ExpiresAt: live}, "Low", "c1", false},
		{"in scope", on("Medium", "c1", "c2"), "Low", "c2", true},
		{"out of scope", on("Medium", "c1"), "Low", "c3", false},
	}
	for _, tc := range cases {
		if got := policyAllows(tc.policy, tc.risk, tc.connID, now); got != tc.want {
			t.Errorf("%s: policyAllows = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAgentTokens(t *testing.T) {
	store := newTestStore(t)
	token, row, err := newAgentToken("codex", true)
	if err != nil {
		t.Fatal(err)
	}
	if row.TokenHash != hashAgentToken(token) || row.Preview != agentTokenPrefix+"…"+token[len(token)-4:] {
		t.Fatalf("token row: %+v", row)
	}
	if err := store.saveAgentToken(row); err != nil {
		t.Fatal(err)
	}
	var plain int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM agent_tokens WHERE token_hash = ? OR preview = ?`, token, token).Scan(&plain); err != nil || plain != 0 {
		t.Fatalf("plaintext token stored")
	}
	got, ok, err := store.loadAgentTokenByHash(hashAgentToken(token))
	if err != nil || !ok || got.AgentName != "codex" || !got.Bypass || got.CreatedAt == "" {
		t.Fatalf("lookup = %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := store.loadAgentTokenByHash(hashAgentToken(token + "x")); ok {
		t.Fatalf("wrong token accepted")
	}
	if p, _, _ := store.loadAgentPolicy("codex"); p.HasToken {
		t.Fatalf("HasToken without a policy row")
	}
	if err := store.saveAgentPolicy(AgentPolicy{AgentName: "codex", ApprovalBypass: true}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := store.loadAgentPolicy("codex"); !p.HasToken {
		t.Fatalf("HasToken not derived: %+v", p)
	}

	// Rotation replaces the hash, so the old token stops resolving.
	rotated, rotatedRow, _ := newAgentToken("codex", false)
	if err := store.saveAgentToken(rotatedRow); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.loadAgentTokenByHash(hashAgentToken(token)); ok {
		t.Fatalf("old token still valid after rotation")
	}
	if got, ok, _ := store.loadAgentTokenByHash(hashAgentToken(rotated)); !ok || got.Bypass {
		t.Fatalf("rotated token: %+v ok=%v", got, ok)
	}
	if ok, _ := store.setAgentTokenBypass("codex", true); !ok {
		t.Fatalf("setAgentTokenBypass missed")
	}
	if ok, _ := store.setAgentTokenBypass("nobody", true); ok {
		t.Fatalf("setAgentTokenBypass on unknown name")
	}
	tokens, err := store.loadAgentTokens()
	if err != nil || len(tokens) != 1 || !tokens[0].Bypass || tokens[0].TokenHash != rotatedRow.TokenHash {
		t.Fatalf("tokens = %+v err=%v", tokens, err)
	}
	if ok, _ := store.deleteAgentToken("codex"); !ok {
		t.Fatalf("delete missed")
	}
	if ok, _ := store.deleteAgentToken("codex"); ok {
		t.Fatalf("double delete reported success")
	}
	if _, ok, _ := store.loadAgentTokenByHash(hashAgentToken(rotated)); ok {
		t.Fatalf("deleted token still valid")
	}
	if _, _, err := newAgentToken(" ", false); err == nil {
		t.Fatalf("empty agent name accepted")
	}
	if err := store.saveAgentToken(AgentToken{AgentName: "x"}); err == nil {
		t.Fatalf("token without hash accepted")
	}
}

func TestSerialColumnsPersist(t *testing.T) {
	store := newTestStore(t)
	serial := Connection{ID: "c-serial", Name: "console", Type: "Serial", SerialPort: "/dev/cu.usbserial", BaudRate: 9600,
		DataBits: 7, Parity: "odd", StopBits: "1.5", Tags: []string{}}
	ssh := Connection{ID: "c-ssh", Name: "web", Type: "SSH", Host: "10.0.0.1", Port: 22, User: "u", Tags: []string{}}
	for _, c := range []Connection{serial, ssh} {
		if err := store.insertConnection(c); err != nil {
			t.Fatal(err)
		}
	}
	serial.BaudRate, serial.StopBits = 115200, "2"
	if ok, err := store.updateConnection(serial); err != nil || !ok {
		t.Fatalf("updateConnection: %v %v", ok, err)
	}
	conns, err := store.loadConnections()
	if err != nil || len(conns) != 2 {
		t.Fatalf("conns = %+v err=%v", conns, err)
	}
	byID := map[string]Connection{}
	for _, c := range conns {
		byID[c.ID] = c
	}
	if c := byID["c-serial"]; c.BaudRate != 115200 || c.DataBits != 7 || c.Parity != "odd" || c.StopBits != "2" {
		t.Fatalf("serial params: %+v", c)
	}
	if c := byID["c-ssh"]; c.DataBits != 0 || c.Parity != "" || c.StopBits != "" {
		t.Fatalf("ssh row got serial params: %+v", c)
	}
}

func TestRequestExpiresAtPersists(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	req := testRequest("1", "codex", "", now)
	req.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339)
	if err := store.insertCommandRequest(req); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.loadCommandRequest(req.ID)
	if got.ExpiresAt != req.ExpiresAt {
		t.Fatalf("expires_at = %q, want %q", got.ExpiresAt, req.ExpiresAt)
	}
	got.Status = stateExpired
	if err := store.updateCommandRequestFrom(got, statePending); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := store.loadCommandRequest(req.ID); again.Status != stateExpired || again.ExpiresAt != req.ExpiresAt {
		t.Fatalf("after transition: %+v", again)
	}
}

func testRequest(id string, agent string, clientID string, createdAt time.Time) CommandRequest {
	return CommandRequest{
		ID:              fmt.Sprintf("REQ-%d-%s", createdAt.UnixMilli(), id),
		ConnectionID:    "c1",
		Host:            "web",
		User:            "u",
		Command:         "uptime",
		RequestedBy:     agent,
		RequestedAt:     createdAt.Format(time.RFC3339),
		Risk:            "Low",
		Status:          statePending,
		Stdout:          "out-" + id,
		Stderr:          "err-" + id,
		ClientRequestID: clientID,
	}
}

// TestLoadRequestByClientID: the SQLite half of client_request_id dedup
// (execute_command serializes on the key and checks memory first; the
// concurrent behaviour is TestConcurrentClientRequestIDRunsOnce).
func TestLoadRequestByClientID(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	first := testRequest("1", "codex", "cr-1", now)
	for _, req := range []CommandRequest{first, testRequest("3", "other", "cr-1", now), testRequest("4", "codex", "", now)} {
		if err := store.insertCommandRequest(req); err != nil {
			t.Fatal(err)
		}
	}
	found, ok, err := store.loadRequestByClientID("codex", "cr-1", now)
	if err != nil || !ok || found.ID != first.ID {
		t.Fatalf("find: %+v ok=%v err=%v", found, ok, err)
	}
	if _, ok, _ := store.loadRequestByClientID("codex", "", now); ok {
		t.Fatalf("empty client id matched")
	}
	if found, ok, _ := store.loadRequestByClientID("other", "cr-1", now); !ok || found.ID == first.ID {
		t.Fatalf("client id was shared across agents: %+v", found)
	}
	// After 24 hours the id may be reused.
	later := now.Add(clientRequestIDWindow + time.Minute)
	if _, ok, _ := store.loadRequestByClientID("codex", "cr-1", later); ok {
		t.Fatalf("request older than 24h matched")
	}
}

func TestLoadCommandHistory(t *testing.T) {
	store := newTestStore(t)
	base := time.Now().Add(-time.Hour)
	for i := range 30 {
		req := testRequest(fmt.Sprint(i), "codex", "", base.Add(time.Duration(i)*time.Second))
		if i%3 == 0 {
			req.RequestedBy = "other"
		}
		if i%2 == 0 {
			req.Status = stateSucceeded
		}
		if i == 29 {
			// A request whose connection was deleted keeps only the host name.
			req.ConnectionID = ""
			req.Host = "gone"
		}
		if err := store.insertCommandRequest(req); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.loadCommandHistory(historyFilter{})
	if err != nil || len(all) != defaultHistoryLimit {
		t.Fatalf("default limit: %d err=%v", len(all), err)
	}
	if all[0].Host != "gone" || all[0].Stdout != "" || all[0].Stderr != "" {
		t.Fatalf("history must be newest first without output: %+v", all[0])
	}
	if big, _ := store.loadCommandHistory(historyFilter{Limit: 10_000}); len(big) != 30 {
		t.Fatalf("limit cap: %d", len(big))
	}
	mine, _ := store.loadCommandHistory(historyFilter{RequestedBy: "codex", Limit: 500})
	for _, r := range mine {
		if r.RequestedBy != "codex" {
			t.Fatalf("agent filter leaked %+v", r)
		}
	}
	if len(mine) != 20 {
		t.Fatalf("agent filter count: %d", len(mine))
	}
	ok, _ := store.loadCommandHistory(historyFilter{State: stateSucceeded, Limit: 500})
	if len(ok) != 15 {
		t.Fatalf("state filter count: %d", len(ok))
	}
	byConn, _ := store.loadCommandHistory(historyFilter{ConnectionID: "c1", HostName: "web", Limit: 500})
	if len(byConn) != 29 {
		t.Fatalf("connection filter count: %d", len(byConn))
	}
	byName, _ := store.loadCommandHistory(historyFilter{HostName: "gone", Limit: 500})
	if len(byName) != 1 || byName[0].Host != "gone" {
		t.Fatalf("host name filter: %+v", byName)
	}

	one, found, err := store.loadCommandRequest(all[0].ID)
	if err != nil || !found || one.Stdout != "out-29" {
		t.Fatalf("loadCommandRequest keeps output: %+v found=%v err=%v", one, found, err)
	}
	if _, found, _ := store.loadCommandRequest("REQ-missing"); found {
		t.Fatalf("missing request found")
	}
}

// --- OS keychain (phase5-keychain.md) -------------------------------------

const (
	authPassword = "パスワード保管"
	authKey      = "SSH鍵認証"
)

var plaintextPasswords = []string{"pw-alpha-7c1e", "pw-bravo-93fd", "pw-charlie-0b2a"}

// makeV3Database writes a v3-shaped database (no credential_store /
// passphrase_saved / secret_gc) holding three plaintext passwords, one key
// connection and one serial connection.
func makeV3Database(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh-gete.sqlite")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, pw := range plaintextPasswords {
		conn := Connection{ID: fmt.Sprintf("c%d", i+1), Name: fmt.Sprintf("srv%d", i+1), Type: "SSH", Host: "10.0.0.1",
			Port: 22, User: "u", AuthMethod: authPassword, Credential: pw, ConnectTimeout: 10, CommandTimeout: 30}
		if err := store.insertConnection(conn); err != nil {
			t.Fatal(err)
		}
	}
	for _, conn := range []Connection{
		{ID: "k1", Name: "keyed", Type: "SSH", Host: "10.0.0.2", Port: 22, User: "u", AuthMethod: authKey, Credential: "~/.ssh/id_ed25519"},
		{ID: "s1", Name: "serial", Type: "Serial", SerialPort: "/dev/cu.usbserial", BaudRate: 9600},
	} {
		if err := store.insertConnection(conn); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`DROP TABLE secret_gc`,
		`ALTER TABLE connections DROP COLUMN credential_store`,
		`ALTER TABLE connections DROP COLUMN passphrase_saved`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	store.Close()
	return path
}

func credentialRow(t *testing.T, store *Store, id string) (credential, credStore string) {
	t.Helper()
	if err := store.db.QueryRow(`SELECT credential, credential_store FROM connections WHERE id = ?`, id).Scan(&credential, &credStore); err != nil {
		t.Fatal(err)
	}
	return credential, credStore
}

func secretGCAccounts(t *testing.T, store *Store) []string {
	t.Helper()
	accounts, err := store.loadSecretGC()
	if err != nil {
		t.Fatal(err)
	}
	return accounts
}

func fileContainsAny(t *testing.T, path string, needles []string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range needles {
		if strings.Contains(string(data), n) {
			return n
		}
	}
	return ""
}

func TestMigrationV3ToV4KeepsCredentialsUntilMoved(t *testing.T) {
	path := makeV3Database(t)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.prevVersion != 3 || userVersion(t, path) != schemaVersion || !fileExists(path+".bak-v3") {
		t.Fatalf("v3 -> v4 migration: prev=%d version=%d", store.prevVersion, userVersion(t, path))
	}
	if !tableExists(t, path, "secret_gc") {
		t.Fatalf("secret_gc missing")
	}
	// The schema step alone moves nothing: credential_store defaults to db.
	for i, pw := range plaintextPasswords {
		if cred, cs := credentialRow(t, store, fmt.Sprintf("c%d", i+1)); cred != pw || cs != credentialStoreDB {
			t.Fatalf("c%d after schema migration: %q %q", i+1, cred, cs)
		}
	}
	var secureDelete int
	if err := store.db.QueryRow(`PRAGMA secure_delete`).Scan(&secureDelete); err != nil || secureDelete != 1 {
		t.Fatalf("secure_delete = %d, %v", secureDelete, err)
	}
}

func TestMigrateCredentialsToKeyring(t *testing.T) {
	path := makeV3Database(t)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets := newMemorySecretStore()
	ctx := context.Background()

	res, err := store.migrateCredentialsToKeyring(ctx, secrets, time.Second)
	if err != nil || res.Moved != 3 || res.Remaining != 0 || res.Err != nil {
		t.Fatalf("migrate = %+v, %v", res, err)
	}
	for i, pw := range plaintextPasswords {
		id := fmt.Sprintf("c%d", i+1)
		if cred, cs := credentialRow(t, store, id); cred != "" || cs != credentialStoreKeyring {
			t.Fatalf("%s row: %q %q", id, cred, cs)
		}
		if v, err := secrets.Get(ctx, passwordAccount(id)); err != nil || v != pw {
			t.Fatalf("%s secret = %q, %v", id, v, err)
		}
	}
	if cred, cs := credentialRow(t, store, "k1"); cred != "~/.ssh/id_ed25519" || cs != credentialStoreDB {
		t.Fatalf("key path touched: %q %q", cred, cs)
	}

	conns, err := store.loadConnections()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		switch c.ID {
		case "c1", "c2", "c3":
			if c.Credential != "" || !c.CredentialSet || !credentialInKeyring(c) {
				t.Fatalf("loaded keychain row: %+v", c)
			}
		case "k1":
			if c.Credential == "" || credentialInKeyring(c) {
				t.Fatalf("loaded key row: %+v", c)
			}
		}
	}

	// No plaintext is left in the database file or its WAL.
	for _, p := range []string{path, path + "-wal"} {
		if hit := fileContainsAny(t, p, plaintextPasswords); hit != "" {
			t.Fatalf("%s still contains %q", filepath.Base(p), hit)
		}
	}
	// ...but the pre-migration backup does, and is reported.
	found, err := findPlaintextBackups(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Path != path+".bak-v3" || found[0].Passwords != 3 || found[0].Unreadable {
		t.Fatalf("plaintext backups = %+v", found)
	}

	// Running again is a no-op.
	if res, err := store.migrateCredentialsToKeyring(ctx, secrets, time.Second); err != nil || res.Moved != 0 || res.Remaining != 0 {
		t.Fatalf("second run = %+v, %v", res, err)
	}
}

// Only rows whose keychain write was verified lose their plaintext; the rest
// stay in the column and the next run finishes the job.
func TestMigrateCredentialsPartialFailure(t *testing.T) {
	path := makeV3Database(t)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	inner := newMemorySecretStore()
	f := newFaultSecretStore(inner)
	f.hook = func(op, _ string, n int) error {
		if op == "set" && n == 2 {
			return fmt.Errorf("%w: unlock refused", ErrSecretUnavailable)
		}
		return nil
	}
	res, err := store.migrateCredentialsToKeyring(ctx, f, time.Second)
	if err != nil || res.Moved != 1 || res.Remaining != 2 || !errors.Is(res.Err, ErrSecretUnavailable) {
		t.Fatalf("migrate = %+v, %v", res, err)
	}
	if f.count("set") != 2 {
		t.Fatalf("set calls = %d: an unavailable store must stop the run", f.count("set"))
	}
	if cred, cs := credentialRow(t, store, "c1"); cred != "" || cs != credentialStoreKeyring {
		t.Fatalf("c1 not moved: %q %q", cred, cs)
	}
	for i := 2; i <= 3; i++ {
		id := fmt.Sprintf("c%d", i)
		if cred, cs := credentialRow(t, store, id); cred != plaintextPasswords[i-1] || cs != credentialStoreDB {
			t.Fatalf("%s lost its value: %q %q", id, cred, cs)
		}
	}

	// A value too big for this row only skips that row.
	f.hook = func(op, account string, _ int) error {
		if op == "set" && account == passwordAccount("c2") {
			return ErrSecretTooBig
		}
		return nil
	}
	res, err = store.migrateCredentialsToKeyring(ctx, f, time.Second)
	if err != nil || res.Moved != 1 || res.Remaining != 1 || !errors.Is(res.Err, ErrSecretTooBig) {
		t.Fatalf("second run = %+v, %v", res, err)
	}
	f.hook = nil
	res, err = store.migrateCredentialsToKeyring(ctx, f, time.Second)
	if err != nil || res.Moved != 1 || res.Remaining != 0 {
		t.Fatalf("third run = %+v, %v", res, err)
	}
}

// A store that times out (Linux prompt never answered) fails the run at the
// per-item deadline and keeps every plaintext row.
func TestMigrateCredentialsHungStore(t *testing.T) {
	path := makeV3Database(t)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f := newFaultSecretStore(newMemorySecretStore())
	f.hang.Store(true)
	start := time.Now()
	res, err := store.migrateCredentialsToKeyring(context.Background(), f, 50*time.Millisecond)
	if err != nil || res.Moved != 0 || res.Remaining != 3 || !errors.Is(res.Err, ErrSecretUnavailable) {
		t.Fatalf("migrate = %+v, %v", res, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("hung store held the migration for %v", time.Since(start))
	}
}

// A mangled read-back keeps the plaintext; a crash after Set (item written,
// row not yet cleared) is finished by the next run without duplicates.
func TestMigrateCredentialsReadBackAndRetry(t *testing.T) {
	path := makeV3Database(t)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	res, err := store.migrateCredentialsToKeyring(ctx, mangleStore{newMemorySecretStore()}, time.Second)
	if err != nil || res.Moved != 0 || res.Remaining != 3 {
		t.Fatalf("mangled = %+v, %v", res, err)
	}
	secrets := newMemorySecretStore()
	secrets.Set(ctx, passwordAccount("c1"), plaintextPasswords[0]) // written before a crash
	res, err = store.migrateCredentialsToKeyring(ctx, secrets, time.Second)
	if err != nil || res.Moved != 3 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if v, _ := secrets.Get(ctx, passwordAccount("c1")); v != plaintextPasswords[0] {
		t.Fatalf("c1 = %q", v)
	}
}

func TestKeyringConnectionRoundTripAndGC(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	secrets := newMemorySecretStore()
	conn := Connection{ID: "c1", Name: "srv", Type: "SSH", Host: "h", Port: 22, User: "u",
		AuthMethod: authPassword, CredentialSet: true}
	secrets.Set(ctx, passwordAccount("c1"), "pw")
	if err := store.insertConnection(conn); err != nil {
		t.Fatal(err)
	}
	if cred, cs := credentialRow(t, store, "c1"); cred != "" || cs != credentialStoreKeyring {
		t.Fatalf("insert: %q %q", cred, cs)
	}

	// Switching to key auth queues the password item; GC deletes it.
	conn.AuthMethod, conn.Credential, conn.CredentialSet = authKey, "~/.ssh/id_ed25519", false
	if ok, err := store.updateConnection(conn); !ok || err != nil {
		t.Fatalf("update: %v %v", ok, err)
	}
	if got := secretGCAccounts(t, store); len(got) != 1 || got[0] != passwordAccount("c1") {
		t.Fatalf("queued = %v", got)
	}
	if n, err := store.runSecretGC(ctx, secrets); n != 1 || err != nil {
		t.Fatalf("gc = %d, %v", n, err)
	}
	if _, err := secrets.Get(ctx, passwordAccount("c1")); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("password item survived gc")
	}
	if got := secretGCAccounts(t, store); len(got) != 0 {
		t.Fatalf("queue not drained: %v", got)
	}

	// Back to password before GC runs: the queued account is live again and
	// must not be deleted.
	conn.AuthMethod, conn.Credential, conn.CredentialSet = authPassword, "", true
	store.enqueueSecretGC(passwordAccount("c1"))
	secrets.Set(ctx, passwordAccount("c1"), "pw2")
	if ok, err := store.updateConnection(conn); !ok || err != nil {
		t.Fatalf("update: %v %v", ok, err)
	}
	if got := secretGCAccounts(t, store); len(got) != 0 {
		t.Fatalf("live account still queued: %v", got)
	}
	store.enqueueSecretGC(passwordAccount("c1")) // a stale entry from a race
	if n, _ := store.runSecretGC(ctx, secrets); n != 0 {
		t.Fatalf("gc deleted a live item")
	}
	if v, _ := secrets.Get(ctx, passwordAccount("c1")); v != "pw2" {
		t.Fatalf("live item = %q", v)
	}

	// Deleting the connection queues its item in the same transaction. With
	// the store down the entry stays queued for the next run.
	if err := store.deleteConnection("c1"); err != nil {
		t.Fatal(err)
	}
	down := newFaultSecretStore(secrets)
	down.hook = func(string, string, int) error { return ErrSecretUnavailable }
	if n, err := store.runSecretGC(ctx, down); n != 0 || !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("gc with store down = %d, %v", n, err)
	}
	if got := secretGCAccounts(t, store); len(got) != 1 {
		t.Fatalf("failed delete dropped from queue: %v", got)
	}
	if n, err := store.runSecretGC(ctx, secrets); n != 1 || err != nil {
		t.Fatalf("gc retry = %d, %v", n, err)
	}
	if _, err := secrets.Get(ctx, passwordAccount("c1")); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("deleted connection's item survived")
	}
}

func TestPassphraseSavedLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	secrets := newMemorySecretStore()
	conn := Connection{ID: "k1", Name: "keyed", Type: "SSH", Host: "h", Port: 22, User: "u",
		AuthMethod: authKey, Credential: "~/.ssh/id_a"}
	if err := store.insertConnection(conn); err != nil {
		t.Fatal(err)
	}
	if ids, _ := store.passphraseSavedIDs(); len(ids) != 0 {
		t.Fatalf("passphrase saved by default: %v", ids)
	}
	// A stale key path is refused.
	if ok, err := store.setPassphraseSaved("k1", "~/.ssh/other", true); ok || err != nil {
		t.Fatalf("stale path: %v %v", ok, err)
	}
	accountA := passphraseAccount("k1", "~/.ssh/id_a")
	secrets.Set(ctx, accountA, "phrase")
	if ok, err := store.setPassphraseSaved("k1", "~/.ssh/id_a", true); !ok || err != nil {
		t.Fatalf("save: %v %v", ok, err)
	}
	if ids, _ := store.passphraseSavedIDs(); !ids["k1"] {
		t.Fatalf("not marked saved")
	}
	// Renaming keeps it; changing the key path drops it and queues the item.
	conn.Name = "renamed"
	store.updateConnection(conn)
	if ids, _ := store.passphraseSavedIDs(); !ids["k1"] {
		t.Fatalf("rename dropped the saved passphrase")
	}
	conn.Credential = "~/.ssh/id_b"
	store.updateConnection(conn)
	if ids, _ := store.passphraseSavedIDs(); ids["k1"] {
		t.Fatalf("key path change kept passphrase_saved")
	}
	if got := secretGCAccounts(t, store); len(got) != 1 || got[0] != accountA {
		t.Fatalf("queued = %v", got)
	}
	store.runSecretGC(ctx, secrets)
	if _, err := secrets.Get(ctx, accountA); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("old passphrase item survived")
	}

	// Turning it off queues the item; deleting the row queues it too.
	accountB := passphraseAccount("k1", "~/.ssh/id_b")
	secrets.Set(ctx, accountB, "phrase-b")
	store.setPassphraseSaved("k1", "~/.ssh/id_b", true)
	if err := store.deleteConnection("k1"); err != nil {
		t.Fatal(err)
	}
	if got := secretGCAccounts(t, store); len(got) != 1 || got[0] != accountB {
		t.Fatalf("queued after delete = %v", got)
	}
}

func TestDenyPlaintextPasswords(t *testing.T) {
	store := newTestStore(t)
	legacy := Connection{ID: "c1", Name: "legacy", Type: "SSH", Host: "h", Port: 22, User: "u",
		AuthMethod: authPassword, Credential: "old-plain"}
	if err := store.insertConnection(legacy); err != nil {
		t.Fatal(err)
	}
	store.denyPlaintextPasswords.Store(true)

	fresh := legacy
	fresh.ID, fresh.Name, fresh.Credential = "c2", "fresh", "new-plain"
	if err := store.insertConnection(fresh); !errors.Is(err, errPlaintextPassword) {
		t.Fatalf("insert plaintext: %v", err)
	}
	// An unchanged legacy value may stay (not migrated yet)...
	legacy.Description = "edited"
	if ok, err := store.updateConnection(legacy); !ok || err != nil {
		t.Fatalf("unchanged legacy: %v %v", ok, err)
	}
	// ...a new one may not.
	legacy.Credential = "changed-plain"
	if ok, err := store.updateConnection(legacy); ok || !errors.Is(err, errPlaintextPassword) {
		t.Fatalf("changed plaintext: %v %v", ok, err)
	}
	if cred, _ := credentialRow(t, store, "c1"); cred != "old-plain" {
		t.Fatalf("refused update wrote %q", cred)
	}
	// Keychain-backed and key connections are unaffected.
	fresh.Credential, fresh.CredentialSet = "", true
	if err := store.insertConnection(fresh); err != nil {
		t.Fatalf("keychain insert: %v", err)
	}
	key := Connection{ID: "k1", Name: "key", Type: "SSH", Host: "h", Port: 22, User: "u", AuthMethod: authKey, Credential: "~/.ssh/id"}
	if err := store.insertConnection(key); err != nil {
		t.Fatalf("key insert: %v", err)
	}
}

func TestFindAndRemovePlaintextBackups(t *testing.T) {
	path := makeV3Database(t)
	store, err := openStore(path) // writes .bak-v3 with three passwords
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.migrateCredentialsToKeyring(context.Background(), newMemorySecretStore(), time.Second); err != nil {
		t.Fatal(err)
	}
	// A clean backup (taken after the move) is not reported.
	if _, err := store.db.Exec(`VACUUM INTO ?`, path+".bak-v4"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	dir := filepath.Dir(path)
	partial := path + ".bak-v2"
	os.WriteFile(partial, []byte("partial"), 0o600)
	os.WriteFile(path+".bak-v3.tmp-1", []byte("x"), 0o600)
	legacy := filepath.Join(dir, "connections.json.migrated")
	os.WriteFile(legacy, []byte(`[{"name":"a","authMethod":"パスワード保管","credential":"json-pw"},{"name":"b","authMethod":"SSH鍵認証","credential":"~/.ssh/id"}]`), 0o600)
	other := filepath.Join(dir, "unrelated.txt")
	os.WriteFile(other, []byte("keep"), 0o600)

	found, err := findPlaintextBackups(path, legacy, filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PlaintextBackup{}
	for _, b := range found {
		got[filepath.Base(b.Path)] = b
	}
	// A leftover .tmp- (VACUUM INTO cut off) is reported as unreadable.
	if len(got) != 4 || got["ssh-gete.sqlite.bak-v3"].Passwords != 3 || !got["ssh-gete.sqlite.bak-v2"].Unreadable ||
		!got["ssh-gete.sqlite.bak-v3.tmp-1"].Unreadable || got["connections.json.migrated"].Passwords != 1 {
		t.Fatalf("found = %+v", found)
	}

	removed, err := removePlaintextBackups(path, []string{path + ".bak-v3", path + ".bak-v3.tmp-1", other, path, path + ".bak-v4"}, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != path+".bak-v3" || removed[1] != path+".bak-v3.tmp-1" {
		t.Fatalf("removed = %v", removed)
	}
	for _, keep := range []string{other, path, path + ".bak-v4", legacy, partial} {
		if !fileExists(keep) {
			t.Fatalf("%s was removed", filepath.Base(keep))
		}
	}
	if fileExists(path + ".bak-v3") {
		t.Fatalf("backup not removed")
	}
}

// rawExec runs statements on path without openStore's secure_delete, the way
// a pre-v4 build wrote the file.
func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=secure_delete(off)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// A v0.1.0 row switched from password SSH to Serial kept the password in the
// credential column; the v4 migration must not leave it there.
func TestMigrateCredentialsClearsSerialPassword(t *testing.T) {
	path := makeV3Database(t)
	const serialPW = "pw-serial-left-5e9d"
	rawExec(t, path, `UPDATE connections SET credential = '`+serialPW+`', auth_method = 'パスワード保管' WHERE id = 's1'`)
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := store.migrateCredentialsToKeyring(context.Background(), newMemorySecretStore(), time.Second)
	if err != nil || res.Moved != 3 || res.Remaining != 0 {
		t.Fatalf("migrate = %+v, %v", res, err)
	}
	if cred, _ := credentialRow(t, store, "s1"); cred != "" {
		t.Fatalf("serial credential = %q", cred)
	}
	for _, f := range []string{path, path + "-wal"} {
		if n := fileContainsAny(t, f, []string{serialPW}); n != "" {
			t.Fatalf("%s still holds %q", filepath.Base(f), n)
		}
	}
}

// Passwords deleted before v4 (secure_delete off) sit in free pages; the
// first start of v4 scrubs them even when no password row is left to move.
func TestMigrateCredentialsScrubsOnUpgradeWithoutMoves(t *testing.T) {
	path := makeV3Database(t)
	rawExec(t, path, `DELETE FROM connections WHERE id IN ('c1', 'c2', 'c3')`, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if fileContainsAny(t, path, plaintextPasswords) == "" {
		t.Skip("sqlite already overwrote the deleted cells; nothing to scrub")
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := store.migrateCredentialsToKeyring(context.Background(), newMemorySecretStore(), time.Second)
	if err != nil || res.Moved != 0 || res.Remaining != 0 {
		t.Fatalf("migrate = %+v, %v", res, err)
	}
	for _, f := range []string{path, path + "-wal"} {
		if n := fileContainsAny(t, f, plaintextPasswords); n != "" {
			t.Fatalf("%s still holds %q", filepath.Base(f), n)
		}
	}
}

// #12: the pre-migration backup holds every credential and is owner-only
// from the moment it exists; the config directory is tightened too.
func TestMigrationBackupIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	path := makeV2Database(t, time.Now().Format(time.RFC3339))
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, err := os.Stat(path + ".bak-v2")
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("backup mode %o, want 600", mode)
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode %o, want 700", dir.Mode().Perm())
	}
}

// #47: a pre-v2 "承認待ち" row is never migrated into an approvable pending
// request: with traces of a run it is interrupted, otherwise expired.
func TestMigrationLegacyAwaitingApprovalIsNotPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-gete.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE connections (name TEXT PRIMARY KEY, host TEXT NOT NULL, port INTEGER NOT NULL, user TEXT NOT NULL,
			auth_method TEXT NOT NULL, credential TEXT NOT NULL, connect_timeout INTEGER NOT NULL, command_timeout INTEGER NOT NULL,
			tags_json TEXT NOT NULL, description TEXT NOT NULL, status TEXT NOT NULL, last_checked TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE command_requests (id TEXT PRIMARY KEY, host TEXT NOT NULL, user TEXT NOT NULL, command TEXT NOT NULL,
			reason TEXT NOT NULL, requested_by TEXT NOT NULL, requested_at TEXT NOT NULL, risk TEXT NOT NULL, status TEXT NOT NULL,
			stdout TEXT NOT NULL, stderr TEXT NOT NULL, duration TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO command_requests VALUES ('REQ-1700000000000-1','old','u','whoami','','a','12:00:00','Low','承認待ち','','','-',1700000000000,'')`,
		`INSERT INTO command_requests VALUES ('REQ-1700000000001-2','old','u','systemctl restart app','','a','12:00:01','High','承認待ち','','','1.2s',1700000000001,'')`,
		`INSERT INTO command_requests VALUES ('REQ-1700000000002-3','old','u','rm -rf /tmp/b','','a','12:00:02','High','承認待ち','partial','','-',1700000000002,'')`,
		`INSERT INTO command_requests VALUES ('REQ-1700000000003-4','old','u','ls','','a','12:00:03','Low','実行済み','out','','1s',1700000000003,'')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reqs, err := store.loadCommandRequests(10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range reqs {
		got[r.ID] = r.Status
	}
	want := map[string]string{
		"REQ-1700000000000-1": stateExpired,
		"REQ-1700000000001-2": stateInterrupted,
		"REQ-1700000000002-3": stateInterrupted,
		"REQ-1700000000003-4": stateSucceeded,
	}
	for id, state := range want {
		if got[id] != state {
			t.Fatalf("%s: %s, want %s (all: %v)", id, got[id], state, got)
		}
	}
	// A pre-v2 database already imported its JSON long ago: it only renames.
	if state, _ := store.legacyMigrationState(); state != legacyMigrationRename {
		t.Fatalf("legacy_migrated=%q", state)
	}
}

// #3: the single-instance lock is exclusive and released by Close.
func TestStoreInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-gete.sqlite")
	first, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(path); !errors.Is(err, errAnotherInstance) {
		first.Close()
		t.Fatalf("second open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openStore(path)
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	second.Close()
}

// #34: freshness is decided by the content, not by the file existing.
func TestOpenStoreEmptyFileIsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-gete.sqlite")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.fresh {
		t.Fatal("empty database file not treated as new")
	}
	if state, _ := store.legacyMigrationState(); state != legacyMigrationPending {
		t.Fatalf("legacy_migrated=%q", state)
	}
	if matches, _ := filepath.Glob(path + ".bak-v*"); len(matches) != 0 {
		t.Fatalf("backup of an empty database: %v", matches)
	}
}
