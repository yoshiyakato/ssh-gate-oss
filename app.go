package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	// defaultBypassTTL is how long an approval-bypass grant lasts (§5.5).
	defaultBypassTTL = 8 * time.Hour
	// defaultBypassMaxRisk is the highest risk a bypass grant covers; High is
	// always queued regardless.
	defaultBypassMaxRisk = "Medium"
	// agentTokenPrefix marks per-agent tokens so they are distinguishable
	// from the shared MCP token.
	agentTokenPrefix = "sga_"
	// pendingRequestTTL is how long a request waits for a human before it
	// expires. It matches the client_request_id deduplication window.
	pendingRequestTTL = 24 * time.Hour
	// expirySweepInterval is how often pending requests and bypass grants are
	// checked for expiry in the background.
	expirySweepInterval = 30 * time.Second
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
	mcpEnvToken   string
	mcpAgents     map[string]time.Time
	agentPolicies map[string]AgentPolicy
	// agentTokens maps agent name to its per-agent token (hash only). A
	// request authenticated with one may use that agent's approval bypass.
	agentTokens map[string]AgentToken
	// policyMu serializes agent policy and token mutations across the
	// SQLite write and the in-memory publish, so concurrent grant/revoke or
	// issue/revoke end in the same order in both. It is taken before a.mu
	// (never while holding a.mu) and nothing blocks on it while holding a.mu.
	policyMu       sync.Mutex
	sshPassphrases map[string]string
	// reqCond (L = &a.mu) is broadcast whenever a request is added, changes
	// state or is deleted, waking get_request_status long-polls.
	reqCond *sync.Cond
	// mcpRuntime holds the execution and long-poll slots of the MCP server
	// (mcp_status.go). Its zero value is ready to use.
	mcpRuntime mcpRuntime
	// now is the clock for expiry decisions; tests may replace it.
	now func() time.Time

	// appCtx is cancelled (cause errShuttingDown) when the app shuts down;
	// every command execution derives from it or is tied to it by trackExec.
	appCtx    context.Context
	appCancel context.CancelFunc
	// wg counts background loops started from appCtx (the expiry sweeper) so
	// shutdown can wait for them before closing the store. Command
	// executions are counted by execWG instead.
	wg sync.WaitGroup

	lifecycleMu sync.Mutex
	mcpServer   *http.Server
	// mcpServerStop is mcpServer's stop signal (mcp_status.go).
	mcpServerStop *mcpListenerStop

	// execMu is a leaf lock guarding execs/execSeq/execsClosed: never call
	// out while holding it. execs is the one registry of command executions
	// (GUI-approved and approval-bypassed alike): it backs the running
	// counters, DisconnectConnection, and the shutdown drain.
	execMu  sync.Mutex
	execs   map[uint64]runningExec
	execSeq uint64
	// execsClosed is set by drainMCPCommands: from then on trackExec refuses
	// new executions, so execWG.Add never races execWG.Wait.
	execsClosed  bool
	execWG       sync.WaitGroup
	runningTotal atomic.Int64 // §6.1: running commands, shown without a pool
	sessionsSeq  atomic.Uint64
	// sessionsMu serializes building a sessions snapshot with stamping its
	// sessionsSeq, so a higher seq never carries older data. It is the
	// outermost lock: taken only by notifySessionsChanged / GetInitialData,
	// never while holding a.mu, termMu or execMu.
	sessionsMu sync.Mutex

	terminalState
	emit              func(name string, payload any) // test hook; nil means Wails runtime
	onSessionsChanged func()                         // set by NewApp
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
	Credential    string `json:"credential"`
	CredentialSet bool   `json:"credentialSet"`
	KeyPassphrase string `json:"keyPassphrase,omitempty"`
	SerialPort    string `json:"serialPort"`
	BaudRate      int    `json:"baudRate"`
	// DataBits (5-8), Parity (none/odd/even/mark/space) and StopBits
	// ("1"/"1.5"/"2") are serial-only. RTS/CTS flow control is not supported.
	DataBits        int      `json:"dataBits"`
	Parity          string   `json:"parity"`
	StopBits        string   `json:"stopBits"`
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
	stateExpired       = "expired"
	stateWithdrawn     = "withdrawn"
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
	// ExpiresAt is when a pending request becomes expired (RFC3339).
	ExpiresAt string `json:"expiresAt"`
	// TargetFP is user@host:port of the connection when the request was
	// made, so the approval screen can show whether the target changed since.
	TargetFP string `json:"targetFp"`
}

