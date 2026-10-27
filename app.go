package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// requestIDSeq guarantees per-process uniqueness of command request IDs even
// when several requests are created within the same millisecond (e.g. an agent
// with approval-bypass firing commands in parallel). Without this the
// millisecond-resolution ID collides and the SQLite upsert overwrites earlier
// history rows.
var requestIDSeq atomic.Uint64

const (
	appName          = "ssh-gete"
	defaultMCPAddr   = "127.0.0.1:8787"
	defaultOutputCap = 128 * 1024
	maxMCPAgents     = 256
)

type App struct {
	ctx               context.Context
	mu                sync.RWMutex
	store             *Store
	storageError      string
	dbPath            string
	legacyConnections string
	legacyMCPSettings string
	conns             []Connection
	requests          []CommandRequest
	mcp               MCPSettings
	mcpToken          string
	// mcpEnvToken comes from SSH_GETE_TOKEN. It overrides mcpToken at runtime
	// and is never written to SQLite.
	mcpEnvToken    string
	mcpAgents      map[string]time.Time
	agentPolicies  map[string]AgentPolicy
	sshPassphrases map[string]string
	// requestsChanged is closed and replaced whenever a request changes state,
	// waking get_request_status long-polls.
	requestsChanged chan struct{}

	lifecycleMu sync.Mutex
	mcpServer   *http.Server

	termMu sync.Mutex
	term   *terminalSession
}

type Connection struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	AuthMethod string `json:"authMethod"`
	// Credential is the key path for key auth, or the password for password
	// auth. Passwords are write-only: they are never sent back to the UI.
	Credential      string   `json:"credential"`
	CredentialSet   bool     `json:"credentialSet"`
	KeyPassphrase   string   `json:"keyPassphrase,omitempty"`
	SerialPort      string   `json:"serialPort"`
	BaudRate        int      `json:"baudRate"`
	ConnectTimeout  int      `json:"connectTimeout"`
	CommandTimeout  int      `json:"commandTimeout"`
	Tags            []string `json:"tags"`
	Description     string   `json:"description"`
	MCPFirstTrust   bool     `json:"mcpFirstTrust"`
	LastTestResult  string   `json:"lastTestResult"`
	LastTestAt      string   `json:"lastTestAt"`
	LastTestMessage string   `json:"lastTestMessage"`
}

// connectionType normalizes the transport, defaulting legacy rows to SSH.
func (c Connection) connectionType() string {
	if c.Type == "Serial" {
		return "Serial"
	}
	return "SSH"
}

func (c Connection) usesPassword() bool {
	return strings.Contains(c.AuthMethod, "パスワード")
}

// Request state codes. The UI translates them; they are also the values the
// MCP tools return.
const (
	statePending       = "pending"
	stateRunning       = "running"
	stateSucceeded     = "succeeded"
	stateFailed        = "failed"
	stateRejected      = "rejected"
	stateInterrupted   = "interrupted"
	stateAutoRunning   = "auto_running"
	stateAutoSucceeded = "auto_succeeded"
	stateAutoFailed    = "auto_failed"
)

