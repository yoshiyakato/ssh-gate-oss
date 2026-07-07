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
)

type App struct {
	ctx               context.Context
	mu                sync.RWMutex
	store             *Store
	dbPath            string
	legacyConnections string
	legacyMCPSettings string
	conns             []Connection
	requests          []CommandRequest
	mcp               MCPSettings
	mcpToken          string
	mcpServer         *http.Server
	mcpAgents         map[string]time.Time
	agentPolicies     map[string]AgentPolicy
	sshPassphrases    map[string]string
	termMu            sync.Mutex
	term              *terminalSession
}

type Connection struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	User           string   `json:"user"`
	AuthMethod     string   `json:"authMethod"`
	Credential     string   `json:"credential"`
	KeyPassphrase  string   `json:"keyPassphrase,omitempty"`
	SerialPort     string   `json:"serialPort"`
	BaudRate       int      `json:"baudRate"`
	ConnectTimeout int      `json:"connectTimeout"`
	CommandTimeout int      `json:"commandTimeout"`
	Tags           []string `json:"tags"`
	Description    string   `json:"description"`
	Status         string   `json:"status"`
	LastChecked    string   `json:"lastChecked"`
}

// connectionType normalizes the transport, defaulting legacy rows to SSH.
func (c Connection) connectionType() string {
	if c.Type == "Serial" {
		return "Serial"
	}
	return "SSH"
}