// AgentPolicy is an approval-bypass grant. It only takes effect for requests
// authenticated with that agent's own token (see AgentToken).
type AgentPolicy struct {
	AgentName      string `json:"agentName"`
	ApprovalBypass bool   `json:"approvalBypass"`
	UpdatedAt      string `json:"updatedAt"`
	// MaxRisk is the highest risk auto-executed (Low or Medium; default
	// Medium). High is always queued.
	MaxRisk string `json:"maxRisk"`
	// ScopeConnIDs limits the grant to these connections; empty means all.
	ScopeConnIDs []string `json:"scopeConnIds"`
	GrantedAt    string   `json:"grantedAt"`
	GrantedBy    string   `json:"grantedBy"`
	// ExpiresAt ends the grant (RFC3339). Empty means GrantedAt (or
	// UpdatedAt for legacy rows) + defaultBypassTTL.
	ExpiresAt string `json:"expiresAt"`
	// Derived, never persisted.
	Active   bool `json:"active"`
	HasToken bool `json:"hasToken"`
}

// AgentToken is a per-agent Bearer token. Only the SHA-256 of the token is
// kept; the token itself is shown once when issued.
type AgentToken struct {
	AgentName string `json:"agentName"`
	TokenHash string `json:"-"`
	// Bypass allows this token to use the agent's approval-bypass grant.
	Bypass    bool   `json:"bypass"`
	CreatedAt string `json:"createdAt"`
	// Preview is a non-secret hint (prefix + last 4 characters).
	Preview string `json:"preview"`
}

// mcpCaller is who sent an MCP request, as established by authentication
// (authenticateMCP), not by the self-declared agent_name.
type mcpCaller struct {
	// Agent is the agent bound to the presented per-agent token; empty for
	// the shared token or an unauthenticated loopback client.
	Agent string
	// Shared reports that the shared Bearer token authenticated the request.
	Shared bool
}

// mayBypassAs reports whether the caller proved it is agentName. The shared
// token proves only "some client of this gateway", so it never qualifies.
func (c mcpCaller) mayBypassAs(agentName string) bool {
	return c.Agent != "" && strings.TrimSpace(agentName) == c.Agent
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
	AgentTokens   []AgentToken     `json:"agentTokens"`
	MCP           MCPSettings      `json:"mcp"`
	StorageError  string           `json:"storageError"`
	Sessions      []SessionSummary `json:"sessions"` // every saved connection, in a.conns order
	Terminals     []TerminalInfo   `json:"terminals"`
	RunningTotal  int              `json:"runningTotal"`
	SessionsSeq   uint64           `json:"sessionsSeq"`
}

// SessionSummary is the live runtime state of one saved connection (§5.4
// connState). It is derived on every call and never persisted.
type SessionSummary struct {
	ConnID         string `json:"connId"`
	ConnName       string `json:"connName"`
	Kind           string `json:"kind"`                     // "ssh" | "serial"
	ConnState      string `json:"connState"`                // connected | connecting | error | disconnected | in_use (serial)
	Shells         int    `json:"shells"`                   // open tabs
	Connecting     int    `json:"connecting"`               // tabs still dialing
	Running        int    `json:"running"`                  // commands being executed
	Pending        int    `json:"pending"`                  // requests awaiting approval
	ConnectedSince string `json:"connectedSince,omitempty"` // RFC3339, earliest open tab
	LastError      string `json:"lastError,omitempty"`
	Label          string `json:"label,omitempty"` // serial only: "/dev/cu.usbserial-10 @115200"
}

// sessionsPayload is the sessions:changed event body; the field names match
// DashboardData so the frontend can merge either one the same way.
type sessionsPayload struct {
	Sessions     []SessionSummary `json:"sessions"`
	Terminals    []TerminalInfo   `json:"terminals"`
	RunningTotal int              `json:"runningTotal"`
	SessionsSeq  uint64           `json:"sessionsSeq"`
}

// Connection runtime states (§5.4). idle / available / missing belong to
// later stages and are not produced here.
const (
	connStateConnected    = "connected"
	connStateConnecting   = "connecting"
	connStateError        = "error"
	connStateDisconnected = "disconnected"
	connStateInUse        = "in_use"
)

// runningExec is one command execution in flight, cancellable by connection.
type runningExec struct {
	connID string
	cancel context.CancelCauseFunc
}

