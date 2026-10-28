package main

// Headless configuration file (phase6-headless.md §1.2-§1.5): the YAML
// schema, loading and validation, and applying it to the database.
//
// The file is the seed: once it has been applied, changes made from the web
// admin win until the file itself changes (config_applied keeps the hash of
// what was last applied). Secrets never appear in the file; it can only name
// an environment variable or a 0600 file to read them from.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

const (
	configApplySeed    = "seed"
	configApplyEnforce = "enforce"

	defaultWebListen      = "127.0.0.1:8788"
	defaultWebIdleMinutes = 30
	defaultWebMaxHours    = 12
	defaultWebMaxSessions = 32

	// configMaxBytes bounds the YAML file; a real one is a few KiB.
	configMaxBytes = 4 << 20
	// configSecretFileMax bounds a passwordFile.
	configSecretFileMax = 64 << 10
)

// FileConfig is the whole YAML document (§1.3).
type FileConfig struct {
	Version     int                `yaml:"version"`
	DataDir     string             `yaml:"dataDir"`
	Apply       string             `yaml:"apply"`
	Web         WebConfig          `yaml:"web"`
	MCP         *MCPConfig         `yaml:"mcp"`
	Policy      *PolicyConfig      `yaml:"policy"`
	Secrets     SecretsConfig      `yaml:"secrets"`
	Connections []ConnectionConfig `yaml:"connections"`
	Prune       bool               `yaml:"prune"`

	// path is where the file was read from (for messages).
	path string
}

// WebConfig is the web admin listener. It is never stored in the database:
// only the file and the command line decide it (§1.4).
type WebConfig struct {
	Listen         string       `yaml:"listen"`
	PublicURL      string       `yaml:"publicURL"`
	TLS            WebTLSConfig `yaml:"tls"`
	AdminTokenFile string       `yaml:"adminTokenFile"`
	// AdminPasswordFile holds the argon2id hash of the optional admin
	// password (--set-admin-password). Empty: <dataDir>/admin-password.
	AdminPasswordFile string        `yaml:"adminPasswordFile"`
	Session           SessionConfig `yaml:"session"`
	MaxSessions       int           `yaml:"maxSessions"`
}

