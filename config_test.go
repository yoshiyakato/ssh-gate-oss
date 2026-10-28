package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// writeConfig writes body as a 0600 YAML file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoadConfig(t *testing.T, body string) *FileConfig {
	t.Helper()
	cfg, err := loadConfigFile(writeConfig(t, body))
	if err != nil {
		t.Fatalf("loadConfigFile: %v", err)
	}
	return cfg
}

func loadConfigErr(t *testing.T, body string) string {
	t.Helper()
	_, err := loadConfigFile(writeConfig(t, body))
	if err == nil {
		t.Fatalf("loadConfigFile accepted:\n%s", body)
	}
	return err.Error()
}

const fullConfig = `version: 1
apply: seed
web:
  listen: 127.0.0.1:8788
  tls:
    mode: off
  session:
    idleMinutes: 30
    maxHours: 12
  maxSessions: 32
mcp:
  enabled: false
  listenAddress: 127.0.0.1
  port: 8787
  bearerEnabled: true
  tokenEnv: SSH_GETE_TOKEN
  allowedOrigins: []
  requestTimeout: 30
  maxOutputKB: 256
  maxBodyKB: 256
policy:
  strictHostKey: true
  requireApprovalSudo: true
  defaultConnectTimeout: 10
secrets:
  backend: memory
connections:
  - name: prod-web-1
    type: ssh
    host: 10.0.0.5
    port: 22
    user: deploy
    auth: key
    keyPath: /etc/ssh-gete/keys/prod_ed25519
    tags: [prod, web]
    description: 本番 Web 1号機
  - name: legacy-db
    host: 10.0.0.20
    user: admin
    auth: password
    passwordEnv: LEGACY_DB_PASSWORD
  - name: core-switch-console
    type: serial
    serialPort: /dev/ttyUSB0
    baudRate: 115200
    dataBits: 8
    parity: none
    stopBits: 1
prune: false
`

func TestLoadConfigFull(t *testing.T) {
	cfg := mustLoadConfig(t, fullConfig)
	if cfg.Version != 1 || len(cfg.Connections) != 3 || cfg.MCP == nil || cfg.Policy == nil {
		t.Fatalf("decoded %+v", cfg)
	}
	if got := *cfg.Connections[2].StopBits; got != "1" {
		t.Fatalf("stopBits 1 (int in YAML) decoded as %q", got)
	}
	if cfg.MCP.Enabled == nil || *cfg.MCP.Enabled {
		t.Fatalf("mcp.enabled = %v", cfg.MCP.Enabled)
	}
	if cfg.Policy.AutoExecuteLowRisk != nil {
		t.Fatalf("an unwritten key must stay nil")
	}
	conn := cfg.Connections[0].toConnection()
	if conn.Type != "SSH" || conn.AuthMethod != configAuthKey || conn.Credential != "/etc/ssh-gete/keys/prod_ed25519" ||
		conn.Host != "10.0.0.5" || !reflect.DeepEqual(conn.Tags, []string{"prod", "web"}) {
		t.Fatalf("toConnection = %+v", conn)
	}
	pw := cfg.Connections[1].toConnection()
	if pw.AuthMethod != configAuthPassword || pw.Credential != "" || pw.Type != "SSH" {
		t.Fatalf("password toConnection = %+v", pw)
	}
	serial := cfg.Connections[2].toConnection()
	if serial.Type != "Serial" || serial.SerialPort != "/dev/ttyUSB0" || serial.BaudRate != 115200 {
		t.Fatalf("serial toConnection = %+v", serial)
	}
}

// The example shipped in deploy/ must stay loadable (when it is in the tree).
func TestLoadConfigShippedExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("deploy", "headless.example.yaml"))
	if err != nil {
		t.Skip("deploy/headless.example.yaml not present")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/credentials/ssh-gete.service")
	cfg := mustLoadConfig(t, string(data))
	if len(cfg.Connections) == 0 {
		t.Fatalf("example has no connections")
	}
}