var (
	errDisconnected = errors.New("接続を切断したため中止しました")
	errShuttingDown = errors.New("アプリの終了により中断しました")
	// errClientGone cancels an approval-bypassed run whose MCP client
	// disconnected while waiting for the synchronous answer (§5.3).
	errClientGone = errors.New("MCP クライアントが切断したため中断しました")
	// shutdownExecGrace bounds how long drainMCPCommands waits for running
	// commands before cancelling them, and again for cancelled ones to record
	// their outcome (§5.0 step 2). Tests shorten it.
	shutdownExecGrace = 5 * time.Second
)

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
	// SessionID is the terminal tab a terminal API call acted on.
	SessionID string `json:"sessionId,omitempty"`
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
		agentTokens:       map[string]AgentToken{},
		sshPassphrases:    map[string]string{},
		execs:             map[uint64]runningExec{},
		now:               time.Now,
	}
	// One wake-up mechanism for get_request_status long-polls: reqCond.
	app.reqCond = sync.NewCond(&app.mu)
	appCtx, cancelApp := context.WithCancelCause(context.Background())
	app.appCtx = appCtx
	app.appCancel = func() { cancelApp(errShuttingDown) }
	app.onSessionsChanged = app.notifySessionsChanged

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
		tokens, err := store.loadAgentTokens()
		if err != nil {
			log.Printf("load agent tokens from sqlite: %v", err)
		}
		for _, token := range tokens {
			app.agentTokens[token.AgentName] = token
		}
	}
	// Pending rows written before expires_at existed get a full lifetime from
	// now, instead of expiring the moment the update is installed.
	for i := range app.requests {
		req := app.requests[i]
		if req.Status != statePending || req.ExpiresAt != "" {
			continue
		}
		req.ExpiresAt = app.clock().Add(pendingRequestTTL).Format(time.RFC3339)
		if store != nil {
			if err := store.updateCommandRequestFrom(req, statePending); err != nil {
				log.Printf("set expiry of %s: %v", req.ID, err)
				continue
			}
		}
		app.requests[i] = req
	}
	// Expire what lapsed while the app was not running, then keep sweeping.
	app.expireOverdue()
	app.wg.Add(1)
	go app.runExpirySweeper(app.appCtx)
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

// shutdown follows §5.0: stop accepting MCP calls, interrupt running
// commands and give them a bounded grace to record "interrupted", close every
// terminal, stop background loops, then close the store.
//
// appCancel comes first: once appCtx is done trackExec refuses new
// executions (so intake is already stopped), and running ones (GUI-approved
// and approval-bypassed) are cancelled with errShuttingDown. Waiting for them
// first (§5.0 step 2 taken literally) would hold the window open for the
// whole grace on a long command; cancelling first records them as
// interrupted at once. stopMCPServer then signals the listener stop, so
// in-flight execute_command handlers answer instead of holding Shutdown to
// its deadline, and drainMCPCommands waits for every tracked execution to
// store its outcome before the store closes.
func (a *App) shutdown(ctx context.Context) {
	a.appCancel()
	a.cancelExecs(func(runningExec) bool { return true }, errShuttingDown)
	a.stopMCPServer(ctx)
	a.drainMCPCommands(shutdownExecGrace)
	a.CloseAllTerminals()
	a.wg.Wait()
	if err := a.store.Close(); err != nil {
		log.Printf("close sqlite store: %v", err)
	}
}

// drainMCPCommands is shutdown step 2 (§5.0). It closes execution intake
// (trackExec refuses from now on, and MCP execution / long-poll slots are
// refused), waits up to grace for every tracked execution, GUI-approved or
// approval-bypassed, then cancels the rest with errShuttingDown (recorded as
// interrupted) and waits up to grace again for them to store their outcome.
// Each tracker is released only after finishRequest, so once execWG is done
// no request is left running. It reports whether all finished within the
// first grace.
func (a *App) drainMCPCommands(grace time.Duration) bool {
	a.mcpRuntime.close()
	// trackExec adds to execWG under execMu only while intake is open, so
	// after this no Add can race with Wait.
	a.execMu.Lock()
	a.execsClosed = true
	a.execMu.Unlock()
	done := make(chan struct{})
	go func() {
		a.execWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
	}
	a.cancelExecs(func(runningExec) bool { return true }, errShuttingDown)
	timer.Reset(grace)
	select {
	case <-done:
	case <-timer.C:
		log.Printf("shutdown: commands still running %s after cancelling them", grace)
	}
	return false
}

// effectiveTokenLocked is the token MCP clients must present. Callers hold a.mu.
func (a *App) effectiveTokenLocked() string {
	if a.mcpEnvToken != "" {
		return a.mcpEnvToken
	}
	return a.mcpToken
}

// GetInitialData copies the persisted state under a.mu, then (after
// releasing it) adds the live sessions snapshot, so a.mu and termMu are never
// held together.
func (a *App) GetInitialData() DashboardData {
	a.mu.RLock()
	data := DashboardData{
		Connections:   publicConnections(a.conns),
		Requests:      cloneRequests(a.requests),
		AgentPolicies: a.agentPoliciesViewLocked(),
		AgentTokens:   a.agentTokensViewLocked(),
		MCP:           a.mcpSnapshotLocked(),
		StorageError:  a.storageError,
	}
	a.mu.RUnlock()
	snapshot := a.sessionsSnapshot()
	data.Sessions = snapshot.Sessions
	data.Terminals = snapshot.Terminals
	data.RunningTotal = snapshot.RunningTotal
	data.SessionsSeq = snapshot.SessionsSeq
	return data
}