// WebTLSConfig: Mode is "off" | "files" | "self-signed".
type WebTLSConfig struct {
	Mode string `yaml:"mode"`
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// SessionConfig bounds an admin login session.
type SessionConfig struct {
	IdleMinutes int `yaml:"idleMinutes"`
	MaxHours    int `yaml:"maxHours"`
}

// MCPConfig maps onto the listener fields of MCPSettings. A nil field was not
// written in the file and keeps the database value.
type MCPConfig struct {
	Enabled        *bool     `yaml:"enabled" json:"enabled,omitempty"`
	ListenAddress  *string   `yaml:"listenAddress" json:"listenAddress,omitempty"`
	Port           *int      `yaml:"port" json:"port,omitempty"`
	BearerEnabled  *bool     `yaml:"bearerEnabled" json:"bearerEnabled,omitempty"`
	AllowedOrigins *[]string `yaml:"allowedOrigins" json:"allowedOrigins,omitempty"`
	RequestTimeout *int      `yaml:"requestTimeout" json:"requestTimeout,omitempty"`
	MaxOutputKB    *int      `yaml:"maxOutputKB" json:"maxOutputKB,omitempty"`
	MaxBodyKB      *int      `yaml:"maxBodyKB" json:"maxBodyKB,omitempty"`
	// TokenEnv names the variable holding the Bearer token. It is copied to
	// SSH_GETE_TOKEN before NewApp (applyConfigEnv) and never stored.
	TokenEnv string `yaml:"tokenEnv" json:"-"`
}

// PolicyConfig maps onto the policy fields of MCPSettings.
type PolicyConfig struct {
	StrictHostKey          *bool `yaml:"strictHostKey" json:"strictHostKey,omitempty"`
	AutoExecuteLowRisk     *bool `yaml:"autoExecuteLowRisk" json:"autoExecuteLowRisk,omitempty"`
	RequireApprovalSudo    *bool `yaml:"requireApprovalSudo" json:"requireApprovalSudo,omitempty"`
	RequireApprovalProd    *bool `yaml:"requireApprovalProd" json:"requireApprovalProd,omitempty"`
	RequireApprovalWriteOp *bool `yaml:"requireApprovalWriteOp" json:"requireApprovalWriteOp,omitempty"`
	DefaultConnectTimeout  *int  `yaml:"defaultConnectTimeout" json:"defaultConnectTimeout,omitempty"`
	DefaultCommandTimeout  *int  `yaml:"defaultCommandTimeout" json:"defaultCommandTimeout,omitempty"`
}

// SecretsConfig picks where passwords live (§7.1). Backend is
// "keychain" (default) | "file" | "env" | "memory".
type SecretsConfig struct {
	Backend string `yaml:"backend"`
	File    string `yaml:"file"`
}

// ConnectionConfig is one connections[] entry. Name is the key matched
// against the database; a nil field was not written and keeps the database
// value when the entry overrides an existing row.
type ConnectionConfig struct {
	Name           string    `yaml:"name" json:"name"`
	Type           *string   `yaml:"type" json:"type,omitempty"`
	Host           *string   `yaml:"host" json:"host,omitempty"`
	Port           *int      `yaml:"port" json:"port,omitempty"`
	User           *string   `yaml:"user" json:"user,omitempty"`
	Auth           *string   `yaml:"auth" json:"auth,omitempty"`
	KeyPath        *string   `yaml:"keyPath" json:"keyPath,omitempty"`
	Tags           *[]string `yaml:"tags" json:"tags,omitempty"`
	Description    *string   `yaml:"description" json:"description,omitempty"`
	ConnectTimeout *int      `yaml:"connectTimeout" json:"connectTimeout,omitempty"`
	CommandTimeout *int      `yaml:"commandTimeout" json:"commandTimeout,omitempty"`
	MCPFirstTrust  *bool     `yaml:"mcpFirstTrust" json:"mcpFirstTrust,omitempty"`
	SerialPort     *string   `yaml:"serialPort" json:"serialPort,omitempty"`
	BaudRate       *int      `yaml:"baudRate" json:"baudRate,omitempty"`
	DataBits       *int      `yaml:"dataBits" json:"dataBits,omitempty"`
	Parity         *string   `yaml:"parity" json:"parity,omitempty"`
	StopBits       *string   `yaml:"stopBits" json:"stopBits,omitempty"`

	// Where secrets come from. Not database fields: they are bound in
	// memory on every start and are not part of the hash.
	PassphraseEnv string `yaml:"passphraseEnv" json:"-"`
	PasswordEnv   string `yaml:"passwordEnv" json:"-"`
	PasswordFile  string `yaml:"passwordFile" json:"-"`
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// loadConfigFile reads, decodes (unknown keys are errors), expands ${VAR} in
// the path keys and validates path. Any error means "refuse to start" (exit 2).
func loadConfigFile(path string) (*FileConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("設定ファイルのパスが空です")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("設定ファイルを開けません: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("設定ファイルを確認できません: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("設定ファイル %s は通常のファイルではありません", path)
	}
	if err := checkConfigFileMode(path, info.Mode().Perm()); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, configMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("設定ファイルを読めません: %w", err)
	}
	if len(data) > configMaxBytes {
		return nil, fmt.Errorf("設定ファイル %s が大きすぎます（上限 %d MiB）", path, configMaxBytes>>20)
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("設定ファイル %s: %w", path, err)
	}
	cfg.path = path
	if err := cfg.expandPaths(os.LookupEnv); err != nil {
		return nil, fmt.Errorf("設定ファイル %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("設定ファイル %s: %w", path, err)
	}
	return cfg, nil
}

// checkConfigFileMode refuses a file others can write (they could swap the
// connections or the policy) and warns when others can read it.
func checkConfigFileMode(path string, perm os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm&0o022 != 0 {
		return fmt.Errorf("設定ファイル %s はグループか他のユーザーが書き込めます（%04o）。chmod go-w %s を実行してください", path, perm, path)
	}
	if perm&0o044 != 0 {
		log.Printf("config: %s is readable by group/others (%04o); it holds no secrets, but consider chmod 600", path, perm)
	}
	return nil
}

// parseConfig decodes one YAML document with unknown keys rejected.
func parseConfig(data []byte) (*FileConfig, error) {
	var cfg FileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("中身が空です（version: 1 が必要です）")
		}
		msg := err.Error()
		if strings.Contains(msg, "field password ") || strings.Contains(msg, "field credential ") ||
			strings.Contains(msg, "field token ") || strings.Contains(msg, "field passphrase ") {
			msg += "（秘密の値は設定ファイルに書けません。passwordEnv / passwordFile / tokenEnv で読み込み元を指定してください）"
		}
		return nil, errors.New(msg)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("YAML のドキュメントは1つだけにしてください")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &cfg, nil
}

// expandPaths replaces ${VAR} / $VAR in the keys that hold paths, and only
// there (§1.2). A referenced variable that is not set is an error, so a
// missing ${CREDENTIALS_DIRECTORY} does not silently become "/name".
func (c *FileConfig) expandPaths(lookup func(string) (string, bool)) error {
	var missing []string
	expand := func(s string) string {
		if !strings.Contains(s, "$") {
			return s
		}
		return os.Expand(s, func(name string) string {
			v, ok := lookup(name)
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
	}
	c.DataDir = expand(c.DataDir)
	c.Web.TLS.Cert = expand(c.Web.TLS.Cert)
	c.Web.TLS.Key = expand(c.Web.TLS.Key)
	c.Web.AdminTokenFile = expand(c.Web.AdminTokenFile)
	c.Web.AdminPasswordFile = expand(c.Web.AdminPasswordFile)
	c.Secrets.File = expand(c.Secrets.File)
	for i := range c.Connections {
		cc := &c.Connections[i]
		if cc.KeyPath != nil {
			v := expand(*cc.KeyPath)
			cc.KeyPath = &v
		}
		cc.PasswordFile = expand(cc.PasswordFile)
	}
	if len(missing) > 0 {
		return fmt.Errorf("パスの中の環境変数が設定されていません: %s", strings.Join(uniqueStrings(missing), ", "))
	}
	return nil
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// validate applies the rules of §1.3. The listener safety rules of §6.1 are
// checked later, once the TLS and MCP settings are known (validateWebListen).
func (c *FileConfig) validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.Version != 1 {
		add("version は 1 にしてください（現在 %d）", c.Version)
	}
	switch c.Apply {
	case "", configApplySeed, configApplyEnforce:
	default:
		add("apply は seed か enforce です（%q）", c.Apply)
	}

	if c.Web.Listen != "" {
		if _, _, err := splitListen(c.Web.Listen); err != nil {
			add("web.listen: %v", err)
		}
	}
	if u := strings.TrimSpace(c.Web.PublicURL); u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		add("web.publicURL は http:// か https:// で始めてください")
	}
	switch c.Web.TLS.Mode {
	case "", "off", "self-signed":
		if c.Web.TLS.Cert != "" || c.Web.TLS.Key != "" {
			add("web.tls.cert / key は mode: files のときだけ書けます")
		}
	case "files":
		if strings.TrimSpace(c.Web.TLS.Cert) == "" || strings.TrimSpace(c.Web.TLS.Key) == "" {
			add("web.tls.mode: files には cert と key の両方が必要です")
		}
	default:
		add("web.tls.mode は off / files / self-signed のどれかです（%q）", c.Web.TLS.Mode)
	}
	if v := c.Web.Session.IdleMinutes; v != 0 && (v < 5 || v > 720) {
		add("web.session.idleMinutes は 5〜720 です（%d）", v)
	}
	if v := c.Web.Session.MaxHours; v != 0 && (v < 1 || v > 168) {
		add("web.session.maxHours は 1〜168 です（%d）", v)
	}
	if v := c.Web.MaxSessions; v < 0 || v > 1024 {
		add("web.maxSessions は 1〜1024 です（%d）", v)
	}

	if m := c.MCP; m != nil {
		if m.Port != nil && !validPort(*m.Port) {
			add("mcp.port は 1〜65535 です（%d）", *m.Port)
		}
		if m.ListenAddress != nil && strings.TrimSpace(*m.ListenAddress) == "" {
			add("mcp.listenAddress が空です")
		}
		if m.TokenEnv != "" && !envNamePattern.MatchString(m.TokenEnv) {
			add("mcp.tokenEnv %q は環境変数名として使えません", m.TokenEnv)
		}
		for name, v := range map[string]*int{"requestTimeout": m.RequestTimeout, "maxOutputKB": m.MaxOutputKB, "maxBodyKB": m.MaxBodyKB} {
			if v != nil && *v <= 0 {
				add("mcp.%s は 1 以上です（%d）", name, *v)
			}
		}
		if m.AllowedOrigins != nil {
			for _, o := range *m.AllowedOrigins {
				if strings.Contains(o, "*") {
					add("mcp.allowedOrigins にワイルドカードは使えません（%q）", o)
				}
			}
		}
	}
	if p := c.Policy; p != nil {
		for name, v := range map[string]*int{"defaultConnectTimeout": p.DefaultConnectTimeout, "defaultCommandTimeout": p.DefaultCommandTimeout} {
			if v != nil && *v <= 0 {
				add("policy.%s は 1 以上です（%d）", name, *v)
			}
		}
	}
	if webHost, webPort, err := splitListen(c.webListenOrDefault()); err == nil && c.MCP != nil {
		mcpHost, mcpPort := "127.0.0.1", 8787
		if c.MCP.ListenAddress != nil {
			mcpHost = strings.Trim(strings.TrimSpace(*c.MCP.ListenAddress), "[]")
		}
		if c.MCP.Port != nil {
			mcpPort = *c.MCP.Port
		}
		if webPort == mcpPort && listenOverlaps(webHost, mcpHost) {
			add("web.listen（%s）と MCP の待受（%s:%d）が同じです", c.webListenOrDefault(), mcpHost, mcpPort)
		}
	}

	switch c.Secrets.Backend {
	case "", "keychain", "file", "env":
	case "memory":
		// Every saved password would be lost on restart (REQ-014: no silent
		// fallback). Only test binaries may use it.
		if !memorySecretsAllowed() {
			add("secrets.backend: memory はテスト専用です。keychain / file / env のどれかにしてください")
		}
	default:
		add("secrets.backend は keychain / file / env のどれかです（%q）", c.Secrets.Backend)
	}
	if c.Secrets.File != "" && c.Secrets.Backend != "file" {
		add("secrets.file は backend: file のときだけ書けます")
	}

	seen := map[string]bool{}
	for i, cc := range c.Connections {
		label := fmt.Sprintf("connections[%d]", i)
		name := strings.TrimSpace(cc.Name)
		if name == "" {
			add("%s: name は必須です", label)
		} else {
			label = fmt.Sprintf("connections[%d] (%s)", i, name)
			if seen[name] {
				add("%s: name が重複しています", label)
			}
			seen[name] = true
		}
		for _, msg := range cc.validate() {
			add("%s: %s", label, msg)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func (c *FileConfig) webListenOrDefault() string {
	if c == nil || strings.TrimSpace(c.Web.Listen) == "" {
		return defaultWebListen
	}
	return strings.TrimSpace(c.Web.Listen)
}

// splitListen parses "host:port" (IPv6 in brackets). The host may be empty
// only as an error.
func splitListen(addr string) (string, int, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, fmt.Errorf("%q は host:port の形にしてください", addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || !validPort(port) {
		return "", 0, fmt.Errorf("%q のポートは 1〜65535 です", addr)
	}
	if host == "" {
		return "", 0, fmt.Errorf("%q にホストがありません（全インターフェースなら 0.0.0.0 と書いてください）", addr)
	}
	return host, port, nil
}

// listenOverlaps reports whether two listen hosts can collide on one port:
// equal, or either one is a wildcard.
func listenOverlaps(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.Trim(strings.TrimSpace(h), "[]"))
		if h == "localhost" {
			return "127.0.0.1"
		}
		return h
	}
	a, b = norm(a), norm(b)
	wild := func(h string) bool { return h == "0.0.0.0" || h == "::" }
	return a == b || wild(a) || wild(b)
}

func (cc ConnectionConfig) isSerial() bool {
	return cc.Type != nil && strings.EqualFold(strings.TrimSpace(*cc.Type), "serial")
}

func (cc ConnectionConfig) usesPasswordAuth() bool {
	return cc.Auth != nil && strings.EqualFold(strings.TrimSpace(*cc.Auth), "password")
}

func (cc ConnectionConfig) validate() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if cc.Type != nil {
		switch strings.ToLower(strings.TrimSpace(*cc.Type)) {
		case "ssh", "serial":
		default:
			add("type は ssh か serial です（%q）", *cc.Type)
		}
	}
	if cc.Auth != nil {
		switch strings.ToLower(strings.TrimSpace(*cc.Auth)) {
		case "key", "password":
		default:
			add("auth は key か password です（%q）", *cc.Auth)
		}
	}
	if cc.PasswordEnv != "" && cc.PasswordFile != "" {
		add("passwordEnv と passwordFile は同時に書けません")
	}
	if (cc.PasswordEnv != "" || cc.PasswordFile != "") && !cc.usesPasswordAuth() {
		add("passwordEnv / passwordFile は auth: password のときだけ書けます")
	}
	for key, name := range map[string]string{"passwordEnv": cc.PasswordEnv, "passphraseEnv": cc.PassphraseEnv} {
		if name != "" && !envNamePattern.MatchString(name) {
			add("%s %q は環境変数名として使えません", key, name)
		}
	}
	if cc.PassphraseEnv != "" && cc.usesPasswordAuth() {
		add("passphraseEnv は auth: key のときだけ書けます")
	}
	if cc.KeyPath != nil && cc.usesPasswordAuth() {
		add("keyPath は auth: key のときだけ書けます")
	}
	if cc.Port != nil && !validPort(*cc.Port) {
		add("port は 1〜65535 です（%d）", *cc.Port)
	}
	for key, v := range map[string]*int{"connectTimeout": cc.ConnectTimeout, "commandTimeout": cc.CommandTimeout} {
		if v != nil && *v <= 0 {
			add("%s は 1 以上です（%d）", key, *v)
		}
	}
	if cc.isSerial() {
		if cc.SerialPort == nil || strings.TrimSpace(*cc.SerialPort) == "" {
			add("type: serial には serialPort が必要です")
		}
		for key, set := range map[string]bool{"host": cc.Host != nil, "user": cc.User != nil, "auth": cc.Auth != nil,
			"keyPath": cc.KeyPath != nil, "passphraseEnv": cc.PassphraseEnv != "", "mcpFirstTrust": cc.MCPFirstTrust != nil} {
			if set {
				add("%s は type: ssh のときだけ書けます", key)
			}
		}
		if err := validateSerialSettings(serialGOOS, serialSettingsOf(cc.toConnection())); err != nil {
			add("%v", err)
		}
	} else {
		if cc.Host == nil || strings.TrimSpace(*cc.Host) == "" {
			add("host は必須です")
		}
		if cc.User == nil || strings.TrimSpace(*cc.User) == "" {
			add("user は必須です")
		}
		for key, set := range map[string]bool{"serialPort": cc.SerialPort != nil, "baudRate": cc.BaudRate != nil,
			"dataBits": cc.DataBits != nil, "parity": cc.Parity != nil, "stopBits": cc.StopBits != nil} {
			if set {
				add("%s は type: serial のときだけ書けます", key)
			}
		}
	}
	sort.Strings(errs) // map iteration above; keep messages stable
	return errs
}

func (c *FileConfig) applyMode() string {
	if c == nil || c.Apply == "" {
		return configApplySeed
	}
	return c.Apply
}

// resolveWebConfig merges the file's web: section with the command line
// (flags win) and fills the defaults. c may be nil (no --config).
func resolveWebConfig(c *FileConfig, opts CLIOptions) (WebConfig, error) {
	var w WebConfig
	if c != nil {
		w = c.Web
	}
	if s := strings.TrimSpace(opts.Listen); s != "" {
		w.Listen = s
	}
	w.Listen = strings.TrimSpace(w.Listen)
	if w.Listen == "" {
		w.Listen = defaultWebListen
	}
	if _, _, err := splitListen(w.Listen); err != nil {
		return WebConfig{}, fmt.Errorf("--listen / web.listen: %w", err)
	}
	if opts.TLSSelfSigned && (opts.TLSCert != "" || opts.TLSKey != "") {
		return WebConfig{}, errors.New("--tls-self-signed と --tls-cert / --tls-key は同時に指定できません")
	}
	if (opts.TLSCert == "") != (opts.TLSKey == "") {
		return WebConfig{}, errors.New("--tls-cert と --tls-key は両方指定してください")
	}
	switch {
	case opts.TLSCert != "":
		w.TLS = WebTLSConfig{Mode: "files", Cert: opts.TLSCert, Key: opts.TLSKey}
	case opts.TLSSelfSigned:
		w.TLS = WebTLSConfig{Mode: "self-signed"}
	}
	if w.TLS.Mode == "" {
		w.TLS.Mode = "off"
	}
	w.PublicURL = strings.TrimRight(strings.TrimSpace(w.PublicURL), "/")
	if w.Session.IdleMinutes == 0 {
		w.Session.IdleMinutes = defaultWebIdleMinutes
	}
	if w.Session.MaxHours == 0 {
		w.Session.MaxHours = defaultWebMaxHours
	}
	if w.MaxSessions == 0 {
		w.MaxSessions = defaultWebMaxSessions
	}
	return w, nil
}

// adminTokenPath is web.adminTokenFile, or <dataDir>/admin-token.
func adminTokenPath(w WebConfig, dataDir string) string {
	if p := strings.TrimSpace(w.AdminTokenFile); p != "" {
		return p
	}
	return filepath.Join(dataDir, "admin-token")
}

// adminPasswordPath is web.adminPasswordFile, or <dataDir>/admin-password.
func adminPasswordPath(w WebConfig, dataDir string) string {
	if p := strings.TrimSpace(w.AdminPasswordFile); p != "" {
		return p
	}
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	return filepath.Join(dataDir, adminPasswordFileName)
}

// secretsBackend is the effective backend: SSH_GETE_KEYRING (file / env,
// and memory only in a test binary) wins over the file, for tests and
// emergencies (§7.1, keyringEnvOverride).
func secretsBackend(c *FileConfig) string {
	if v := keyringEnvOverride(); v != "" {
		return v
	}
	if c != nil && c.Secrets.Backend != "" {
		return c.Secrets.Backend
	}
	return "keychain"
}

// applyConfigEnv runs before NewApp (§1.5 step 2): mcp.tokenEnv other than
// SSH_GETE_TOKEN is copied into SSH_GETE_TOKEN so the existing env token path
// (mcpEnvToken, never persisted) carries it.
func applyConfigEnv(c *FileConfig) error {
	if c == nil || c.MCP == nil || c.MCP.TokenEnv == "" || c.MCP.TokenEnv == "SSH_GETE_TOKEN" {
		return nil
	}
	v, ok := os.LookupEnv(c.MCP.TokenEnv)
	if !ok || strings.TrimSpace(v) == "" {
		return fmt.Errorf("mcp.tokenEnv の環境変数 %s が設定されていません", c.MCP.TokenEnv)
	}
	return os.Setenv("SSH_GETE_TOKEN", v)
}

// toConnection is the entry as a new Connection, with the defaults
// normalizeConnection would give the fields left out.
func (cc ConnectionConfig) toConnection() Connection {
	conn := Connection{Type: "SSH", AuthMethod: configAuthKey, Tags: []string{}}
	cc.overlay(&conn)
	return conn
}

const (
	configAuthKey      = "SSH鍵認証"
	configAuthPassword = "パスワード保管"
)

// overlay writes the fields the entry sets onto conn and returns the names
// of the fields whose value changed. The password itself is never touched:
// saveConnection keeps the stored one when Credential is left empty.
func (cc ConnectionConfig) overlay(conn *Connection) []string {
	var changed []string
	setStr := func(field string, dst *string, src *string, conv func(string) string) {
		if src == nil {
			return
		}
		v := *src
		if conv != nil {
			v = conv(v)
		}
		if *dst != v {
			*dst = v
			changed = append(changed, field)
		}
	}
	setInt := func(field string, dst *int, src *int) {
		if src != nil && *dst != *src {
			*dst = *src
			changed = append(changed, field)
		}
	}
	setStr("name", &conn.Name, &cc.Name, strings.TrimSpace)
	setStr("type", &conn.Type, cc.Type, func(s string) string {
		if strings.EqualFold(strings.TrimSpace(s), "serial") {
			return "Serial"
		}
		return "SSH"
	})
	setStr("host", &conn.Host, cc.Host, func(s string) string { return strings.ToLower(strings.TrimSpace(s)) })
	setInt("port", &conn.Port, cc.Port)
	setStr("user", &conn.User, cc.User, strings.TrimSpace)

	wasPassword := conn.usesPassword()
	setStr("auth", &conn.AuthMethod, cc.Auth, func(s string) string {
		if strings.EqualFold(strings.TrimSpace(s), "password") {
			return configAuthPassword
		}
		return configAuthKey
	})
	switch {
	case conn.Type == "Serial":
	case conn.usesPassword():
		if !wasPassword {
			// The column held a key path: it must not become the password.
			conn.Credential, conn.CredentialSet = "", false
		}
	default:
		if wasPassword {
			// The column may hold a legacy plaintext password.
			conn.Credential, conn.CredentialSet = "", false
		}
		setStr("keyPath", &conn.Credential, cc.KeyPath, strings.TrimSpace)
	}
	if cc.Tags != nil {
		tags := append([]string{}, (*cc.Tags)...)
		if !equalStrings(conn.Tags, tags) {
			conn.Tags = tags
			changed = append(changed, "tags")
		}
	}
	setStr("description", &conn.Description, cc.Description, nil)
	setInt("connectTimeout", &conn.ConnectTimeout, cc.ConnectTimeout)
	setInt("commandTimeout", &conn.CommandTimeout, cc.CommandTimeout)
	if cc.MCPFirstTrust != nil && conn.MCPFirstTrust != *cc.MCPFirstTrust {
		conn.MCPFirstTrust = *cc.MCPFirstTrust
		changed = append(changed, "mcpFirstTrust")
	}
	setStr("serialPort", &conn.SerialPort, cc.SerialPort, strings.TrimSpace)
	setInt("baudRate", &conn.BaudRate, cc.BaudRate)
	setInt("dataBits", &conn.DataBits, cc.DataBits)
	setStr("parity", &conn.Parity, cc.Parity, func(s string) string { return strings.ToLower(strings.TrimSpace(s)) })
	setStr("stopBits", &conn.StopBits, cc.StopBits, strings.TrimSpace)
	return changed
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// configHash is the sha256 of the item's written keys as JSON (pointer fields
// left out by omitempty), so adding, removing or changing a key changes it.
func configHash(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err) // plain structs of strings, ints and bools
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Applying the file to the database (§1.4)

// ConfigApplyReport lists what the file did, by item ("connection:<name>",
// "mcp", "policy"). Overridden entries carry the field names in brackets.
// Removed are items the file once created and no longer lists, kept because
// prune is off.
type ConfigApplyReport struct {
	Created    []string
	Overridden []string
	Kept       []string
	Pruned     []string
	Removed    []string
	// Notes are things the operator should know (secrets that could not be
	// bound, SSH_GETE_MCP_ADDR winning over the file, ...).
	Notes []string
}

// Summary is the one-line form for the startup banner.
func (r ConfigApplyReport) Summary() string {
	s := fmt.Sprintf("%d created, %d overridden, %d kept", len(r.Created), len(r.Overridden), len(r.Kept))
	if len(r.Pruned) > 0 {
		s += fmt.Sprintf(", %d pruned", len(r.Pruned))
	}
	if len(r.Removed) > 0 {
		s += fmt.Sprintf(", %d no longer in file (kept)", len(r.Removed))
	}
	return s
}

// configAudit records an admin_audit entry for a config action
// (config_create / config_override / config_prune). nil records nothing.
type configAudit func(action, target, detail string)

// configSecretBinder is the part of envSecretStore applyConfig needs
// (§7.3): reads of account are served from the environment variable.
type configSecretBinder interface {
	bind(account, envName string)
}

const configAppliedSchema = `CREATE TABLE IF NOT EXISTS config_applied (
  kind       TEXT NOT NULL,
  key        TEXT NOT NULL,
  conn_id    TEXT NOT NULL DEFAULT '',
  hash       TEXT NOT NULL,
  applied_at TEXT NOT NULL,
  PRIMARY KEY (kind, key)
)`

// ensureConfigAppliedTable creates config_applied. user_version is not
// raised so storage.go migrations stay untouched.
func ensureConfigAppliedTable(s *Store) error {
	if s == nil || s.db == nil {
		return errStoreUnavailable
	}
	_, err := s.db.Exec(configAppliedSchema)
	return err
}

type configAppliedRow struct {
	Kind, Key, ConnID, Hash string
}

// loadConfigApplied reads config_applied; a missing table is empty.
func loadConfigApplied(s *Store) (map[string]configAppliedRow, error) {
	out := map[string]configAppliedRow{}
	if s == nil || s.db == nil {
		return out, nil
	}
	var name string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='config_applied'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT kind, key, conn_id, hash FROM config_applied`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r configAppliedRow
		if err := rows.Scan(&r.Kind, &r.Key, &r.ConnID, &r.Hash); err != nil {
			return nil, err
		}
		out[r.Kind+"\x00"+r.Key] = r
	}
	return out, rows.Err()
}

func recordConfigApplied(s *Store, r configAppliedRow, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO config_applied (kind, key, conn_id, hash, applied_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(kind, key) DO UPDATE SET conn_id = excluded.conn_id, hash = excluded.hash, applied_at = excluded.applied_at`,
		r.Kind, r.Key, r.ConnID, r.Hash, now.UTC().Format(time.RFC3339))
	return err
}

func deleteConfigApplied(s *Store, kind, key string) error {
	_, err := s.db.Exec(`DELETE FROM config_applied WHERE kind = ? AND key = ?`, kind, key)
	return err
}

type configAction int

const (
	actionCreate configAction = iota
	actionOverride
	actionKeep
	actionPrune
	actionRemoved
)

// configStep is one decision of the plan. Executing it is applyConfig's job;
// planConfig only reports.
type configStep struct {
	action configAction
	kind   string // connection | mcp | policy
	key    string
	hash   string
	label  string
	fields []string

	entry   *ConnectionConfig
	conn    Connection // connection: the row to save (overlay applied)
	connID  string     // connection: existing row / prune target
	mcp     MCPSettings
	pruneID string
}

func (s configStep) reportLabel() string {
	if s.action == actionOverride && len(s.fields) > 0 {
		return s.label + " [" + strings.Join(s.fields, " ") + "]"
	}
	return s.label
}

// planSteps decides every item against the database (§1.4) without writing.
func planSteps(a *App, c *FileConfig) ([]configStep, []string, error) {
	applied, err := loadConfigApplied(a.store)
	if err != nil {
		return nil, nil, fmt.Errorf("config_applied を読めません: %w", err)
	}
	enforce := c.applyMode() == configApplyEnforce
	a.mu.RLock()
	conns := make([]Connection, len(a.conns))
	copy(conns, a.conns)
	for i := range conns {
		conns[i].Tags = append([]string{}, conns[i].Tags...)
	}
	mcp := a.mcp
	mcp.AllowedOrigins = append([]string{}, a.mcp.AllowedOrigins...)
	a.mu.RUnlock()

	byID := map[string]int{}
	byName := map[string]int{}
	for i, conn := range conns {
		byID[conn.ID] = i
		byName[conn.Name] = i
	}

	var steps []configStep
	var notes []string
	claimed := map[string]bool{}
	for i := range c.Connections {
		cc := &c.Connections[i]
		name := strings.TrimSpace(cc.Name)
		step := configStep{kind: "connection", key: name, hash: configHash(cc), label: "connection:" + name, entry: cc}
		rec, hasRec := applied["connection\x00"+name]
		idx, found := -1, false
		if hasRec && rec.ConnID != "" {
			idx, found = byID[rec.ConnID]
		}
		if !found {
			idx, found = byName[name]
		}
		if found && claimed[conns[idx].ID] {
			found = false // another entry already maps to this row
		}
		switch {
		case !found:
			step.action = actionCreate
			step.conn = cc.toConnection()
		case hasRec && rec.Hash == step.hash && !enforce:
			step.action = actionKeep
			step.connID = conns[idx].ID
		default:
			step.action = actionOverride
			conn := conns[idx]
			step.connID = conn.ID
			step.fields = cc.overlay(&conn)
			step.conn = conn
		}
		if found {
			claimed[conns[idx].ID] = true
		}
		steps = append(steps, step)
	}

	listed := map[string]bool{}
	for _, cc := range c.Connections {
		listed[strings.TrimSpace(cc.Name)] = true
	}
	var gone []configAppliedRow
	for _, rec := range applied {
		if rec.Kind == "connection" && !listed[rec.Key] {
			gone = append(gone, rec)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].Key < gone[j].Key })
	for _, rec := range gone {
		step := configStep{kind: "connection", key: rec.Key, label: "connection:" + rec.Key, action: actionRemoved}
		if c.Prune {
			step.action = actionPrune
			if _, ok := byID[rec.ConnID]; ok && !claimed[rec.ConnID] {
				step.pruneID = rec.ConnID
			}
		}
		steps = append(steps, step)
	}

	if c.MCP != nil || c.Policy != nil {
		envAddr := strings.TrimSpace(os.Getenv("SSH_GETE_MCP_ADDR")) != ""
		if envAddr && c.MCP != nil && (c.MCP.ListenAddress != nil || c.MCP.Port != nil) {
			notes = append(notes, "SSH_GETE_MCP_ADDR が設定されているため、mcp.listenAddress / mcp.port は反映しません")
		}
		next := mcp
		decide := func(kind string, item any, overlay func() []string) {
			step := configStep{kind: kind, key: kind, hash: configHash(item), label: kind}
			rec, hasRec := applied[kind+"\x00"+kind]
			if hasRec && rec.Hash == step.hash && !enforce {
				step.action = actionKeep
			} else {
				step.action = actionOverride
				step.fields = overlay()
			}
			steps = append(steps, step)
		}
		if c.MCP != nil {
			decide("mcp", c.MCP, func() []string { return c.MCP.overlay(&next, envAddr) })
		}
		if c.Policy != nil {
			decide("policy", c.Policy, func() []string { return c.Policy.overlay(&next) })
		}
		// Both steps save the same merged settings; only overridden parts
		// changed next.
		for i := range steps {
			if steps[i].kind == "mcp" || steps[i].kind == "policy" {
				steps[i].mcp = next
			}
		}
	}
	return steps, notes, nil
}

func (m *MCPConfig) overlay(s *MCPSettings, skipListen bool) []string {
	var changed []string
	setBool := func(field string, dst *bool, src *bool) {
		if src != nil && *dst != *src {
			*dst = *src
			changed = append(changed, field)
		}
	}
	setInt := func(field string, dst *int, src *int) {
		if src != nil && *dst != *src {
			*dst = *src
			changed = append(changed, field)
		}
	}
	setBool("enabled", &s.Enabled, m.Enabled)
	if !skipListen {
		if m.ListenAddress != nil {
			v := strings.Trim(strings.TrimSpace(*m.ListenAddress), "[]")
			if s.ListenAddress != v {
				s.ListenAddress = v
				changed = append(changed, "listenAddress")
			}
		}
		setInt("port", &s.Port, m.Port)
	}
	setBool("bearerEnabled", &s.BearerEnabled, m.BearerEnabled)
	if m.AllowedOrigins != nil {
		origins := append([]string{}, (*m.AllowedOrigins)...)
		if !equalStrings(s.AllowedOrigins, origins) {
			s.AllowedOrigins = origins
			changed = append(changed, "allowedOrigins")
		}
	}
	setInt("requestTimeout", &s.RequestTimeout, m.RequestTimeout)
	setInt("maxOutputKB", &s.MaxOutputKB, m.MaxOutputKB)
	setInt("maxBodyKB", &s.MaxBodyKB, m.MaxBodyKB)
	return changed
}

func (p *PolicyConfig) overlay(s *MCPSettings) []string {
	var changed []string
	for _, f := range []struct {
		name string
		dst  *bool
		src  *bool
	}{
		{"strictHostKey", &s.StrictHostKey, p.StrictHostKey},
		{"autoExecuteLowRisk", &s.AutoExecuteLowRisk, p.AutoExecuteLowRisk},
		{"requireApprovalSudo", &s.RequireApprovalSudo, p.RequireApprovalSudo},
		{"requireApprovalProd", &s.RequireApprovalProd, p.RequireApprovalProd},
		{"requireApprovalWriteOp", &s.RequireApprovalWriteOp, p.RequireApprovalWriteOp},
	} {
		if f.src != nil && *f.dst != *f.src {
			*f.dst = *f.src
			changed = append(changed, f.name)
		}
	}
	for _, f := range []struct {
		name string
		dst  *int
		src  *int
	}{
		{"defaultConnectTimeout", &s.DefaultConnectTimeout, p.DefaultConnectTimeout},
		{"defaultCommandTimeout", &s.DefaultCommandTimeout, p.DefaultCommandTimeout},
	} {
		if f.src != nil && *f.dst != *f.src {
			*f.dst = *f.src
			changed = append(changed, f.name)
		}
	}
	return changed
}

func reportOf(steps []configStep, notes []string) ConfigApplyReport {
	r := ConfigApplyReport{Notes: notes}
	for _, s := range steps {
		switch s.action {
		case actionCreate:
			r.Created = append(r.Created, s.reportLabel())
		case actionOverride:
			r.Overridden = append(r.Overridden, s.reportLabel())
		case actionKeep:
			r.Kept = append(r.Kept, s.reportLabel())
		case actionPrune:
			r.Pruned = append(r.Pruned, s.reportLabel())
		case actionRemoved:
			r.Removed = append(r.Removed, s.reportLabel())
		}
	}
	return r
}

// planConfig is --check-config: the same decisions as applyConfig, nothing
// written.
func planConfig(a *App, c *FileConfig) (ConfigApplyReport, error) {
	if c == nil {
		return ConfigApplyReport{}, nil
	}
	steps, notes, err := planSteps(a, c)
	if err != nil {
		return ConfigApplyReport{}, err
	}
	return reportOf(steps, notes), nil
}

// applyConfig is §1.5 step 6, run after NewApp and before startup (the MCP
// listener is not running yet). Connections go through SaveConnection so
// validation, secret handling and conflict checks stay in one place; the MCP
// and policy settings go through applyMCPSettingsNoRestart. Secrets named by
// the file are bound on every start (binder may be nil: then they are kept
// for this process only, like "use for this session").
func applyConfig(a *App, c *FileConfig, binder configSecretBinder, audit configAudit) (ConfigApplyReport, error) {
	if c == nil {
		return ConfigApplyReport{}, nil
	}
	if a.store == nil {
		return ConfigApplyReport{}, errStoreUnavailable
	}
	if err := ensureConfigAppliedTable(a.store); err != nil {
		return ConfigApplyReport{}, fmt.Errorf("config_applied を作れません: %w", err)
	}
	steps, notes, err := planSteps(a, c)
	if err != nil {
		return ConfigApplyReport{}, err
	}
	record := func(action, target, detail string) {
		if audit != nil {
			audit(action, target, detail)
		}
	}
	now := a.clock()
	backend := secretsBackend(c)
	mcpSaved := false
	for i := range steps {
		step := &steps[i]
		switch step.kind {
		case "connection":
			switch step.action {
			case actionCreate, actionOverride:
				result := a.SaveConnection(step.conn)
				if !result.OK {
					return ConfigApplyReport{}, fmt.Errorf("接続先 %s を保存できません: %s", step.key, result.Message)
				}
				step.connID = result.ID
				if err := recordConfigApplied(a.store, configAppliedRow{Kind: "connection", Key: step.key, ConnID: result.ID, Hash: step.hash}, now); err != nil {
					return ConfigApplyReport{}, fmt.Errorf("config_applied に記録できません: %w", err)
				}
				if step.action == actionCreate {
					record("config_create", step.label, "")
				} else {
					record("config_override", step.label, strings.Join(step.fields, ","))
					if len(step.fields) > 0 {
						log.Printf("config: %s overridden from file: %s", step.label, strings.Join(step.fields, ", "))
					}
				}
			case actionKeep:
				// Keep the DB row but keep the record's conn_id current.
			case actionPrune:
				if step.pruneID != "" {
					result := a.DeleteConnection(step.pruneID)
					if !result.OK {
						return ConfigApplyReport{}, fmt.Errorf("接続先 %s を削除できません: %s", step.key, result.Message)
					}
				}
				if err := deleteConfigApplied(a.store, "connection", step.key); err != nil {
					return ConfigApplyReport{}, err
				}
				record("config_prune", step.label, "")
			case actionRemoved:
				log.Printf("config: %s is no longer in the file; kept in the database (prune: false)", step.label)
			}
			if step.entry != nil && step.connID != "" {
				notes = append(notes, a.bindConfigSecrets(step.connID, *step.entry, backend, binder)...)
			}
		case "mcp", "policy":
			if step.action != actionOverride {
				continue
			}
			if !mcpSaved {
				if err := a.applyMCPSettingsNoRestart(step.mcp); err != nil {
					return ConfigApplyReport{}, fmt.Errorf("MCP の設定を反映できません: %w", err)
				}
				mcpSaved = true
			}
			if err := recordConfigApplied(a.store, configAppliedRow{Kind: step.kind, Key: step.key, Hash: step.hash}, now); err != nil {
				return ConfigApplyReport{}, fmt.Errorf("config_applied に記録できません: %w", err)
			}
			record("config_override", step.label, strings.Join(step.fields, ","))
		}
	}
	return reportOf(steps, notes), nil
}

// bindConfigSecrets connects the entry's passwordEnv / passwordFile /
// passphraseEnv to the saved connection id (§7.3). It returns notes for the
// startup log; the secret values never appear in them.
func (a *App) bindConfigSecrets(connID string, cc ConnectionConfig, backend string, binder configSecretBinder) []string {
	var notes []string
	label := "connection:" + strings.TrimSpace(cc.Name)
	conn, ok := a.connectionByID(connID)
	if !ok {
		return nil
	}
	if cc.PasswordEnv != "" && conn.usesPassword() {
		value, set := os.LookupEnv(cc.PasswordEnv)
		switch {
		case binder != nil:
			binder.bind(passwordAccount(connID), cc.PasswordEnv)
			if set && value != "" {
				conn.Credential = value
				if r := a.SaveConnection(conn); !r.OK {
					notes = append(notes, fmt.Sprintf("%s: %s のパスワードを保存できません: %s", label, cc.PasswordEnv, r.Message))
				}
			}
		case set && value != "":
			if r := a.UseCredentialForSession(connID, value); !r.OK {
				notes = append(notes, fmt.Sprintf("%s: %s のパスワードを使えません: %s", label, cc.PasswordEnv, r.Message))
			}
		}
		if !set || value == "" {
			notes = append(notes, fmt.Sprintf("%s: 環境変数 %s が空です（パスワードは Web 画面から入れられます）", label, cc.PasswordEnv))
		}
	}
	if cc.PasswordFile != "" && conn.usesPassword() {
		value, err := readSecretFile(cc.PasswordFile)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("%s: passwordFile: %v", label, err))
		case backend == "keychain" || backend == "file":
			conn.Credential = value
			if r := a.SaveConnection(conn); !r.OK {
				// No usable keychain on this host: keep it for this process.
				if r2 := a.UseCredentialForSession(connID, value); !r2.OK {
					notes = append(notes, fmt.Sprintf("%s: passwordFile のパスワードを使えません: %s", label, r2.Message))
				} else {
					notes = append(notes, fmt.Sprintf("%s: passwordFile のパスワードを保存できないため、このプロセスの間だけ使います（%s）", label, r.Message))
				}
			}
		default:
			if r := a.UseCredentialForSession(connID, value); !r.OK {
				notes = append(notes, fmt.Sprintf("%s: passwordFile のパスワードを使えません: %s", label, r.Message))
			}
		}
	}
	if cc.PassphraseEnv != "" && !conn.usesPassword() && conn.connectionType() != "Serial" {
		if value, set := os.LookupEnv(cc.PassphraseEnv); set && value != "" {
			// Like a passphrase typed for this session: memory only, read
			// again from the environment on every start.
			a.mu.Lock()
			a.sshPassphrases[connID] = value
			a.mu.Unlock()
			if binder != nil {
				binder.bind(passphraseAccount(connID, conn.Credential), cc.PassphraseEnv)
			}
		} else {
			notes = append(notes, fmt.Sprintf("%s: 環境変数 %s が空です", label, cc.PassphraseEnv))
		}
	}
	return notes
}