type CommandRequest struct {
	ID          string `json:"id"`
	Host        string `json:"host"`
	User        string `json:"user"`
	Command     string `json:"command"`
	Reason      string `json:"reason"`
	RequestedBy string `json:"requestedBy"`
	RequestedAt string `json:"requestedAt"`
	Risk        string `json:"risk"`
	Status      string `json:"status"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	Duration    string `json:"duration"`
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
}

type DashboardData struct {
	Connections   []Connection     `json:"connections"`
	Requests      []CommandRequest `json:"requests"`
	AgentPolicies []AgentPolicy    `json:"agentPolicies"`
	MCP           MCPSettings      `json:"mcp"`
}

type ActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type commandResult struct {
	Stdout   string
	Stderr   string
	Duration time.Duration
}

type storedMCPSettings struct {
	MCPSettings
	Token string `json:"token,omitempty"`
}

func NewApp() *App {
	dbPath := defaultDatabasePath()
	dbExisted := fileExists(dbPath)
	legacyConnections := defaultLegacyConnectionsPath()
	legacyMCPSettings := defaultLegacyMCPSettingsPath()

	store, err := openStore(dbPath)
	if err != nil {
		log.Printf("open sqlite store: %v", err)
	}

	if store != nil {
		if err := migrateLegacyData(store, dbPath, legacyConnections, legacyMCPSettings); err != nil {
			log.Printf("migrate legacy data: %v", err)
		}
	}

	var conns []Connection
	if store != nil {
		conns, err = store.loadConnections()
		if err != nil {
			log.Printf("load connections from sqlite: %v", err)
		}
	}
	if len(conns) == 0 && (store == nil || !dbExisted) {
		conns = seedConnections()
		if store != nil {
			for _, conn := range conns {
				if err := store.saveConnection(conn); err != nil {
					log.Printf("seed connection: %v", err)
				}
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
			mcp = defaultMCPSettings(dbPath)
			token = ""
		}
		if !loaded {
			mcp = defaultMCPSettings(dbPath)
		}
	}
	applyMCPEnv(&mcp, &token)
	mcp = sanitizeMCPSettings(mcp, token, "停止中", "")

	requests := seedRequests()
	if store != nil {
		requests, err = store.loadCommandRequests(500)
		if err != nil {
			log.Printf("load command requests from sqlite: %v", err)
			requests = seedRequests()
		}
	}

	agentPolicies := map[string]AgentPolicy{}
	if store != nil {
		loadedPolicies, err := store.loadAgentPolicies()
		if err != nil {
			log.Printf("load agent policies from sqlite: %v", err)
		}
		for _, policy := range loadedPolicies {
			agentPolicies[policy.AgentName] = policy
		}
	}

	return &App{
		store:             store,
		dbPath:            dbPath,
		legacyConnections: legacyConnections,
		legacyMCPSettings: legacyMCPSettings,
		conns:             conns,
		requests:          requests,
		mcp:               mcp,
		mcpToken:          token,
		mcpAgents:         map[string]time.Time{},
		agentPolicies:     agentPolicies,
		sshPassphrases:    map[string]string{},
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if a.mcp.Enabled {
		if err := a.startMCPServer(); err != nil {
			a.setMCPStatus("エラー", err.Error())
			log.Printf("start mcp server: %v", err)
		}
	}
}

func (a *App) shutdown(ctx context.Context) {
	a.CloseTerminal()
	if a.mcpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := a.mcpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown mcp server: %v", err)
		}
	}
	if err := a.store.Close(); err != nil {
		log.Printf("close sqlite store: %v", err)
	}
}

func (a *App) GetInitialData() DashboardData {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return DashboardData{
		Connections:   cloneConnections(a.conns),
		Requests:      cloneRequests(a.requests),
		AgentPolicies: cloneAgentPolicies(a.agentPolicies),
		MCP:           a.mcpSnapshotLocked(),
	}
}

func (a *App) TestConnection(name string) ActionResult {
	conn, ok := a.findConnection(name)
	if !ok {
		return ActionResult{OK: false, Message: "接続先が選択されていません"}
	}
	return a.testConnectionConfig(conn, "")
}

func (a *App) TestConnectionWithPassphrase(name string, passphrase string) ActionResult {
	conn, ok := a.findConnection(name)
	if !ok {
		return ActionResult{OK: false, Message: "接続先が選択されていません"}
	}
	return a.testConnectionConfig(conn, passphrase)
}

func (a *App) TestConnectionConfig(conn Connection, passphrase string) ActionResult {
	conn = normalizeConnection(conn)
	if conn.Host == "" || conn.User == "" {
		return ActionResult{OK: false, Message: "ホスト名/IPアドレスとユーザー名を入力してください"}
	}
	return a.testConnectionConfig(conn, passphrase)
}

func (a *App) DisconnectConnection(name string) ActionResult {
	name = strings.TrimSpace(name)
	if name == "" {
		return ActionResult{OK: false, Message: "切断する接続先が選択されていません"}
	}
	conn, ok := a.findConnection(name)
	if !ok {
		return ActionResult{OK: false, Message: "接続先が見つかりません"}
	}
	a.setConnectionStatus(conn.Name, "Disconnected")
	return ActionResult{OK: true, Message: fmt.Sprintf("%s を切断中にしました", conn.Name)}
}

func (a *App) testConnectionConfig(conn Connection, passphrase string) ActionResult {
	displayName := conn.Name
	if strings.TrimSpace(displayName) == "" {
		displayName = conn.Host
	}
	if strings.TrimSpace(passphrase) != "" {
		conn.KeyPassphrase = passphrase
	} else {
		a.mu.RLock()
		conn.KeyPassphrase = a.sshPassphrases[conn.Name]
		a.mu.RUnlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeoutSeconds(conn.ConnectTimeout, 10))
	defer cancel()
	a.mu.RLock()
	strictHostKey := a.mcp.StrictHostKey
	a.mu.RUnlock()
	if err := testSSHConnection(ctx, conn, strictHostKey); err != nil {
		a.setConnectionStatus(conn.Name, "Error")
		return ActionResult{OK: false, Message: fmt.Sprintf("%s へのSSH疎通に失敗しました: %s", displayName, explainSSHError(conn, err))}
	}
	if strings.TrimSpace(passphrase) != "" {
		a.mu.Lock()
		a.sshPassphrases[conn.Name] = passphrase
		a.mu.Unlock()
	}
	a.setConnectionStatus(conn.Name, "Online")
	return ActionResult{OK: true, Message: fmt.Sprintf("%s へのSSH疎通を確認しました", displayName)}
}

func (a *App) SaveConnection(connection Connection) ActionResult {
	connection = normalizeConnection(connection)
	if connection.Name == "" {
		return ActionResult{OK: false, Message: "接続先名は必須です"}
	}
	if connection.Type == "Serial" {
		if connection.SerialPort == "" {
			return ActionResult{OK: false, Message: "シリアルポートを選択してください"}
		}
	} else if connection.Host == "" || connection.User == "" {
		return ActionResult{OK: false, Message: "ホスト、ユーザー名は必須です"}
	}

	a.mu.Lock()
	replaced := false
	for i := range a.conns {
		if a.conns[i].Name == connection.Name {
			a.conns[i] = connection
			replaced = true
			break
		}
	}
	if !replaced {
		a.conns = append(a.conns, connection)
	}
	a.mu.Unlock()

	if a.store != nil {
		if err := a.store.saveConnection(connection); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("SQLite保存に失敗しました: %v", err)}
		}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s の設定を保存しました", connection.Name)}
}

func (a *App) DeleteConnection(name string) ActionResult {
	name = strings.TrimSpace(name)
	if name == "" {
		return ActionResult{OK: false, Message: "削除する接続先が選択されていません"}
	}

	a.mu.RLock()
	found := false
	for _, conn := range a.conns {
		if conn.Name == name {
			found = true
			break
		}
	}
	a.mu.RUnlock()
	if !found {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s は接続先一覧にありません", name)}
	}

	if a.store != nil {
		if err := a.store.deleteConnection(name); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("%s の削除に失敗しました: %v", name, err)}
		}
	}

	a.mu.Lock()
	next := a.conns[:0]
	for _, conn := range a.conns {
		if conn.Name != name {
			next = append(next, conn)
		}
	}
	a.conns = next
	delete(a.sshPassphrases, name)
	a.mu.Unlock()

	return ActionResult{OK: true, Message: fmt.Sprintf("%s を削除しました。コマンド履歴は監査用に残しています", name)}
}

func (a *App) SaveMCPSettings(settings MCPSettings) ActionResult {
	settings.ConfigPath = a.dbPath

	a.mu.Lock()
	token := a.mcpToken
	if strings.TrimSpace(settings.TokenInput) != "" {
		token = strings.TrimSpace(settings.TokenInput)
	}
	settings.TokenInput = ""
	settings = sanitizeMCPSettings(settings, token, "再起動中", "")
	a.mcp = settings
	a.mcpToken = token
	a.mu.Unlock()

	if a.store != nil {
		if err := a.store.saveMCPSettings(settings, token); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("MCP設定のSQLite保存に失敗しました: %v", err)}
		}
	} else {
		return ActionResult{OK: false, Message: "SQLiteストアが初期化されていないため保存できません"}
	}
	if err := a.restartMCPServer(); err != nil {
		a.setMCPStatus("エラー", err.Error())
		return ActionResult{OK: false, Message: fmt.Sprintf("MCPサーバーの再起動に失敗しました: %v", err)}
	}
	return ActionResult{OK: true, Message: "MCP設定を保存し、待受を再起動しました"}
}

func (a *App) DeleteMCPSettings() ActionResult {
	if a.mcpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.mcpServer.Shutdown(ctx)
		a.mcpServer = nil
	}
	if a.store != nil {
		if err := a.store.deleteMCPSettings(); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("MCP設定の削除に失敗しました: %v", err)}
		}
	}

	settings := defaultMCPSettings(a.dbPath)
	settings.Enabled = false
	settings = sanitizeMCPSettings(settings, "", "停止中", "")

	a.mu.Lock()
	a.mcp = settings
	a.mcpToken = ""
	a.mcpAgents = map[string]time.Time{}
	a.mu.Unlock()

	return ActionResult{OK: true, Message: "MCPサーバー待受設定を削除し、待受を停止しました"}
}

func (a *App) GetMCPSettings() MCPSettings {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mcpSnapshotLocked()
}

func (a *App) mcpSnapshotLocked() MCPSettings {
	settings := a.mcp
	settings.ConnectedAgents = a.connectedAgentsLocked()
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

func (a *App) ApproveCommand(id string, command string) ActionResult {
	id = strings.TrimSpace(id)
	command = strings.TrimSpace(command)
	if id == "" || command == "" {
		return ActionResult{OK: false, Message: "承認対象のコマンドが不足しています"}
	}

	req, ok := a.findRequest(id)
	if !ok {
		return ActionResult{OK: false, Message: "承認対象のリクエストが見つかりません"}
	}
	conn, ok := a.findConnection(req.Host)
	if !ok {
		return ActionResult{OK: false, Message: "対象ホストが接続先一覧にありません"}
	}

	result, err := a.executeSSH(conn, command)
	if err != nil {
		a.updateRequestResult(id, command, "実行失敗", result, err)
		return ActionResult{OK: false, Message: fmt.Sprintf("%s の実行に失敗しました: %s", id, explainSSHError(conn, err))}
	}
	a.updateRequestResult(id, command, "実行済み", result, nil)
	return ActionResult{OK: true, Message: fmt.Sprintf("%s を承認し、実行しました", id)}
}

func (a *App) ApproveCommandAndBypassAgent(id string, command string) ActionResult {
	id = strings.TrimSpace(id)
	req, ok := a.findRequest(id)
	if !ok {
		return ActionResult{OK: false, Message: "承認対象のリクエストが見つかりません"}
	}
	agentName := strings.TrimSpace(req.RequestedBy)
	if agentName == "" || agentName == "未申告エージェント" {
		return ActionResult{OK: false, Message: "承認省略にはエージェント名が必要です"}
	}
	if result := a.setAgentApprovalBypass(agentName, true); !result.OK {
		return result
	}
	result := a.ApproveCommand(id, command)
	if !result.OK {
		return result
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s を承認して実行し、%s の承認省略を許可しました", id, agentName)}
}

func (a *App) RejectCommand(id string) ActionResult {
	id = strings.TrimSpace(id)
	if id == "" {
		return ActionResult{OK: false, Message: "拒否対象のリクエストが選択されていません"}
	}
	a.mu.Lock()
	var updated CommandRequest
	found := false
	for i := range a.requests {
		if a.requests[i].ID == id {
			a.requests[i].Status = "拒否"
			updated = a.requests[i]
			found = true
			break
		}
	}
	a.mu.Unlock()
	if found {
		a.persistCommandRequest(updated)
		return ActionResult{OK: true, Message: fmt.Sprintf("%s を拒否しました", id)}
	}
	return ActionResult{OK: false, Message: "拒否対象のリクエストが見つかりません"}
}

func (a *App) DeleteCommandRequest(id string) ActionResult {
	id = strings.TrimSpace(id)
	if id == "" {
		return ActionResult{OK: false, Message: "削除するコマンド履歴が選択されていません"}
	}

	a.mu.RLock()
	found := false
	for _, req := range a.requests {
		if req.ID == id {
			found = true
			break
		}
	}
	a.mu.RUnlock()
	if !found {
		return ActionResult{OK: false, Message: "削除対象のコマンド履歴が見つかりません"}
	}

	if a.store != nil {
		if err := a.store.deleteCommandRequest(id); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("%s の削除に失敗しました: %v", id, err)}
		}
	}

	a.mu.Lock()
	next := a.requests[:0]
	for _, req := range a.requests {
		if req.ID != id {
			next = append(next, req)
		}
	}
	a.requests = next
	a.mu.Unlock()

	return ActionResult{OK: true, Message: fmt.Sprintf("%s をコマンド履歴から削除しました", id)}
}

func (a *App) setAgentApprovalBypass(agentName string, enabled bool) ActionResult {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return ActionResult{OK: false, Message: "エージェント名が不足しています"}
	}
	policy := AgentPolicy{
		AgentName:      agentName,
		ApprovalBypass: enabled,
		UpdatedAt:      time.Now().Format(time.RFC3339),
	}
	if a.store != nil {
		if err := a.store.saveAgentPolicy(policy); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("%s の承認省略設定を保存できません: %v", agentName, err)}
		}
	}
	a.mu.Lock()
	if a.agentPolicies == nil {
		a.agentPolicies = map[string]AgentPolicy{}
	}
	a.agentPolicies[agentName] = policy
	a.mu.Unlock()
	return ActionResult{OK: true, Message: fmt.Sprintf("%s の承認省略を有効にしました", agentName)}
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

func (a *App) queueCommand(host string, command string, reason string, requestedBy string, risk string) CommandRequest {
	req := newCommandRequest(host, command, reason, requestedBy, risk, "承認待ち")
	a.recordCommandRequest(req)
	return req
}

func newCommandRequest(host string, command string, reason string, requestedBy string, risk string, status string) CommandRequest {
	now := time.Now()
	seq := requestIDSeq.Add(1)
	return CommandRequest{
		ID:          fmt.Sprintf("REQ-%d-%d", now.UnixNano()/int64(time.Millisecond), seq),
		Host:        host,
		User:        "assistant",
		Command:     command,
		Reason:      reason,
		RequestedBy: requestedBy,
		RequestedAt: now.Format("15:04:05"),
		Risk:        risk,
		Status:      status,
		Duration:    "-",
	}
}

func (a *App) recordCommandRequest(req CommandRequest) {
	a.mu.Lock()
	a.requests = append([]CommandRequest{req}, a.requests...)
	a.mu.Unlock()
	a.persistCommandRequest(req)
}

func (a *App) executeCommandAndLog(conn Connection, command string, reason string, requestedBy string, risk string) CommandRequest {
	req := newCommandRequest(conn.Name, command, reason, requestedBy, risk, "自動実行中")
	result, err := a.executeSSH(conn, command)
	req.Stdout = result.Stdout
	req.Stderr = result.Stderr
	if err != nil {
		req.Status = "自動実行失敗"
		if req.Stderr != "" {
			req.Stderr += "\n"
		}
		req.Stderr += explainSSHError(conn, err)
	} else {
		req.Status = "自動実行済み"
	}
	if result.Duration > 0 {
		req.Duration = result.Duration.Round(100 * time.Millisecond).String()
	}
	a.recordCommandRequest(req)
	return req
}

func (a *App) executeSSH(conn Connection, command string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeoutSeconds(conn.CommandTimeout, 30))
	defer cancel()
	a.mu.RLock()
	outputCap := a.mcp.MaxOutputKB * 1024
	strictHostKey := a.mcp.StrictHostKey
	conn.KeyPassphrase = a.sshPassphrases[conn.Name]
	a.mu.RUnlock()
	if outputCap <= 0 {
		outputCap = defaultOutputCap
	}
	return runSSHCommand(ctx, conn, command, outputCap, strictHostKey)
}

func (a *App) findConnection(name string) (Connection, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, conn := range a.conns {
		if conn.Name == name || conn.Host == name {
			return conn, true
		}
	}
	return Connection{}, false
}

func (a *App) setConnectionStatus(name string, status string) {
	a.mu.Lock()
	var updated Connection
	found := false
	for i := range a.conns {
		if a.conns[i].Name == name {
			a.conns[i].Status = status
			a.conns[i].LastChecked = time.Now().Format("15:04")
			updated = a.conns[i]
			found = true
			break
		}
	}
	a.mu.Unlock()
	if found && a.store != nil {
		if err := a.store.saveConnection(updated); err != nil {
			log.Printf("persist connection status: %v", err)
		}
	}
}

func (a *App) findRequest(id string) (CommandRequest, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, req := range a.requests {
		if req.ID == id {
			return req, true
		}
	}
	return CommandRequest{}, false
}

func (a *App) updateRequestResult(id string, command string, status string, result commandResult, execErr error) {
	a.mu.Lock()
	var updated CommandRequest
	found := false
	for i := range a.requests {
		if a.requests[i].ID != id {
			continue
		}
		a.requests[i].Command = command
		a.requests[i].Status = status
		a.requests[i].Stdout = result.Stdout
		a.requests[i].Stderr = result.Stderr
		if execErr != nil {
			if a.requests[i].Stderr != "" {
				a.requests[i].Stderr += "\n"
			}
			a.requests[i].Stderr += execErr.Error()
		}
		if result.Duration > 0 {
			a.requests[i].Duration = result.Duration.Round(100 * time.Millisecond).String()
		}
		updated = a.requests[i]
		found = true
		break
	}
	a.mu.Unlock()
	if found {
		a.persistCommandRequest(updated)
	}
}

func (a *App) persistCommandRequest(req CommandRequest) {
	if a.store == nil {
		return
	}
	if err := a.store.saveCommandRequest(req); err != nil {
		log.Printf("persist command request: %v", err)
	}
}

func explainSSHError(conn Connection, err error) string {
	message := err.Error()
	lower := strings.ToLower(message)
	if strings.Contains(lower, "unable to authenticate") {
		if strings.Contains(conn.AuthMethod, "パスワード") {
			return fmt.Sprintf("パスワード認証に失敗しました。ユーザー名、パスワード、接続先SSHのPasswordAuthentication設定を確認してください。詳細: %v", err)
		}
		return fmt.Sprintf("公開鍵認証に失敗しました。ユーザー名、鍵ファイルパス、接続先ユーザーの ~/.ssh/authorized_keys、またはssh-agent登録鍵を確認してください。パスワードで接続する場合は認証方式を「パスワード保管」に変更してください。詳細: %v", err)
	}
	if strings.Contains(lower, "no such file") || strings.Contains(lower, "read ssh key") {
		return fmt.Sprintf("SSH鍵ファイルを読めません。鍵ファイルパスを確認してください。詳細: %v", err)
	}
	if strings.Contains(lower, "passphrase protected") {
		return message
	}
	if strings.Contains(lower, "key mismatch") {
		return fmt.Sprintf("known_hosts のホスト鍵が一致しません。MCP設定の known_hosts厳格検証をOFFにするか、~/.ssh/known_hosts の該当ホスト鍵を更新してください。詳細: %v", err)
	}
	return message
}

func normalizeConnection(connection Connection) Connection {
	connection.Name = strings.TrimSpace(connection.Name)
	connection.Type = connection.connectionType()
	connection.Host = strings.TrimSpace(connection.Host)
	connection.User = strings.TrimSpace(connection.User)
	connection.Credential = strings.TrimSpace(connection.Credential)
	connection.SerialPort = strings.TrimSpace(connection.SerialPort)
	connection.KeyPassphrase = ""
	if connection.Port == 0 {
		connection.Port = 22
	}
	if connection.ConnectTimeout == 0 {
		connection.ConnectTimeout = 10
	}
	if connection.CommandTimeout == 0 {
		connection.CommandTimeout = 30
	}
	if connection.AuthMethod == "" {
		connection.AuthMethod = "SSH鍵認証"
	}
	if connection.Type == "Serial" && connection.BaudRate == 0 {
		connection.BaudRate = 115200
	}
	return connection
}

func appConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("SSH_GETE_CONFIG_DIR")); dir != "" {
		return dir
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, appName)
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

func defaultConnectionsPath() string {
	return defaultLegacyConnectionsPath()
}

func defaultMCPSettingsPath() string {
	return defaultLegacyMCPSettingsPath()
}

func migrateLegacyData(store *Store, dbPath string, connectionsPath string, settingsPath string) error {
	existingConnections, err := store.loadConnections()
	if err != nil {
		return err
	}
	if len(existingConnections) == 0 {
		legacyConnections, err := loadConnections(connectionsPath)
		if err != nil {
			return err
		}
		for _, conn := range legacyConnections {
			if err := store.saveConnection(conn); err != nil {
				return err
			}
		}
	}

	_, _, loaded, err := store.loadMCPSettings(dbPath)
	if err != nil {
		return err
	}
	if !loaded {
		if _, err := os.Stat(settingsPath); err == nil {
			settings, token, err := loadMCPSettings(settingsPath)
			if err != nil {
				return err
			}
			settings.ConfigPath = dbPath
			if err := store.saveMCPSettings(settings, token); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
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

func saveConnections(path string, conns []Connection) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(conns, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o600)
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

func saveMCPSettings(path string, settings MCPSettings, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	settings.TokenInput = ""
	settings.TokenPreview = ""
	stored := storedMCPSettings{
		MCPSettings: settings,
		Token:       token,
	}
	payload, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o600)
}

func defaultMCPSettings(path string) MCPSettings {
	return MCPSettings{
		Enabled:                true,
		ListenAddress:          "127.0.0.1",
		Port:                   8787,
		Transport:              "Streamable HTTP",
		BaseURL:                "http://127.0.0.1:8787",
		MCPPath:                "/mcp",
		HealthPath:             "/healthz",
		AuditPath:              "/audit/events",
		BearerEnabled:          false,
		TokenName:              "local-admin-token",
		AllowedOrigins:         []string{"http://localhost:*", "wails://wails.localhost"},
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

func sanitizeMCPSettings(settings MCPSettings, token string, status string, lastErr string) MCPSettings {
	if settings.ListenAddress == "" {
		settings.ListenAddress = "127.0.0.1"
	}
	if settings.Port == 0 {
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
	if settings.MaxBodyKB == 0 {
		settings.MaxBodyKB = 256
	}
	if settings.MaxOutputKB == 0 {
		settings.MaxOutputKB = 128
	}
	if settings.RequestTimeout == 0 {
		settings.RequestTimeout = 30
	}
	if settings.DefaultConnectTimeout == 0 {
		settings.DefaultConnectTimeout = 10
	}
	if settings.DefaultCommandTimeout == 0 {
		settings.DefaultCommandTimeout = 30
	}
	if settings.BaseURL == "" {
		settings.BaseURL = fmt.Sprintf("http://%s:%d", settings.ListenAddress, settings.Port)
	}
	if len(settings.AllowedOrigins) == 0 {
		settings.AllowedOrigins = []string{"http://localhost:*", "wails://wails.localhost"}
	}
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

func applyMCPEnv(settings *MCPSettings, token *string) {
	if addr := strings.TrimSpace(os.Getenv("SSH_GETE_MCP_ADDR")); addr != "" {
		host, port, ok := strings.Cut(addr, ":")
		if ok {
			settings.ListenAddress = host
			if parsedPort, err := strconv.Atoi(port); err == nil {
				settings.Port = parsedPort
			}
		}
	}
	if envToken := os.Getenv("SSH_GETE_TOKEN"); envToken != "" {
		*token = envToken
		settings.BearerEnabled = true
	}
	if os.Getenv("SSH_GETE_STRICT_HOSTKEY") == "1" {
		settings.StrictHostKey = true
	}
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
		out[i].Tags = append([]string(nil), out[i].Tags...)
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
			Status:         "Unknown",
			LastChecked:    "-",
		},
	}
}

func seedRequests() []CommandRequest {
	return []CommandRequest{}
}