type CommandRequest struct {
	ID              string `json:"id"`
	ConnectionID    string `json:"connectionId"`
	Host            string `json:"host"`
	User            string `json:"user"`
	Command         string `json:"command"`
	ExecutedCommand string `json:"executedCommand"`
	Reason          string `json:"reason"`
	RequestedBy     string `json:"requestedBy"`
	RequestedAt     string `json:"requestedAt"`
	Risk            string `json:"risk"`
	ApprovedRisk    string `json:"approvedRisk"`
	Status          string `json:"status"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	Duration        string `json:"duration"`
	DecidedBy       string `json:"decidedBy"`
	DecidedAt       string `json:"decidedAt"`
	FinishedAt      string `json:"finishedAt"`
	ExitCode        *int   `json:"exitCode"`
	ErrorKind       string `json:"errorKind"`
	ClientRequestID string `json:"clientRequestId,omitempty"`
}

type AgentPolicy struct {
	AgentName      string `json:"agentName"`
	ApprovalBypass bool   `json:"approvalBypass"`
	UpdatedAt      string `json:"updatedAt"`
}

type MCPSettings struct {
	Enabled                bool     `json:"enabled"`
	ListenAddress          string   `json:"listenAddress"`
	Port                   int      `json:"port"`
	Transport              string   `json:"transport"`
	BaseURL                string   `json:"baseURL"`
	MCPPath                string   `json:"mcpPath"`
	HealthPath             string   `json:"healthPath"`
	AuditPath              string   `json:"auditPath"`
	BearerEnabled          bool     `json:"bearerEnabled"`
	TokenName              string   `json:"tokenName"`
	TokenInput             string   `json:"tokenInput,omitempty"`
	TokenPreview           string   `json:"tokenPreview"`
	TokenConfigured        bool     `json:"tokenConfigured"`
	TokenFromEnv           bool     `json:"tokenFromEnv"`
	AllowedOrigins         []string `json:"allowedOrigins"`
	TLSMode                string   `json:"tlsMode"`
	ProxyMode              string   `json:"proxyMode"`
	MaxBodyKB              int      `json:"maxBodyKB"`
	MaxOutputKB            int      `json:"maxOutputKB"`
	RequestTimeout         int      `json:"requestTimeout"`
	DefaultConnectTimeout  int      `json:"defaultConnectTimeout"`
	DefaultCommandTimeout  int      `json:"defaultCommandTimeout"`
	StrictHostKey          bool     `json:"strictHostKey"`
	AutoExecuteLowRisk     bool     `json:"autoExecuteLowRisk"`
	RequireApprovalSudo    bool     `json:"requireApprovalSudo"`
	RequireApprovalProd    bool     `json:"requireApprovalProd"`
	RequireApprovalWriteOp bool     `json:"requireApprovalWriteOp"`
	ConfigPath             string   `json:"configPath"`
	Status                 string   `json:"status"`
	LastError              string   `json:"lastError"`
	ConnectedAgents        int      `json:"connectedAgents"`
	// Derived, never persisted: where agents should connect.
	LocalURL      string `json:"localURL"`
	LANAddress    string `json:"lanAddress"`
	LANURL        string `json:"lanURL"`
	LANReachable  bool   `json:"lanReachable"`
	BypassAllowed bool   `json:"bypassAllowed"`
}

type DashboardData struct {
	Connections   []Connection     `json:"connections"`
	Requests      []CommandRequest `json:"requests"`
	AgentPolicies []AgentPolicy    `json:"agentPolicies"`
	MCP           MCPSettings      `json:"mcp"`
	StorageError  string           `json:"storageError"`
}

type ActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	// Code is a machine-readable reason for failures the UI reacts to
	// (needs_passphrase, risk_increased, ...).
	Code string `json:"code,omitempty"`
	// ID is the affected entity (e.g. the saved connection's ID).
	ID string `json:"id,omitempty"`
	// Token is only set by GenerateMCPToken, shown once to the user.
	Token string `json:"token,omitempty"`
}

type commandResult struct {
	Stdout   string
	Stderr   string
	Duration time.Duration
	ExitCode *int
}

type storedMCPSettings struct {
	MCPSettings
	Token string `json:"token,omitempty"`
}

func NewApp() *App {
	dbPath := defaultDatabasePath()
	legacyConnections := defaultLegacyConnectionsPath()
	legacyMCPSettings := defaultLegacyMCPSettingsPath()

	app := &App{
		dbPath:            dbPath,
		legacyConnections: legacyConnections,
		legacyMCPSettings: legacyMCPSettings,
		mcpAgents:         map[string]time.Time{},
		agentPolicies:     map[string]AgentPolicy{},
		sshPassphrases:    map[string]string{},
		requestsChanged:   make(chan struct{}),
	}

	store, err := openStore(dbPath)
	if err != nil {
		log.Printf("open sqlite store: %v", err)
		app.storageError = err.Error()
	}
	app.store = store

	if store != nil {
		if err := migrateLegacyData(store, dbPath, legacyConnections, legacyMCPSettings); err != nil {
			log.Printf("migrate legacy data: %v", err)
		}
		if err := store.recoverInterruptedRequests(); err != nil {
			log.Printf("recover interrupted requests: %v", err)
		}
		app.conns, err = store.loadConnections()
		if err != nil {
			log.Printf("load connections from sqlite: %v", err)
		}
		if len(app.conns) == 0 && store.fresh && os.Getenv("SSH_GETE_NO_SEED") != "1" {
			for _, conn := range seedConnections() {
				conn.ID = newID()
				if err := store.insertConnection(conn); err != nil {
					log.Printf("seed connection: %v", err)
					continue
				}
				app.conns = append(app.conns, conn)
			}
		}
	}

	mcp := defaultMCPSettings(dbPath)
	token := ""
	if store != nil {
		var loaded bool
		mcp, token, loaded, err = store.loadMCPSettings(dbPath)
		if err != nil {
			log.Printf("load mcp settings from sqlite: %v", err)
			mcp, token = defaultMCPSettings(dbPath), ""
		}
		if !loaded {
			mcp = defaultMCPSettings(dbPath)
		}
	}
	app.mcpToken = token
	app.mcpEnvToken = applyMCPEnv(&mcp)
	app.mcp = sanitizeMCPSettings(mcp, app.effectiveTokenLocked(), "停止中", "")

	if store != nil {
		app.requests, err = store.loadCommandRequests(500)
		if err != nil {
			log.Printf("load command requests from sqlite: %v", err)
		}
		policies, err := store.loadAgentPolicies()
		if err != nil {
			log.Printf("load agent policies from sqlite: %v", err)
		}
		for _, policy := range policies {
			app.agentPolicies[policy.AgentName] = policy
		}
	}
	return app
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.mu.RLock()
	enabled := a.mcp.Enabled
	a.mu.RUnlock()
	if enabled {
		if err := a.startMCPServer(); err != nil {
			a.setMCPStatus("エラー", err.Error())
			log.Printf("start mcp server: %v", err)
		}
	}
}

func (a *App) shutdown(ctx context.Context) {
	a.CloseTerminal()
	a.stopMCPServer(ctx)
	if err := a.store.Close(); err != nil {
		log.Printf("close sqlite store: %v", err)
	}
}

// effectiveTokenLocked is the token MCP clients must present. Callers hold a.mu.
func (a *App) effectiveTokenLocked() string {
	if a.mcpEnvToken != "" {
		return a.mcpEnvToken
	}
	return a.mcpToken
}

func (a *App) GetInitialData() DashboardData {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return DashboardData{
		Connections:   publicConnections(a.conns),
		Requests:      cloneRequests(a.requests),
		AgentPolicies: cloneAgentPolicies(a.agentPolicies),
		MCP:           a.mcpSnapshotLocked(),
		StorageError:  a.storageError,
	}
}

// publicConnections strips secrets before data leaves the backend.
func publicConnections(in []Connection) []Connection {
	out := cloneConnections(in)
	for i := range out {
		out[i].KeyPassphrase = ""
		out[i].CredentialSet = out[i].Credential != ""
		if out[i].usesPassword() {
			out[i].Credential = ""
		}
	}
	return out
}

func (a *App) TestConnection(id string) ActionResult {
	conn, ok := a.findConnection(id)
	if !ok {
		return ActionResult{OK: false, Message: "接続先が選択されていません"}
	}
	return a.testConnectionConfig(conn, "")
}

func (a *App) TestConnectionWithPassphrase(id string, passphrase string) ActionResult {
	conn, ok := a.findConnection(id)
	if !ok {
		return ActionResult{OK: false, Message: "接続先が選択されていません"}
	}
	return a.testConnectionConfig(conn, passphrase)
}

// TestConnectionConfig tests the (possibly unsaved) form values. A blank
// password for a saved connection means "use the stored one".
func (a *App) TestConnectionConfig(conn Connection, passphrase string) ActionResult {
	conn = normalizeConnection(conn)
	if conn.ID != "" && conn.usesPassword() && conn.Credential == "" {
		if saved, ok := a.connectionByID(conn.ID); ok && saved.usesPassword() {
			conn.Credential = saved.Credential
		}
	}
	if conn.Type == "Serial" {
		return a.testSerialConnection(conn)
	}
	if conn.Host == "" || conn.User == "" {
		return ActionResult{OK: false, Message: "ホスト名/IPアドレスとユーザー名を入力してください"}
	}
	return a.testConnectionConfig(conn, passphrase)
}

func (a *App) testConnectionConfig(conn Connection, passphrase string) ActionResult {
	if conn.Type == "Serial" {
		return a.testSerialConnection(conn)
	}
	displayName := conn.Name
	if strings.TrimSpace(displayName) == "" {
		displayName = conn.Host
	}
	if strings.TrimSpace(passphrase) != "" {
		conn.KeyPassphrase = passphrase
	} else {
		a.mu.RLock()
		conn.KeyPassphrase = a.sshPassphrases[conn.ID]
		a.mu.RUnlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeoutSeconds(conn.ConnectTimeout, 10))
	defer cancel()
	a.mu.RLock()
	strictHostKey := a.mcp.StrictHostKey
	a.mu.RUnlock()
	if err := testSSHConnection(ctx, conn, strictHostKey); err != nil {
		message := fmt.Sprintf("%s へのSSH疎通に失敗しました: %s", displayName, explainSSHError(conn, err))
		a.recordConnectionTest(conn, "failed", message)
		result := ActionResult{OK: false, Message: message, ID: conn.ID}
		var missing *passphraseMissingError
		if errors.As(err, &missing) {
			result.Code = "needs_passphrase"
		}
		return result
	}
	if strings.TrimSpace(passphrase) != "" && conn.ID != "" {
		a.mu.Lock()
		a.sshPassphrases[conn.ID] = passphrase
		a.mu.Unlock()
	}
	message := fmt.Sprintf("%s へのSSH疎通を確認しました", displayName)
	a.recordConnectionTest(conn, "ok", message)
	return ActionResult{OK: true, Message: message, ID: conn.ID}
}

// recordConnectionTest stores the last test outcome, but only when the tested
// values are the saved ones: testing unsaved edits must not label the saved
// connection.
func (a *App) recordConnectionTest(tested Connection, result string, message string) {
	if tested.ID == "" {
		return
	}
	saved, ok := a.connectionByID(tested.ID)
	if !ok || !sameTarget(saved, tested) {
		return
	}
	at := nowRFC3339()
	if a.store != nil {
		if err := a.store.updateConnectionTest(tested.ID, result, at, message); err != nil {
			log.Printf("persist connection test: %v", err)
			return
		}
	}
	a.mu.Lock()
	for i := range a.conns {
		if a.conns[i].ID == tested.ID {
			a.conns[i].LastTestResult = result
			a.conns[i].LastTestAt = at
			a.conns[i].LastTestMessage = message
		}
	}
	a.mu.Unlock()
}

func sameTarget(x Connection, y Connection) bool {
	return x.connectionType() == y.connectionType() && x.Host == y.Host && x.Port == y.Port && x.User == y.User &&
		x.AuthMethod == y.AuthMethod && x.Credential == y.Credential && x.SerialPort == y.SerialPort &&
		x.BaudRate == y.BaudRate
}

// DisconnectConnection closes the interactive terminal attached to the
// connection, if any.
func (a *App) DisconnectConnection(id string) ActionResult {
	conn, ok := a.findConnection(strings.TrimSpace(id))
	if !ok {
		return ActionResult{OK: false, Message: "接続先が見つかりません"}
	}
	a.closeTerminalsForConnection(conn.ID)
	return ActionResult{OK: true, Message: fmt.Sprintf("%s のセッションを切断しました", conn.Name)}
}

// SaveConnection creates (empty ID) or updates (ID set, rename allowed) a
// connection. The database is written first; memory only changes on success.
func (a *App) SaveConnection(connection Connection) ActionResult {
	connection = normalizeConnection(connection)
	if connection.Name == "" {
		return ActionResult{OK: false, Message: "接続先名は必須です"}
	}
	if connection.Type == "Serial" {
		if connection.SerialPort == "" {
			return ActionResult{OK: false, Message: "シリアルポートを選択してください"}
		}
		connection.Host, connection.User, connection.Credential = "", "", ""
	} else if connection.Host == "" || connection.User == "" {
		return ActionResult{OK: false, Message: "ホスト、ユーザー名は必須です"}
	}
	if a.store == nil {
		return ActionResult{OK: false, Message: errStoreUnavailable.Error()}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	index := -1
	for i := range a.conns {
		if connection.ID != "" && a.conns[i].ID == connection.ID {
			index = i
			continue
		}
		if a.conns[i].Name == connection.Name {
			return ActionResult{OK: false, Message: fmt.Sprintf("「%s」という名前の接続先は既にあります。別の名前にしてください", connection.Name)}
		}
	}
	if connection.ID != "" && index < 0 {
		return ActionResult{OK: false, Message: "編集中の接続先は削除されています。新規として保存し直してください"}
	}

	if index < 0 {
		connection.ID = newID()
		if err := a.store.insertConnection(connection); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("SQLite保存に失敗しました: %v", err)}
		}
		a.conns = append(a.conns, connection)
		return ActionResult{OK: true, Message: fmt.Sprintf("%s を登録しました", connection.Name), ID: connection.ID}
	}

	previous := a.conns[index]
	if connection.usesPassword() && previous.usesPassword() && connection.Credential == "" {
		connection.Credential = previous.Credential // write-only field left blank: keep it
	}
	connection.LastTestResult = previous.LastTestResult
	connection.LastTestAt = previous.LastTestAt
	connection.LastTestMessage = previous.LastTestMessage
	updated, err := a.store.updateConnection(connection)
	if err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("SQLite保存に失敗しました: %v", err)}
	}
	if !updated {
		return ActionResult{OK: false, Message: "編集中の接続先は削除されています。新規として保存し直してください"}
	}
	if previous.Name != connection.Name {
		if err := a.store.updatePendingRequestsConnection(previous.Name, connection.ID, connection.Name); err != nil {
			log.Printf("rename pending requests: %v", err)
		}
		for i := range a.requests {
			if a.requests[i].ConnectionID == connection.ID && a.requests[i].Status == statePending {
				a.requests[i].Host = connection.Name
			}
		}
	}
	if !sameTarget(previous, connection) {
		delete(a.sshPassphrases, connection.ID)
	}
	a.conns[index] = connection
	return ActionResult{OK: true, Message: fmt.Sprintf("%s の設定を保存しました", connection.Name), ID: connection.ID}
}

func (a *App) DeleteConnection(id string) ActionResult {
	conn, ok := a.connectionByID(strings.TrimSpace(id))
	if !ok {
		return ActionResult{OK: false, Message: "削除する接続先が見つかりません"}
	}
	if a.store == nil {
		return ActionResult{OK: false, Message: errStoreUnavailable.Error()}
	}
	if err := a.store.deleteConnection(conn.ID); err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s の削除に失敗しました: %v", conn.Name, err)}
	}
	a.closeTerminalsForConnection(conn.ID)

	a.mu.Lock()
	next := make([]Connection, 0, len(a.conns))
	for _, c := range a.conns {
		if c.ID != conn.ID {
			next = append(next, c)
		}
	}
	a.conns = next
	delete(a.sshPassphrases, conn.ID)
	a.mu.Unlock()

	return ActionResult{OK: true, Message: fmt.Sprintf("%s を削除しました。コマンド履歴は監査用に残しています", conn.Name)}
}

func (a *App) SaveMCPSettings(settings MCPSettings) ActionResult {
	if a.store == nil {
		return ActionResult{OK: false, Message: errStoreUnavailable.Error()}
	}
	settings.ConfigPath = a.dbPath

	a.mu.Lock()
	token := a.mcpToken
	if strings.TrimSpace(settings.TokenInput) != "" {
		token = strings.TrimSpace(settings.TokenInput)
	}
	if a.mcpEnvToken != "" {
		settings.BearerEnabled = true
	}
	effective := token
	if a.mcpEnvToken != "" {
		effective = a.mcpEnvToken
	}
	settings = sanitizeMCPSettings(settings, effective, "再起動中", "")
	if err := validateListenSettings(settings, effective); err != nil {
		a.mu.Unlock()
		return ActionResult{OK: false, Message: err.Error()}
	}
	if err := a.store.saveMCPSettings(settings, token); err != nil {
		a.mu.Unlock()
		return ActionResult{OK: false, Message: fmt.Sprintf("MCP設定のSQLite保存に失敗しました: %v", err)}
	}
	a.mcp = settings
	a.mcpToken = token
	a.mu.Unlock()

	if err := a.restartMCPServer(); err != nil {
		a.setMCPStatus("エラー", err.Error())
		return ActionResult{OK: false, Message: fmt.Sprintf("MCPサーバーの再起動に失敗しました: %v", err)}
	}
	return ActionResult{OK: true, Message: "MCP設定を保存し、待受を再起動しました"}
}

// GenerateMCPToken creates a random Bearer token, enables Bearer auth and
// restarts the listener. The token is returned once so the user can paste it
// into the MCP client configuration.
func (a *App) GenerateMCPToken() ActionResult {
	if a.store == nil {
		return ActionResult{OK: false, Message: errStoreUnavailable.Error()}
	}
	token := newID() + newID()
	a.mu.Lock()
	if a.mcpEnvToken != "" {
		a.mu.Unlock()
		return ActionResult{OK: false, Message: "環境変数 SSH_GETE_TOKEN でトークンが指定されているため、ここでは発行できません"}
	}
	settings := a.mcp
	settings.BearerEnabled = true
	settings = sanitizeMCPSettings(settings, token, "再起動中", "")
	if err := a.store.saveMCPSettings(settings, token); err != nil {
		a.mu.Unlock()
		return ActionResult{OK: false, Message: fmt.Sprintf("MCP設定のSQLite保存に失敗しました: %v", err)}
	}
	a.mcp = settings
	a.mcpToken = token
	a.mu.Unlock()

	if err := a.restartMCPServer(); err != nil {
		a.setMCPStatus("エラー", err.Error())
		return ActionResult{OK: false, Message: fmt.Sprintf("MCPサーバーの再起動に失敗しました: %v", err), Token: token}
	}
	return ActionResult{OK: true, Message: "トークンを発行し、Bearer認証を有効にしました。MCPクライアントの設定に貼り付けてください（この画面を離れると再表示できません）", Token: token}
}

// DeleteMCPSettings resets the listener settings to defaults, stopped. The
// reset is persisted (rather than deleting the row) so a restart does not
// silently bring the listener back with the enabled-by-default settings.
func (a *App) DeleteMCPSettings() ActionResult {
	if a.store == nil {
		return ActionResult{OK: false, Message: errStoreUnavailable.Error()}
	}
	a.stopMCPServer(context.Background())

	settings := defaultMCPSettings(a.dbPath)
	settings.Enabled = false
	a.mu.Lock()
	settings = sanitizeMCPSettings(settings, a.mcpEnvToken, "停止中", "")
	if err := a.store.saveMCPSettings(settings, ""); err != nil {
		a.mu.Unlock()
		return ActionResult{OK: false, Message: fmt.Sprintf("MCP設定の削除に失敗しました: %v", err)}
	}
	a.mcp = settings
	a.mcpToken = ""
	a.mcpAgents = map[string]time.Time{}
	a.mu.Unlock()

	return ActionResult{OK: true, Message: "MCPサーバー待受設定を初期化し、待受を停止しました"}
}

func (a *App) GetMCPSettings() MCPSettings {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mcpSnapshotLocked()
}

func (a *App) mcpSnapshotLocked() MCPSettings {
	settings := a.mcp
	settings.ConnectedAgents = a.connectedAgentsLocked()
	settings.BypassAllowed = settings.BearerEnabled && a.effectiveTokenLocked() != ""
	fillConnectURLs(&settings)
	return settings
}

func (a *App) connectedAgentsLocked() int {
	cutoff := time.Now().Add(-5 * time.Minute)
	count := 0
	for _, seenAt := range a.mcpAgents {
		if seenAt.After(cutoff) {
			count++
		}
	}
	return count
}

func (a *App) setAgentApprovalBypass(agentName string, enabled bool) ActionResult {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return ActionResult{OK: false, Message: "エージェント名が不足しています"}
	}
	policy := AgentPolicy{
		AgentName:      agentName,
		ApprovalBypass: enabled,
		UpdatedAt:      nowRFC3339(),
	}
	if a.store != nil {
		if err := a.store.saveAgentPolicy(policy); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("%s の承認省略設定を保存できません: %v", agentName, err)}
		}
	}
	a.mu.Lock()
	a.agentPolicies[agentName] = policy
	a.mu.Unlock()
	if enabled {
		return ActionResult{OK: true, Message: fmt.Sprintf("%s の承認省略を有効にしました", agentName)}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s の承認省略を取り消しました", agentName)}
}

// RevokeAgentApprovalBypass turns approval bypass off for an agent.
func (a *App) RevokeAgentApprovalBypass(agentName string) ActionResult {
	return a.setAgentApprovalBypass(agentName, false)
}

func (a *App) agentApprovalBypassEnabled(agentName string) bool {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	policy, ok := a.agentPolicies[agentName]
	return ok && policy.ApprovalBypass
}

func (a *App) connectionByID(id string) (Connection, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, conn := range a.conns {
		if conn.ID == id {
			return conn, true
		}
	}
	return Connection{}, false
}

// findConnection resolves a reference from the UI or an agent: ID first, then
// exact name, then host/IP — the host match only when exactly one SSH
// connection uses it, so an ambiguous IP never picks an arbitrary target.
func (a *App) findConnection(ref string) (Connection, bool) {
	conn, _, ok := a.resolveConnection(ref)
	return conn, ok
}

func (a *App) resolveConnection(ref string) (Connection, []string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Connection{}, nil, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, conn := range a.conns {
		if conn.ID == ref || conn.Name == ref {
			return conn, nil, true
		}
	}
	var matches []Connection
	for _, conn := range a.conns {
		if conn.connectionType() == "SSH" && strings.EqualFold(conn.Host, ref) {
			matches = append(matches, conn)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil, true
	}
	candidates := make([]string, 0, len(matches))
	for _, conn := range matches {
		candidates = append(candidates, conn.Name)
	}
	return Connection{}, candidates, false
}

func explainSSHError(conn Connection, err error) string {
	message := err.Error()
	lower := strings.ToLower(message)
	if strings.Contains(lower, "unable to authenticate") {
		if conn.usesPassword() {
			return fmt.Sprintf("パスワード認証に失敗しました。ユーザー名、パスワード、接続先SSHのPasswordAuthentication設定を確認してください。詳細: %v", err)
		}
		return fmt.Sprintf("公開鍵認証に失敗しました。ユーザー名、鍵ファイルパス、接続先ユーザーの ~/.ssh/authorized_keys、またはssh-agent登録鍵を確認してください。パスワードで接続する場合は認証方式を「パスワード保管」に変更してください。詳細: %v", err)
	}
	var missing *passphraseMissingError
	if errors.As(err, &missing) {
		return "SSH鍵がパスフレーズで保護されています。パスフレーズを入力するか、ssh-agent に鍵を登録してください。"
	}
	if strings.Contains(lower, "no such file") || strings.Contains(lower, "read ssh key") {
		return fmt.Sprintf("SSH鍵ファイルを読めません。鍵ファイルパスを確認してください。詳細: %v", err)
	}
	if strings.Contains(lower, "key mismatch") {
		return fmt.Sprintf("known_hosts のホスト鍵が一致しません。中間者攻撃の可能性があります。接続先のホスト鍵が正当に変わった場合のみ ~/.ssh/known_hosts の該当行を更新してください。詳細: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "タイムアウトしました。ホスト名・ポート・ファイアウォール、またはコマンドの実行時間を確認してください。"
	}
	return message
}

func normalizeConnection(connection Connection) Connection {
	connection.ID = strings.TrimSpace(connection.ID)
	connection.Name = strings.TrimSpace(connection.Name)
	connection.Type = connection.connectionType()
	connection.Host = strings.TrimSpace(connection.Host)
	connection.User = strings.TrimSpace(connection.User)
	if !connection.usesPassword() {
		connection.Credential = strings.TrimSpace(connection.Credential)
	}
	connection.SerialPort = strings.TrimSpace(connection.SerialPort)
	connection.KeyPassphrase = ""
	connection.CredentialSet = false
	if connection.Port <= 0 || connection.Port > 65535 {
		connection.Port = 22
	}
	if connection.ConnectTimeout <= 0 {
		connection.ConnectTimeout = 10
	}
	if connection.CommandTimeout <= 0 {
		connection.CommandTimeout = 30
	}
	if connection.AuthMethod == "" {
		connection.AuthMethod = "SSH鍵認証"
	}
	if connection.Type == "Serial" && connection.BaudRate <= 0 {
		connection.BaudRate = 115200
	}
	if connection.Tags == nil {
		connection.Tags = []string{}
	}
	return connection
}

func appConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("SSH_GETE_CONFIG_DIR")); dir != "" {
		return dir
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, appName)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "."+appName)
	}
	return filepath.Join(os.TempDir(), appName)
}

// currentUsername resolves the OS login name across platforms. Unix exposes it
// via USER, Windows via USERNAME; we check both so seed defaults are sensible
// on either OS.
func currentUsername() string {
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u
	}
	return strings.TrimSpace(os.Getenv("USERNAME"))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func defaultDatabasePath() string {
	return filepath.Join(appConfigDir(), "ssh-gete.sqlite")
}

func defaultLegacyConnectionsPath() string {
	return filepath.Join(appConfigDir(), "connections.json")
}

func defaultLegacyMCPSettingsPath() string {
	return filepath.Join(appConfigDir(), "mcp-settings.json")
}

// migrateLegacyData imports the pre-SQLite JSON files exactly once. Earlier
// versions re-imported them on every start whenever the table was empty,
// which resurrected deleted connections and plaintext tokens. The files are
// renamed to *.migrated afterwards.
func migrateLegacyData(store *Store, dbPath string, connectionsPath string, settingsPath string) error {
	if store.prevVersion >= 2 {
		return nil
	}
	// Only a brand-new database imports; an existing pre-v2 database already
	// imported them earlier and may have had rows deleted since.
	importData := store.fresh
	if importData {
		legacyConnections, err := loadConnections(connectionsPath)
		if err != nil {
			return err
		}
		for _, conn := range legacyConnections {
			conn = normalizeConnection(conn)
			conn.ID = newID()
			if err := store.insertConnection(conn); err != nil {
				return err
			}
		}
		if fileExists(settingsPath) {
			settings, token, err := loadMCPSettings(settingsPath)
			if err != nil {
				return err
			}
			settings.ConfigPath = dbPath
			if err := store.saveMCPSettings(settings, token); err != nil {
				return err
			}
		}
	}
	for _, path := range []string{connectionsPath, settingsPath} {
		if fileExists(path) {
			if err := os.Rename(path, path+".migrated"); err != nil {
				return err
			}
			_ = os.Chmod(path+".migrated", 0o600)
		}
	}
	return nil
}

func (a *App) setMCPStatus(status string, lastErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mcp.Status = status
	a.mcp.LastError = lastErr
	a.mcp.ConnectedAgents = a.connectedAgentsLocked()
}

func loadConnections(path string) ([]Connection, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var conns []Connection
	if err := json.Unmarshal(content, &conns); err != nil {
		return nil, err
	}
	return conns, nil
}

func loadMCPSettings(path string) (MCPSettings, string, error) {
	settings := defaultMCPSettings(path)
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, "", nil
	}
	if err != nil {
		return settings, "", err
	}
	var stored storedMCPSettings
	if err := json.Unmarshal(content, &stored); err != nil {
		return settings, "", err
	}
	stored.MCPSettings.ConfigPath = path
	return stored.MCPSettings, stored.Token, nil
}

func defaultMCPSettings(path string) MCPSettings {
	return MCPSettings{
		Enabled:                true,
		ListenAddress:          "127.0.0.1",
		Port:                   8787,
		Transport:              "Streamable HTTP",
		MCPPath:                "/mcp",
		HealthPath:             "/healthz",
		AuditPath:              "/audit/events",
		BearerEnabled:          false,
		TokenName:              "local-admin-token",
		AllowedOrigins:         []string{},
		TLSMode:                "ローカル平文",
		ProxyMode:              "無効",
		MaxBodyKB:              256,
		MaxOutputKB:            128,
		RequestTimeout:         30,
		DefaultConnectTimeout:  10,
		DefaultCommandTimeout:  30,
		StrictHostKey:          false,
		AutoExecuteLowRisk:     false,
		RequireApprovalSudo:    true,
		RequireApprovalProd:    true,
		RequireApprovalWriteOp: true,
		ConfigPath:             path,
		Status:                 "停止中",
	}
}

// legacyDefaultOrigins were the permissive defaults of earlier versions.
// "http://localhost:*" let any local web page drive the MCP endpoint, so they
// are dropped from stored settings.
var legacyDefaultOrigins = map[string]bool{"http://localhost:*": true, "wails://wails.localhost": true}

func sanitizeMCPSettings(settings MCPSettings, token string, status string, lastErr string) MCPSettings {
	settings.ListenAddress = strings.TrimSpace(settings.ListenAddress)
	if settings.ListenAddress == "" {
		settings.ListenAddress = "127.0.0.1"
	}
	settings.ListenAddress = strings.Trim(settings.ListenAddress, "[]")
	if settings.Port <= 0 || settings.Port > 65535 {
		settings.Port = 8787
	}
	if settings.Transport == "" {
		settings.Transport = "Streamable HTTP"
	}
	if settings.MCPPath == "" {
		settings.MCPPath = "/mcp"
	}
	if settings.HealthPath == "" {
		settings.HealthPath = "/healthz"
	}
	if settings.AuditPath == "" {
		settings.AuditPath = "/audit/events"
	}
	if settings.MaxBodyKB <= 0 {
		settings.MaxBodyKB = 256
	}
	if settings.MaxOutputKB <= 0 {
		settings.MaxOutputKB = 128
	}
	if settings.RequestTimeout <= 0 {
		settings.RequestTimeout = 30
	}
	if settings.DefaultConnectTimeout <= 0 {
		settings.DefaultConnectTimeout = 10
	}
	if settings.DefaultCommandTimeout <= 0 {
		settings.DefaultCommandTimeout = 30
	}
	// BaseURL is an explicit override (e.g. behind a reverse proxy). Earlier
	// versions stored the auto-derived value, which then went stale when the
	// listen address changed.
	settings.BaseURL = strings.TrimRight(strings.TrimSpace(settings.BaseURL), "/")
	if settings.BaseURL == "http://127.0.0.1:8787" || settings.BaseURL == fmt.Sprintf("http://%s:%d", settings.ListenAddress, settings.Port) {
		settings.BaseURL = ""
	}
	origins := make([]string, 0, len(settings.AllowedOrigins))
	for _, origin := range settings.AllowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" || legacyDefaultOrigins[origin] || strings.Contains(origin, "*") {
			continue
		}
		origins = append(origins, origin)
	}
	settings.AllowedOrigins = origins
	settings.TokenConfigured = token != ""
	settings.TokenPreview = maskToken(token)
	settings.TokenInput = ""
	settings.AutoExecuteLowRisk = false
	if status != "" {
		settings.Status = status
	}
	settings.LastError = lastErr
	return settings
}

// applyMCPEnv applies environment overrides and returns the env token, which
// is kept separate so it is never persisted.
func applyMCPEnv(settings *MCPSettings) string {
	if addr := strings.TrimSpace(os.Getenv("SSH_GETE_MCP_ADDR")); addr != "" {
		host, port, err := splitHostPortLoose(addr)
		if err == nil {
			settings.ListenAddress = host
			if parsedPort, err := strconv.Atoi(port); err == nil {
				settings.Port = parsedPort
			}
		}
	}
	if os.Getenv("SSH_GETE_STRICT_HOSTKEY") == "1" {
		settings.StrictHostKey = true
	}
	envToken := strings.TrimSpace(os.Getenv("SSH_GETE_TOKEN"))
	if envToken != "" {
		settings.BearerEnabled = true
		settings.TokenFromEnv = true
	}
	return envToken
}

func maskToken(token string) string {
	if token == "" {
		return "未設定"
	}
	if len(token) <= 8 {
		return "********"
	}
	return token[:4] + "************************" + token[len(token)-4:]
}

func cloneConnections(in []Connection) []Connection {
	out := make([]Connection, len(in))
	copy(out, in)
	for i := range out {
		out[i].Tags = append([]string{}, out[i].Tags...)
	}
	return out
}

func cloneRequests(in []CommandRequest) []CommandRequest {
	out := make([]CommandRequest, len(in))
	copy(out, in)
	return out
}

func cloneAgentPolicies(in map[string]AgentPolicy) []AgentPolicy {
	out := make([]AgentPolicy, 0, len(in))
	for _, policy := range in {
		out = append(out, policy)
	}
	return out
}

func timeoutSeconds(seconds int, fallback int) time.Duration {
	if seconds <= 0 {
		seconds = fallback
	}
	return time.Duration(seconds) * time.Second
}

func seedConnections() []Connection {
	return []Connection{
		{
			Name:           "ローカル確認用",
			Type:           "SSH",
			Host:           "127.0.0.1",
			Port:           22,
			User:           currentUsername(),
			AuthMethod:     "SSH鍵認証",
			Credential:     "~/.ssh/id_ed25519",
			ConnectTimeout: 10,
			CommandTimeout: 30,
			Tags:           []string{"local", "dev"},
			Description:    "ローカルSSH確認用。macOS側でRemote Loginが有効な場合のみ接続できます。",
		},
	}
}