// readSecretFile reads a passwordFile: owner-only on Unix (like ssh keys),
// bounded, one trailing newline removed.
func readSecretFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s の権限が広すぎます（%04o）。chmod 600 %s を実行してください", path, info.Mode().Perm(), path)
	}
	data, err := io.ReadAll(io.LimitReader(f, configSecretFileMax+1))
	if err != nil {
		return "", err
	}
	if len(data) > configSecretFileMax {
		return "", fmt.Errorf("%s が大きすぎます", path)
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if s == "" {
		return "", fmt.Errorf("%s が空です", path)
	}
	return s, nil
}

// applyMCPSettingsNoRestart is SaveMCPSettings without restartMCPServer, for
// applying the file before startup (§1.5 step 6): startup then starts the
// listener once. It refuses while a listener is running.
func (a *App) applyMCPSettingsNoRestart(settings MCPSettings) error {
	if a.store == nil {
		return errStoreUnavailable
	}
	a.lifecycleMu.Lock()
	running := a.mcpServer != nil
	a.lifecycleMu.Unlock()
	if running {
		return errors.New("MCP サーバーの動作中は使えません（SaveMCPSettings を使ってください）")
	}
	settings.ConfigPath = a.dbPath
	settings.TokenInput = ""

	a.mu.Lock()
	defer a.mu.Unlock()
	token := a.mcpToken
	if a.mcpEnvToken != "" {
		settings.BearerEnabled = true
	}
	effective := token
	if a.mcpEnvToken != "" {
		effective = a.mcpEnvToken
	}
	settings = sanitizeMCPSettings(settings, effective, "再起動中", "")
	if err := validateListenSettings(settings, effective); err != nil {
		return err
	}
	if err := a.store.saveMCPSettings(settings, token); err != nil {
		return fmt.Errorf("MCP設定のSQLite保存に失敗しました: %w", err)
	}
	// What startup shows until the listener reports in.
	settings.Status = "停止中"
	a.mcp = settings
	return nil
}