func TestLoadConfigRejectsUnknownAndSecretKeys(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown top", "version: 1\nfoo: 1\n", "foo"},
		{"password", "version: 1\nconnections:\n  - name: a\n    host: h\n    user: u\n    auth: password\n    password: hunter2\n", "passwordEnv"},
		{"mcp token", "version: 1\nmcp:\n  token: abc\n", "tokenEnv"},
		{"empty", "", "version"},
		{"two docs", "version: 1\n---\nversion: 1\n", "1つだけ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := loadConfigErr(t, tc.body)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not mention %q", msg, tc.want)
			}
			if strings.Contains(msg, "hunter2") {
				t.Fatalf("error echoes the secret: %q", msg)
			}
		})
	}
}

func TestLoadConfigExpandsOnlyPathKeys(t *testing.T) {
	t.Setenv("CREDS", "/run/creds")
	t.Setenv("WHO", "deploy")
	cfg := mustLoadConfig(t, `version: 1
dataDir: ${CREDS}/data
web:
  adminTokenFile: ${CREDS}/admin
connections:
  - name: a
    host: h
    user: ${WHO}
    keyPath: ${CREDS}/key
    description: ${WHO}
  - name: b
    host: h
    user: u
    auth: password
    passwordFile: $CREDS/b
`)
	if cfg.DataDir != "/run/creds/data" || cfg.Web.AdminTokenFile != "/run/creds/admin" {
		t.Fatalf("path keys not expanded: %q %q", cfg.DataDir, cfg.Web.AdminTokenFile)
	}
	if *cfg.Connections[0].KeyPath != "/run/creds/key" || cfg.Connections[1].PasswordFile != "/run/creds/b" {
		t.Fatalf("keyPath / passwordFile not expanded")
	}
	if *cfg.Connections[0].User != "${WHO}" || *cfg.Connections[0].Description != "${WHO}" {
		t.Fatalf("non-path keys must not be expanded: %q %q", *cfg.Connections[0].User, *cfg.Connections[0].Description)
	}

	msg := loadConfigErr(t, "version: 1\ndataDir: ${SSH_GETE_TEST_UNSET_VAR}/x\n")
	if !strings.Contains(msg, "SSH_GETE_TEST_UNSET_VAR") {
		t.Fatalf("unset variable not reported: %q", msg)
	}
}

func TestLoadConfigRefusesWritableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	path := writeConfig(t, "version: 1\n")
	if err := os.Chmod(path, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(path); err == nil || !strings.Contains(err.Error(), "書き込め") {
		t.Fatalf("group-writable file accepted: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(path); err != nil {
		t.Fatalf("world-readable file is only a warning: %v", err)
	}
}

func TestConfigValidateRules(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"version", "version: 2\n", "version"},
		{"apply", "version: 1\napply: always\n", "apply"},
		{"duplicate name", "version: 1\nconnections:\n  - {name: a, host: h, user: u}\n  - {name: a, host: h2, user: u}\n", "重複"},
		{"missing name", "version: 1\nconnections:\n  - {host: h, user: u}\n", "name は必須"},
		{"ssh needs host", "version: 1\nconnections:\n  - {name: a, user: u}\n", "host は必須"},
		{"both password sources", "version: 1\nconnections:\n  - {name: a, host: h, user: u, auth: password, passwordEnv: X, passwordFile: /f}\n", "同時"},
		{"password source with key auth", "version: 1\nconnections:\n  - {name: a, host: h, user: u, passwordEnv: X}\n", "auth: password"},
		{"bad env name", "version: 1\nconnections:\n  - {name: a, host: h, user: u, auth: password, passwordEnv: 'A-B'}\n", "環境変数名"},
		{"serial needs port", "version: 1\nconnections:\n  - {name: s, type: serial}\n", "serialPort"},
		{"serial bad parity", "version: 1\nconnections:\n  - {name: s, type: serial, serialPort: /dev/ttyUSB0, parity: weird}\n", "parity"},
		{"serial with host", "version: 1\nconnections:\n  - {name: s, type: serial, serialPort: /dev/ttyUSB0, host: h}\n", "host は type: ssh"},
		{"listen collision", "version: 1\nweb: {listen: '127.0.0.1:8787'}\nmcp: {port: 8787}\n", "同じ"},
		{"listen collision wildcard", "version: 1\nweb: {listen: '0.0.0.0:9000'}\nmcp: {listenAddress: 127.0.0.1, port: 9000}\n", "同じ"},
		{"bad listen", "version: 1\nweb: {listen: '8788'}\n", "web.listen"},
		{"tls files need cert", "version: 1\nweb: {tls: {mode: files}}\n", "cert と key"},
		{"tls mode", "version: 1\nweb: {tls: {mode: maybe}}\n", "web.tls.mode"},
		{"idle", "version: 1\nweb: {session: {idleMinutes: 1}}\n", "idleMinutes"},
		{"backend", "version: 1\nsecrets: {backend: vault}\n", "secrets.backend"},
		{"mcp port", "version: 1\nmcp: {port: 70000}\n", "mcp.port"},
		{"wildcard origin", "version: 1\nmcp: {allowedOrigins: ['http://*']}\n", "ワイルドカード"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := loadConfigErr(t, tc.body)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not mention %q", msg, tc.want)
			}
		})
	}
	// Different ports on one host are fine.
	mustLoadConfig(t, "version: 1\nweb: {listen: '127.0.0.1:8788'}\nmcp: {port: 8787}\n")
}

