package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	if store2.prevVersion != schemaVersion || fileExists(path+".bak-v3") {
		t.Fatalf("v3 reopen migrated again")
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
