package main

// MCP tools for AI-only persistent SSH sessions (REQ-016, phase 7 design
// docs/spec/phase7-ai-session.md §2): open_session, send_line, read_output,
// interrupt, close_session, list_sessions.
//
// This file owns the MCP surface only: tool definitions (inputSchema /
// outputSchema / annotations), strict argument decoding and range checks, the
// caller gate (a per-agent token is required; the shared token and an
// unauthenticated loopback client are refused), the owner check, the error
// codes and the agent-facing JSON shape. The session lifecycle, line
// classification, approval and output live behind mcpAISessionBackend, which
// the session code implements on *App (see aiSessionBackend). Until it does,
// every tool answers ai_session_disabled.
//
// Locks: nothing here holds a lock while calling the backend. The only locks
// taken are mcpRuntime's (waiter slot, idempotency key), which are leaves.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	mcpAISMaxReasonRunes      = 500
	mcpAISDefaultSendWait     = 10
	mcpAISMaxSendWait         = 60
	mcpAISMaxTimeoutSeconds   = 3600
	mcpAISDefaultReadMaxBytes = 16384
	mcpAISMaxReadMaxBytes     = 65536
	mcpAISMaxReadWait         = 30
)

// isAISessionID reports whether id has the AI session ID shape.
func isAISessionID(id string) bool {
	return aiSessionIDPattern.MatchString(id)
}

// mcpAISessionStates are the session_state codes (§2.9).
var mcpAISessionStates = []string{"opening", "idle", "pending_approval", "busy", "closing", "closed"}

// mcpAISessionCloseReasons are the close_reason codes (§2.9).
var mcpAISessionCloseReasons = []string{
	"agent_closed", "human_closed", "idle_timeout", "max_lifetime", "disconnected", "connection_deleted",
	"target_changed", "token_revoked", "ai_session_disabled", "remote_exit", "conn_lost", "app_shutdown",
	"app_restart", "open_failed",
}

// mcpAISessionErrorCodes are the error_kind codes the session tools return
// as tool errors (isError). Anything else from the backend becomes internal.
var mcpAISessionErrorCodes = []string{
	"agent_token_required", "ai_session_disabled", "shutting_down",
	"host_not_found", "ambiguous_host", "serial_not_supported", "shell_mode_not_allowed", "session_limit", "connect_failed",
	"session_not_found", "session_closed", "not_owner", "session_busy", "line_rejected", "idempotency_conflict",
	"target_changed", "storage", "invalid_cursor", "server_busy", "interrupt_rate_limited", "invalid_argument",
	"mode_not_available", "internal",
}

// Argument types. They are shared with the session code (openAISession,
// sendSessionLine, ...); the MCP layer hands them over already validated and
// normalized (trimmed, defaults filled in).

type openSessionArgs struct {
	Host            string `json:"host"`
	Reason          string `json:"reason"`
	Mode            string `json:"mode"`  // exec (default) | shell
	Shell           string `json:"shell"` // sh (default) | bash
	ClientRequestID string `json:"client_request_id"`
}

type sendLineArgs struct {
	SessionID       string `json:"session_id"`
	Line            string `json:"line"`
	ClientRequestID string `json:"client_request_id"`
	Reason          string `json:"reason"`
	// WaitSeconds is nil when omitted; after validation it is always set
	// (default mcpAISDefaultSendWait).
	WaitSeconds *int `json:"wait_seconds"`
	// TimeoutSeconds 0 means the connection's CommandTimeout (else 30s).
	TimeoutSeconds int  `json:"timeout_seconds"`
	ForceQueue     bool `json:"force_queue"`
}

// waitDuration is how long send_line waits for an auto-executed line.
func (args sendLineArgs) waitDuration() time.Duration {
	if args.WaitSeconds == nil {
		return mcpAISDefaultSendWait * time.Second
	}
	return time.Duration(*args.WaitSeconds) * time.Second
}

type readOutputArgs struct {
	SessionID string `json:"session_id"`
	// SinceCursor nil means "from the start of the ring".
	SinceCursor *uint64 `json:"since_cursor"`
	// MaxBytes is 1-65536 after validation (default 16384).
	MaxBytes    int `json:"max_bytes"`
	WaitSeconds int `json:"wait_seconds"`
}

func (args readOutputArgs) waitDuration() time.Duration {
	return time.Duration(args.WaitSeconds) * time.Second
}

type sessionIDArgs struct {
	SessionID string `json:"session_id"`
}

type listSessionsArgs struct {
	IncludeClosed bool `json:"include_closed"`
}

// Views: what the backend fills in and the agent receives. Times are RFC3339
// strings (formatAISessionTime), cursors are byte offsets in the AI text
// stream.

type mcpAISessionLimits struct {
	PerAgent  int `json:"per_agent"`
	Total     int `json:"total"`
	RingBytes int `json:"ring_bytes"`
}