func TestResolveWebConfig(t *testing.T) {
	w, err := resolveWebConfig(nil, CLIOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w.Listen != defaultWebListen || w.TLS.Mode != "off" || w.Session.IdleMinutes != 30 || w.Session.MaxHours != 12 || w.MaxSessions != 32 {
		t.Fatalf("defaults = %+v", w)
	}
	cfg := mustLoadConfig(t, "version: 1\nweb:\n  listen: 0.0.0.0:9443\n  tls: {mode: self-signed}\n  publicURL: https://gate.lan:9443/\n")
	w, err = resolveWebConfig(cfg, CLIOptions{Listen: "127.0.0.1:9000", TLSCert: "/c", TLSKey: "/k"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Listen != "127.0.0.1:9000" || w.TLS.Mode != "files" || w.TLS.Cert != "/c" || w.PublicURL != "https://gate.lan:9443" {
		t.Fatalf("flags must win over the file: %+v", w)
	}
	if _, err := resolveWebConfig(nil, CLIOptions{TLSSelfSigned: true, TLSCert: "/c", TLSKey: "/k"}); err == nil {
		t.Fatalf("self-signed with files accepted")
	}
	if _, err := resolveWebConfig(nil, CLIOptions{TLSCert: "/c"}); err == nil {
		t.Fatalf("cert without key accepted")
	}
	if got := adminTokenPath(WebConfig{}, "/data"); got != filepath.Join("/data", "admin-token") {
		t.Fatalf("adminTokenPath = %q", got)
	}
}

func TestSecretsBackendEnvWins(t *testing.T) {
	cfg := &FileConfig{Secrets: SecretsConfig{Backend: "file"}}
	t.Setenv("SSH_GETE_KEYRING", "")
	if got := secretsBackend(cfg); got != "file" {
		t.Fatalf("backend = %q", got)
	}
	t.Setenv("SSH_GETE_KEYRING", "memory")
	if got := secretsBackend(cfg); got != "memory" {
		t.Fatalf("SSH_GETE_KEYRING must win: %q", got)
	}
	t.Setenv("SSH_GETE_KEYRING", "")
	if got := secretsBackend(nil); got != "keychain" {
		t.Fatalf("default backend = %q", got)
	}
}

// --- applyConfig (§1.4) ----------------------------------------------------

func newConfigTestApp(t *testing.T) *App {
	t.Helper()
	isolateEnv(t)
	t.Setenv("SSH_GETE_NO_SEED", "1")
	app := NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	return app
}

func connByName(t *testing.T, app *App, name string) (Connection, bool) {
	t.Helper()
	app.mu.RLock()
	defer app.mu.RUnlock()
	for _, c := range app.conns {
		if c.Name == name {
			return c, true
		}
	}
	return Connection{}, false
}

type auditRec struct{ action, target, detail string }

func recordingAudit(out *[]auditRec) configAudit {
	return func(action, target, detail string) { *out = append(*out, auditRec{action, target, detail}) }
}

const seedOne = `version: 1
connections:
  - name: web-1
    host: 10.0.0.5
    user: deploy
    keyPath: /keys/a
    description: from file
`

func TestApplyConfigCreateKeepOverride(t *testing.T) {
	app := newConfigTestApp(t)
	var audit []auditRec

	// New: created and recorded.
	report, err := applyConfig(app, mustLoadConfig(t, seedOne), nil, recordingAudit(&audit))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Created, []string{"connection:web-1"}) {
		t.Fatalf("report = %+v", report)
	}
	conn, ok := connByName(t, app, "web-1")
	if !ok || conn.Host != "10.0.0.5" || conn.Credential != "/keys/a" || conn.AuthMethod != configAuthKey {
		t.Fatalf("created %+v", conn)
	}
	if len(audit) != 1 || audit[0].action != "config_create" {
		t.Fatalf("audit = %+v", audit)
	}

	// The web admin edits it (a written key and an unwritten one).
	conn.Description = "edited in web"
	conn.CommandTimeout = 99
	if r := app.SaveConnection(conn); !r.OK {
		t.Fatal(r.Message)
	}

	// Same file again: the database wins.
	report, err = applyConfig(app, mustLoadConfig(t, seedOne), nil, recordingAudit(&audit))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Kept, []string{"connection:web-1"}) || len(report.Overridden) != 0 {
		t.Fatalf("unchanged file: report = %+v", report)
	}
	if got, _ := connByName(t, app, "web-1"); got.Description != "edited in web" {
		t.Fatalf("web edit lost: %q", got.Description)
	}

	// The file changes: only the written keys are overwritten.
	changed := strings.Replace(seedOne, "10.0.0.5", "10.0.0.6", 1)
	report, err = applyConfig(app, mustLoadConfig(t, changed), nil, recordingAudit(&audit))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Overridden) != 1 || !strings.Contains(report.Overridden[0], "host") || !strings.Contains(report.Overridden[0], "description") {
		t.Fatalf("changed file: report = %+v", report)
	}
	got, _ := connByName(t, app, "web-1")
	if got.Host != "10.0.0.6" || got.Description != "from file" || got.CommandTimeout != 99 {
		t.Fatalf("override: host=%q description=%q commandTimeout=%d", got.Host, got.Description, got.CommandTimeout)
	}
	if got.ID != conn.ID {
		t.Fatalf("override created a new row")
	}
	last := audit[len(audit)-1]
	if last.action != "config_override" || !strings.Contains(last.detail, "host") {
		t.Fatalf("override audit = %+v", last)
	}
}