// openStoreReadOnly opens the database for --check-config without creating
// or migrating it. A missing file gives (nil, nil): everything is "created".
func openStoreReadOnly(path string) (*Store, error) {
	if !fileExists(path) {
		return nil, nil
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// checkConfigApp builds the minimal App planConfig needs from a read-only
// store: connections and MCP settings as NewApp would load them.
func checkConfigApp(dbPath string) (*App, func(), error) {
	a := &App{dbPath: dbPath, now: time.Now, mcp: defaultMCPSettings(dbPath)}
	store, err := openStoreReadOnly(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("データベースを開けません: %w", err)
	}
	if store == nil {
		return a, func() {}, nil
	}
	a.store = store
	conns, err := store.loadConnections()
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("接続先を読めません（古い形式なら一度通常どおり起動してください）: %w", err)
	}
	a.conns = conns
	if mcp, _, loaded, err := store.loadMCPSettings(dbPath); err == nil && loaded {
		a.mcp = mcp
	}
	applyMCPEnv(&a.mcp)
	return a, func() { _ = store.Close() }, nil
}

// formatReport writes the --check-config table.
func formatReport(w io.Writer, r ConfigApplyReport) {
	section := func(title string, items []string) {
		for _, it := range items {
			fmt.Fprintf(w, "  %-10s %s\n", title, it)
		}
	}
	section("create", r.Created)
	section("override", r.Overridden)
	section("keep", r.Kept)
	section("delete", r.Pruned)
	section("orphan", r.Removed)
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note       %s\n", n)
	}
	fmt.Fprintf(w, "summary: %s\n", r.Summary())
}
