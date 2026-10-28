package main

// ai_session.go is the core of AI sessions (REQ-016, docs/spec/phase7-ai-session.md):
// the session registry kept apart from the human terminal tabs, ownership,
// limits, idle / max-lifetime expiry, close reasons and the close procedure
// (§2.1, §2.9, §4, §5), the output rings with their cursors (§6.1, §6.4), and
// the PTY ("shell" mode) transport with prompt markers (§1.2, §3.5).
//
// Lock rules (design-v1 §5.0 plus phase7 §5): aiSessionManager.mu (aiMu),
// each aiSession.mu and each aiOutput's locks are leaves. None of them is held
// together with another, nor with a.mu, termMu, execMu or sessionsMu.
// transitionRequest, emitEvent, sessionsChanged, the persistence hooks that
// touch session state, and all network I/O are called with no lock held.

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

const (
	aiSessionDataEvent     = "aisession:data"
	aiSessionsChangedEvent = "aisessions:changed"

	aiSessionIDPrefix = "AIS-"

	aiModeExec  = "exec"
	aiModeShell = "shell"
	aiShellSh   = "sh"
	aiShellBash = "bash"
)

// Session states (§2.9).
const (
	aiStateOpening         = "opening"
	aiStateIdle            = "idle"
	aiStatePendingApproval = "pending_approval"
	aiStateBusy            = "busy"
	aiStateClosing         = "closing"
	aiStateClosed          = "closed"
)

// Close reasons (§2.9). The frontend translates them (ais.close.*).
const (
	aiCloseAgent         = "agent_closed"
	aiCloseHuman         = "human_closed"
	aiCloseIdle          = "idle_timeout"
	aiCloseMaxLifetime   = "max_lifetime"
	aiCloseDisconnected  = "disconnected"
	aiCloseConnDeleted   = "connection_deleted"
	aiCloseTargetChanged = "target_changed"
	aiCloseTokenRevoked  = "token_revoked"
	aiCloseDisabled      = "ai_session_disabled"
	aiCloseRemoteExit    = "remote_exit"
	aiCloseConnLost      = "conn_lost"
	aiCloseAppShutdown   = "app_shutdown"
	aiCloseAppRestart    = "app_restart"
	aiCloseOpenFailed    = "open_failed"
)

// aiCloseReasons is every close reason, in the order of §2.9.
var aiCloseReasons = []string{
	aiCloseAgent, aiCloseHuman, aiCloseIdle, aiCloseMaxLifetime, aiCloseDisconnected,
	aiCloseConnDeleted, aiCloseTargetChanged, aiCloseTokenRevoked, aiCloseDisabled,
	aiCloseRemoteExit, aiCloseConnLost, aiCloseAppShutdown, aiCloseAppRestart, aiCloseOpenFailed,
}

// closed_by values (ai_sessions.closed_by).
const (
	aiClosedByGUI    = "gui"
	aiClosedBySystem = "system"
)

func aiClosedByAgent(agent string) string { return "agent:" + agent }

// Error codes the core returns (MCP maps them to isError results, §2).
const (
	aiErrSessionNotFound     = "session_not_found"
	aiErrSessionClosed       = "session_closed"
	aiErrNotOwner            = "not_owner"
	aiErrSessionBusy         = "session_busy"
	aiErrSessionLimit        = "session_limit"
	aiErrAgentTokenRequired  = "agent_token_required"
	aiErrDisabled            = "ai_session_disabled"
	aiErrShuttingDown        = "shutting_down"
	aiErrShellNotAllowed     = "shell_mode_not_allowed"
	aiErrModeNotAvailable    = "mode_not_available"
	aiErrSerialNotSupported  = "serial_not_supported"
	aiErrHostNotFound        = "host_not_found"
	aiErrAmbiguousHost       = "ambiguous_host"
	aiErrConnectFailed       = "connect_failed"
	aiErrInterruptRateLimit  = "interrupt_rate_limited"
	aiErrInvalidCursor       = "invalid_cursor"
	aiErrInvalidArgument     = "invalid_argument"
	aiErrStorage             = "storage"
	aiErrTargetChanged       = "target_changed"
	aiErrInvalidSettings     = "invalid_settings"
	aiErrIdempotencyConflict = "idempotency_conflict"
	aiErrorKindNoPOSIXShell  = "no_posix_shell"
	aiErrorKindIncomplete    = "incomplete_line"
	aiErrorKindCanceled      = "canceled"
	aiErrorKindTimeout       = "timeout"
	aiErrorKindTransportBusy = "transport_busy"
	aiErrorKindCwdGone       = "cwd_gone"
)

// Tunables; package variables so tests can shrink them.
var (
	aiSweepInterval = 5 * time.Second
	aiTombstoneTTL  = 5 * time.Minute
	// A tombstone keeps only the tail read_output / the viewer still need,
	// and there are at most this many of them (per owner / in all).
	aiTombstoneRingBytes = 64 * 1024
	aiTombstonesPerOwner = 16
	aiTombstonesTotal    = 64
	aiInterruptMinGap    = time.Second
	aiShellReadyTimeout  = 10 * time.Second
	aiKeepaliveInterval  = 30 * time.Second
	aiKeepaliveTimeout   = 10 * time.Second
	aiLineCancelGrace    = 2 * time.Second
	aiCloseGrace         = 3 * time.Second
	aiLineOutputMax      = 16 * 1024
	aiRecentLinesMax     = 50
	aiEscapeStringMax    = 4 * 1024
	aiShellPtyCols       = 200
	aiShellPtyRows       = 50
	aiDefaultLineTimeout = 30 * time.Second
)

var aiSessionIDPattern = regexp.MustCompile(`^AIS-[A-Z2-7]{26}$`)

var aiIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newAISessionID is AIS- plus 128 random bits in base32 (26 characters).
func newAISessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return aiSessionIDPrefix + aiIDEncoding.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Errors

// aiSessError is a coded failure of the AI session core. Extra carries the
// fields §2 adds to the error (scope, close_reason, request_id, ...).
type aiSessError struct {
	Code    string
	Message string
	Extra   map[string]any
	cause   error
}