func TestApplyConfigRenameInWebDoesNotDuplicate(t *testing.T) {
	app := newConfigTestApp(t)
	if _, err := applyConfig(app, mustLoadConfig(t, seedOne), nil, nil); err != nil {
		t.Fatal(err)
	}
	conn, _ := connByName(t, app, "web-1")
	conn.Name = "renamed"
	if r := app.SaveConnection(conn); !r.OK {
		t.Fatal(r.Message)
	}
	// Changed file, same entry name: matched by conn_id, not duplicated.
	changed := strings.Replace(seedOne, "from file", "v2", 1)
	if _, err := applyConfig(app, mustLoadConfig(t, changed), nil, nil); err != nil {
		t.Fatal(err)
	}
	app.mu.RLock()
	n := len(app.conns)
	app.mu.RUnlock()
	if n != 1 {
		t.Fatalf("%d connections after a web rename", n)
	}
	got, _ := connByName(t, app, "web-1")
	if got.ID != conn.ID || got.Description != "v2" {
		t.Fatalf("the file's name and fields win on change: %+v", got)
	}
}

func TestApplyConfigPruneOnlyFileOwned(t *testing.T) {
	app := newConfigTestApp(t)
	two := seedOne + "  - name: web-2\n    host: 10.0.0.7\n    user: deploy\n"
	if _, err := applyConfig(app, mustLoadConfig(t, two), nil, nil); err != nil {
		t.Fatal(err)
	}
	if r := app.SaveConnection(Connection{Name: "made-in-web", Host: "10.1.1.1", User: "u"}); !r.OK {
		t.Fatal(r.Message)
	}

	// web-2 removed from the file, prune off: kept.
	report, err := applyConfig(app, mustLoadConfig(t, seedOne), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Removed, []string{"connection:web-2"}) {
		t.Fatalf("report = %+v", report)
	}
	if _, ok := connByName(t, app, "web-2"); !ok {
		t.Fatalf("web-2 deleted without prune")
	}

	// prune on: only web-2 (file-created) goes.
	var audit []auditRec
	report, err = applyConfig(app, mustLoadConfig(t, seedOne+"prune: true\n"), nil, recordingAudit(&audit))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Pruned, []string{"connection:web-2"}) {
		t.Fatalf("report = %+v", report)
	}
	if _, ok := connByName(t, app, "web-2"); ok {
		t.Fatalf("web-2 not pruned")
	}
	for _, name := range []string{"web-1", "made-in-web"} {
		if _, ok := connByName(t, app, name); !ok {
			t.Fatalf("%s pruned", name)
		}
	}
	if len(audit) == 0 || audit[len(audit)-1].action != "config_prune" {
		t.Fatalf("audit = %+v", audit)
	}
	// The record is gone too: a later prune has nothing to do.
	report, err = applyConfig(app, mustLoadConfig(t, seedOne+"prune: true\n"), nil, nil)
	if err != nil || len(report.Pruned) != 0 {
		t.Fatalf("second prune: %+v %v", report, err)
	}
}