// sessionsSnapshot builds the live sessions view and stamps it with the next
// sessionsSeq. sessionsMu makes the stamp order match the snapshot order.
func (a *App) sessionsSnapshot() sessionsPayload {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	return a.sessionsSnapshotLocked()
}

func (a *App) sessionsSnapshotLocked() sessionsPayload {
	terminals := a.listTerminalInfos()
	if terminals == nil {
		terminals = []TerminalInfo{}
	}
	return sessionsPayload{
		Sessions:     a.sessionSummaries(),
		Terminals:    terminals,
		RunningTotal: int(a.runningTotal.Load()),
		SessionsSeq:  a.sessionsSeq.Add(1),
	}
}

// notifySessionsChanged publishes sessions:changed. It is installed as
// a.onSessionsChanged, so every a.sessionsChanged() lands here. Callers must
// not hold a.mu, termMu or execMu.
func (a *App) notifySessionsChanged() {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	// Emitting under sessionsMu keeps events in sessionsSeq order.
	a.emitEvent(sessionsChangedEvent, a.sessionsSnapshotLocked())
}

// sessionSummaries derives one SessionSummary per saved connection: a.mu is
// held only to copy conns and pending counts, then the terminal and exec
// counters are read with their own locks.
func (a *App) sessionSummaries() []SessionSummary {
	a.mu.RLock()
	conns := make([]Connection, len(a.conns))
	copy(conns, a.conns)
	pending := map[string]int{}
	for _, req := range a.requests {
		if req.Status == statePending && req.ConnectionID != "" {
			pending[req.ConnectionID]++
		}
	}
	a.mu.RUnlock()

	terminals := a.terminalRuntime()
	running := a.runningCounts()
	out := make([]SessionSummary, 0, len(conns))
	for _, conn := range conns {
		rt := terminals[conn.ID]
		s := SessionSummary{
			ConnID:     conn.ID,
			ConnName:   conn.Name,
			Kind:       "ssh",
			Shells:     rt.Open,
			Connecting: rt.Connecting,
			Running:    running[conn.ID],
			Pending:    pending[conn.ID],
			LastError:  rt.LastError,
		}
		serial := conn.connectionType() == "Serial"
		if serial {
			s.Kind = "serial"
			baud := conn.BaudRate
			if baud <= 0 {
				baud = 115200
			}
			s.Label = fmt.Sprintf("%s @%d", conn.SerialPort, baud)
		}
		if !rt.Since.IsZero() {
			s.ConnectedSince = rt.Since.Format(time.RFC3339)
		}
		// "Connected" means "has a live tab" (§5.1); running commands do not
		// change connState.
		switch {
		case s.Shells > 0 && serial:
			s.ConnState = connStateInUse
		case s.Shells > 0:
			s.ConnState = connStateConnected
		case s.Connecting > 0:
			s.ConnState = connStateConnecting
		case s.LastError != "":
			s.ConnState = connStateError
		default:
			s.ConnState = connStateDisconnected
		}
		out = append(out, s)
	}
	return out
}

// trackExec registers one command execution for the running counters and
// for cancellation by DisconnectConnection / shutdown. The returned context
// is cancelled with errDisconnected or errShuttingDown as its cause; done
// must be called exactly once when the execution ends (extra calls are
// ignored), and cancels the context. Once shutdown has begun the context
// comes back already cancelled (errShuttingDown) and nothing is tracked.
func (a *App) trackExec(parent context.Context, connID string) (context.Context, func()) {
	ctx, done, _ := a.beginExec(parent, connID)
	return ctx, done
}

// beginExec is trackExec reporting whether the execution was accepted.
// Callers register before the request becomes running (and release after
// finishRequest), so that a refused execution leaves the request untouched
// and the drain sees every running request through execWG.
func (a *App) beginExec(parent context.Context, connID string) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancelCause(parent)
	// Parents not derived from appCtx still stop on shutdown.
	stop := context.AfterFunc(a.appCtx, func() { cancel(errShuttingDown) })

	a.execMu.Lock()
	if a.execsClosed || a.appCtx.Err() != nil {
		a.execMu.Unlock()
		stop()
		cancel(errShuttingDown)
		return ctx, func() {}, false
	}
	a.execSeq++
	key := a.execSeq
	a.execs[key] = runningExec{connID: connID, cancel: cancel}
	a.execWG.Add(1)
	a.execMu.Unlock()
	a.runningTotal.Add(1)
	a.sessionsChanged()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stop()
			a.execMu.Lock()
			delete(a.execs, key)
			a.execMu.Unlock()
			cancel(context.Canceled)
			a.runningTotal.Add(-1)
			a.execWG.Done()
			a.sessionsChanged()
		})
	}, true
}