type mcpAISessionOpenView struct {
	SessionID    string             `json:"session_id"`
	Connection   string             `json:"connection"`
	Mode         string             `json:"mode"`
	Shell        string             `json:"shell"`
	State        string             `json:"state"`
	Cwd          string             `json:"cwd"`
	OpenedAt     string             `json:"opened_at"`
	IdleTimeoutS int                `json:"idle_timeout_s"`
	ExpiresAt    string             `json:"expires_at"`
	OutputCursor uint64             `json:"output_cursor"`
	Limits       mcpAISessionLimits `json:"limits"`
	Deduplicated bool               `json:"deduplicated,omitempty"`
}

type mcpAISessionSendView struct {
	RequestID        string   `json:"request_id"`
	SessionID        string   `json:"session_id"`
	Seq              int      `json:"seq"`
	State            string   `json:"state"`
	Risk             string   `json:"risk"`
	RiskReasons      []string `json:"risk_reasons"`
	RequiresApproval bool     `json:"requires_approval"`
	Effect           string   `json:"effect"`
	Cwd              string   `json:"cwd"`
	CwdPersisted     bool     `json:"cwd_persisted"`
	CursorBefore     uint64   `json:"cursor_before"`
	CursorAfter      *uint64  `json:"cursor_after,omitempty"`
	ExitCode         *int     `json:"exit_code,omitempty"`
	// LineErrorKind is the execution error of a finished line (timeout,
	// canceled, incomplete_line, ...). It is not the tool error code.
	LineErrorKind   string `json:"line_error_kind,omitempty"`
	Output          string `json:"output"`
	OutputTruncated bool   `json:"output_truncated"`
	SessionState    string `json:"session_state"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	Deduplicated    bool   `json:"deduplicated,omitempty"`
	Notice          string `json:"notice,omitempty"`
	NextAction      string `json:"next_action,omitempty"`
}

type mcpAISessionLineRef struct {
	RequestID string `json:"request_id"`
	Seq       int    `json:"seq,omitempty"`
	State     string `json:"state"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

type mcpAISessionReadView struct {
	Text         string               `json:"text"`
	StartCursor  uint64               `json:"start_cursor"`
	Cursor       uint64               `json:"cursor"`
	EndCursor    uint64               `json:"end_cursor"`
	MissedBytes  uint64               `json:"missed_bytes"`
	More         bool                 `json:"more"`
	SessionState string               `json:"session_state"`
	CloseReason  string               `json:"close_reason,omitempty"`
	Line         *mcpAISessionLineRef `json:"line,omitempty"`
	Cwd          string               `json:"cwd"`
	Redactions   int                  `json:"redactions"`
}

type mcpAISessionInterruptView struct {
	SessionID   string               `json:"session_id"`
	Interrupted bool                 `json:"interrupted"`
	Line        *mcpAISessionLineRef `json:"line,omitempty"`
	Hint        string               `json:"hint,omitempty"`
}

type mcpAISessionCloseView struct {
	SessionID     string `json:"session_id"`
	Closed        bool   `json:"closed"`
	CloseReason   string `json:"close_reason"`
	Lines         int    `json:"lines"`
	OutputBytes   uint64 `json:"output_bytes"`
	AlreadyClosed bool   `json:"already_closed,omitempty"`
}

type mcpAISessionListItem struct {
	// Owner is used to filter and never leaves through MCP.
	Owner            string `json:"-"`
	SessionID        string `json:"session_id"`
	Connection       string `json:"connection"`
	Mode             string `json:"mode"`
	State            string `json:"state"`
	Cwd              string `json:"cwd"`
	OpenedAt         string `json:"opened_at"`
	LastActivityAt   string `json:"last_activity_at"`
	IdleExpiresAt    string `json:"idle_expires_at,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	Lines            int    `json:"lines"`
	PendingRequestID string `json:"pending_request_id,omitempty"`
	CloseReason      string `json:"close_reason,omitempty"`
	Tainted          bool   `json:"tainted,omitempty"`
}

// mcpAISessionRef is what the owner check needs to know about a session.
// Closed sessions are returned while their tombstone lasts (5 minutes).
type mcpAISessionRef struct {
	ID          string
	Owner       string
	Closed      bool
	CloseReason string
}

// mcpAISessionBackend is the session side of the MCP tools. The session code
// implements it on *App (a thin adapter over openAISession, sendSessionLine,
// interruptAISession, closeAISession, ...). Every method receives arguments
// that already passed the MCP checks and an owner that is a token-proven
// agent name; methods still re-check ownership and state under their own
// locks (the MCP check is the first gate, not the only one).
//
// Errors: return newMCPAISessionErr(code, message, extra) for the codes in
// mcpAISessionErrorCodes. errShuttingDown becomes shutting_down; any other
// error becomes internal with a generic message (the error text is not shown
// to the agent, since dial errors carry the target address).
type mcpAISessionBackend interface {
	// aiSessionMCPEnabled reports AISessionSettings.Enabled.
	aiSessionMCPEnabled() bool
	// aiSessionMCPLookup finds an open session or a tombstone by ID.
	aiSessionMCPLookup(id string) (mcpAISessionRef, bool)
	aiSessionMCPOpen(ctx context.Context, owner string, caller mcpCaller, args openSessionArgs) (mcpAISessionOpenView, error)
	aiSessionMCPSendLine(ctx context.Context, owner string, caller mcpCaller, args sendLineArgs) (mcpAISessionSendView, error)
	aiSessionMCPReadOutput(ctx context.Context, owner string, args readOutputArgs) (mcpAISessionReadView, error)
	aiSessionMCPInterrupt(ctx context.Context, owner string, sessionID string) (mcpAISessionInterruptView, error)
	aiSessionMCPClose(ctx context.Context, owner string, sessionID string) (mcpAISessionCloseView, error)
	// aiSessionMCPList returns owner's sessions (tombstones only with
	// includeClosed). The MCP layer filters by Owner again.
	aiSessionMCPList(owner string, includeClosed bool) ([]mcpAISessionListItem, error)
}

// mcpAISessionErr is a tool-level failure from the backend.
type mcpAISessionErr struct {
	Code    string
	Message string
	Extra   map[string]any
}

func (e *mcpAISessionErr) Error() string { return e.Code + ": " + e.Message }

func newMCPAISessionErr(code, message string, extra map[string]any) *mcpAISessionErr {
	return &mcpAISessionErr{Code: code, Message: message, Extra: extra}
}

// formatAISessionTime is the time format of every *_at field.
func formatAISessionTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// aiSessionError is a session tool error: isError, the code in error_kind
// and the extra fields next to it. An unknown code is reported as internal
// so the documented list stays closed; extra cannot override error_kind or
// message.
func aiSessionError(code, message string, extra map[string]any) any {
	known := false
	for _, c := range mcpAISessionErrorCodes {
		if c == code {
			known = true
			break
		}
	}
	if !known {
		code, message = "internal", "internal error"
	}
	clean := map[string]any{}
	for k, v := range extra {
		if k == "error_kind" || k == "message" {
			continue
		}
		clean[k] = v
	}
	return toolError(code, message, clean)
}

// aiSessionBackend returns the session implementation on *App, or nil when
// this build does not have it yet (the tools then answer
// ai_session_disabled). The integrator should pin the assertion with
// `var _ mcpAISessionBackend = (*App)(nil)` next to the implementation.
func (a *App) aiSessionBackend() mcpAISessionBackend {
	if backend, ok := any(a).(mcpAISessionBackend); ok {
		return backend
	}
	return nil
}

// callAISessionTool handles the AI session tools. handled=false means the
// name is not one of them and callMCPTool should go on with its own switch.
func (a *App) callAISessionTool(ctx context.Context, caller mcpCaller, params toolCallParams) (any, *rpcError, bool) {
	return a.callAISessionToolWith(ctx, a.aiSessionBackend(), caller, params)
}

func isAISessionTool(name string) bool {
	switch name {
	case "open_session", "send_line", "read_output", "interrupt", "close_session", "list_sessions":
		return true
	}
	return false
}

func (a *App) callAISessionToolWith(ctx context.Context, backend mcpAISessionBackend, caller mcpCaller, params toolCallParams) (any, *rpcError, bool) {
	if !isAISessionTool(params.Name) {
		return nil, nil, false
	}
	raw := params.Arguments
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	// Arguments first: an unknown field (agent_name included) or a wrong
	// type is invalid params whatever the feature state.
	var (
		open   openSessionArgs
		send   sendLineArgs
		read   readOutputArgs
		byID   sessionIDArgs
		list   listSessionsArgs
		target any
	)
	switch params.Name {
	case "open_session":
		target = &open
	case "send_line":
		target = &send
	case "read_output":
		target = &read
	case "interrupt", "close_session":
		target = &byID
	case "list_sessions":
		target = &list
	}
	if err := decodeAISessionArgs(raw, target); err != nil {
		return nil, err, true
	}

	if backend == nil || !backend.aiSessionMCPEnabled() {
		return aiSessionError("ai_session_disabled", "AI sessions are disabled; a human must enable them in the ssh-gete GUI (AI sessions > settings)", nil), nil, true
	}
	owner := caller.Agent
	if strings.TrimSpace(owner) == "" || caller.Agent != strings.TrimSpace(caller.Agent) {
		return aiSessionError("agent_token_required", "AI sessions require the agent's own token (Authorization: Bearer <agent token>); the shared token or no token is not accepted", nil), nil, true
	}

	switch params.Name {
	case "open_session":
		return a.openSessionTool(ctx, backend, owner, caller, open)
	case "send_line":
		return a.sendLineTool(ctx, backend, owner, caller, send)
	case "read_output":
		return a.readOutputTool(ctx, backend, owner, read)
	case "interrupt":
		return a.interruptTool(ctx, backend, owner, byID)
	case "close_session":
		return a.closeSessionTool(ctx, backend, owner, byID)
	default:
		return a.listSessionsTool(backend, owner, list)
	}
}

// decodeAISessionArgs is decodeToolArgs (additionalProperties:false) for one
// argument struct.
func decodeAISessionArgs(raw json.RawMessage, into any) *rpcError {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return invalidParams("invalid arguments: " + err.Error())
	}
	if decoder.More() {
		return invalidParams("invalid arguments: trailing data")
	}
	return nil
}

// checkAISessionOwner is the owner gate shared by the tools that take a
// session_id. allowClosed lets read_output and close_session reach a
// tombstone; the others get session_closed with its close_reason.
func checkAISessionOwner(backend mcpAISessionBackend, owner, id string, allowClosed bool) (mcpAISessionRef, any) {
	notFound := aiSessionError("session_not_found", "no such AI session (it may have expired); open a new one with open_session", nil)
	if !isAISessionID(id) {
		return mcpAISessionRef{}, notFound
	}
	ref, ok := backend.aiSessionMCPLookup(id)
	if !ok || ref.ID != id {
		return mcpAISessionRef{}, notFound
	}
	if ref.Owner != owner {
		return mcpAISessionRef{}, aiSessionError("not_owner", "this AI session belongs to another agent", nil)
	}
	if ref.Closed && !allowClosed {
		return mcpAISessionRef{}, aiSessionError("session_closed", "the AI session is closed", map[string]any{"close_reason": ref.CloseReason})
	}
	return ref, nil
}

// aiSessionBackendError turns a backend error into the tool answer.
func aiSessionBackendError(err error) any {
	var toolErr *mcpAISessionErr
	switch {
	case errors.As(err, &toolErr):
		return aiSessionError(toolErr.Code, toolErr.Message, toolErr.Extra)
	case errors.Is(err, errShuttingDown):
		return aiSessionError("shutting_down", "ssh-gete is shutting down", nil)
	default:
		return aiSessionError("internal", "internal error", nil)
	}
}

func validReasonLength(reason string) bool {
	return utf8.RuneCountInString(reason) <= mcpAISMaxReasonRunes
}

func (a *App) openSessionTool(ctx context.Context, backend mcpAISessionBackend, owner string, caller mcpCaller, args openSessionArgs) (any, *rpcError, bool) {
	args.Host = strings.TrimSpace(args.Host)
	args.Reason = strings.TrimSpace(args.Reason)
	args.Mode = strings.TrimSpace(args.Mode)
	args.Shell = strings.TrimSpace(args.Shell)
	args.ClientRequestID = strings.TrimSpace(args.ClientRequestID)
	if args.Host == "" {
		return nil, invalidParams("host is required"), true
	}
	if args.Reason == "" || !validReasonLength(args.Reason) {
		return nil, invalidParams(fmt.Sprintf("reason is required (1-%d characters)", mcpAISMaxReasonRunes)), true
	}
	switch args.Mode {
	case "":
		args.Mode = "exec"
	case "exec", "shell":
	default:
		return nil, invalidParams("mode must be exec or shell"), true
	}
	switch args.Shell {
	case "":
		args.Shell = "sh"
	case "sh", "bash":
	default:
		return nil, invalidParams("shell must be sh or bash"), true
	}
	if len(args.ClientRequestID) > maxClientRequestIDLen {
		return nil, invalidParams(fmt.Sprintf("client_request_id must be at most %d bytes", maxClientRequestIDLen)), true
	}
	// Concurrent retries with one client_request_id open one session: the
	// key spans the backend's lookup and creation. The prefix keeps it apart
	// from execute_command's keys and from any send_line key.
	if args.ClientRequestID != "" {
		unlock := a.mcpRuntime.lockKey("ai-session-open\x00" + owner + "\x00" + args.ClientRequestID)
		defer unlock()
	}
	view, err := backend.aiSessionMCPOpen(ctx, owner, caller, args)
	if err != nil {
		return aiSessionBackendError(err), nil, true
	}
	out, convErr := aiSessionViewMap(view)
	if convErr != nil {
		return aiSessionError("internal", "internal error", nil), nil, true
	}
	return toolResult(out, false), nil, true
}

func (a *App) sendLineTool(ctx context.Context, backend mcpAISessionBackend, owner string, caller mcpCaller, args sendLineArgs) (any, *rpcError, bool) {
	args.SessionID = strings.TrimSpace(args.SessionID)
	args.ClientRequestID = strings.TrimSpace(args.ClientRequestID)
	args.Reason = strings.TrimSpace(args.Reason)
	// The line itself is not trimmed or otherwise touched: validateSessionLine
	// judges exactly what would be sent (an empty line is line_rejected).
	if args.SessionID == "" {
		return nil, invalidParams("session_id is required"), true
	}
	if args.ClientRequestID == "" {
		return nil, invalidParams("client_request_id is required for send_line (a resent line must not run twice in a stateful session)"), true
	}
	if len(args.ClientRequestID) > maxClientRequestIDLen {
		return nil, invalidParams(fmt.Sprintf("client_request_id must be at most %d bytes", maxClientRequestIDLen)), true
	}
	if !validReasonLength(args.Reason) {
		return nil, invalidParams(fmt.Sprintf("reason must be at most %d characters", mcpAISMaxReasonRunes)), true
	}
	if args.WaitSeconds == nil {
		wait := mcpAISDefaultSendWait
		args.WaitSeconds = &wait
	} else if *args.WaitSeconds < 0 || *args.WaitSeconds > mcpAISMaxSendWait {
		return nil, invalidParams(fmt.Sprintf("wait_seconds must be 0-%d", mcpAISMaxSendWait)), true
	}
	if args.TimeoutSeconds < 0 || args.TimeoutSeconds > mcpAISMaxTimeoutSeconds {
		return nil, invalidParams(fmt.Sprintf("timeout_seconds must be 1-%d", mcpAISMaxTimeoutSeconds)), true
	}
	if _, denied := checkAISessionOwner(backend, owner, args.SessionID, false); denied != nil {
		return denied, nil, true
	}
	// A listener stop ends the wait for an auto-executed line early; the
	// backend answers with the current state.
	waitCtx, cancel := withListenerStop(ctx)
	defer cancel()
	view, err := backend.aiSessionMCPSendLine(waitCtx, owner, caller, args)
	if err != nil {
		return aiSessionBackendError(err), nil, true
	}
	if view.RiskReasons == nil {
		view.RiskReasons = []string{}
	}
	if view.NextAction == "" {
		view.NextAction = sendLineNextAction(view)
	}
	out, convErr := aiSessionViewMap(view)
	if convErr != nil {
		return aiSessionError("internal", "internal error", nil), nil, true
	}
	// Same isError rule as execute_command: a queued line is not an error;
	// an auto-executed line that failed or was interrupted is.
	return toolResult(out, view.State == stateAutoFailed || view.State == stateInterrupted), nil, true
}

func sendLineNextAction(view mcpAISessionSendView) string {
	switch view.State {
	case statePending:
		return fmt.Sprintf("awaiting human approval: call get_request_status with request_id %s and wait_seconds; when it is final, call read_output with since_cursor=%d", view.RequestID, view.CursorBefore)
	case stateRunning, stateAutoRunning:
		return fmt.Sprintf("still running: call read_output with since_cursor=%d and wait_seconds, or get_request_status; interrupt stops it", view.CursorBefore)
	}
	if view.OutputTruncated {
		return fmt.Sprintf("output was truncated: call read_output with since_cursor=%d", view.CursorBefore)
	}
	return ""
}

func (a *App) readOutputTool(ctx context.Context, backend mcpAISessionBackend, owner string, args readOutputArgs) (any, *rpcError, bool) {
	args.SessionID = strings.TrimSpace(args.SessionID)
	if args.SessionID == "" {
		return nil, invalidParams("session_id is required"), true
	}
	if args.MaxBytes == 0 {
		args.MaxBytes = mcpAISDefaultReadMaxBytes
	} else if args.MaxBytes < 1 || args.MaxBytes > mcpAISMaxReadMaxBytes {
		return nil, invalidParams(fmt.Sprintf("max_bytes must be 1-%d", mcpAISMaxReadMaxBytes)), true
	}
	if args.WaitSeconds < 0 || args.WaitSeconds > mcpAISMaxReadWait {
		return nil, invalidParams(fmt.Sprintf("wait_seconds must be 0-%d", mcpAISMaxReadWait)), true
	}
	// A tombstone can still be read: the last output stays for 5 minutes.
	if _, denied := checkAISessionOwner(backend, owner, args.SessionID, true); denied != nil {
		return denied, nil, true
	}
	waitCtx, cancel := withListenerStop(ctx)
	defer cancel()
	if args.WaitSeconds > 0 {
		// Long-polls share get_request_status's slots.
		if !a.mcpRuntime.acquireWaiter() {
			return aiSessionError("server_busy", fmt.Sprintf("too many long-polls waiting (max %d); retry later or use wait_seconds=0", maxMCPWaiters),
				map[string]any{"retry_after_ms": busyRetryAfter.Milliseconds()}), nil, true
		}
		defer a.mcpRuntime.releaseWaiter()
	}
	view, err := backend.aiSessionMCPReadOutput(waitCtx, owner, args)
	if err != nil {
		return aiSessionBackendError(err), nil, true
	}
	out, convErr := aiSessionViewMap(view)
	if convErr != nil {
		return aiSessionError("internal", "internal error", nil), nil, true
	}
	return toolResult(out, false), nil, true
}

func (a *App) interruptTool(ctx context.Context, backend mcpAISessionBackend, owner string, args sessionIDArgs) (any, *rpcError, bool) {
	args.SessionID = strings.TrimSpace(args.SessionID)
	if args.SessionID == "" {
		return nil, invalidParams("session_id is required"), true
	}
	if _, denied := checkAISessionOwner(backend, owner, args.SessionID, false); denied != nil {
		return denied, nil, true
	}
	view, err := backend.aiSessionMCPInterrupt(ctx, owner, args.SessionID)
	if err != nil {
		return aiSessionBackendError(err), nil, true
	}
	out, convErr := aiSessionViewMap(view)
	if convErr != nil {
		return aiSessionError("internal", "internal error", nil), nil, true
	}
	return toolResult(out, false), nil, true
}

func (a *App) closeSessionTool(ctx context.Context, backend mcpAISessionBackend, owner string, args sessionIDArgs) (any, *rpcError, bool) {
	args.SessionID = strings.TrimSpace(args.SessionID)
	if args.SessionID == "" {
		return nil, invalidParams("session_id is required"), true
	}
	// Closing a closed session is not an error: it answers the reason it
	// was closed with (the backend fills lines / output_bytes).
	if _, denied := checkAISessionOwner(backend, owner, args.SessionID, true); denied != nil {
		return denied, nil, true
	}
	view, err := backend.aiSessionMCPClose(ctx, owner, args.SessionID)
	if err != nil {
		return aiSessionBackendError(err), nil, true
	}
	out, convErr := aiSessionViewMap(view)
	if convErr != nil {
		return aiSessionError("internal", "internal error", nil), nil, true
	}
	return toolResult(out, false), nil, true
}

func (a *App) listSessionsTool(backend mcpAISessionBackend, owner string, args listSessionsArgs) (any, *rpcError, bool) {
	items, err := backend.aiSessionMCPList(owner, args.IncludeClosed)
	if err != nil {
		return aiSessionBackendError(err), nil, true
	}
	// Defense in depth: only the caller's own AI sessions ever leave, never
	// another agent's and never a human tab (whose IDs are not AIS-).
	sessions := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if item.Owner != owner || !isAISessionID(item.SessionID) {
			continue
		}
		if !args.IncludeClosed && item.State == "closed" {
			continue
		}
		entry, convErr := aiSessionViewMap(item)
		if convErr != nil {
			return aiSessionError("internal", "internal error", nil), nil, true
		}
		sessions = append(sessions, entry)
	}
	return toolResult(map[string]any{"sessions": sessions}, false), nil, true
}

// aiSessionViewMap turns a view struct into the structuredContent object.
// json.Number keeps uint64 cursors exact past 2^53.
func aiSessionViewMap(view any) (map[string]any, error) {
	payload, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// aiSessionTools are the tools/list entries, appended after mcpTools().
func aiSessionTools() []map[string]any {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	boolean := func(description string) map[string]any {
		return map[string]any{"type": "boolean", "description": description}
	}
	integer := func(description string) map[string]any {
		return map[string]any{"type": "integer", "description": description}
	}
	cursor := func(description string) map[string]any {
		return map[string]any{"type": "integer", "minimum": 0, "description": description}
	}
	enum := func(values []string, description string) map[string]any {
		return map[string]any{"type": "string", "enum": values, "description": description}
	}
	object := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	sessionID := map[string]any{"type": "string", "pattern": aiSessionIDPattern.String(), "description": "AI セッション ID（AIS- で始まる。open_session の session_id）"}
	sessionState := enum(mcpAISessionStates, "セッションの状態")
	closeReason := enum(mcpAISessionCloseReasons, "セッションが閉じた理由")
	lineRef := object(map[string]any{
		"request_id": str("処理中または直近の行の request_id"),
		"seq":        integer("セッション内の行番号"),
		"state":      enum(requestStates, "行の状態"),
		"exit_code":  integer("終了コード（終わっていれば）"),
	})
	errorProps := map[string]any{
		"error_kind":         str("ツールエラーのコード: " + strings.Join(mcpAISessionErrorCodes, " / ")),
		"message":            str("エラーの説明"),
		"close_reason":       closeReason,
		"request_id":         str("session_busy: 処理中の行の request_id"),
		"state":              str("session_busy: 処理中の行の状態"),
		"reject_reason":      str("line_rejected: 拒否した理由（empty / too_long / invalid_utf8 / multiline / control_char / heredoc / line_continuation / unbalanced_quote）"),
		"scope":              enum([]string{"per_agent", "total"}, "session_limit: どの上限に達したか"),
		"candidates":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxCandidates, "description": "候補の接続先名"},
		"connect_error_kind": str("connect_failed: 接続エラーの種類（" + strings.Join(errorKinds, " / ") + " / no_posix_shell）"),
		"needs_passphrase":   boolean("connect_failed: 鍵のパスフレーズが必要（人が GUI で一度接続する）"),
		"retry_after_ms":     integer("server_busy: 再試行までの目安（ミリ秒）"),
	}
	withErrors := func(props map[string]any) map[string]any {
		merged := map[string]any{}
		for k, v := range errorProps {
			merged[k] = v
		}
		for k, v := range props {
			merged[k] = v
		}
		return object(merged)
	}
	sessionIDOnly := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"session_id": sessionID},
		"required":             []string{"session_id"},
		"additionalProperties": false,
	}
	const common = "AI 専用の継続 SSH セッション（人のターミナルタブとは別物）。エージェント用トークンでの認証が必要で、機能は人が GUI で有効にしておく必要があります（無効なら ai_session_disabled）。"

	return []map[string]any{
		{
			"name":        "open_session",
			"title":       "AI セッションを開く",
			"description": common + "接続先に AI 専用のセッションを開き、session_id を返します。開くだけでは何も実行しません。mode=exec（既定）は1行ごとに新しいチャネルで実行し、cd と環境変数はゲートウェイが次の行に持ち越します（alias・関数は持ち越しません）。mode=shell は人が接続先ごとに許可した場合だけ使えます。",
			"annotations": map[string]any{"title": "AI セッションを開く", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":              str("接続先名（list_hosts の name）"),
					"reason":            map[string]any{"type": "string", "minLength": 1, "maxLength": mcpAISMaxReasonRunes, "description": "セッションを開く理由（監査と GUI の一覧に表示されます）"},
					"mode":              enum([]string{"exec", "shell"}, "実行方式（既定 exec）"),
					"shell":             enum([]string{"sh", "bash"}, "使うシェル（既定 sh）"),
					"client_request_id": map[string]any{"type": "string", "maxLength": maxClientRequestIDLen, "description": "冪等キー。同じ値での再送は新しいセッションを開かず既存のものを返します"},
				},
				"required":             []string{"host", "reason"},
				"additionalProperties": false,
			},
			"outputSchema": withErrors(map[string]any{
				"session_id":     str("AI セッション ID"),
				"connection":     str("解決済みの接続先名"),
				"mode":           enum([]string{"exec", "shell"}, "実行方式"),
				"shell":          enum([]string{"sh", "bash"}, "シェル"),
				"state":          sessionState,
				"cwd":            str("作業ディレクトリ"),
				"opened_at":      str("開いた日時（RFC3339）"),
				"idle_timeout_s": integer("アイドルで閉じるまでの秒数"),
				"expires_at":     str("最大寿命で閉じる日時"),
				"output_cursor":  cursor("read_output の since_cursor に使える現在のカーソル"),
				"limits": object(map[string]any{
					"per_agent":  integer("1エージェントあたりの同時セッション数の上限"),
					"total":      integer("全体の同時セッション数の上限"),
					"ring_bytes": integer("出力を保持するバイト数"),
				}),
				"deduplicated": boolean("同じ client_request_id の既存セッションを返したか"),
			}),
		},
		{
			"name":        "send_line",
			"title":       "AI セッションに1行送る",
			"description": common + "セッションに1行だけ送ります。改行・制御文字・ヒアドキュメント・行継続・閉じていない引用は line_rejected で拒否されます。各行は承認の判定を通り、承認が要る行は state=pending で返ります（get_request_status で待つ）。未承認の行がシェルに届くことはありません。承認待ちか実行中の行があるあいだは session_busy です。client_request_id は必須です。",
			"annotations": map[string]any{"title": "AI セッションに1行送る", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id":        sessionID,
					"line":              map[string]any{"type": "string", "minLength": 1, "maxLength": 4096, "description": "送る1行（改行を含めない）"},
					"client_request_id": map[string]any{"type": "string", "minLength": 1, "maxLength": maxClientRequestIDLen, "description": "冪等キー（必須）。同じ値での再送は実行せず既存の request_id を返します（24時間）"},
					"reason":            map[string]any{"type": "string", "maxLength": mcpAISMaxReasonRunes, "description": "この行の理由（承認者に表示されます）"},
					"wait_seconds":      map[string]any{"type": "integer", "minimum": 0, "maximum": mcpAISMaxSendWait, "description": "自動実行された行が終わるまで待つ最大秒数（既定 10）。承認待ちのときは待たずに返します"},
					"timeout_seconds":   map[string]any{"type": "integer", "minimum": 1, "maximum": mcpAISMaxTimeoutSeconds, "description": "行の実行タイムアウト（省略時は接続先の設定）"},
					"force_queue":       boolean("承認省略が許可されていても承認キューに入れる"),
				},
				"required":             []string{"session_id", "line", "client_request_id"},
				"additionalProperties": false,
			},
			"outputSchema": withErrors(map[string]any{
				"request_id":        str("この行の request_id（get_request_status で使う）"),
				"session_id":        str("AI セッション ID"),
				"seq":               integer("セッション内の行番号"),
				"state":             enum(requestStates, "行の状態"),
				"risk":              enum([]string{"Low", "Medium", "High"}, "リスク"),
				"risk_reasons":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "分類の理由コード"},
				"requires_approval": boolean("人間の承認が必要か"),
				"effect":            enum([]string{"none", "cd", "env", "state_change"}, "この行がセッションの状態に与える効果"),
				"cwd":               str("この行の実行後の作業ディレクトリ"),
				"cwd_persisted":     boolean("cd の結果が次の行に持ち越されるか"),
				"cursor_before":     cursor("この行の出力の始まりのカーソル"),
				"cursor_after":      cursor("この行の出力の終わりのカーソル（終わっていれば）"),
				"exit_code":         integer("終了コード（終わっていれば）"),
				"line_error_kind":   str("行の実行エラーの種類（timeout / canceled / incomplete_line など）"),
				"output":            str("この行の出力テキスト（上限 16 KiB、秘密は伏せる）"),
				"output_truncated":  boolean("output が上限で切り詰められたか"),
				"session_state":     sessionState,
				"expires_at":        str("承認待ちの期限"),
				"deduplicated":      boolean("同じ client_request_id の既存の行を返したか"),
				"notice":            str("補足（cd が持ち越されないなど）"),
				"next_action":       str("次に呼ぶべき操作"),
			}),
		},
		{
			"name":        "read_output",
			"title":       "AI セッションの出力を読む",
			"description": common + "セッションの出力をテキスト（エスケープシーケンスを除き、秘密らしい文字列は伏せたもの）で返します。since_cursor に前回の cursor を渡すと続きを読めます。古い出力が捨てられていれば missed_bytes に量が入ります。wait_seconds を指定すると、新しい出力か状態の変化があるまで待ちます。閉じたセッションも 5 分間は読めます。",
			"annotations": map[string]any{"title": "AI セッションの出力を読む", "readOnlyHint": true, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id":   sessionID,
					"since_cursor": cursor("ここから読む（省略時は保持している出力の先頭から）"),
					"max_bytes":    map[string]any{"type": "integer", "minimum": 1, "maximum": mcpAISMaxReadMaxBytes, "description": "返す最大バイト数（既定 16384）"},
					"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": mcpAISMaxReadWait, "description": "新しい出力が来るまで待つ最大秒数（既定 0）"},
				},
				"required":             []string{"session_id"},
				"additionalProperties": false,
			},
			"outputSchema": withErrors(map[string]any{
				"text":          str("出力テキスト"),
				"start_cursor":  cursor("text の始まりのカーソル"),
				"cursor":        cursor("次の since_cursor"),
				"end_cursor":    cursor("いま保持している出力の終わり"),
				"missed_bytes":  cursor("捨てられて読めなかったバイト数"),
				"more":          boolean("まだ続きがあるか"),
				"session_state": sessionState,
				"close_reason":  closeReason,
				"line":          lineRef,
				"cwd":           str("作業ディレクトリ"),
				"redactions":    integer("伏せた秘密の件数"),
			}),
		},
		{
			"name":        "interrupt",
			"title":       "AI セッションの行を中断",
			"description": common + "実行中の行に Ctrl-C（SIGINT）を送ります。承認は要りません。承認待ちの行は取り消しません（close_session を使う）。1秒に1回までです。",
			"annotations": map[string]any{"title": "AI セッションの行を中断", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": true},
			"inputSchema": sessionIDOnly,
			"outputSchema": withErrors(map[string]any{
				"session_id":  str("AI セッション ID"),
				"interrupted": boolean("実行中の行に中断を送ったか"),
				"line":        lineRef,
				"hint":        str("補足（戻らないときは close_session など）"),
			}),
		},
		{
			"name":        "close_session",
			"title":       "AI セッションを閉じる",
			"description": common + "セッションを閉じます。承認待ちの行は取り下げ、実行中の行は中止します。閉じたセッションに対して呼ぶと、閉じた理由を返します。",
			"annotations": map[string]any{"title": "AI セッションを閉じる", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": sessionIDOnly,
			"outputSchema": withErrors(map[string]any{
				"session_id":     str("AI セッション ID"),
				"closed":         boolean("閉じているか（常に true）"),
				"close_reason":   closeReason,
				"lines":          integer("送った行の数"),
				"output_bytes":   cursor("出力の合計バイト数"),
				"already_closed": boolean("呼ぶ前にすでに閉じていたか"),
			}),
		},
		{
			"name":        "list_sessions",
			"title":       "AI セッション一覧",
			"description": common + "呼び出し元のエージェントが開いた AI セッションだけを返します。人のターミナルタブや他のエージェントのセッションは含みません（接続先の一覧は list_hosts）。",
			"annotations": map[string]any{"title": "AI セッション一覧", "readOnlyHint": true, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"include_closed": boolean("閉じてから 5 分以内のセッションも含める"),
				},
				"additionalProperties": false,
			},
			"outputSchema": withErrors(map[string]any{
				"sessions": map[string]any{"type": "array", "items": object(map[string]any{
					"session_id":         str("AI セッション ID"),
					"connection":         str("接続先名"),
					"mode":               enum([]string{"exec", "shell"}, "実行方式"),
					"state":              sessionState,
					"cwd":                str("作業ディレクトリ"),
					"opened_at":          str("開いた日時"),
					"last_activity_at":   str("最後に操作した日時"),
					"idle_expires_at":    str("アイドルで閉じる日時"),
					"expires_at":         str("最大寿命で閉じる日時"),
					"lines":              integer("送った行の数"),
					"pending_request_id": str("承認待ちの行の request_id"),
					"close_reason":       closeReason,
					"tainted":            boolean("シェルの状態が変更され、以後の行がすべて承認制か"),
				})},
			}),
		},
	}
}