func (e *aiSessError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func (e *aiSessError) Unwrap() error { return e.cause }

func newAISessError(code, message string, extra map[string]any) *aiSessError {
	return &aiSessError{Code: code, Message: message, Extra: extra}
}

// aiSessErrorCode is the code of err, or "" when err is not an aiSessError.
func aiSessErrorCode(err error) string {
	var e *aiSessError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// aiSessionClosedError is the cancel cause of a session's context. It
// unwraps to errShuttingDown on app_shutdown (finishRequest then records
// interrupted) and to errDisconnected on disconnected (classified canceled).
type aiSessionClosedError struct{ Reason string }

func (e *aiSessionClosedError) Error() string { return "ai session closed: " + e.Reason }

func (e *aiSessionClosedError) Unwrap() error {
	switch e.Reason {
	case aiCloseAppShutdown:
		return errShuttingDown
	case aiCloseDisconnected:
		return errDisconnected
	}
	return context.Canceled
}

var (
	errAITransportBusy   = errors.New("ai transport: previous line has not returned to the prompt")
	errAITransportClosed = errors.New("ai transport closed")
	errAIIncompleteLine  = errors.New("ai transport: line is incomplete (continuation prompt)")
	errAILineTimeout     = errors.New("ai line timed out")
	errAICwdGone         = errors.New("the session's working directory no longer exists (exit 201)")
)

// ---------------------------------------------------------------------------
// Settings (§5)

// AISessionSettings are the AI session limits, kept in their own table
// (ai_session_settings) rather than MCPSettings (§7.2).
type AISessionSettings struct {
	Enabled       bool     `json:"enabled"`
	IdleMinutes   int      `json:"idleMinutes"`
	MaxMinutes    int      `json:"maxMinutes"`
	PerAgent      int      `json:"perAgent"`
	Total         int      `json:"total"`
	BypassMaxRisk string   `json:"bypassMaxRisk"`
	ShellConnIDs  []string `json:"shellConnIds"`
	RingKB        int      `json:"ringKb"`
	AuditKB       int      `json:"auditKb"`
}

func defaultAISessionSettings() AISessionSettings {
	return AISessionSettings{
		Enabled:       false,
		IdleMinutes:   10,
		MaxMinutes:    60,
		PerAgent:      3,
		Total:         16,
		BypassMaxRisk: "Low",
		ShellConnIDs:  []string{},
		RingKB:        1024,
		AuditKB:       1024,
	}
}

// validateAISessionSettings checks the §5 ranges and returns the cleaned
// value, or the offending field.
func validateAISessionSettings(s AISessionSettings) (AISessionSettings, string) {
	inRange := func(v, lo, hi int) bool { return v >= lo && v <= hi }
	switch {
	case !inRange(s.IdleMinutes, 1, 120):
		return s, "idleMinutes"
	case !inRange(s.MaxMinutes, 5, 480):
		return s, "maxMinutes"
	case !inRange(s.PerAgent, 1, 8):
		return s, "perAgent"
	case !inRange(s.Total, 1, 64):
		return s, "total"
	case s.BypassMaxRisk != "Low" && s.BypassMaxRisk != "Medium":
		return s, "bypassMaxRisk"
	case !inRange(s.RingKB, 64, 8192):
		return s, "ringKb"
	case !inRange(s.AuditKB, 0, 16384):
		return s, "auditKb"
	}
	ids := make([]string, 0, len(s.ShellConnIDs))
	for _, id := range s.ShellConnIDs {
		id = strings.TrimSpace(id)
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	s.ShellConnIDs = ids
	return s, ""
}

// ---------------------------------------------------------------------------
// Views

// AISessionInfo is one session as the GUI list and list_sessions show it.
type AISessionInfo struct {
	ID               string `json:"id"`
	Agent            string `json:"agent"`
	ConnID           string `json:"connId"`
	ConnName         string `json:"connName"`
	Mode             string `json:"mode"`
	Shell            string `json:"shell"`
	State            string `json:"state"`
	Cwd              string `json:"cwd"`
	Reason           string `json:"reason"`
	OpenedAt         string `json:"openedAt"`
	LastActivityAt   string `json:"lastActivityAt"`
	IdleExpiresAt    string `json:"idleExpiresAt,omitempty"`
	ExpiresAt        string `json:"expiresAt"`
	ClosedAt         string `json:"closedAt,omitempty"`
	CloseReason      string `json:"closeReason,omitempty"`
	ClosedBy         string `json:"closedBy,omitempty"`
	Lines            int    `json:"lines"`
	PendingRequestID string `json:"pendingRequestId,omitempty"`
	RunningRequestID string `json:"runningRequestId,omitempty"`
	Tainted          bool   `json:"tainted"`
	OutputBytes      uint64 `json:"outputBytes"`
	RawBytes         uint64 `json:"rawBytes"`
	Redactions       int    `json:"redactions"`
}

// AISessionLine is one sent line as the session remembers it (the
// authoritative state is the command_requests row).
type AISessionLine struct {
	RequestID   string `json:"requestId"`
	Seq         int    `json:"seq"`
	Line        string `json:"line"`
	State       string `json:"state"`
	ExitCode    *int   `json:"exitCode,omitempty"`
	ErrorKind   string `json:"errorKind,omitempty"`
	CursorStart uint64 `json:"cursorStart"`
	CursorEnd   uint64 `json:"cursorEnd"`
	QueuedAt    string `json:"queuedAt"`
}

// AISessionDetail is GetAISession's answer.
type AISessionDetail struct {
	OK      bool            `json:"ok"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Session AISessionInfo   `json:"session"`
	Lines   []AISessionLine `json:"lines"`
}

// AISessionRawRead is ReadAISessionRaw's answer (base64 bytes for xterm).
type AISessionRawRead struct {
	OK          bool   `json:"ok"`
	Code        string `json:"code,omitempty"`
	Data        string `json:"data"`
	StartCursor uint64 `json:"startCursor"`
	Cursor      uint64 `json:"cursor"`
	EndCursor   uint64 `json:"endCursor"`
	MissedBytes uint64 `json:"missedBytes"`
	More        bool   `json:"more"`
	State       string `json:"state"`
}

// AISessionCounts feeds the sidebar line (§8.1).
type AISessionCounts struct {
	Open            int  `json:"open"`
	PendingApproval int  `json:"pendingApproval"`
	Enabled         bool `json:"enabled"`
}

// aiSessionsChangedPayload is the aisessions:changed event.
type aiSessionsChangedPayload struct {
	ID          string          `json:"id"`
	State       string          `json:"state"`
	CloseReason string          `json:"closeReason,omitempty"`
	Counts      AISessionCounts `json:"counts"`
}

// aiSessionDataPayload is the aisession:data event: raw bytes for the
// read-only viewer. Cursor is the raw cursor just after Data.
type aiSessionDataPayload struct {
	ID      string `json:"id"`
	Data    string `json:"data"` // base64
	Cursor  uint64 `json:"cursor"`
	Dropped int    `json:"dropped,omitempty"`
}

// ---------------------------------------------------------------------------
// Host, hooks and transport contracts

// aiSessionHost is what the core needs from the App. Every method is called
// with no AI lock held and may take a.mu.
type aiSessionHost interface {
	now() time.Time
	emit(name string, payload any)
	sessionsChanged()
	agentHasToken(agent string) bool
	// targetFP is the connection's current user@host:port; ok=false when the
	// connection no longer exists.
	targetFP(connID string) (fp string, ok bool)
	// settleLines starts the gate's close: a pending line is withdrawn and a
	// running one cancelled (approval CAS). It does not wait.
	settleLines(g *aiLineGate, reason string)
	// reconcileLine frees the gate when its pending line was decided
	// elsewhere (rejected, expired, deleted).
	reconcileLine(g *aiLineGate)
}

// aiSessionHooks are optional persistence / audit callbacks (storage team,
// wired by the Integrator). nil members are skipped.
type aiSessionHooks struct {
	// onOpen inserts the ai_sessions row; an error aborts the open (storage).
	onOpen func(info AISessionInfo, clientRequestID string) error
	// onClose writes closed_at, close_reason, closed_by and the counters.
	onClose func(info AISessionInfo)
	// onEvent appends an ai_session_events row.
	onEvent func(sessionID, kind string, detail map[string]any)
	// onText receives every text chunk (audit). Called under the session's
	// output write lock: it must not call back into the session or manager.
	onText func(sessionID string, cursorStart uint64, text []byte)
	// newTextFilter builds the per-session text pipeline (ANSI strip and
	// secret redaction); nil means newAIPlainText.
	newTextFilter func() aiTextFilter
	// saveSettings persists the settings before they take effect.
	saveSettings func(AISessionSettings) error
	// newAudit builds the session's batched audit writer for its text
	// (ai_session_output); when set it replaces onText.
	newAudit func(sessionID string, limitBytes int64) *aiAuditWriter
	// lineQueued persists the ai_session_lines row before the request is
	// recorded; an error refuses the line (storage).
	lineQueued func(rec aiLineRecord) error
	// lineFinished writes cursor_end of a line.
	lineFinished func(rec aiLineRecord)
}

// aiTransportOptions configures one transport.
type aiTransportOptions struct {
	Mode  string
	Shell string
	// Out receives remote output (markers already removed).
	Out io.Writer
	// NewFilter builds a text filter for per-line stdout / stderr records
	// (exec mode); nil means newAIPlainText.
	NewFilter func() aiTextFilter
}

// aiTransportFactory opens a transport for a mode. It honours ctx (closing
// the session while it dials cancels ctx).
type aiTransportFactory func(ctx context.Context, conn Connection, opt aiTransportOptions) (aiTransport, error)

// aiTransport runs lines on the remote side. StartLine is reached only from
// aiLineGate.startLine through aiSessionRunner (§3.5).
type aiTransport interface {
	// StartLine hands one approved line to the remote side. Exec mode builds
	// the stdin script from plan (cwd, env overlay, cd probe); shell mode
	// writes plan.Line to the PTY.
	StartLine(ctx context.Context, plan aiLinePlan) (aiLineHandle, error)
	// Interrupt sends Ctrl-C (shell) / SIGINT (exec) to the running line.
	Interrupt() error
	// Ready reports that a new line can be started (the prompt is back).
	Ready() bool
	// Cwd is the working directory the transport found at open ("" if
	// unknown).
	Cwd() string
	// Close releases the channel and the client (owner only).
	Close() error
	// Done is closed when the remote side ended; EndReason then says why
	// (remote_exit or conn_lost).
	Done() <-chan struct{}
	EndReason() string
}

// aiLineHandle is one line in flight on a transport.
type aiLineHandle interface {
	Done() <-chan struct{}
	Outcome() aiTransportOutcome
}

// aiTransportOutcome is how a line ended on the transport. Stdout / Stderr
// are set by transports that keep them apart (exec mode), already filtered;
// nil means "take the line's slice of the text ring".
type aiTransportOutcome struct {
	ExitCode *int
	Err      error
	NewCwd   string
	Stdout   []byte
	Stderr   []byte
}

// ---------------------------------------------------------------------------
// Session

// aiSession is one AI-only session: lifecycle, transport and output. The
// line state (cwd, env overlay, taint, the single line in flight) lives in
// its aiLineGate, which is the only path from an agent line to the remote
// side (§3.5). Fields above mu are set at creation and never change; the
// rest are guarded by mu.
type aiSession struct {
	id, owner, connID, connName, targetFP string
	mode, shell, reason, clientRequestID  string
	openedAt                              time.Time
	mgr                                   *aiSessionManager
	out                                   *aiOutput
	ctx                                   context.Context
	cancel                                context.CancelCauseFunc
	openDone                              chan struct{} // closed when the opener stored the transport (or failed)
	done                                  chan struct{} // closed when the session is fully closed
	closeOnce                             sync.Once
	audit                                 *aiAuditWriter // nil without persistence

	mu            sync.Mutex
	state         string // opening | idle | closing | closed (busy / pending come from the gate)
	transport     aiTransport
	gate          *aiLineGate
	storageFailed bool
	lastActivity  time.Time
	expiresAt     time.Time
	closeReason   string
	closedBy      string
	closedAt      time.Time
	lastInterrupt time.Time
	// rowInserted: onOpen recorded the session (ai_sessions row). closeRecorded:
	// onClose was called for it. Whichever of close() and the opener sees the
	// other's step last calls onClose, so an inserted row is always closed
	// with its real reason.
	rowInserted   bool
	closeRecorded bool
}

// ID is the session id (AIS-…).
func (s *aiSession) ID() string { return s.id }

// Owner is the agent that opened it.
func (s *aiSession) Owner() string { return s.owner }

// Output is the session's output rings.
func (s *aiSession) Output() *aiOutput { return s.out }

// Done is closed once the session is closed.
func (s *aiSession) Done() <-chan struct{} { return s.done }

// Gate is the session's line gate (nil while opening).
func (s *aiSession) Gate() *aiLineGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gate
}

func (s *aiSession) now() time.Time { return s.mgr.host.now() }

// touch records an owner tool call (send_line / read_output / interrupt);
// it restarts the idle timer.
func (s *aiSession) touch() {
	now := s.now()
	s.mu.Lock()
	if s.state != aiStateClosing && s.state != aiStateClosed {
		s.lastActivity = now
	}
	s.mu.Unlock()
}

// closedErrLocked is the session_closed error. Callers hold s.mu.
func (s *aiSession) closedErrLocked() *aiSessError {
	return newAISessError(aiErrSessionClosed, "AI セッションは終了しています / the AI session is closed",
		map[string]any{"close_reason": s.closeReason})
}

// aiSessionSnap is a consistent copy of the session plus its gate. The two
// locks are taken one after the other, never together.
type aiSessionSnap struct {
	State         string
	Base          string
	CloseReason   string
	ClosedBy      string
	ClosedAt      time.Time
	LastActivity  time.Time
	ExpiresAt     time.Time
	StorageFailed bool
	Transport     aiTransport
	Gate          aiLineGateView
	HasGate       bool
}

func (s *aiSession) snap() aiSessionSnap {
	s.mu.Lock()
	sn := aiSessionSnap{Base: s.state, CloseReason: s.closeReason, ClosedBy: s.closedBy, ClosedAt: s.closedAt,
		LastActivity: s.lastActivity, ExpiresAt: s.expiresAt, StorageFailed: s.storageFailed, Transport: s.transport}
	g := s.gate
	s.mu.Unlock()
	if g != nil {
		sn.Gate, sn.HasGate = g.view(), true
		if sn.Gate.LastActivity.After(sn.LastActivity) {
			sn.LastActivity = sn.Gate.LastActivity
		}
	}
	sn.State = sn.Base
	if sn.Base == aiStateIdle {
		switch {
		case sn.HasGate && sn.Gate.Inflight != nil && sn.Gate.Inflight.Phase == aiLinePending:
			sn.State = aiStatePendingApproval
		case sn.HasGate && sn.Gate.Inflight != nil:
			sn.State = aiStateBusy
		case sn.Transport != nil && !sn.Transport.Ready():
			sn.State = aiStateBusy
		}
	}
	return sn
}

// State is the current session state (§2.9).
func (s *aiSession) State() string { return s.snap().State }

// Closed reports whether the session is closing or closed.
func (s *aiSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == aiStateClosing || s.state == aiStateClosed
}

// interrupt sends one Ctrl-C / SIGINT to the running line (§2.5). No
// approval: it can only stop things. At most one per aiInterruptMinGap.
func (s *aiSession) interrupt() (bool, string, error) {
	now := s.now()
	sn := s.snap()
	s.mu.Lock()
	if s.state == aiStateClosing || s.state == aiStateClosed {
		err := s.closedErrLocked()
		s.mu.Unlock()
		return false, "", err
	}
	if !s.lastInterrupt.IsZero() && now.Sub(s.lastInterrupt) < aiInterruptMinGap {
		s.mu.Unlock()
		return false, "", newAISessError(aiErrInterruptRateLimit, "interrupt は 1 秒に 1 回までです / at most one interrupt per second", nil)
	}
	tr := s.transport
	reqID := ""
	running := tr != nil && !tr.Ready()
	if sn.HasGate && sn.Gate.Inflight != nil && sn.Gate.Inflight.Phase == aiLineRunning {
		running = true
		reqID = sn.Gate.Inflight.RequestID
	}
	if running {
		s.lastInterrupt = now
	}
	s.lastActivity = now
	s.mu.Unlock()
	if !running || tr == nil {
		return false, reqID, nil
	}
	err := tr.Interrupt()
	s.mgr.event(s.id, "interrupt", map[string]any{"request_id": reqID, "ok": err == nil})
	return err == nil, reqID, err
}

// markStorageFailed stops further send_line until the session closes
// (§7.2: no execution without an audit trail).
func (s *aiSession) markStorageFailed() {
	s.mu.Lock()
	already := s.storageFailed
	s.storageFailed = true
	s.mu.Unlock()
	if !already {
		log.Printf("ai session %s: audit storage failed; further lines are refused", s.id)
		s.mgr.host.emit(aiSessionsChangedEvent, aiSessionsChangedPayload{ID: s.id, State: s.State(), Counts: s.mgr.Counts()})
	}
}

// StorageFailed reports whether an audit write failed.
func (s *aiSession) StorageFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storageFailed
}

// info is the list view of the session.
func (s *aiSession) info(settings AISessionSettings) AISessionInfo {
	textEnd, rawEnd, redactions := s.out.Totals()
	sn := s.snap()
	info := AISessionInfo{
		ID: s.id, Agent: s.owner, ConnID: s.connID, ConnName: s.connName, Mode: s.mode, Shell: s.shell,
		State: sn.State, Reason: s.reason,
		OpenedAt:       rfc3339(s.openedAt),
		LastActivityAt: rfc3339(sn.LastActivity),
		ExpiresAt:      rfc3339(sn.ExpiresAt),
		CloseReason:    sn.CloseReason, ClosedBy: sn.ClosedBy,
		OutputBytes: textEnd, RawBytes: rawEnd, Redactions: redactions,
	}
	if sn.HasGate {
		info.Cwd, info.Tainted, info.Lines = sn.Gate.Cwd, sn.Gate.Tainted, sn.Gate.Seq
		if in := sn.Gate.Inflight; in != nil {
			if in.Phase == aiLinePending {
				info.PendingRequestID = in.RequestID
			} else {
				info.RunningRequestID = in.RequestID
			}
		}
	}
	if !sn.ClosedAt.IsZero() {
		info.ClosedAt = rfc3339(sn.ClosedAt)
	}
	if sn.State == aiStateIdle {
		idle := sn.LastActivity.Add(time.Duration(settings.IdleMinutes) * time.Minute)
		if idle.After(sn.ExpiresAt) {
			idle = sn.ExpiresAt
		}
		info.IdleExpiresAt = rfc3339(idle)
	}
	return info
}

// recentLines lists the lines the gate remembers, plus the one in flight.
func (s *aiSession) recentLines() []AISessionLine {
	sn := s.snap()
	out := []AISessionLine{}
	if !sn.HasGate {
		return out
	}
	add := func(r aiLineRecord) {
		out = append(out, AISessionLine{RequestID: r.RequestID, Seq: r.Seq, Line: r.Line, State: r.State,
			ExitCode: r.ExitCode, CursorStart: r.CursorStart, CursorEnd: r.CursorEnd, QueuedAt: rfc3339(r.QueuedAt)})
	}
	for _, r := range sn.Gate.History {
		add(r)
	}
	if in := sn.Gate.Inflight; in != nil {
		r := *in
		if r.State == "" {
			r.State = statePending
			if r.Phase == aiLineRunning {
				r.State = stateRunning
			}
		}
		add(r)
	}
	slices.SortFunc(out, func(x, y AISessionLine) int { return x.Seq - y.Seq })
	return out
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// expiryReason is the close reason the sweeper applies, or "". Pending
// approval and running lines keep a session from idling out; the maximum
// lifetime always applies (§5).
func (s *aiSession) expiryReason(now time.Time, settings AISessionSettings) string {
	sn := s.snap()
	if sn.Base != aiStateIdle {
		return ""
	}
	if !now.Before(sn.ExpiresAt) {
		return aiCloseMaxLifetime
	}
	if sn.State == aiStateIdle && now.Sub(sn.LastActivity) >= time.Duration(settings.IdleMinutes)*time.Minute {
		return aiCloseIdle
	}
	return ""
}

// close runs the close procedure once (§5). It reports whether this call
// closed the session.
func (s *aiSession) close(reason, by string) bool {
	did := false
	s.closeOnce.Do(func() {
		did = true
		m := s.mgr
		s.mu.Lock()
		s.state = aiStateClosing
		s.closeReason = reason
		s.closedBy = by
		s.mu.Unlock()
		// Step 1: cancel the open in progress and the running line.
		s.cancel(&aiSessionClosedError{Reason: reason})
		s.out.Notify()
		m.changed(s)

		deadline := time.NewTimer(aiCloseGrace)
		defer deadline.Stop()
		select {
		case <-s.openDone:
		case <-deadline.C:
		}
		s.mu.Lock()
		tr, g := s.transport, s.gate
		s.mu.Unlock()
		// Step 2: withdraw the pending line / cancel the running one through
		// the gate (CAS in transitionRequest).
		var runDone <-chan struct{}
		if g != nil {
			runDone = g.runDoneChan()
			m.host.settleLines(g, reason)
		}
		// Step 3: the owner closes the transport and its client.
		if tr != nil {
			_ = tr.Close()
		}
		if runDone != nil {
			select {
			case <-runDone:
			case <-deadline.C:
			}
		}
		s.out.Close()
		if s.audit != nil {
			s.audit.close()
		}

		// Step 4: persist the close, then step 5: tombstone and notify.
		now := m.host.now()
		s.mu.Lock()
		s.state = aiStateClosed
		s.closedAt = now
		record := s.rowInserted && !s.closeRecorded
		if record {
			s.closeRecorded = true
		}
		s.mu.Unlock()
		if g != nil {
			g.markClosed()
		}
		info := s.info(m.Settings())
		if m.hooks.onClose != nil && record {
			m.hooks.onClose(info)
		}
		m.retire(s, now)
		close(s.done)
		m.changed(s)
		m.host.sessionsChanged()
	})
	return did
}

// ---------------------------------------------------------------------------
// Runner: the gate's aiLineRunner over the session's transport

// aiSessionRunner runs one line the gate decided to deliver. It adds the
// gateway echo, waits for the transport (timeout and cancellation send one
// interrupt, then give aiLineCancelGrace), and reports the line's AI text.
type aiSessionRunner struct{ s *aiSession }

func (r aiSessionRunner) RunLine(ctx context.Context, plan aiLinePlan) (aiLineOutcome, error) {
	s := r.s
	s.mu.Lock()
	tr, state := s.transport, s.state
	s.mu.Unlock()
	if state == aiStateClosing || state == aiStateClosed {
		return aiLineOutcome{}, fmt.Errorf("%w: %w", errAISessionClosed, context.Canceled)
	}
	if tr == nil || !tr.Ready() {
		return aiLineOutcome{}, errAITransportBusy
	}
	lineCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(s.ctx, func() { cancel(context.Cause(s.ctx)) })
	defer stop()

	started := s.now()
	redBefore := s.out.Redactions()
	cursorStart := s.out.TextEnd()
	// The gateway's own echo (§1.2: ECHO=0, and exec mode has no echo).
	_, _ = s.out.Write([]byte("$ " + plan.Line + "\n"))
	echoEnd := s.out.TextEnd()

	h, err := tr.StartLine(lineCtx, plan)
	var oc aiTransportOutcome
	if err != nil {
		oc = aiTransportOutcome{Err: err}
	} else {
		oc = waitTransportLine(lineCtx, tr, h)
	}
	s.out.Flush()
	cursorEnd := s.out.TextEnd()
	out := aiLineOutcome{NewCwd: oc.NewCwd, Duration: s.now().Sub(started), CursorStart: cursorStart, CursorEnd: cursorEnd}
	if oc.Stdout != nil || oc.Stderr != nil {
		out.Stdout, out.Stderr = string(oc.Stdout), string(oc.Stderr)
	} else {
		read, _ := s.out.ReadText(echoEnd, 1<<20)
		out.Stdout = read.Text
	}
	if n := s.out.Redactions() - redBefore; n > 0 {
		s.mgr.event(s.id, aiEventSecretRedacted, map[string]any{"seq": plan.Seq, "request_id": plan.RequestID, "count": n})
	}
	if oc.ExitCode != nil {
		out.ExitCode = *oc.ExitCode
	}
	runErr := oc.Err
	if runErr == nil && oc.ExitCode == nil {
		runErr = errors.New("the remote side reported no exit status")
	}
	return out, runErr
}

// waitTransportLine waits for the line to end. When ctx ends (timeout,
// close, shutdown) it sends one interrupt and waits aiLineCancelGrace for the
// prompt; the error is ctx's cause.
func waitTransportLine(ctx context.Context, tr aiTransport, h aiLineHandle) aiTransportOutcome {
	select {
	case <-h.Done():
		return h.Outcome()
	case <-ctx.Done():
	}
	_ = tr.Interrupt()
	grace := time.NewTimer(aiLineCancelGrace)
	defer grace.Stop()
	oc := aiTransportOutcome{}
	select {
	case <-h.Done():
		oc = h.Outcome()
	case <-grace.C:
	case <-tr.Done():
	}
	oc.Err = context.Cause(ctx)
	if errors.Is(oc.Err, context.DeadlineExceeded) {
		oc.Err = fmt.Errorf("%w: %w", errAILineTimeout, context.DeadlineExceeded)
	}
	return oc
}

// ---------------------------------------------------------------------------
// Manager

type aiTombstone struct {
	s        *aiSession
	closedAt time.Time
	seq      uint64 // retire order (the clock may not move between closes)
}

// aiSessionManager is the registry of AI sessions (aiMu = mu). It is kept
// apart from terminalState: the human tabs live in a.terms and no AI path
// reads them (§4).
type aiSessionManager struct {
	host       aiSessionHost
	hooks      aiSessionHooks
	transports map[string]aiTransportFactory // set before use; read-only after
	newID      func() string

	mu         sync.Mutex
	sessions   map[string]*aiSession
	tombstones map[string]aiTombstone
	byIdem     map[string]string // owner \x00 client_request_id -> session id
	settings   AISessionSettings
	closed     bool // shutdown began: no more opens
	tombSeq    uint64
}

func newAISessionManager(host aiSessionHost, hooks aiSessionHooks) *aiSessionManager {
	return &aiSessionManager{
		host:       host,
		hooks:      hooks,
		transports: map[string]aiTransportFactory{},
		newID:      newAISessionID,
		sessions:   map[string]*aiSession{},
		tombstones: map[string]aiTombstone{},
		byIdem:     map[string]string{},
		settings:   defaultAISessionSettings(),
	}
}

func (m *aiSessionManager) event(id, kind string, detail map[string]any) {
	if m.hooks.onEvent != nil {
		m.hooks.onEvent(id, kind, detail)
	}
}

// changed emits aisessions:changed. Never called with a lock held.
func (m *aiSessionManager) changed(s *aiSession) {
	sn := s.snap()
	state, reason := sn.State, sn.CloseReason
	m.host.emit(aiSessionsChangedEvent, aiSessionsChangedPayload{ID: s.id, State: state, CloseReason: reason, Counts: m.Counts()})
}

// Settings returns a copy of the settings.
func (m *aiSessionManager) Settings() AISessionSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings
	s.ShellConnIDs = slices.Clone(s.ShellConnIDs)
	return s
}

// setSettings replaces the settings in memory (after they were stored).
func (m *aiSessionManager) setSettings(s AISessionSettings) {
	m.mu.Lock()
	m.settings = s
	m.mu.Unlock()
}

// aiOpenRequest is one open_session after the MCP layer resolved its host.
type aiOpenRequest struct {
	Owner           string
	Conn            Connection
	Reason          string
	Mode            string
	Shell           string
	ClientRequestID string
}

func idemKey(owner, crid string) string { return owner + "\x00" + crid }

// open opens a session for req.Owner (§2.2). The second result reports a
// deduplicated open (same owner and client_request_id as an open session).
func (m *aiSessionManager) open(ctx context.Context, req aiOpenRequest) (*aiSession, bool, error) {
	req.Owner = strings.TrimSpace(req.Owner)
	req.ClientRequestID = strings.TrimSpace(req.ClientRequestID)
	if req.Owner == "" {
		return nil, false, newAISessError(aiErrAgentTokenRequired, "エージェント専用トークンが必要です / an agent token is required", nil)
	}
	if req.Conn.connectionType() == "Serial" {
		return nil, false, newAISessError(aiErrSerialNotSupported, "シリアル接続では AI セッションを開けません / serial connections are not supported", nil)
	}
	if req.Mode == "" {
		req.Mode = aiModeExec
	}
	if req.Shell == "" {
		req.Shell = aiShellSh
	}
	if req.Mode != aiModeExec && req.Mode != aiModeShell {
		return nil, false, newAISessError(aiErrInvalidArgument, "mode は exec か shell です / mode must be exec or shell", map[string]any{"field": "mode"})
	}
	if req.Shell != aiShellSh && req.Shell != aiShellBash {
		return nil, false, newAISessError(aiErrInvalidArgument, "shell は sh か bash です / shell must be sh or bash", map[string]any{"field": "shell"})
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(req.Reason)); n < 1 || n > 500 {
		return nil, false, newAISessError(aiErrInvalidArgument, "reason は 1〜500 文字です / reason must be 1-500 characters", map[string]any{"field": "reason"})
	}
	now := m.host.now()

	m.mu.Lock()
	settings := m.settings
	switch {
	case m.closed:
		m.mu.Unlock()
		return nil, false, newAISessError(aiErrShuttingDown, "アプリを終了しています / the app is shutting down", nil)
	case !settings.Enabled:
		m.mu.Unlock()
		return nil, false, newAISessError(aiErrDisabled, "AI セッションは無効です。GUI の設定で有効にしてください / AI sessions are disabled; enable them in the GUI", nil)
	}
	if req.ClientRequestID != "" {
		if s := m.sessions[m.byIdem[idemKey(req.Owner, req.ClientRequestID)]]; s != nil {
			m.mu.Unlock()
			if s.connID != req.Conn.ID || s.mode != req.Mode {
				return nil, false, newAISessError(aiErrIdempotencyConflict, "同じ client_request_id で別のセッションがあります / client_request_id was used for another session", nil)
			}
			<-s.openDone
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.state == aiStateClosing || s.state == aiStateClosed {
				return nil, false, s.closedErrLocked()
			}
			return s, true, nil
		}
	}
	if req.Mode == aiModeShell && !slices.Contains(settings.ShellConnIDs, req.Conn.ID) {
		m.mu.Unlock()
		return nil, false, newAISessError(aiErrShellNotAllowed, "この接続先では shell モードが許可されていません / shell mode is not allowed for this host", nil)
	}
	factory := m.transports[req.Mode]
	if factory == nil {
		m.mu.Unlock()
		return nil, false, newAISessError(aiErrModeNotAvailable, "このモードは使えません / this mode is not available", map[string]any{"mode": req.Mode})
	}
	perAgent := 0
	for _, s := range m.sessions {
		if s.owner == req.Owner {
			perAgent++
		}
	}
	if perAgent >= settings.PerAgent {
		m.mu.Unlock()
		return nil, false, newAISessError(aiErrSessionLimit, "エージェントあたりのセッション数の上限です / per-agent session limit reached",
			map[string]any{"scope": "per_agent", "limit": settings.PerAgent})
	}
	if len(m.sessions) >= settings.Total {
		m.mu.Unlock()
		return nil, false, newAISessError(aiErrSessionLimit, "セッション数の上限です / total session limit reached",
			map[string]any{"scope": "total", "limit": settings.Total})
	}
	id := m.newID()
	for m.sessions[id] != nil || m.tombstones[id].s != nil {
		id = m.newID()
	}
	sctx, cancel := context.WithCancelCause(context.Background())
	s := &aiSession{
		id: id, owner: req.Owner, connID: req.Conn.ID, connName: req.Conn.Name, targetFP: targetFingerprint(req.Conn),
		mode: req.Mode, shell: req.Shell, reason: strings.TrimSpace(req.Reason), clientRequestID: req.ClientRequestID,
		openedAt: now, mgr: m, ctx: sctx, cancel: cancel,
		openDone: make(chan struct{}), done: make(chan struct{}),
		state: aiStateOpening, lastActivity: now,
		expiresAt: now.Add(time.Duration(settings.MaxMinutes) * time.Minute),
	}
	m.sessions[id] = s
	if req.ClientRequestID != "" {
		m.byIdem[idemKey(req.Owner, req.ClientRequestID)] = id
	}
	m.mu.Unlock()

	var filter aiTextFilter
	if m.hooks.newTextFilter != nil {
		filter = m.hooks.newTextFilter()
	}
	var onText func(uint64, []byte)
	if m.hooks.newAudit != nil {
		s.audit = m.hooks.newAudit(id, int64(settings.AuditKB)*1024)
	}
	switch {
	case s.audit != nil:
		onText = s.audit.add
	case m.hooks.onText != nil:
		onText = func(start uint64, p []byte) { m.hooks.onText(id, start, p) }
	}
	s.out = newAIOutput(settings.RingKB*1024, filter, onText, func(data []byte, cursor uint64, dropped int) {
		m.host.emit(aiSessionDataEvent, aiSessionDataPayload{ID: id, Data: base64.StdEncoding.EncodeToString(data), Cursor: cursor, Dropped: dropped})
	})

	openCtx, openCancel := context.WithCancelCause(sctx)
	stopCaller := context.AfterFunc(ctx, func() { openCancel(context.Cause(ctx)) })
	tr, err := factory(openCtx, req.Conn, aiTransportOptions{Mode: req.Mode, Shell: req.Shell, Out: s.out, NewFilter: m.hooks.newTextFilter})
	stopCaller()
	openCancel(nil)
	if err == nil && m.hooks.onOpen != nil {
		if herr := m.hooks.onOpen(s.info(settings), req.ClientRequestID); herr != nil {
			_ = tr.Close()
			tr = nil
			err = &aiSessError{Code: aiErrStorage, Message: "監査に記録できません / could not record the session", cause: herr}
		} else {
			// The row exists now. If close() already finished (it gave up
			// waiting on openDone), it could not record the close: do it here.
			s.mu.Lock()
			s.rowInserted = true
			late := s.state == aiStateClosed && !s.closeRecorded
			if late {
				s.closeRecorded = true
			}
			s.mu.Unlock()
			if late && m.hooks.onClose != nil {
				m.hooks.onClose(s.info(settings))
			}
		}
	}
	s.mu.Lock()
	// closing covers a close() still running and one that already finished
	// after its openDone grace: in both cases the session must not come back
	// to life, and the opener owns (and closes) the transport it got.
	closing := s.state == aiStateClosing || s.state == aiStateClosed
	if err == nil && !closing {
		s.transport = tr
		s.gate = m.newGate(s, tr.Cwd())
		s.state = aiStateIdle
		s.lastActivity = m.host.now()
	}
	s.mu.Unlock()
	if err == nil && closing {
		_ = tr.Close()
	}
	close(s.openDone)

	if err != nil {
		s.closeOnce.Do(func() {
			s.cancel(&aiSessionClosedError{Reason: aiCloseOpenFailed})
			s.out.Close()
			if s.audit != nil {
				s.audit.close()
			}
			m.mu.Lock()
			delete(m.sessions, id)
			if m.byIdem[idemKey(req.Owner, req.ClientRequestID)] == id {
				delete(m.byIdem, idemKey(req.Owner, req.ClientRequestID))
			}
			m.mu.Unlock()
			s.mu.Lock()
			s.state, s.closeReason, s.closedBy, s.closedAt = aiStateClosed, aiCloseOpenFailed, aiClosedBySystem, m.host.now()
			s.mu.Unlock()
			close(s.done)
		})
		if code := aiSessErrorCode(err); code != "" {
			return nil, false, err
		}
		if closing || sctx.Err() != nil {
			return nil, false, newAISessError(aiErrSessionClosed, "開いている途中で終了しました / closed while opening", map[string]any{"close_reason": s.closeReason})
		}
		return nil, false, &aiSessError{Code: aiErrConnectFailed, Message: err.Error(),
			Extra: map[string]any{"error_kind": aiConnectErrorKind(err)}, cause: err}
	}
	if closing {
		// Closed (human / shutdown) while dialing: the transport was closed
		// above; wait for close() to finish.
		<-s.done
		s.mu.Lock()
		e := s.closedErrLocked()
		s.mu.Unlock()
		return nil, false, e
	}
	m.event(id, "open", map[string]any{"mode": req.Mode, "shell": req.Shell, "connection_id": req.Conn.ID})
	go m.watch(s, tr)
	m.changed(s)
	m.host.sessionsChanged()
	return s, false, nil
}

// aiConnectErrorKind classifies a failed open (§2.2 error_kind).
func aiConnectErrorKind(err error) string {
	var se *aiSessError
	if errors.As(err, &se) {
		if k, ok := se.Extra["error_kind"].(string); ok {
			return k
		}
	}
	if errors.Is(err, errAINoPOSIXShell) {
		return aiErrorKindNoPOSIXShell
	}
	return classifyExecError(err)
}

var errAINoPOSIXShell = errors.New("no POSIX shell answered with the prompt marker")

// newGate builds the session's line gate once its transport is up.
func (m *aiSessionManager) newGate(s *aiSession, cwd string) *aiLineGate {
	return newAILineGate(aiLineGateConfig{
		SessionID: s.id, Owner: s.owner, ConnID: s.connID, TargetFP: s.targetFP, Mode: s.mode, Cwd: cwd,
		ExpiresAt: s.expiresAt, Runner: aiSessionRunner{s}, Now: m.host.now,
		Hooks: aiLineHooks{
			Event:        func(kind string, detail map[string]any) { m.event(s.id, kind, detail) },
			LineQueued:   m.hooks.lineQueued,
			LineFinished: m.hooks.lineFinished,
			OutputTail:   s.outputTail,
			CloseSession: func(reason string) { s.close(reason, aiClosedBySystem) },
			Changed:      func() { m.changed(s) },
			Cursor:       s.out.TextEnd,
		},
	})
}

// outputTail is the last n lines of AI text (approval screen, §3.7).
func (s *aiSession) outputTail(n int) string {
	end := s.out.TextEnd()
	from := uint64(0)
	if end > 8192 {
		from = end - 8192
	}
	read, err := s.out.ReadText(from, 8192)
	if err != nil {
		return ""
	}
	text := strings.TrimRight(read.Text, "\n")
	lines := strings.Split(text, "\n")
	if read.StartCursor > 0 && len(lines) > 1 {
		lines = lines[1:] // the first one may be cut
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// watch closes the session when its transport ends on its own.
func (m *aiSessionManager) watch(s *aiSession, tr aiTransport) {
	select {
	case <-s.done:
	case <-tr.Done():
		reason := tr.EndReason()
		if reason == "" {
			reason = aiCloseConnLost
		}
		s.close(reason, aiClosedBySystem)
	}
}

// retire moves a closed session from the live map to the tombstones.
func (m *aiSessionManager) retire(s *aiSession, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[s.id] == s {
		delete(m.sessions, s.id)
	}
	if s.clientRequestID != "" && m.byIdem[idemKey(s.owner, s.clientRequestID)] == s.id {
		delete(m.byIdem, idemKey(s.owner, s.clientRequestID))
	}
	if s.out != nil {
		s.out.Retire(aiTombstoneRingBytes)
	}
	m.tombSeq++
	m.tombstones[s.id] = aiTombstone{s: s, closedAt: now, seq: m.tombSeq}
	m.capTombstonesLocked(s.owner)
}

// capTombstonesLocked keeps at most aiTombstonesPerOwner tombstones for owner
// and aiTombstonesTotal in all, dropping the oldest. Only live sessions count
// toward the open limits, so without this an agent could grow memory by
// opening and closing sessions in a loop.
func (m *aiSessionManager) capTombstonesLocked(owner string) {
	oldest := func(match func(aiTombstone) bool) (string, int) {
		id, n := "", 0
		var at uint64
		for tid, t := range m.tombstones {
			if !match(t) {
				continue
			}
			n++
			if id == "" || t.seq < at {
				id, at = tid, t.seq
			}
		}
		return id, n
	}
	for {
		id, n := oldest(func(t aiTombstone) bool { return t.s.owner == owner })
		if n <= aiTombstonesPerOwner {
			break
		}
		delete(m.tombstones, id)
	}
	for {
		id, n := oldest(func(aiTombstone) bool { return true })
		if n <= aiTombstonesTotal {
			break
		}
		delete(m.tombstones, id)
	}
}

// lookup finds a session for owner (§2.1). A closed session within the
// tombstone window comes back together with a session_closed error so
// read_output can still read its last output. owner "" skips the owner check
// (human bindings only).
func (m *aiSessionManager) lookup(id, owner string, humans bool) (*aiSession, error) {
	notFound := newAISessError(aiErrSessionNotFound, "AI セッションが見つかりません / AI session not found", nil)
	if !aiSessionIDPattern.MatchString(id) {
		return nil, notFound
	}
	m.mu.Lock()
	s := m.sessions[id]
	tomb, inTomb := m.tombstones[id]
	m.mu.Unlock()
	if s == nil && inTomb {
		if m.host.now().Sub(tomb.closedAt) >= aiTombstoneTTL {
			return nil, notFound
		}
		s = tomb.s
	}
	if s == nil {
		return nil, notFound
	}
	if !humans && s.owner != owner {
		return nil, newAISessError(aiErrNotOwner, "このセッションの所有者ではありません / not the owner of this session", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == aiStateClosing || s.state == aiStateClosed {
		return s, s.closedErrLocked()
	}
	return s, nil
}

// closeSession closes one live session; false if it was not open.
func (m *aiSessionManager) closeSession(id, reason, by string) bool {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return false
	}
	return s.close(reason, by)
}

// closeWhere closes every live session matching, in parallel, waiting up to
// grace in total. It returns how many it closed (or started closing).
func (m *aiSessionManager) closeWhere(match func(*aiSession) bool, reason, by string, grace time.Duration) int {
	m.mu.Lock()
	var list []*aiSession
	for _, s := range m.sessions {
		if match(s) {
			list = append(list, s)
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	n := 0
	for _, s := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.close(reason, by) {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	if grace <= 0 {
		<-done
	} else {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			log.Printf("ai sessions: %d still closing after %s", len(list), grace)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n < len(list) {
		// Count the stragglers too: they have been told to close.
		for _, s := range list {
			select {
			case <-s.done:
			default:
				n++
			}
		}
		n = min(n, len(list))
	}
	return n
}

// shutdown refuses new opens and closes everything with reason.
func (m *aiSessionManager) shutdown(reason string, grace time.Duration) int {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return m.closeWhere(func(*aiSession) bool { return true }, reason, aiClosedBySystem, grace)
}

// sweep is one pass of the sweeper (§5): idle, max lifetime, revoked token,
// disabled feature, deleted or changed target; and old tombstones.
func (m *aiSessionManager) sweep() int {
	now := m.host.now()
	m.mu.Lock()
	settings := m.settings
	list := make([]*aiSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	for id, t := range m.tombstones {
		if now.Sub(t.closedAt) >= aiTombstoneTTL {
			delete(m.tombstones, id)
		}
	}
	m.mu.Unlock()
	closed := 0
	for _, s := range list {
		if g := s.Gate(); g != nil {
			m.host.reconcileLine(g)
		}
		reason := s.expiryReason(now, settings)
		if reason == "" && s.State() != aiStateOpening {
			switch fp, ok := m.host.targetFP(s.connID); {
			case !settings.Enabled:
				reason = aiCloseDisabled
			case !m.host.agentHasToken(s.owner):
				reason = aiCloseTokenRevoked
			case !ok:
				reason = aiCloseConnDeleted
			case fp != s.targetFP:
				reason = aiCloseTargetChanged
			}
		}
		if reason != "" && s.close(reason, aiClosedBySystem) {
			closed++
		}
	}
	return closed
}

// run is the sweeper loop; it stops with ctx.
func (m *aiSessionManager) run(ctx context.Context) {
	ticker := time.NewTicker(aiSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

// Counts is the sidebar summary.
func (m *aiSessionManager) Counts() AISessionCounts {
	m.mu.Lock()
	list := make([]*aiSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	enabled := m.settings.Enabled
	m.mu.Unlock()
	c := AISessionCounts{Enabled: enabled}
	for _, s := range list {
		s.mu.Lock()
		if s.state != aiStateClosing && s.state != aiStateClosed {
			c.Open++
			if s.state == aiStatePendingApproval {
				c.PendingApproval++
			}
		}
		s.mu.Unlock()
	}
	return c
}

// list returns the sessions of owner ("" = everyone, for humans), newest
// first, with tombstones when includeClosed.
func (m *aiSessionManager) list(owner string, includeClosed bool) []AISessionInfo {
	now := m.host.now()
	m.mu.Lock()
	settings := m.settings
	var list []*aiSession
	for _, s := range m.sessions {
		if owner == "" || s.owner == owner {
			list = append(list, s)
		}
	}
	if includeClosed {
		for _, t := range m.tombstones {
			if (owner == "" || t.s.owner == owner) && now.Sub(t.closedAt) < aiTombstoneTTL {
				list = append(list, t.s)
			}
		}
	}
	m.mu.Unlock()
	out := make([]AISessionInfo, 0, len(list))
	for _, s := range list {
		out = append(out, s.info(settings))
	}
	slices.SortFunc(out, func(x, y AISessionInfo) int {
		if c := strings.Compare(y.OpenedAt, x.OpenedAt); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

// ---------------------------------------------------------------------------
// Output rings (§6.1, §6.4)

// aiRing keeps the last len(data) bytes of a stream. end is the absolute
// offset (cursor) of the next byte; the held bytes are [end-held, end).
type aiRing struct {
	data []byte
	held int
	end  uint64
}

func newAIRing(size int) aiRing { return aiRing{data: make([]byte, max(size, 1))} }

func (r *aiRing) start() uint64 { return r.end - uint64(r.held) }

// write appends p; the byte at cursor c always sits at index c % size.
func (r *aiRing) write(p []byte) {
	size := len(r.data)
	if len(p) > size {
		r.end += uint64(len(p) - size)
		p = p[len(p)-size:]
	}
	pos := int(r.end % uint64(size))
	n := copy(r.data[pos:], p)
	copy(r.data, p[n:])
	r.end += uint64(len(p))
	r.held = min(r.held+len(p), size)
}

// shrink keeps only the last keep bytes in a ring of that size. Cursors stay
// absolute, so readers see the dropped head as missed bytes.
func (r *aiRing) shrink(keep int) {
	keep = max(keep, 1)
	if len(r.data) <= keep {
		return
	}
	n := min(r.held, keep)
	tail := r.read(r.end-uint64(n), n)
	nr := aiRing{data: make([]byte, keep), end: r.end - uint64(n)}
	nr.write(tail)
	*r = nr
}

// read copies up to n bytes starting at from (which must be within the ring).
func (r *aiRing) read(from uint64, n int) []byte {
	avail := int(r.end - from)
	n = min(n, avail)
	if n <= 0 {
		return nil
	}
	size := len(r.data)
	out := make([]byte, n)
	pos := int(from % uint64(size))
	c := copy(out, r.data[pos:])
	copy(out[c:], r.data)
	return out
}

// aiTextFilter turns raw terminal bytes into AI text (ANSI strip, secret
// redaction). Feed is called in stream order; it may hold bytes back.
type aiTextFilter interface {
	Feed(p []byte) []byte
}

// aiRedactionCounter is implemented by filters that redact secrets.
type aiRedactionCounter interface {
	Redactions() int
}

// aiOutput holds the raw ring (human viewer, never stored) and the text ring
// (AI, audit) with their cursors. writeMu serializes writers (stream order,
// hook order); mu guards the rings. Both are leaves, and mu is never held
// while calling a hook or emitting.
type aiOutput struct {
	writeMu sync.Mutex
	filter  aiTextFilter
	onText  func(start uint64, p []byte)
	emitter *outputBuffer

	mu     sync.Mutex
	cond   *sync.Cond
	raw    aiRing
	text   aiRing
	gen    uint64
	closed bool
}

// newAIOutput builds the rings. emitRaw receives coalesced raw chunks
// (terminal.go's outputBuffer: 16 ms / 32 KiB) with the raw cursor after the
// chunk.
func newAIOutput(ringBytes int, filter aiTextFilter, onText func(uint64, []byte), emitRaw func(data []byte, cursor uint64, dropped int)) *aiOutput {
	if ringBytes <= 0 {
		ringBytes = defaultAISessionSettings().RingKB * 1024
	}
	if filter == nil {
		filter = newAIPlainText()
	}
	o := &aiOutput{filter: filter, onText: onText, raw: newAIRing(ringBytes), text: newAIRing(ringBytes)}
	o.cond = sync.NewCond(&o.mu)
	if emitRaw != nil {
		var emitted uint64
		o.emitter = newOutputBuffer(func(data []byte, dropped int) {
			emitted += uint64(dropped) + uint64(len(data))
			emitRaw(data, emitted, dropped)
		})
	}
	return o
}

// Write appends remote output (or the gateway's echo).
func (o *aiOutput) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	o.writeMu.Lock()
	defer o.writeMu.Unlock()
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return len(p), nil
	}
	o.raw.write(p)
	o.mu.Unlock()
	if o.emitter != nil {
		_, _ = o.emitter.Write(p)
	}
	o.appendTextLocked(o.filter.Feed(p))
	return len(p), nil
}

// appendTextLocked adds filtered text to the text ring and the audit hook.
// Callers hold writeMu.
func (o *aiOutput) appendTextLocked(text []byte) {
	if len(text) == 0 {
		return
	}
	o.mu.Lock()
	start := o.text.end
	o.text.write(text)
	o.cond.Broadcast()
	o.mu.Unlock()
	if o.onText != nil {
		o.onText(start, text)
	}
}

// aiTextFlusher is implemented by filters that hold bytes back (an
// unfinished line, a PEM block): Flush releases them at the end of a line.
type aiTextFlusher interface {
	Flush() []byte
}

// Flush releases text the filter still holds (end of a line, close).
func (o *aiOutput) Flush() {
	o.writeMu.Lock()
	defer o.writeMu.Unlock()
	o.mu.Lock()
	closed := o.closed
	o.mu.Unlock()
	if closed {
		return
	}
	if f, ok := o.filter.(aiTextFlusher); ok {
		o.appendTextLocked(f.Flush())
	}
}

// Redactions is how many secrets the filter replaced so far.
func (o *aiOutput) Redactions() int {
	_, _, n := o.Totals()
	return n
}

// Notify wakes long-polls (line state changed, session closing).
func (o *aiOutput) Notify() {
	o.mu.Lock()
	o.gen++
	o.cond.Broadcast()
	o.mu.Unlock()
}

// Close stops accepting output, flushes the viewer events and wakes waiters.
// The rings stay readable (tombstone).
func (o *aiOutput) Close() {
	o.writeMu.Lock()
	if f, ok := o.filter.(aiTextFlusher); ok {
		o.mu.Lock()
		closed := o.closed
		o.mu.Unlock()
		if !closed {
			o.appendTextLocked(f.Flush())
		}
	}
	o.mu.Lock()
	already := o.closed
	o.closed = true
	o.gen++
	o.cond.Broadcast()
	o.mu.Unlock()
	o.writeMu.Unlock()
	if !already && o.emitter != nil {
		o.emitter.close()
	}
}

// Retire shrinks both rings to their last keep bytes once the session is a
// tombstone (the rings are allocated at full size up front).
func (o *aiOutput) Retire(keep int) {
	o.mu.Lock()
	o.raw.shrink(keep)
	o.text.shrink(keep)
	o.mu.Unlock()
}

// TextEnd is the text cursor after the last byte (MarkLine in the design).
func (o *aiOutput) TextEnd() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.end
}

// Totals reports the text and raw byte totals and the redaction count.
func (o *aiOutput) Totals() (text, raw uint64, redactions int) {
	o.mu.Lock()
	text, raw = o.text.end, o.raw.end
	o.mu.Unlock()
	if rc, ok := o.filter.(aiRedactionCounter); ok {
		o.writeMu.Lock()
		redactions = rc.Redactions()
		o.writeMu.Unlock()
	}
	return text, raw, redactions
}

// aiTextRead is one read_output slice.
type aiTextRead struct {
	Text        string
	StartCursor uint64 // where Text starts
	Cursor      uint64 // the next since_cursor
	EndCursor   uint64 // end of the stream now
	MissedBytes uint64 // bytes before StartCursor the reader never got
	More        bool
}

// ReadText returns AI text from since, at most max bytes, cut on UTF-8
// boundaries. since before the ring start reads from the start and reports
// missed_bytes; since after the end is invalid_cursor.
func (o *aiOutput) ReadText(since uint64, max int) (aiTextRead, error) {
	if max <= 0 {
		max = 16 * 1024
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	end, start := o.text.end, o.text.start()
	if since > end {
		return aiTextRead{EndCursor: end}, newAISessError(aiErrInvalidCursor, "カーソルが出力の末尾より後です / cursor is past the end of the output",
			map[string]any{"end_cursor": end})
	}
	var missed uint64
	if since < start {
		missed = start - since
		since = start
		// The ring may have dropped the head of a character: skip its tail.
		for since < end {
			b := o.text.read(since, 1)
			if len(b) == 0 || utf8.RuneStart(b[0]) {
				break
			}
			since++
			missed++
		}
	}
	data := o.text.read(since, max)
	if len(data) == max && since+uint64(len(data)) < end {
		data = trimIncompleteRune(data)
	}
	next := since + uint64(len(data))
	return aiTextRead{Text: string(data), StartCursor: since, Cursor: next, EndCursor: end, MissedBytes: missed, More: next < end}, nil
}

// trimIncompleteRune drops a trailing partial UTF-8 sequence.
func trimIncompleteRune(p []byte) []byte {
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				return p[:i]
			}
			return p
		}
	}
	return p
}

// aiRawRead is one raw slice for the viewer.
type aiRawRead struct {
	Data        []byte
	StartCursor uint64
	Cursor      uint64
	EndCursor   uint64
	MissedBytes uint64
	More        bool
}

// ReadRaw returns raw bytes from since (viewer), at most max.
func (o *aiOutput) ReadRaw(since uint64, max int) (aiRawRead, error) {
	if max <= 0 {
		max = 256 * 1024
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	end, start := o.raw.end, o.raw.start()
	if since > end {
		return aiRawRead{EndCursor: end}, newAISessError(aiErrInvalidCursor, "cursor is past the end of the output", map[string]any{"end_cursor": end})
	}
	var missed uint64
	if since < start {
		missed, since = start-since, start
	}
	data := o.raw.read(since, max)
	next := since + uint64(len(data))
	return aiRawRead{Data: data, StartCursor: since, Cursor: next, EndCursor: end, MissedBytes: missed, More: next < end}, nil
}

// WaitText blocks until text past since exists, Notify / Close is called, or
// ctx ends. It reports whether new text is available.
func (o *aiOutput) WaitText(ctx context.Context, since uint64) bool {
	stop := context.AfterFunc(ctx, func() {
		o.mu.Lock()
		o.cond.Broadcast()
		o.mu.Unlock()
	})
	defer stop()
	o.mu.Lock()
	defer o.mu.Unlock()
	gen := o.gen
	for o.text.end <= since && !o.closed && o.gen == gen && ctx.Err() == nil {
		o.cond.Wait()
	}
	return o.text.end > since
}

// ---------------------------------------------------------------------------
// Default text filter

// aiPlainText is the default AI text pipeline: a stateful terminal stripper
// (§6.2) that survives escape sequences split across chunks and keeps
// output valid UTF-8. Secret redaction is a separate filter (redact team)
// that can wrap this one through hooks.newTextFilter.
type aiPlainText struct {
	state    int
	strLen   int
	dropped  bool // oversized string notice already given for this string
	pendCR   bool
	utf8Tail []byte
}

const (
	aiTextNormal = iota
	aiTextEsc
	aiTextCSI
	aiTextString    // OSC / DCS / SOS / PM / APC body
	aiTextStringEsc // ESC seen inside a string (maybe ST)
	aiTextEscInter  // ESC + intermediate bytes (e.g. ESC ( B)
)

func newAIPlainText() *aiPlainText { return &aiPlainText{} }

// Feed implements aiTextFilter.
func (f *aiPlainText) Feed(p []byte) []byte {
	if len(f.utf8Tail) > 0 {
		p = append(f.utf8Tail, p...)
		f.utf8Tail = nil
	}
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		b := p[i]
		switch f.state {
		case aiTextEsc:
			i++
			switch {
			case b == '[':
				f.state = aiTextCSI
			case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
				f.state, f.strLen, f.dropped = aiTextString, 0, false
			case b >= 0x20 && b <= 0x2f:
				f.state = aiTextEscInter
			default:
				f.state = aiTextNormal
			}
			continue
		case aiTextEscInter:
			i++
			if b < 0x20 || b > 0x2f {
				f.state = aiTextNormal
			}
			continue
		case aiTextCSI:
			i++
			if b >= 0x40 && b <= 0x7e {
				f.state = aiTextNormal
			}
			continue
		case aiTextString, aiTextStringEsc:
			i++
			switch {
			case b == 0x07:
				f.state = aiTextNormal
			case f.state == aiTextStringEsc && b == '\\':
				f.state = aiTextNormal
			case b == 0x1b:
				f.state = aiTextStringEsc
			default:
				f.state = aiTextString
				f.strLen++
				if f.strLen > aiEscapeStringMax && !f.dropped {
					f.dropped = true
					out = append(out, "[ssh-gate: oversized escape dropped]"...)
				}
			}
			continue
		}
		// Normal state.
		if f.pendCR {
			f.pendCR = false
			if b != '\n' {
				out = append(out, '\n')
			}
		}
		switch {
		case b == 0x1b:
			f.state = aiTextEsc
			i++
		case b == '\r':
			f.pendCR = true
			i++
		case b == '\n' || b == '\t':
			out = append(out, b)
			i++
		case b < 0x20 || b == 0x7f:
			i++
		case b < utf8.RuneSelf:
			out = append(out, b)
			i++
		default:
			if !utf8.FullRune(p[i:]) {
				f.utf8Tail = append([]byte(nil), p[i:]...)
				return out
			}
			r, size := utf8.DecodeRune(p[i:])
			i += size
			switch {
			case r == utf8.RuneError && size == 1:
				out = append(out, "�"...)
			case r == 0x9b: // C1 CSI
				f.state = aiTextCSI
			case r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f: // C1 OSC/DCS/SOS/PM/APC
				f.state, f.strLen, f.dropped = aiTextString, 0, false
			case r >= 0x80 && r <= 0x9f:
			default:
				out = utf8.AppendRune(out, r)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// PTY transport: shell mode (§1.2)

// aiShellTransport runs a non-login, rc-less shell on a PTY with ECHO off and
// detects the end of each line from a PS1 marker carrying a per-session
// nonce and $?. A PS2 marker means the line is incomplete: Ctrl-C is sent and
// the line fails with incomplete_line. Until the prompt returns the
// transport is not Ready, so a line never reaches a foreground program.
type aiShellTransport struct {
	client  *ssh.Client
	session *ssh.Session
	stdin   io.Writer
	out     io.Writer
	nonce   string

	// writeMu is a leaf: held only around stdin writes.
	writeMu sync.Mutex
	// feedMu serializes output processing (stdout and stderr share the
	// parser). Held while taking mu, writeMu and the output's locks, and
	// only ever by feed.
	feedMu sync.Mutex

	mu        sync.Mutex // leaf: parser and line state
	ready     bool
	readyOnce sync.Once
	readyCh   chan struct{}
	cur       *aiShellLine
	parse     aiMarkerParser
	doneOnce  sync.Once
	done      chan struct{}
	endReason string
	closeOnce sync.Once
}

type aiShellLine struct {
	done       chan struct{}
	once       sync.Once
	outcome    aiTransportOutcome
	incomplete bool
}

func (l *aiShellLine) Done() <-chan struct{}       { return l.done }
func (l *aiShellLine) Outcome() aiTransportOutcome { <-l.done; return l.outcome }

func (l *aiShellLine) finish(oc aiTransportOutcome) {
	l.once.Do(func() {
		l.outcome = oc
		close(l.done)
	})
}

// aiShellCommand is the fixed command started on the PTY (no agent input).
func aiShellCommand(shell string) string {
	if shell == aiShellBash {
		return "exec env -u BASH_ENV -u ENV -u PROMPT_COMMAND TERM=dumb bash --noprofile --norc --noediting -i"
	}
	return "exec env -u BASH_ENV -u ENV -u PROMPT_COMMAND TERM=dumb /bin/sh -i"
}

// aiShellInitLine sets the marker prompts. It is written once by
// writeControl and holds no agent input. The 0x1E bytes are literal (dash
// does not interpret backslash escapes in PS1).
func aiShellInitLine(nonce string) []byte {
	return []byte("PS1='\x1eSG:" + nonce + ":$?\x1e'; PS2='\x1eSG2:" + nonce + "\x1e'; unset PROMPT_COMMAND; trap - DEBUG 2>/dev/null\n")
}

// newShellTransport starts the marker shell on client, which it then owns.
// It returns once the first marker prompt arrived (or fails with
// errAINoPOSIXShell, closing the client).
func newShellTransport(ctx context.Context, client *ssh.Client, shell string, out io.Writer) (aiTransport, error) {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		_ = client.Close()
		return nil, err
	}
	t := &aiShellTransport{client: client, out: out, nonce: hex.EncodeToString(nb[:]),
		readyCh: make(chan struct{}), done: make(chan struct{})}
	t.parse = newAIMarkerParser(t.nonce)
	stopWatch := context.AfterFunc(ctx, func() { _ = client.Close() })
	fail := func(err error) (aiTransport, error) {
		stopWatch()
		t.Close()
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return nil, err
	}
	session, err := newSessionContext(ctx, client, aiShellReadyTimeout)
	if err != nil {
		return fail(err)
	}
	t.session = session
	modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := session.RequestPty("dumb", aiShellPtyRows, aiShellPtyCols, modes); err != nil {
		return fail(err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return fail(err)
	}
	t.stdin = stdin
	sink := aiShellSink{t}
	session.Stdout = sink
	session.Stderr = sink
	if err := session.Start(aiShellCommand(shell)); err != nil {
		return fail(err)
	}
	go t.waitSession()
	if err := t.writeControl(aiControlInit); err != nil {
		return fail(err)
	}
	timer := time.NewTimer(aiShellReadyTimeout)
	defer timer.Stop()
	select {
	case <-t.readyCh:
	case <-t.done:
		return fail(errAINoPOSIXShell)
	case <-timer.C:
		return fail(errAINoPOSIXShell)
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	if !stopWatch() {
		return fail(context.Canceled)
	}
	go t.keepalive()
	return t, nil
}

// aiShellSink is the PTY output writer (stdout and stderr share one PTY).
type aiShellSink struct{ t *aiShellTransport }

func (w aiShellSink) Write(p []byte) (int, error) {
	w.t.feed(p)
	return len(p), nil
}

// feed splits output into data and markers. Data before the first prompt
// (the shell's own banner and default prompt) is dropped.
func (t *aiShellTransport) feed(p []byte) {
	t.feedMu.Lock()
	defer t.feedMu.Unlock()
	t.mu.Lock()
	data, marks := t.parse.feed(p)
	started := t.ready || t.cur != nil
	t.mu.Unlock()
	// Data and markers are interleaved; process in order.
	for _, seg := range aiInterleave(data, marks) {
		if seg.mark == nil {
			if started && len(seg.data) > 0 {
				_, _ = t.out.Write(seg.data)
			}
			continue
		}
		started = true
		t.onMarker(*seg.mark)
	}
}

func (t *aiShellTransport) onMarker(mk aiMarker) {
	if mk.continuation {
		t.mu.Lock()
		if t.cur != nil {
			t.cur.incomplete = true
		}
		t.mu.Unlock()
		_ = t.writeControl(aiControlInterrupt)
		return
	}
	t.mu.Lock()
	cur := t.cur
	t.cur = nil
	t.ready = true
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.readyCh) })
	if cur != nil {
		code := mk.code
		oc := aiTransportOutcome{ExitCode: &code}
		if cur.incomplete {
			oc.Err = errAIIncompleteLine
		}
		cur.finish(oc)
	}
}

func (t *aiShellTransport) waitSession() {
	err := t.session.Wait()
	reason := aiCloseRemoteExit
	var exitErr *ssh.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		reason = aiCloseConnLost
	}
	t.end(reason)
}

func (t *aiShellTransport) end(reason string) {
	t.doneOnce.Do(func() {
		t.mu.Lock()
		if t.endReason == "" {
			t.endReason = reason
		}
		cur := t.cur
		t.cur = nil
		t.ready = false
		t.mu.Unlock()
		if cur != nil {
			cur.finish(aiTransportOutcome{Err: errAITransportClosed})
		}
		close(t.done)
	})
}

// keepalive sends keepalive@openssh.com; two misses in a row end the
// session with conn_lost (§5).
func (t *aiShellTransport) keepalive() {
	ticker := time.NewTicker(aiKeepaliveInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
		}
		res := make(chan error, 1)
		go func() {
			_, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil)
			res <- err
		}()
		timer := time.NewTimer(aiKeepaliveTimeout)
		var err error
		select {
		case err = <-res:
		case <-timer.C:
			err = errors.New("keepalive timeout")
		case <-t.done:
			timer.Stop()
			return
		}
		timer.Stop()
		if err == nil {
			misses = 0
			continue
		}
		if misses++; misses >= 2 {
			t.mu.Lock()
			if t.endReason == "" {
				t.endReason = aiCloseConnLost
			}
			t.mu.Unlock()
			_ = t.client.Close()
			return
		}
	}
}

type aiControl int

const (
	aiControlInit aiControl = iota
	aiControlInterrupt
)

// writeControl is the only non-line write to the PTY (§3.5): the init line
// at open and the single byte 0x03.
func (t *aiShellTransport) writeControl(c aiControl) error {
	var p []byte
	switch c {
	case aiControlInit:
		p = aiShellInitLine(t.nonce)
	case aiControlInterrupt:
		p = []byte{0x03}
	default:
		return errors.New("unknown control")
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err := t.stdin.Write(p)
	return err
}

// StartLine writes one line (plus newline) when the prompt is back.
func (t *aiShellTransport) StartLine(ctx context.Context, plan aiLinePlan) (aiLineHandle, error) {
	line := plan.Line
	if strings.ContainsAny(line, "\r\n\x03") {
		// validateSessionLine already refuses these; never let one through.
		return nil, errors.New("line contains a newline or control character")
	}
	t.mu.Lock()
	select {
	case <-t.done:
		t.mu.Unlock()
		return nil, errAITransportClosed
	default:
	}
	if !t.ready || t.cur != nil {
		t.mu.Unlock()
		return nil, errAITransportBusy
	}
	l := &aiShellLine{done: make(chan struct{})}
	t.cur = l
	t.ready = false
	t.mu.Unlock()
	t.writeMu.Lock()
	_, err := t.stdin.Write([]byte(line + "\n"))
	t.writeMu.Unlock()
	if err != nil {
		l.finish(aiTransportOutcome{Err: err})
	}
	return l, nil
}

func (t *aiShellTransport) Interrupt() error { return t.writeControl(aiControlInterrupt) }

func (t *aiShellTransport) Ready() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ready && t.cur == nil
}

// Cwd is unknown in shell mode (the shell holds it).
func (t *aiShellTransport) Cwd() string { return "" }

func (t *aiShellTransport) Close() error {
	t.closeOnce.Do(func() {
		if t.session != nil {
			_ = t.session.Close()
		}
		_ = t.client.Close()
		if t.session == nil {
			t.end(aiCloseConnLost)
		}
	})
	return nil
}

func (t *aiShellTransport) Done() <-chan struct{} { return t.done }

func (t *aiShellTransport) EndReason() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endReason
}

// aiMarker is one prompt marker found in the output.
type aiMarker struct {
	at           int // index into the data returned with it
	code         int
	continuation bool
}

type aiSegment struct {
	data []byte
	mark *aiMarker
}

// aiInterleave rebuilds the order of data and markers.
func aiInterleave(data []byte, marks []aiMarker) []aiSegment {
	var out []aiSegment
	prev := 0
	for i := range marks {
		if marks[i].at > prev {
			out = append(out, aiSegment{data: data[prev:marks[i].at]})
		}
		out = append(out, aiSegment{mark: &marks[i]})
		prev = marks[i].at
	}
	if prev < len(data) {
		out = append(out, aiSegment{data: data[prev:]})
	}
	return out
}

// aiMarkerParser finds "\x1eSG:<nonce>:<code>\x1e" and "\x1eSG2:<nonce>\x1e"
// in a byte stream, across chunk boundaries. Anything that turns out not to
// be a marker passes through as data.
type aiMarkerParser struct {
	ps1  []byte // "\x1eSG:<nonce>:"
	ps2  []byte // "\x1eSG2:<nonce>\x1e"
	pend []byte
}

func newAIMarkerParser(nonce string) aiMarkerParser {
	return aiMarkerParser{ps1: []byte("\x1eSG:" + nonce + ":"), ps2: []byte("\x1eSG2:" + nonce + "\x1e")}
}

const aiMarkerCodeMax = 4 // $? is 0-255; allow a little slack

// feed returns the data bytes and the markers (with their position in data).
func (m *aiMarkerParser) feed(p []byte) ([]byte, []aiMarker) {
	var data []byte
	var marks []aiMarker
	queue := p
	for len(queue) > 0 {
		b := queue[0]
		queue = queue[1:]
		if len(m.pend) == 0 {
			if b == 0x1e {
				m.pend = append(m.pend, b)
			} else {
				data = append(data, b)
			}
			continue
		}
		m.pend = append(m.pend, b)
		switch v := m.check(); {
		case v == aiMarkPartial:
		case v == aiMarkPS2:
			marks = append(marks, aiMarker{at: len(data), continuation: true})
			m.pend = m.pend[:0]
		case v >= 0:
			marks = append(marks, aiMarker{at: len(data), code: v})
			m.pend = m.pend[:0]
		default:
			// Not a marker: the first byte is data; rescan the rest.
			data = append(data, m.pend[0])
			rest := append([]byte(nil), m.pend[1:]...)
			m.pend = m.pend[:0]
			queue = append(rest, queue...)
		}
	}
	return data, marks
}

const (
	aiMarkPartial = -1
	aiMarkNone    = -2
	aiMarkPS2     = -3
)

// check classifies pend: partial prefix, a complete PS1 (returns the code),
// a complete PS2, or none.
func (m *aiMarkerParser) check() int {
	p := m.pend
	if len(p) <= len(m.ps2) && string(m.ps2[:len(p)]) == string(p) {
		if len(p) == len(m.ps2) {
			return aiMarkPS2
		}
		return aiMarkPartial
	}
	if len(p) <= len(m.ps1) {
		if string(m.ps1[:len(p)]) == string(p) {
			return aiMarkPartial
		}
		return aiMarkNone
	}
	if string(p[:len(m.ps1)]) != string(m.ps1) {
		return aiMarkNone
	}
	rest := p[len(m.ps1):]
	last := rest[len(rest)-1]
	if last == 0x1e {
		digits := rest[:len(rest)-1]
		if len(digits) == 0 {
			return aiMarkNone
		}
		code, err := strconv.Atoi(string(digits))
		if err != nil {
			return aiMarkNone
		}
		return code
	}
	if last < '0' || last > '9' || len(rest) > aiMarkerCodeMax {
		return aiMarkNone
	}
	return aiMarkPartial
}

// ---------------------------------------------------------------------------
// App wiring

// aiSessionManagers maps *App to its manager until the Integrator embeds the
// manager in App (replace aiMgr's body with a field read).
var aiSessionManagers sync.Map

// aiMgr returns the App's AI session manager, creating it on first use.
func (a *App) aiMgr() *aiSessionManager {
	if v, ok := aiSessionManagers.Load(a); ok {
		return v.(*aiSessionManager)
	}
	m := newAISessionManager(appAIHost{a}, aiSessionHooks{})
	m.transports[aiModeExec] = a.newAIExecTransport
	m.transports[aiModeShell] = a.newAIShellTransport
	v, _ := aiSessionManagers.LoadOrStore(a, m)
	return v.(*aiSessionManager)
}

// configureAISessions installs the persistence hooks and the stored
// settings. Call it once at startup, before any session opens (hooks are
// read without a lock afterwards).
func (a *App) configureAISessions(hooks aiSessionHooks, settings AISessionSettings) {
	m := a.aiMgr()
	m.hooks = hooks
	if clean, field := validateAISessionSettings(settings); field == "" {
		m.setSettings(clean)
	}
}

// forgetAISessions drops the App's manager (tests, after shutdown).
func (a *App) forgetAISessions() {
	aiSessionManagers.Delete(a)
	aiLineRegistries.Delete(a)
}

// appAIHost adapts App to aiSessionHost.
type appAIHost struct{ a *App }

func (h appAIHost) now() time.Time                { return h.a.clock() }
func (h appAIHost) emit(name string, payload any) { h.a.emitEvent(name, payload) }
func (h appAIHost) sessionsChanged()              { h.a.sessionsChanged() }
func (h appAIHost) agentHasToken(agent string) bool {
	return h.a.agentHasToken(agent)
}

func (h appAIHost) targetFP(connID string) (string, bool) {
	conn, ok := h.a.connectionByID(connID)
	if !ok {
		return "", false
	}
	return targetFingerprint(conn), true
}

// newAIShellTransport dials a dedicated client, then starts the marker
// shell on it.
func (a *App) newAIShellTransport(ctx context.Context, conn Connection, opt aiTransportOptions) (aiTransport, error) {
	client, err := a.dialAISessionClient(ctx, conn)
	if err != nil {
		return nil, err
	}
	return newShellTransport(ctx, client, opt.Shell, opt.Out)
}

// dialAISessionClient opens the session's own client with the MCP host-key
// policy and credential timeout (§2.2). The session owns and closes it.
func (a *App) dialAISessionClient(ctx context.Context, conn Connection) (*ssh.Client, error) {
	credCtx, credCancel := context.WithTimeout(ctx, mcpCredentialTimeout)
	conn, err := a.resolveCredential(credCtx, conn)
	credCancel()
	if err != nil {
		return nil, err
	}
	a.mu.RLock()
	strict := a.mcp.StrictHostKey
	if conn.KeyPassphrase == "" {
		conn.KeyPassphrase = a.sshPassphrases[conn.ID]
	}
	a.mu.RUnlock()
	dialCtx, dialCancel := context.WithTimeout(ctx, connectTimeout(conn))
	client, err := openSSHClientWithPolicy(dialCtx, conn, hostKeyPolicy{Origin: hostKeyFromMCP, Strict: strict})
	dialCancel()
	if err != nil {
		return nil, err
	}
	return client, nil
}

// openAISession opens a session for an MCP caller. Only a per-agent token
// owns sessions (§2.1); host is resolved like the other MCP tools.
func (a *App) openAISession(ctx context.Context, caller mcpCaller, host string, req aiOpenRequest) (*aiSession, bool, error) {
	if caller.Agent == "" {
		return nil, false, newAISessError(aiErrAgentTokenRequired, "エージェント専用トークンが必要です / an agent token is required", nil)
	}
	conn, candidates, ok := a.resolveConnection(host)
	if !ok {
		if len(candidates) > 1 {
			return nil, false, newAISessError(aiErrAmbiguousHost, "接続先が複数あります / host is ambiguous", map[string]any{"candidates": candidates})
		}
		return nil, false, newAISessError(aiErrHostNotFound, "接続先が見つかりません / host not found", nil)
	}
	req.Owner = caller.Agent
	req.Conn = conn
	return a.aiMgr().open(ctx, req)
}

// lookupAISession finds owner's session (MCP tools). TERM- ids never match.
func (a *App) lookupAISession(id, owner string) (*aiSession, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, newAISessError(aiErrAgentTokenRequired, "エージェント専用トークンが必要です / an agent token is required", nil)
	}
	return a.aiMgr().lookup(strings.TrimSpace(id), owner, false)
}

// listAISessionsFor is list_sessions: the caller's sessions only.
func (a *App) listAISessionsFor(owner string, includeClosed bool) []AISessionInfo {
	if strings.TrimSpace(owner) == "" {
		return []AISessionInfo{}
	}
	return a.aiMgr().list(owner, includeClosed)
}

// closeAISession closes one session; it reports whether this call closed it.
func (a *App) closeAISession(id, reason, by string) bool {
	return a.aiMgr().closeSession(id, reason, by)
}

// closeAllAISessions is the shutdown step between drainMCPCommands and
// CloseAllTerminals: no more opens, every session closed within grace.
func (a *App) closeAllAISessions(reason string, grace time.Duration) {
	a.aiMgr().shutdown(reason, grace)
}

// closeAISessionsForConnection closes the connection's sessions
// (DisconnectConnection: disconnected; DeleteConnection: connection_deleted).
func (a *App) closeAISessionsForConnection(connID, reason string) int {
	return a.aiMgr().closeWhere(func(s *aiSession) bool { return s.connID == connID }, reason, aiClosedBySystem, aiCloseGrace)
}

// runAISessionSweeper runs the sweeper until ctx ends (start it under a.wg).
func (a *App) runAISessionSweeper(ctx context.Context) { a.aiMgr().run(ctx) }

// aiSessionCounts is the sidebar summary for sessionsPayload.
func (a *App) aiSessionCounts() AISessionCounts { return a.aiMgr().Counts() }

// ListAISessions lists every session for the GUI, with recent tombstones.
func (a *App) ListAISessions() []AISessionInfo { return a.aiMgr().list("", true) }

// GetAISession returns one session with its recent lines (GUI).
func (a *App) GetAISession(id string) AISessionDetail {
	s, err := a.aiMgr().lookup(strings.TrimSpace(id), "", true)
	if s == nil {
		return AISessionDetail{OK: false, Code: aiSessErrorCode(err), Message: err.Error(), Lines: []AISessionLine{}}
	}
	return AISessionDetail{OK: true, Session: s.info(a.aiMgr().Settings()), Lines: s.recentLines()}
}

// ReadAISessionRaw returns raw output from since for the read-only viewer.
// Humans cannot write to a session: there is no input binding.
func (a *App) ReadAISessionRaw(id string, since uint64) AISessionRawRead {
	s, err := a.aiMgr().lookup(strings.TrimSpace(id), "", true)
	if s == nil {
		return AISessionRawRead{OK: false, Code: aiSessErrorCode(err)}
	}
	r, err := s.out.ReadRaw(since, 0)
	if err != nil {
		return AISessionRawRead{OK: false, Code: aiSessErrorCode(err), EndCursor: r.EndCursor, State: s.State()}
	}
	return AISessionRawRead{OK: true, Data: base64.StdEncoding.EncodeToString(r.Data), StartCursor: r.StartCursor,
		Cursor: r.Cursor, EndCursor: r.EndCursor, MissedBytes: r.MissedBytes, More: r.More, State: s.State()}
}

// CloseAISession force-closes a session (human_closed).
func (a *App) CloseAISession(id string) ActionResult {
	id = strings.TrimSpace(id)
	if a.aiMgr().closeSession(id, aiCloseHuman, aiClosedByGUI) {
		return ActionResult{OK: true, ID: id, Message: "AI セッションを終了しました / AI session closed"}
	}
	return ActionResult{OK: false, ID: id, Code: aiErrSessionNotFound, Message: "AI セッションが見つかりません / AI session not found"}
}

// CloseAllAISessions force-closes every session (human_closed).
func (a *App) CloseAllAISessions() ActionResult {
	n := a.aiMgr().closeWhere(func(*aiSession) bool { return true }, aiCloseHuman, aiClosedByGUI, aiCloseGrace)
	return ActionResult{OK: true, Message: fmt.Sprintf("AI セッション %d 件を終了しました / closed %d AI sessions", n, n)}
}

// GetAISessionSettings returns the settings.
func (a *App) GetAISessionSettings() AISessionSettings { return a.aiMgr().Settings() }

// SaveAISessionSettings validates, stores, then applies the settings.
// Turning the feature off closes every session (ai_session_disabled).
func (a *App) SaveAISessionSettings(settings AISessionSettings) ActionResult {
	m := a.aiMgr()
	clean, field := validateAISessionSettings(settings)
	if field != "" {
		return ActionResult{OK: false, Code: aiErrInvalidSettings, Field: field, Message: "設定値が範囲外です / a setting is out of range"}
	}
	if m.hooks.saveSettings != nil {
		if err := m.hooks.saveSettings(clean); err != nil {
			return ActionResult{OK: false, Code: aiErrStorage, Message: "設定を保存できません / could not save the settings: " + err.Error()}
		}
	}
	m.setSettings(clean)
	if !clean.Enabled {
		m.closeWhere(func(*aiSession) bool { return true }, aiCloseDisabled, aiClosedByGUI, aiCloseGrace)
	}
	a.sessionsChanged()
	return ActionResult{OK: true, Message: "AI セッションの設定を保存しました / AI session settings saved"}
}