// runningCounts returns the number of executions in flight per connection ID.
func (a *App) runningCounts() map[string]int {
	a.execMu.Lock()
	defer a.execMu.Unlock()
	counts := map[string]int{}
	for _, exec := range a.execs {
		counts[exec.connID]++
	}
	return counts
}

// cancelExecsForConnection cancels every execution on connID with cause and
// returns how many were cancelled.
func (a *App) cancelExecsForConnection(connID string, cause error) int {
	return a.cancelExecs(func(exec runningExec) bool { return exec.connID == connID }, cause)
}

func (a *App) cancelExecs(match func(runningExec) bool, cause error) int {
	a.execMu.Lock()
	var cancels []context.CancelCauseFunc
	for _, exec := range a.execs {
		if match(exec) {
			cancels = append(cancels, exec.cancel)
		}
	}
	a.execMu.Unlock()
	for _, cancel := range cancels {
		cancel(cause)
	}
	return len(cancels)
}

// activeWork is what an OnBeforeClose confirmation needs to decide whether
// to ask (§5.0): live tabs (open or connecting), running commands and
// requests awaiting approval.
type activeWork struct {
	Terminals int
	Running   int
	Pending   int
}

func (a *App) activeWork() activeWork {
	work := activeWork{Running: int(a.runningTotal.Load())}
	for _, rt := range a.terminalRuntime() {
		work.Terminals += rt.Open + rt.Connecting
	}
	a.mu.RLock()
	for _, req := range a.requests {
		if req.Status == statePending {
			work.Pending++
		}
	}
	a.mu.RUnlock()
	return work
}

func (w activeWork) any() bool {
	return w.Terminals > 0 || w.Running > 0 || w.Pending > 0
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
	// A GUI test is the one place an unknown host key gets recorded (TOFU),
	// so show what is now trusted.
	if keys, err := recordedHostKeys(hostKeyAddress(conn)); err == nil && len(keys) > 0 {
		fingerprints := make([]string, 0, len(keys))
		for _, key := range keys {
			fingerprints = append(fingerprints, key.Type+" "+key.Fingerprint)
		}
		message += "（ホスト鍵: " + strings.Join(fingerprints, ", ") + "）"
	}
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
		x.BaudRate == y.BaudRate && x.DataBits == y.DataBits && x.Parity == y.Parity && x.StopBits == y.StopBits
}

// DisconnectConnection closes every terminal tab of the connection and
// cancels its running MCP / approved commands (§5.1). The confirmation
// dialog is the frontend's job.
func (a *App) DisconnectConnection(id string) ActionResult {
	conn, ok := a.connectionByID(strings.TrimSpace(id))
	if !ok {
		return ActionResult{OK: false, Code: "not_found", Message: "接続先が見つかりません"}
	}
	k := a.closeTerminalsForConnection(conn.ID)
	j := a.cancelExecsForConnection(conn.ID, errDisconnected)
	a.sessionsChanged()
	return ActionResult{
		OK:      true,
		ID:      conn.ID,
		Message: fmt.Sprintf("%s を切断しました（ターミナル %d 件・実行中コマンド %d 件を終了）", conn.Name, k, j),
	}
}

// SaveConnection creates (empty ID) or updates (ID set, rename allowed) a
// connection. The database is written first; memory only changes on success.
// Open tabs are kept: saving must not drop the user's shells.
func (a *App) SaveConnection(connection Connection) ActionResult {
	result := a.saveConnection(connection)
	if result.OK {
		a.sessionsChanged() // the sessions list (names, new rows) follows a.conns
	}
	return result
}