func TestApplyConfigEnforce(t *testing.T) {
	app := newConfigTestApp(t)
	enforce := strings.Replace(seedOne, "version: 1\n", "version: 1\napply: enforce\n", 1)
	if _, err := applyConfig(app, mustLoadConfig(t, enforce), nil, nil); err != nil {
		t.Fatal(err)
	}
	conn, _ := connByName(t, app, "web-1")
	conn.Description = "edited in web"
	if r := app.SaveConnection(conn); !r.OK {
		t.Fatal(r.Message)
	}
	report, err := applyConfig(app, mustLoadConfig(t, enforce), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Overridden) != 1 {
		t.Fatalf("enforce: report = %+v", report)
	}
	if got, _ := connByName(t, app, "web-1"); got.Description != "from file" {
		t.Fatalf("enforce did not restore: %q", got.Description)
	}
}

func TestApplyConfigAuthSwitchNeverLeaksKeyPathAsPassword(t *testing.T) {
	app := newConfigTestApp(t)
	if _, err := applyConfig(app, mustLoadConfig(t, seedOne), nil, nil); err != nil {
		t.Fatal(err)
	}
	toPassword := strings.Replace(seedOne, "    keyPath: /keys/a\n", "    auth: password\n", 1)
	if _, err := applyConfig(app, mustLoadConfig(t, toPassword), nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := connByName(t, app, "web-1")
	if got.AuthMethod != configAuthPassword || got.Credential != "" || got.CredentialSet {
		t.Fatalf("switch to password: %+v", got)
	}
	ctx := context.Background()
	if _, err := app.secrets.Get(ctx, passwordAccount(got.ID)); err == nil {
		t.Fatalf("the key path was stored as the password")
	}
}

func TestApplyConfigMCPMatchesSaveMCPSettings(t *testing.T) {
	body := `version: 1
mcp:
  enabled: false
  port: 9787
  allowedOrigins: [https://ok.example]
  requestTimeout: 45
policy:
  strictHostKey: true
  requireApprovalProd: false
  defaultCommandTimeout: 77
`
	viaConfig := newConfigTestApp(t)
	// The file's mcp.* must apply, so drop the harness's SSH_GETE_MCP_ADDR.
	t.Setenv("SSH_GETE_MCP_ADDR", "")
	report, err := applyConfig(viaConfig, mustLoadConfig(t, body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Overridden, []string{
		"mcp [enabled port allowedOrigins requestTimeout]",
		"policy [strictHostKey requireApprovalProd defaultCommandTimeout]"}) {
		t.Fatalf("report = %+v", report.Overridden)
	}
	viaGUI := newConfigTestApp(t)
	t.Setenv("SSH_GETE_MCP_ADDR", "")
	s := viaGUI.GetMCPSettings()
	s.Enabled, s.Port, s.AllowedOrigins, s.RequestTimeout = false, 9787, []string{"https://ok.example"}, 45
	s.StrictHostKey, s.RequireApprovalProd, s.DefaultCommandTimeout = true, false, 77
	if r := viaGUI.SaveMCPSettings(s); !r.OK {
		t.Fatal(r.Message)
	}

	load := func(app *App) MCPSettings {
		m, _, loaded, err := app.store.loadMCPSettings(app.dbPath)
		if err != nil || !loaded {
			t.Fatalf("load: %v %v", loaded, err)
		}
		m.ConfigPath = ""
		return m
	}
	a, b := load(viaConfig), load(viaGUI)
	// Derived, per-instance fields are not part of the comparison.
	for _, m := range []*MCPSettings{&a, &b} {
		m.LocalURL, m.LANAddress, m.LANURL, m.LANReachable, m.Status = "", "", "", false, ""
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("stored rows differ\nconfig: %+v\ngui:    %+v", a, b)
	}

	// Unchanged file: kept; the DB value changed in the web stays.
	s = viaConfig.GetMCPSettings()
	s.RequestTimeout = 50
	if r := viaConfig.SaveMCPSettings(s); !r.OK {
		t.Fatal(r.Message)
	}
	report, err = applyConfig(viaConfig, mustLoadConfig(t, body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Kept, []string{"mcp", "policy"}) {
		t.Fatalf("report = %+v", report)
	}
	if got := viaConfig.GetMCPSettings().RequestTimeout; got != 50 {
		t.Fatalf("web change lost: %d", got)
	}
}

func TestApplyConfigMCPAddrEnvWins(t *testing.T) {
	app := newConfigTestApp(t)
	t.Setenv("SSH_GETE_MCP_ADDR", "127.0.0.1:9911")
	report, err := applyConfig(app, mustLoadConfig(t, "version: 1\nmcp:\n  enabled: false\n  port: 9787\n"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Notes) == 0 || !strings.Contains(report.Notes[0], "SSH_GETE_MCP_ADDR") {
		t.Fatalf("notes = %v", report.Notes)
	}
	if got := app.GetMCPSettings().Port; got == 9787 {
		t.Fatalf("file port applied despite SSH_GETE_MCP_ADDR")
	}
}

func TestApplyConfigThenStartupListensOnce(t *testing.T) {
	app := newConfigTestApp(t)
	// The file's mcp.* must apply, so drop the harness's SSH_GETE_MCP_ADDR.
	t.Setenv("SSH_GETE_MCP_ADDR", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	body := "version: 1\nmcp:\n  enabled: true\n  listenAddress: 127.0.0.1\n  port: " + strconv.Itoa(port) + "\n"
	if _, err := applyConfig(app, mustLoadConfig(t, body), nil, nil); err != nil {
		t.Fatal(err)
	}
	app.lifecycleMu.Lock()
	running := app.mcpServer != nil
	app.lifecycleMu.Unlock()
	if running {
		t.Fatalf("applyConfig started the MCP listener")
	}
	app.startup(nil)
	if s := app.GetMCPSettings(); s.Status == "エラー" {
		t.Fatalf("startup after applyConfig: %s", s.LastError)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("MCP not listening on the file's port: %v", err)
	}
	conn.Close()
	if err := app.applyMCPSettingsNoRestart(app.GetMCPSettings()); err == nil {
		t.Fatalf("applyMCPSettingsNoRestart must refuse while the listener runs")
	}
}

func TestApplyConfigRefusesLANWithoutBearer(t *testing.T) {
	app := newConfigTestApp(t)
	// The file's mcp.* must apply, so drop the harness's SSH_GETE_MCP_ADDR.
	t.Setenv("SSH_GETE_MCP_ADDR", "")
	_, err := applyConfig(app, mustLoadConfig(t, "version: 1\nmcp:\n  enabled: true\n  listenAddress: 0.0.0.0\n  bearerEnabled: false\n"), nil, nil)
	if err == nil {
		t.Fatalf("LAN listener without Bearer accepted")
	}
}

func TestPlanConfigWritesNothing(t *testing.T) {
	app := newConfigTestApp(t)
	report, err := planConfig(app, mustLoadConfig(t, seedOne))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Created, []string{"connection:web-1"}) {
		t.Fatalf("report = %+v", report)
	}
	if _, ok := connByName(t, app, "web-1"); ok {
		t.Fatalf("planConfig created a connection")
	}
	var n int
	err = app.store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='config_applied'`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("planConfig created config_applied (%d, %v)", n, err)
	}
}

// --- secrets named by the file (§7.3) --------------------------------------

type recordingBinder struct{ binds map[string]string }

func (b *recordingBinder) bind(account, envName string) { b.binds[account] = envName }

func TestApplyConfigPasswordEnvWithoutBinderIsSessionOnly(t *testing.T) {
	app := newConfigTestApp(t)
	t.Setenv("CFG_TEST_PW", "s3cret-from-env")
	cfg := mustLoadConfig(t, "version: 1\nconnections:\n  - {name: db, host: h, user: u, auth: password, passwordEnv: CFG_TEST_PW}\n")
	if _, err := applyConfig(app, cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	conn, _ := connByName(t, app, "db")
	if conn.CredentialSet || conn.Credential != "" {
		t.Fatalf("env password persisted: %+v", conn)
	}
	dial, err := app.resolveCredential(context.Background(), conn)
	if err != nil || dial.Credential != "s3cret-from-env" {
		t.Fatalf("resolveCredential = %q, %v", dial.Credential, err)
	}
}

func TestApplyConfigPasswordEnvWithBinder(t *testing.T) {
	app := newConfigTestApp(t)
	t.Setenv("CFG_TEST_PW", "bound-secret")
	binder := &recordingBinder{binds: map[string]string{}}
	cfg := mustLoadConfig(t, "version: 1\nconnections:\n  - {name: db, host: h, user: u, auth: password, passwordEnv: CFG_TEST_PW}\n")
	if _, err := applyConfig(app, cfg, binder, nil); err != nil {
		t.Fatal(err)
	}
	conn, _ := connByName(t, app, "db")
	if binder.binds[passwordAccount(conn.ID)] != "CFG_TEST_PW" {
		t.Fatalf("binds = %v", binder.binds)
	}
	if !conn.CredentialSet {
		t.Fatalf("the env value was not saved through SaveConnection")
	}
}

func TestApplyConfigPasswordFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	app := newConfigTestApp(t)
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoadConfig(t, "version: 1\nsecrets: {backend: memory}\nconnections:\n  - {name: db, host: h, user: u, auth: password, passwordFile: "+pwFile+"}\n")
	report, err := applyConfig(app, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Notes) == 0 || !strings.Contains(strings.Join(report.Notes, "\n"), "chmod 600") {
		t.Fatalf("a 0644 password file must be refused with a note: %v", report.Notes)
	}
	if strings.Contains(strings.Join(report.Notes, "\n"), "from-file") {
		t.Fatalf("note leaks the secret")
	}
	if err := os.Chmod(pwFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfig(app, cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	conn, _ := connByName(t, app, "db")
	dial, err := app.resolveCredential(context.Background(), conn)
	if err != nil || dial.Credential != "from-file" {
		t.Fatalf("resolveCredential = %q, %v", dial.Credential, err)
	}
}

func TestApplyConfigPassphraseEnv(t *testing.T) {
	app := newConfigTestApp(t)
	t.Setenv("CFG_TEST_PP", "pp-value")
	cfg := mustLoadConfig(t, "version: 1\nconnections:\n  - {name: k, host: h, user: u, keyPath: /k, passphraseEnv: CFG_TEST_PP}\n")
	if _, err := applyConfig(app, cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	conn, _ := connByName(t, app, "k")
	app.mu.RLock()
	got := app.sshPassphrases[conn.ID]
	app.mu.RUnlock()
	if got != "pp-value" {
		t.Fatalf("passphrase = %q", got)
	}
}

func TestCheckConfigAppReadOnly(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "ssh-gete.sqlite")
	// Missing database: everything is a create, and no file appears.
	app, closeFn, err := checkConfigApp(db)
	if err != nil {
		t.Fatal(err)
	}
	report, err := planConfig(app, mustLoadConfig(t, seedOne))
	closeFn()
	if err != nil || len(report.Created) != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
	if fileExists(db) {
		t.Fatalf("--check-config created the database")
	}

	// Existing database: opened read-only.
	store, err := openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.insertConnection(normalizeConnection(Connection{ID: newID(), Name: "web-1", Host: "x", User: "u"})); err != nil {
		t.Fatal(err)
	}
	store.Close()
	app, closeFn, err = checkConfigApp(db)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	report, err = planConfig(app, mustLoadConfig(t, seedOne))
	if err != nil || len(report.Overridden) != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
	if _, err := app.store.db.Exec(`CREATE TABLE x (a)`); err == nil {
		t.Fatalf("check-config store is writable")
	}
}

// withReleaseSecrets makes memorySecretsAllowed behave as in a release build.
func withReleaseSecrets(t *testing.T) {
	t.Helper()
	prev := memorySecretsAllowed
	memorySecretsAllowed = func() bool { return false }
	t.Cleanup(func() { memorySecretsAllowed = prev })
}

// REQ-014（黙って落とさない）: リリースのビルドでは memory を選べない。
func TestConfigRefusesMemoryBackendOutsideTests(t *testing.T) {
	body := "version: 1\nsecrets:\n  backend: memory\n"
	if cfg := mustLoadConfig(t, body); cfg.Secrets.Backend != "memory" {
		t.Fatalf("test binary must accept memory: %+v", cfg.Secrets)
	}
	withReleaseSecrets(t)
	if msg := loadConfigErr(t, body); !strings.Contains(msg, "テスト専用") {
		t.Fatalf("message = %s", msg)
	}
	t.Setenv("SSH_GETE_KEYRING", "memory")
	if got := secretsBackend(&FileConfig{Secrets: SecretsConfig{Backend: "file"}}); got != "file" {
		t.Fatalf("SSH_GETE_KEYRING=memory must be ignored outside tests: %q", got)
	}
	if got := secretsBackend(nil); got != "keychain" {
		t.Fatalf("default = %q", got)
	}
	t.Setenv("SSH_GETE_KEYRING", "env")
	if got := secretsBackend(nil); got != "env" {
		t.Fatalf("SSH_GETE_KEYRING=env = %q", got)
	}
	t.Setenv("SSH_GETE_KEYRING", "memory")
	if _, _, err := builtinSecretBackend(SecretsConfig{}, "memory", ""); err == nil {
		t.Fatal("builtin memory backend accepted outside tests")
	}
}

func TestConfigAdminPasswordFile(t *testing.T) {
	t.Setenv("PWDIR", "/srv/gate")
	cfg := mustLoadConfig(t, "version: 1\nweb:\n  adminPasswordFile: ${PWDIR}/admin-password\n")
	if cfg.Web.AdminPasswordFile != "/srv/gate/admin-password" {
		t.Fatalf("expanded = %q", cfg.Web.AdminPasswordFile)
	}
	if got := adminPasswordPath(cfg.Web, "/data"); got != "/srv/gate/admin-password" {
		t.Fatalf("path = %q", got)
	}
	if got := adminPasswordPath(WebConfig{}, "/data"); got != filepath.Join("/data", "admin-password") {
		t.Fatalf("default = %q", got)
	}
	// 平文のパスワードはキーとして存在しない
	if msg := loadConfigErr(t, "version: 1\nweb:\n  adminPassword: hunter2hunter2\n"); msg == "" {
		t.Fatal("plaintext password key accepted")
	}
}