func (a *App) saveConnection(connection Connection) ActionResult {
	connection = normalizeConnection(connection)
	if connection.Name == "" {
		return ActionResult{OK: false, Message: "接続先名は必須です"}
	}
	if connection.Type == "Serial" {
		if connection.SerialPort == "" {
			return ActionResult{OK: false, Message: "シリアルポートを選択してください"}
		}
		if message := validateSerialParams(connection); message != "" {
			return ActionResult{OK: false, Message: message}
		}
		// A serial row must not keep SSH leftovers: the password would stay
		// in SQLite, and the host would still match MCP host lookups.
		connection.Host, connection.User, connection.Credential = "", "", ""
		connection.MCPFirstTrust = false
	} else {
		if connection.Host == "" || connection.User == "" {
			return ActionResult{OK: false, Message: "ホスト、ユーザー名は必須です"}
		}
		connection.SerialPort, connection.BaudRate = "", 0
		connection.DataBits, connection.Parity, connection.StopBits = 0, "", ""
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
	if connection.Type != "Serial" && connection.usesPassword() && previous.usesPassword() && connection.Credential == "" {
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
	a.sessionsChanged()

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

// setAgentApprovalBypass grants (with the default limits: Medium, all
// connections, defaultBypassTTL) or revokes an agent's approval bypass. The
// grant only takes effect for requests made with the agent's own token.
func (a *App) setAgentApprovalBypass(agentName string, enabled bool) ActionResult {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	return a.setAgentApprovalBypassLocked(agentName, enabled)
}

// setAgentApprovalBypassLocked is setAgentApprovalBypass for callers that
// already hold a.policyMu.
func (a *App) setAgentApprovalBypassLocked(agentName string, enabled bool) ActionResult {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return ActionResult{OK: false, Message: "エージェント名が不足しています"}
	}
	now := a.clock()
	a.mu.RLock()
	policy, existed := a.agentPolicies[agentName]
	a.mu.RUnlock()
	policy.AgentName = agentName
	policy.ApprovalBypass = enabled
	policy.UpdatedAt = now.Format(time.RFC3339)
	if enabled {
		policy.MaxRisk = defaultBypassMaxRisk
		policy.ScopeConnIDs = []string{}
		policy.GrantedAt = policy.UpdatedAt
		policy.GrantedBy = decidedByGUI
		policy.ExpiresAt = now.Add(defaultBypassTTL).Format(time.RFC3339)
	} else if !existed {
		return ActionResult{OK: true, Message: fmt.Sprintf("%s には承認省略が設定されていません", agentName)}
	}
	if err := a.saveAgentPolicy(policy); err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s の承認省略設定を保存できません: %v", agentName, err)}
	}
	if enabled {
		return ActionResult{OK: true, Message: fmt.Sprintf("%s の承認省略を有効にしました（%s まで）", agentName, policy.ExpiresAt)}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s の承認省略を取り消しました", agentName)}
}

// saveAgentPolicy persists the policy, then publishes it in memory. Callers
// hold a.policyMu so the two steps are not interleaved with another write.
func (a *App) saveAgentPolicy(policy AgentPolicy) error {
	policy.Active, policy.HasToken = false, false
	if a.store != nil {
		if err := a.store.saveAgentPolicy(policy); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.agentPolicies[policy.AgentName] = policy
	a.mu.Unlock()
	return nil
}

// RevokeAgentApprovalBypass turns approval bypass off for an agent.
func (a *App) RevokeAgentApprovalBypass(agentName string) ActionResult {
	return a.setAgentApprovalBypass(agentName, false)
}

// RevokeAllAgentApprovalBypasses turns approval bypass off for every agent.
func (a *App) RevokeAllAgentApprovalBypasses() ActionResult {
	a.mu.RLock()
	names := make([]string, 0, len(a.agentPolicies))
	for name, policy := range a.agentPolicies {
		if policy.ApprovalBypass {
			names = append(names, name)
		}
	}
	a.mu.RUnlock()
	slices.Sort(names)
	var failed []string
	for _, name := range names {
		if result := a.setAgentApprovalBypass(name, false); !result.OK {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return ActionResult{OK: false, Message: "承認省略を取り消せなかったエージェントがあります: " + strings.Join(failed, ", ")}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("承認省略を %d 件取り消しました", len(names))}
}

// ListAgentApprovalBypasses returns the granted bypasses (expired ones are
// swept first), newest grant first. Active is false when the agent has no
// token yet, because the grant cannot take effect without one.
func (a *App) ListAgentApprovalBypasses() []AgentPolicy {
	a.expireAgentPolicies()
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := []AgentPolicy{}
	for _, policy := range a.agentPoliciesViewLocked() {
		if policy.ApprovalBypass {
			out = append(out, policy)
		}
	}
	slices.SortFunc(out, func(x, y AgentPolicy) int { return strings.Compare(y.GrantedAt, x.GrantedAt) })
	return out
}

// agentApprovalBypassEnabled reports whether agentName holds an unexpired
// bypass grant (independent of whether it has a token).
func (a *App) agentApprovalBypassEnabled(agentName string) bool {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return false
	}
	now := a.clock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	policy, ok := a.agentPolicies[agentName]
	return ok && policyActiveAt(policy, now)
}

// agentBypassCovers reports whether the agent's token and grant together
// allow auto-executing a command of this risk on conn.
func (a *App) agentBypassCovers(agentName string, conn Connection, risk string) bool {
	now := a.clock()
	a.mu.RLock()
	defer a.mu.RUnlock()
	token, ok := a.agentTokens[agentName]
	if !ok || !token.Bypass {
		return false
	}
	policy, ok := a.agentPolicies[agentName]
	return ok && policyAllows(policy, risk, conn.ID, now)
}

// agentPoliciesViewLocked returns the policies sorted by name with the
// derived fields filled. Callers hold a.mu.
func (a *App) agentPoliciesViewLocked() []AgentPolicy {
	now := a.clock()
	out := make([]AgentPolicy, 0, len(a.agentPolicies))
	for _, policy := range a.agentPolicies {
		policy.ScopeConnIDs = append([]string{}, policy.ScopeConnIDs...)
		token, hasToken := a.agentTokens[policy.AgentName]
		policy.HasToken = hasToken
		policy.Active = policyActiveAt(policy, now) && hasToken && token.Bypass
		out = append(out, policy)
	}
	slices.SortFunc(out, func(x, y AgentPolicy) int { return strings.Compare(x.AgentName, y.AgentName) })
	return out
}

// IssueAgentToken creates (or rotates) the per-agent Bearer token for
// agentName. Only its hash is stored; the token is returned once. A request
// made with it is attributed to agentName and may use that agent's approval
// bypass, which the shared token never can.
func (a *App) IssueAgentToken(agentName string) ActionResult {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return ActionResult{OK: false, Message: "エージェント名を入力してください"}
	}
	if a.store == nil {
		return ActionResult{OK: false, Message: errStoreUnavailable.Error()}
	}
	secret, token, err := newAgentToken(agentName, true)
	if err != nil {
		return ActionResult{OK: false, Message: err.Error()}
	}
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	if err := a.store.saveAgentToken(token); err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s のトークンを保存できません: %v", agentName, err)}
	}
	a.mu.Lock()
	a.agentTokens[agentName] = token
	a.mu.Unlock()
	return ActionResult{
		OK:      true,
		Message: fmt.Sprintf("%s 専用のトークンを発行しました。MCPクライアントの Authorization: Bearer に設定してください（この画面を離れると再表示できません）", agentName),
		Token:   secret,
	}
}

// RevokeAgentToken deletes the agent's token and its bypass grant, so a
// later re-issued token does not silently inherit the old grant.
func (a *App) RevokeAgentToken(agentName string) ActionResult {
	agentName = strings.TrimSpace(agentName)
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	a.mu.RLock()
	_, ok := a.agentTokens[agentName]
	a.mu.RUnlock()
	if !ok {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s のトークンは発行されていません", agentName)}
	}
	if a.store != nil {
		if _, err := a.store.deleteAgentToken(agentName); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("%s のトークンを削除できません: %v", agentName, err)}
		}
	}
	a.mu.Lock()
	delete(a.agentTokens, agentName)
	a.mu.Unlock()
	note := ""
	if a.agentApprovalBypassEnabled(agentName) {
		if result := a.setAgentApprovalBypassLocked(agentName, false); result.OK {
			note = "。承認省略も取り消しました"
		} else {
			note = "（承認省略の取り消しに失敗: " + result.Message + "）"
		}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s のトークンを失効させました%s", agentName, note)}
}

// ListAgentTokens returns the issued per-agent tokens (no secrets).
func (a *App) ListAgentTokens() []AgentToken {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.agentTokensViewLocked()
}

func (a *App) agentTokensViewLocked() []AgentToken {
	out := make([]AgentToken, 0, len(a.agentTokens))
	for _, token := range a.agentTokens {
		token.TokenHash = ""
		out = append(out, token)
	}
	slices.SortFunc(out, func(x, y AgentToken) int { return strings.Compare(x.AgentName, y.AgentName) })
	return out
}

func (a *App) agentHasToken(agentName string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.agentTokens[strings.TrimSpace(agentName)]
	return ok
}

// lookupAgentToken resolves a presented Bearer token to its agent and the
// token's bypass flag. Hashes are compared in constant time against every
// issued token; the store is not touched, so a.mu is held only briefly.
func (a *App) lookupAgentToken(presented string) (string, bool, bool) {
	if !strings.HasPrefix(presented, agentTokenPrefix) {
		return "", false, false
	}
	want := []byte(hashAgentToken(presented))
	a.mu.RLock()
	defer a.mu.RUnlock()
	var match AgentToken
	found := false
	for _, token := range a.agentTokens {
		if subtle.ConstantTimeCompare([]byte(token.TokenHash), want) == 1 {
			match, found = token, true
		}
	}
	return match.AgentName, match.Bypass, found
}

// authenticateMCP establishes the caller of an MCP (or audit) request. A
// per-agent token is always honoured and binds the caller to that agent.
// Otherwise, with Bearer enabled the shared token is required; with it off
// the (loopback-only) caller is anonymous. ok=false means 401.
func (a *App) authenticateMCP(r *http.Request, settings MCPSettings, sharedToken string) (mcpCaller, bool) {
	if presented, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
		if agent, _, ok := a.lookupAgentToken(strings.TrimSpace(presented)); ok {
			return mcpCaller{Agent: agent}, true
		}
	}
	if !settings.BearerEnabled {
		return mcpCaller{}, true
	}
	if checkBearer(r, sharedToken) {
		return mcpCaller{Shared: true}, true
	}
	return mcpCaller{}, false
}

// GetHostKeyInfo returns the host keys ssh-gete has recorded for the
// connection's host:port, for the GUI to show the fingerprints.
func (a *App) GetHostKeyInfo(connID string) (HostKeyInfo, error) {
	return a.hostKeyFingerprint(strings.TrimSpace(connID))
}

// ResetHostKey forgets the recorded host keys for the connection's host:port
// (after a legitimate key change). The next GUI connection test records the
// current key; MCP runs fail with hostkey_unknown until then.
func (a *App) ResetHostKey(connID string) ActionResult {
	connID = strings.TrimSpace(connID)
	conn, ok := a.connectionByID(connID)
	if !ok {
		return ActionResult{OK: false, Message: "接続先が見つかりません"}
	}
	removed, err := a.resetHostKey(connID)
	if err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s のホスト鍵をリセットできません: %v", conn.Name, err), ID: conn.ID}
	}
	if removed == 0 {
		return ActionResult{OK: true, Message: fmt.Sprintf("%s のホスト鍵は記録されていません", conn.Name), ID: conn.ID}
	}
	return ActionResult{
		OK:      true,
		Message: fmt.Sprintf("%s（%s）のホスト鍵 %d 件を削除しました。同じホストとポートの接続先すべてに影響します。接続テストで新しい鍵の指紋を確認してください", conn.Name, hostKeyAddress(conn), removed),
		ID:      conn.ID,
	}
}

func (a *App) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
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
	switch hostKeyErrorKind(err) {
	case "hostkey_unknown":
		return "このホストの鍵はまだ記録されていません。ssh-gete の接続先画面で「接続テスト」を一度実行し、表示された指紋を確認してから再度お試しください。"
	case "hostkey":
		return fmt.Sprintf("ホスト鍵が記録済みのものと一致しません。中間者攻撃の可能性があります。接続先のホスト鍵が正当に変わった場合のみ、接続先画面でホスト鍵をリセットしてから接続テストをやり直してください。詳細: %v", err)
	}
	var missing *passphraseMissingError
	if errors.As(err, &missing) {
		return "SSH鍵がパスフレーズで保護されています。パスフレーズを入力するか、ssh-agent に鍵を登録してください。"
	}
	if strings.Contains(lower, "no such file") || strings.Contains(lower, "read ssh key") {
		return fmt.Sprintf("SSH鍵ファイルを読めません。鍵ファイルパスを確認してください。詳細: %v", err)
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
	// Lowercased like hostKeyAddress, so the saved host, the dial and the
	// known_hosts identity agree.
	connection.Host = strings.ToLower(strings.TrimSpace(connection.Host))
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
	if connection.Type == "Serial" {
		if connection.BaudRate <= 0 {
			connection.BaudRate = 115200
		}
		if connection.DataBits == 0 {
			connection.DataBits = 8
		}
		connection.Parity = strings.ToLower(strings.TrimSpace(connection.Parity))
		if connection.Parity == "" {
			connection.Parity = "none"
		}
		connection.StopBits = strings.TrimSpace(connection.StopBits)
		if connection.StopBits == "" {
			connection.StopBits = "1"
		}
	}
	if connection.Tags == nil {
		connection.Tags = []string{}
	}
	return connection
}

var (
	serialParities = []string{"none", "odd", "even", "mark", "space"}
	serialStopBits = []string{"1", "1.5", "2"}
)

// validateSerialParams checks a normalized serial connection's line settings
// and returns a user-facing message, or "" when they are valid.
func validateSerialParams(conn Connection) string {
	if conn.DataBits < 5 || conn.DataBits > 8 {
		return "データビットは 5〜8 で指定してください"
	}
	if !slices.Contains(serialParities, conn.Parity) {
		return "パリティは none / odd / even / mark / space のいずれかを指定してください"
	}
	if !slices.Contains(serialStopBits, conn.StopBits) {
		return "ストップビットは 1 / 1.5 / 2 のいずれかを指定してください"
	}
	return ""
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
