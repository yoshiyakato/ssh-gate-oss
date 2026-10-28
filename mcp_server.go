package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const latestProtocolVersion = "2025-06-18"

var supportedProtocolVersions = []string{"2025-06-18", "2025-03-26"}

const (
	maxClientRequestIDLen = 200
	maxCandidates         = 10
	// get_command_history limit (also used by Store.loadCommandHistory).
	defaultHistoryLimit   = 20
	maxHistoryLimit       = 500
	outputTruncatedMarker = "[ssh-gete: output truncated]"
)

// requestStates are the state codes agents may filter on (§5.3).
var requestStates = []string{
	statePending, stateRunning, stateSucceeded, stateFailed, stateRejected, stateExpired, stateWithdrawn,
	stateInterrupted, stateAutoRunning, stateAutoSucceeded, stateAutoFailed,
}

var errorKinds = []string{"connect", "auth", "credential_unavailable", "hostkey", "hostkey_unknown", "timeout", "exit_nonzero", "canceled"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func rpcErrorResponse(id json.RawMessage, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func invalidParams(message string) *rpcError {
	return &rpcError{Code: -32602, Message: message}
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type executeCommandArgs struct {
	Host      string `json:"host"`
	Command   string `json:"command"`
	Reason    string `json:"reason"`
	AgentName string `json:"agent_name"`
	// RequestedBy is the pre-v1 name of agent_name.
	RequestedBy     string `json:"requested_by"`
	ClientRequestID string `json:"client_request_id"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	ForceQueue      bool   `json:"force_queue"`
}

type listHostsArgs struct {
	ConnectedOnly bool   `json:"connected_only"`
	Tag           string `json:"tag"`
}

type requestStatusArgs struct {
	RequestIDs    []string `json:"request_ids"`
	WaitSeconds   int      `json:"wait_seconds"`
	IncludeOutput *bool    `json:"include_output"`
}

type historyArgs struct {
	Host      string `json:"host"`
	State     string `json:"state"`
	AgentName string `json:"agent_name"`
	Limit     int    `json:"limit"`
	AllAgents bool   `json:"all_agents"`
}

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request, settings MCPSettings, token string) {
	if r.Method != http.MethodPost {
		// Stateless Streamable HTTP: no server-initiated SSE stream.
		w.Header().Set("Allow", "POST, OPTIONS")
		writeJSON(w, http.StatusMethodNotAllowed, rpcErrorResponse(nil, -32600, "POST only"))
		return
	}
	caller, ok := a.authenticateMCP(r, settings, token)
	if !ok {
		writeUnauthorized(w)
		return
	}
	a.rememberMCPAgent(r)
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(supportedProtocolVersions, v) {
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(nil, -32600, "unsupported MCP-Protocol-Version: "+v))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(settings.MaxBodyKB)*1024)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, rpcErrorResponse(nil, -32600, "request too large"))
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(nil, -32600, "JSON-RPC batch is not supported"))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(nil, -32700, "invalid json"))
		return
	}
	if len(req.ID) == 0 || (req.Method == "" && isRPCResponse(body)) {
		// Notifications and client responses (id with result or error, no
		// method) get no JSON-RPC reply.
		// notifications/cancelled is not handled: a dropped HTTP request is
		// the cancellation signal.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32600, "invalid request"))
		return
	}

	// r.Context() ends when the client disconnects; tools treat that as
	// cancellation.
	result, rpcErr := a.dispatchMCP(r.Context(), caller, req)
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr})
}

// isRPCResponse reports a JSON-RPC response object: it carries result or
// error instead of method.
func isRPCResponse(body []byte) bool {
	var probe map[string]json.RawMessage
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	_, hasResult := probe["result"]
	_, hasError := probe["error"]
	return hasResult || hasError
}

func (a *App) handleAudit(w http.ResponseWriter, r *http.Request, settings MCPSettings, token string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, rpcErrorResponse(nil, -32600, "GET only"))
		return
	}
	caller, ok := a.authenticateMCP(r, settings, token)
	if !ok {
		writeUnauthorized(w)
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	a.mu.RLock()
	var requests []CommandRequest
	for _, req := range a.requests {
		if len(requests) >= limit {
			break
		}
		// An agent token sees only its own agent's requests, as in
		// get_request_status (N12).
		if caller.Agent != "" && req.RequestedBy != caller.Agent {
			continue
		}
		requests = append(requests, req)
	}
	agents := a.connectedAgentsLocked()
	a.mu.RUnlock()
	// The listener is reachable by agents: answer in the agent-facing shape
	// of get_request_status (requestView), which withholds the SSH login
	// user, the connection ID, the target address and the local error
	// detail (N13).
	views := make([]map[string]any, 0, len(requests))
	for _, req := range requests {
		views = append(views, requestView(req, true))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests":         views,
		"connected_agents": agents,
	})
}

// rememberMCPAgent tracks recently active clients for the status display.
// It runs after authentication and is bounded so it cannot grow without limit.
func (a *App) rememberMCPAgent(r *http.Request) {
	key := r.Header.Get("User-Agent")
	if len(key) > 200 {
		key = key[:200]
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		key = host + " " + key
	}
	now := time.Now()
	cutoff := now.Add(-5 * time.Minute)
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, seen := range a.mcpAgents {
		if seen.Before(cutoff) {
			delete(a.mcpAgents, k)
		}
	}
	if _, known := a.mcpAgents[key]; known || len(a.mcpAgents) < maxMCPAgents {
		a.mcpAgents[key] = now
	}
	a.mcp.ConnectedAgents = a.connectedAgentsLocked()
}

func (a *App) dispatchMCP(ctx context.Context, caller mcpCaller, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		protocolVersion := latestProtocolVersion
		var params initializeParams
		if err := json.Unmarshal(req.Params, &params); err == nil && slices.Contains(supportedProtocolVersions, params.ProtocolVersion) {
			protocolVersion = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    appName,
				"version": appVersion,
			},
			"instructions": "ssh-gete は人間の承認を挟む SSH ゲートウェイです。execute_command は原則として承認キューに入り、結果は get_request_status(request_ids, wait_seconds) で待って取得します。再送には同じ client_request_id を付けてください。cd や環境変数を保ったまま複数の行を順に送るときは、open_session / send_line / read_output / close_session（AI 専用セッション。GUI で有効にしたときだけ使えます）を使います。",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, invalidParams("invalid tool params")
		}
		if len(params.Arguments) == 0 || string(params.Arguments) == "null" {
			params.Arguments = json.RawMessage(`{}`)
		}
		return a.callMCPTool(ctx, caller, params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

// decodeToolArgs enforces additionalProperties:false: unknown fields and
// wrong types are JSON-RPC invalid params, not tool errors.
func decodeToolArgs(raw json.RawMessage, into any) *rpcError {
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

func (a *App) callMCPTool(ctx context.Context, caller mcpCaller, params toolCallParams) (any, *rpcError) {
	switch params.Name {
	case "list_hosts":
		var args listHostsArgs
		if err := decodeToolArgs(params.Arguments, &args); err != nil {
			return nil, err
		}
		return toolResult(map[string]any{"hosts": a.listHostsForAgent(args)}, false), nil
	case "get_command_history":
		var args historyArgs
		if err := decodeToolArgs(params.Arguments, &args); err != nil {
			return nil, err
		}
		return a.commandHistoryTool(caller, args)
	case "get_request_status":
		var args requestStatusArgs
		if err := decodeToolArgs(params.Arguments, &args); err != nil {
			return nil, err
		}
		return a.requestStatusTool(ctx, caller, args)
	case "execute_command", "request_command_execution":
		var args executeCommandArgs
		if err := decodeToolArgs(params.Arguments, &args); err != nil {
			return nil, err
		}
		return a.executeCommandTool(ctx, caller, args)
	default:
		if result, rpcErr, handled := a.callAISessionTool(ctx, caller, params); handled { // REQ-016
			return result, rpcErr
		}
		return nil, invalidParams("unknown tool: " + params.Name)
	}
}

// listHostsForAgent returns the connections with their live state. Serial
// rows carry only name/type/executable/in_use: no host, port, credential or
// fingerprint ever leaves through MCP.
func (a *App) listHostsForAgent(args listHostsArgs) []map[string]any {
	a.mu.RLock()
	conns := cloneConnections(a.conns)
	pending := map[string]int{}
	inflight := map[string]int{}
	inflightSince := map[string]string{}
	for _, req := range a.requests {
		if req.ConnectionID == "" {
			continue
		}
		switch req.Status {
		case statePending:
			pending[req.ConnectionID]++
		case stateRunning, stateAutoRunning:
			inflight[req.ConnectionID]++
			// RFC3339 in one zone sorts lexically.
			if since := inflightSince[req.ConnectionID]; since == "" || (req.DecidedAt != "" && req.DecidedAt < since) {
				inflightSince[req.ConnectionID] = req.DecidedAt
			}
		}
	}
	a.mu.RUnlock()
	// terminalRuntime takes termMu; a.mu is released first (§5.0). It is
	// the same per-connection view the sidebar's SessionSummary is built from.
	terminals := a.terminalRuntime()
	tag := strings.TrimSpace(args.Tag)

	hosts := make([]map[string]any, 0, len(conns))
	for _, conn := range conns {
		if tag != "" && !slices.ContainsFunc(conn.Tags, func(t string) bool { return strings.EqualFold(strings.TrimSpace(t), tag) }) {
			continue
		}
		if conn.connectionType() == "Serial" {
			inUse := terminals[conn.ID].Open > 0
			if args.ConnectedOnly && !inUse {
				continue
			}
			hosts = append(hosts, map[string]any{
				"name":       conn.Name,
				"type":       "serial",
				"executable": false,
				"in_use":     inUse,
			})
			continue
		}
		rt := terminals[conn.ID]
		connected := rt.Open > 0 || inflight[conn.ID] > 0
		if args.ConnectedOnly && !connected {
			continue
		}
		tags := conn.Tags
		if tags == nil {
			tags = []string{}
		}
		entry := map[string]any{
			"name":              conn.Name,
			"type":              "ssh",
			"executable":        true,
			"tags":              tags,
			"description":       conn.Description,
			"connected":         connected,
			"open_terminals":    rt.Open,
			"inflight":          inflight[conn.ID],
			"pending_approvals": pending[conn.ID],
		}
		// connected_since is the earliest open tab (SessionSummary's
		// ConnectedSince); without tabs, the earliest running command.
		switch {
		case !rt.Since.IsZero():
			entry["connected_since"] = rt.Since.Format(time.RFC3339)
		case inflightSince[conn.ID] != "":
			entry["connected_since"] = inflightSince[conn.ID]
		}
		hosts = append(hosts, entry)
	}
	return hosts
}

func (a *App) executeCommandTool(ctx context.Context, caller mcpCaller, args executeCommandArgs) (any, *rpcError) {
	args.Host = strings.TrimSpace(args.Host)
	args.Command = strings.TrimSpace(args.Command)
	args.ClientRequestID = strings.TrimSpace(args.ClientRequestID)
	if args.Host == "" || args.Command == "" {
		return nil, invalidParams("host and command are required")
	}
	if len(args.ClientRequestID) > maxClientRequestIDLen {
		return nil, invalidParams(fmt.Sprintf("client_request_id must be at most %d bytes", maxClientRequestIDLen))
	}
	if args.TimeoutSeconds < 0 || args.TimeoutSeconds > 3600 {
		return nil, invalidParams("timeout_seconds must be 1-3600")
	}
	agentName := commandAgentName(args)
	if caller.Agent != "" {
		if agentName == "" {
			agentName = caller.Agent
		} else if agentName != caller.Agent {
			return toolError("unauthorized_bypass", "agent_name does not match the agent token", nil), nil
		}
	}
	if agentName == "" {
		return nil, invalidParams("agent_name is required")
	}
	// A name bound to an agent token can only be used with that token (N14):
	// otherwise the shared token (or no token) could queue requests in its
	// name and take its client_request_id first, so the real agent's retry
	// would be answered with someone else's request (deduplicated).
	if caller.Agent == "" && a.agentHasToken(agentName) {
		return toolError("unauthorized_bypass", "this agent_name has its own agent token; authenticate with that token", nil), nil
	}

	// The key lock spans lookup and creation so concurrent retries with the
	// same client_request_id create one request.
	unlock := func() {}
	if args.ClientRequestID != "" {
		unlock = a.mcpRuntime.lockKey(agentName + "\x00" + args.ClientRequestID)
	}
	defer unlock()
	if existing, ok := a.existingClientRequest(agentName, args.ClientRequestID); ok {
		// AI session lines share the client_request_id namespace, but only
		// their owner's token may read them (REQ-016 §2.1, mayReadRequest).
		// execute_command never answers with one, not even to the owner: no
		// request id, no output, no session id.
		if a.isAISessionLine(existing) {
			return toolError("idempotency_conflict", "client_request_id was already used for an AI session line; use a different value", nil), nil
		}
		out := executeView(existing)
		out["deduplicated"] = true
		// Same isError rule as the original answer (runBypassedCommand).
		return toolResult(out, executeFailed(existing)), nil
	}

	conn, matches, ok := a.resolveConnection(args.Host)
	if !ok {
		if len(matches) > 1 {
			return toolError("ambiguous_host", "host matches several connections; use the connection name",
				map[string]any{"candidates": matches[:min(len(matches), maxCandidates)]}), nil
		}
		return toolError("host_not_found", "host not found; use a name from list_hosts",
			map[string]any{"candidates": a.hostCandidates(args.Host, maxCandidates)}), nil
	}
	if conn.connectionType() == "Serial" {
		return toolError("serial_not_supported", "serial connections are interactive-only; not executable via MCP", nil), nil
	}

	risk := classifyRisk(args.Command)
	if a.needsHumanApproval(conn, risk, args.Command, caller, agentName, args.ForceQueue) {
		req, err := a.queueCommandWithTimeout(conn, args.Command, args.Reason, agentName, risk, args.ClientRequestID, args.TimeoutSeconds)
		if err != nil {
			return toolError("storage", err.Error(), nil), nil
		}
		out := executeView(req)
		// Approval bypass needs the agent's own token; with the shared token
		// (or none) a grant for this name is ignored.
		if !args.ForceQueue && !caller.mayBypassAs(agentName) && a.agentApprovalBypassEnabled(agentName) {
			out["notice"] = "approval bypass is granted to this agent name but only works with its agent token; queued for human approval"
		}
		return toolResult(out, false), nil
	}
	return a.runBypassedCommand(ctx, conn, args, agentName, risk, unlock), nil
}

// runBypassedCommand executes an approval-bypassed command. The request is
// persisted as auto_running first (a crash still leaves an audit record), and
// the agent waits at most RequestTimeout: after that it gets state=running and
// the command keeps going, detached from the HTTP request. Until then an HTTP
// disconnect cancels the command (errClientGone, recorded as interrupted).
// The run is registered with trackExec like a GUI-approved one, so
// DisconnectConnection cancels it (failed, canceled) and shutdown interrupts
// it and waits for its outcome.
func (a *App) runBypassedCommand(ctx context.Context, conn Connection, args executeCommandArgs, agentName string, risk string, unlockKey func()) any {
	if !a.mcpRuntime.acquireExec() {
		return busyResult()
	}
	// The run belongs to the app, not to the HTTP request, so that it can
	// outlive the request after RequestTimeout; clientGone is how an HTTP
	// disconnect cancels it while the agent still waits.
	runCtx, clientGone := context.WithCancelCause(a.appCtx)
	// Reserved before the auto_running record so a disconnect between the
	// record and the SSH start cancels it (N2); counted as running only once
	// the record exists (merge audit #2).
	reservation, ok := a.reserveExec(runCtx, conn.ID)
	execCtx := reservation.ctx
	release := func() {
		reservation.release()
		clientGone(nil)
		a.mcpRuntime.releaseExec()
	}
	if !ok {
		release()
		return busyResult()
	}
	req := newCommandRequest(conn, args.Command, args.Reason, agentName, risk, stateAutoRunning)
	req.ExecutedCommand = args.Command
	req.ApprovedRisk = risk
	req.DecidedBy = "bypass:" + agentName
	req.DecidedAt = req.RequestedAt
	req.ClientRequestID = args.ClientRequestID
	if err := a.recordCommandRequest(req); err != nil {
		release()
		return toolError("storage", err.Error(), nil)
	}
	reservation.activate()
	// Recorded: a retry now finds it by client_request_id.
	unlockKey()

	// While the agent waits synchronously, an HTTP disconnect cancels the run.
	// The listener being stopped or restarted by ssh-gete also ends ctx (via
	// http.Server.Close), but is not a disconnect: the run is detached and
	// left to shutdown / drainMCPCommands.
	listenerStop := listenerStopping(ctx)
	stopWatch := context.AfterFunc(ctx, func() {
		if !isClosed(listenerStop) {
			clientGone(errClientGone)
		}
	})
	done := make(chan finishedRun, 1)
	go func() {
		// Released after finishRequest, so the drain sees the stored outcome.
		defer release()
		var result commandResult
		var execErr error
		if cause := stopCauseOf(execCtx); cause != nil {
			// Disconnected (or shut down, or the client left) before the
			// SSH start: record it without running the command.
			execErr = cause
		} else {
			result, execErr = a.executeSSH(execCtx, conn, args.Command, args.TimeoutSeconds, mcpCredentialTimeout)
		}
		state := stateAutoSucceeded
		switch {
		case execErr == nil:
		case errors.Is(execErr, errClientGone):
			state = stateInterrupted
		default:
			// errShuttingDown is turned into interrupted by finishRequest;
			// errDisconnected stays auto_failed with error_kind canceled.
			state = stateAutoFailed
		}
		final, err := a.finishRequest(req, state, result, execErr)
		if final.ID == "" {
			final = req
			final.Status = state
		}
		done <- finishedRun{req: final, err: err}
	}()

	timer := time.NewTimer(a.syncResponseLimit())
	defer timer.Stop()
	select {
	case run := <-done:
		stopWatch()
		var notSaved *errFinalStateNotSaved
		if errors.As(run.err, &notSaved) {
			// The command ran but SQLite refused its outcome (audit #37).
			return toolError("storage", "the command ran but its result could not be saved: "+notSaved.err.Error(),
				map[string]any{"request_id": run.req.ID, "state": run.req.Status})
		}
		return toolResult(executeView(run.req), executeFailed(run.req))
	case <-timer.C:
		// Detach: from here on only shutdown cancels the command.
		stopWatch()
	case <-listenerStop:
		// Answer state=running before the listener closes; the run goes on.
		stopWatch()
	case <-ctx.Done():
	}
	current := req
	if latest, ok := a.findRequest(req.ID); ok {
		current = latest
	}
	out := executeView(current)
	if current.Status == stateAutoRunning {
		// §5.3 and the tool description: a detached run answers
		// state=running; get_request_status then reports auto_running until
		// it ends (N27).
		out["state"] = stateRunning
	}
	return toolResult(out, false)
}

// finishedRun is a bypassed run's recorded outcome and finishRequest's error.
type finishedRun struct {
	req CommandRequest
	err error
}

// syncResponseLimit is how long execute_command waits for a bypassed command
// before answering state=running.
func (a *App) syncResponseLimit() time.Duration {
	a.mu.RLock()
	seconds := a.mcp.RequestTimeout
	a.mu.RUnlock()
	return timeoutSeconds(seconds, 30)
}

func busyResult() any {
	return toolError("server_busy", fmt.Sprintf("too many commands running (max %d); retry later or use force_queue", maxMCPExec),
		map[string]any{"retry_after_ms": busyRetryAfter.Milliseconds()})
}

func (a *App) requestStatusTool(ctx context.Context, caller mcpCaller, args requestStatusArgs) (any, *rpcError) {
	if len(args.RequestIDs) == 0 || len(args.RequestIDs) > 20 {
		return nil, invalidParams("request_ids must contain 1-20 ids")
	}
	if args.WaitSeconds < 0 || args.WaitSeconds > 60 {
		return nil, invalidParams("wait_seconds must be 0-60")
	}
	includeOutput := args.IncludeOutput == nil || *args.IncludeOutput
	wait := time.Duration(args.WaitSeconds) * time.Second
	// An agent token may only read its own agent's requests (N12): another
	// agent's request answers not_found, exactly as an unknown ID would (so
	// it also ends a long-poll at once). The shared token and an
	// unauthenticated loopback client keep seeing every request.
	ids := args.RequestIDs
	foreign := map[string]bool{}
	if caller.Agent != "" {
		ids = nil
		for _, id := range args.RequestIDs {
			if owner, ok := a.requestOwner(id); ok && owner != caller.Agent {
				foreign[id] = true
				continue
			}
			ids = append(ids, id)
		}
		if len(foreign) > 0 {
			wait = 0
		}
	}
	// A listener stop answers a long-poll with the current state instead of
	// cutting it off without a response.
	ctx, cancel := withListenerStop(ctx)
	defer cancel()
	if wait > 0 {
		if !a.mcpRuntime.acquireWaiter() {
			return busyResult(), nil
		}
		defer a.mcpRuntime.releaseWaiter()
	}
	var views []map[string]any
	if len(ids) > 0 {
		views = a.requestStatus(ctx, caller, ids, wait, includeOutput)
	}
	if len(foreign) == 0 {
		return toolResult(map[string]any{"requests": views}, false), nil
	}
	// Put the not_found entries back in the order the agent asked.
	out := make([]map[string]any, 0, len(args.RequestIDs))
	next := 0
	for _, id := range args.RequestIDs {
		if foreign[id] {
			out = append(out, map[string]any{"request_id": id, "error_kind": "not_found", "message": "request not found"})
			continue
		}
		out = append(out, views[next])
		next++
	}
	return toolResult(map[string]any{"requests": out}, false), nil
}

// requestOwner returns the agent that made request id (memory first, then
// SQLite); ok=false when the request is unknown.
func (a *App) requestOwner(id string) (string, bool) {
	if req, ok := a.findRequest(id); ok {
		return req.RequestedBy, true
	}
	if a.store == nil {
		return "", false
	}
	req, ok, err := a.store.loadCommandRequest(id)
	if err != nil || !ok {
		return "", false
	}
	return req.RequestedBy, true
}

// requestView is the agent-facing shape of a request (get_request_status and,
// without output, get_command_history).
func requestView(req CommandRequest, includeOutput bool) map[string]any {
	out := map[string]any{
		"request_id":        req.ID,
		"connection":        req.Host,
		"state":             req.Status,
		"risk":              req.Risk,
		"requested_command": req.Command,
		"executed_command":  req.ExecutedCommand,
		"modified_by_human": req.ExecutedCommand != "" && req.ExecutedCommand != req.Command,
		"requested_by":      req.RequestedBy,
		"requested_at":      req.RequestedAt,
		"timed_out":         req.ErrorKind == "timeout",
		"stdout_truncated":  strings.Contains(req.Stdout, outputTruncatedMarker),
		"stderr_truncated":  strings.Contains(req.Stderr, outputTruncatedMarker),
	}
	if req.ClientRequestID != "" {
		out["client_request_id"] = req.ClientRequestID
	}
	if req.SessionID != "" { // a line of an AI session (REQ-016)
		out["session_id"] = req.SessionID
		out["session_seq"] = req.SessionSeq
	}
	if req.DecidedBy != "" {
		out["decided_by"] = req.DecidedBy
		out["decided_at"] = req.DecidedAt
	}
	if req.ExpiresAt != "" {
		out["expires_at"] = req.ExpiresAt
	}
	if req.ExitCode != nil {
		out["exit_code"] = *req.ExitCode
	}
	if req.ErrorKind != "" {
		out["error_kind"] = req.ErrorKind
	}
	if req.FinishedAt != "" {
		out["finished_at"] = req.FinishedAt
		if d, err := time.ParseDuration(req.Duration); err == nil {
			out["duration_ms"] = d.Milliseconds()
		}
	}
	if includeOutput && requestIsFinal(req.Status) {
		out["stdout"] = req.Stdout
		out["stderr"] = agentStderr(req.Stderr, req.ErrorKind)
	}
	return out
}

// executeView is execute_command's answer: the request plus how it was
// handled and what the agent should do next.
func executeView(req CommandRequest) map[string]any {
	out := requestView(req, true)
	autoExecuted := strings.HasPrefix(req.DecidedBy, "bypass:")
	out["auto_executed"] = autoExecuted
	out["requires_approval"] = !autoExecuted
	switch {
	case !requestIsFinal(req.Status):
		out["next_action"] = "call get_request_status with this request_id and wait_seconds to receive the result"
	case req.ErrorKind == "hostkey_unknown":
		out["next_action"] = "the host key is not trusted yet; ask the human to connect once from the ssh-gete GUI and verify the fingerprint"
	}
	return out
}

// executeFailed is execute_command's isError rule: an approval-bypassed run
// that failed or was interrupted. Queued requests report their outcome
// through get_request_status, so their execute_command answer is not an error.
func executeFailed(req CommandRequest) bool {
	return req.Status == stateAutoFailed || (req.Status == stateInterrupted && strings.HasPrefix(req.DecidedBy, "bypass:"))
}

func requestIsFinal(state string) bool {
	switch state {
	case statePending, stateRunning, stateAutoRunning:
		return false
	}
	return true
}

// commandHistoryTool returns summaries (no stdout/stderr), newest first. It
// is scoped to the calling agent: the agent token's name, else agent_name.
// all_agents lifts the scope, only for Bearer-authenticated callers.
func (a *App) commandHistoryTool(caller mcpCaller, args historyArgs) (any, *rpcError) {
	if args.Limit < 0 {
		return nil, invalidParams("limit must be 1-500")
	}
	limit := args.Limit
	if limit == 0 {
		limit = defaultHistoryLimit
	}
	limit = min(limit, maxHistoryLimit)
	args.State = strings.TrimSpace(args.State)
	if args.State != "" && !slices.Contains(requestStates, args.State) {
		return nil, invalidParams("unknown state: " + args.State)
	}
	agent := strings.TrimSpace(args.AgentName)
	if caller.Agent != "" {
		if agent != "" && agent != caller.Agent {
			return toolError("unauthorized_bypass", "agent_name does not match the agent token", nil), nil
		}
		agent = caller.Agent
	}
	if args.AllAgents {
		if !a.bearerAuthenticated(caller) {
			return toolError("unauthorized_bypass", "all_agents requires Bearer authentication", nil), nil
		}
		agent = ""
	} else if agent == "" {
		return nil, invalidParams("agent_name is required (or all_agents with Bearer authentication)")
	}

	// AI session lines of other agents stay hidden even with all_agents
	// (REQ-016 isolation: only the owner's token sees them).
	filter := historyFilter{HostName: strings.TrimSpace(args.Host), State: args.State, RequestedBy: agent, Limit: limit,
		HideSessionLines: true, SessionViewer: caller.Agent}
	if filter.HostName != "" {
		if conn, _, ok := a.resolveConnection(filter.HostName); ok {
			filter.ConnectionID = conn.ID
		}
	}
	requests, err := a.commandHistory(filter)
	if err != nil {
		return toolError("storage", err.Error(), nil), nil
	}
	out := make([]map[string]any, 0, len(requests))
	for _, req := range requests {
		out = append(out, requestView(req, false))
	}
	scope := agent
	if scope == "" {
		scope = "*"
	}
	return toolResult(map[string]any{"requests": out, "agent": scope}, false), nil
}

// bearerAuthenticated reports whether Bearer auth is on and the caller
// presented a valid token (shared or per-agent).
func (a *App) bearerAuthenticated(caller mcpCaller) bool {
	a.mu.RLock()
	enabled := a.mcp.BearerEnabled
	a.mu.RUnlock()
	return enabled && (caller.Shared || caller.Agent != "")
}

// hostCandidates suggests SSH connection names for an unknown host: names
// containing ref first, then the rest, at most limit. Only names are matched:
// ranking by conn.Host would let an agent recover hidden addresses prefix by
// prefix. Exact host matching stays in resolveConnection.
func (a *App) hostCandidates(ref string, limit int) []string {
	lower := strings.ToLower(strings.TrimSpace(ref))
	a.mu.RLock()
	defer a.mu.RUnlock()
	var near, rest []string
	for _, conn := range a.conns {
		if conn.connectionType() != "SSH" {
			continue
		}
		if lower != "" && strings.Contains(strings.ToLower(conn.Name), lower) {
			near = append(near, conn.Name)
		} else {
			rest = append(rest, conn.Name)
		}
	}
	names := append(near, rest...)
	if names == nil {
		names = []string{}
	}
	return names[:min(len(names), limit)]
}

func commandAgentName(args executeCommandArgs) string {
	if agentName := strings.TrimSpace(args.AgentName); agentName != "" {
		return agentName
	}
	return strings.TrimSpace(args.RequestedBy)
}

// toolError is a tool-level failure: isError with the code in both the text
// block and structuredContent.
func toolError(code string, message string, extra map[string]any) any {
	value := map[string]any{"error_kind": code, "message": message}
	for k, v := range extra {
		value[k] = v
	}
	return toolResult(value, true)
}

func mcpTools() []map[string]any {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	boolean := func(description string) map[string]any {
		return map[string]any{"type": "boolean", "description": description}
	}
	strArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	errorProps := map[string]any{
		"error_kind":     str("ツールエラーのコード（host_not_found / ambiguous_host / serial_not_supported / server_busy / unauthorized_bypass）または実行エラーの種類"),
		"message":        str("エラーの説明"),
		"candidates":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxCandidates, "description": "候補の接続先名"},
		"retry_after_ms": map[string]any{"type": "integer", "description": "server_busy のとき、再試行までの目安（ミリ秒）"},
	}
	requestProps := map[string]any{
		"request_id":        str("要求 ID"),
		"connection":        str("解決済みの接続先名"),
		"state":             map[string]any{"type": "string", "enum": requestStates},
		"risk":              map[string]any{"type": "string", "enum": []string{"Low", "Medium", "High"}},
		"requested_command": str("要求されたコマンド"),
		"executed_command":  str("実際に実行した（する）コマンド。承認時に人間が修正した場合は異なる"),
		"modified_by_human": boolean("承認時に人間がコマンドを修正したか"),
		"requested_by":      str("要求元エージェント名"),
		"requested_at":      str("要求日時（RFC3339）"),
		"client_request_id": str("冪等キー"),
		"session_id":        str("AI セッションの行なら、その session_id（AIS-…）"),
		"session_seq":       map[string]any{"type": "integer", "description": "AI セッション内の何行目か"},
		"decided_by":        str("承認・拒否した主体（gui / bypass:<agent>）"),
		"decided_at":        str("承認・拒否の日時"),
		"expires_at":        str("承認待ちの期限"),
		"exit_code":         map[string]any{"type": "integer"},
		"error_kind":        map[string]any{"type": "string", "description": "実行エラーの種類: " + strings.Join(errorKinds, " / ")},
		"timed_out":         boolean("実行がタイムアウトしたか"),
		"duration_ms":       map[string]any{"type": "integer"},
		"finished_at":       str("終了日時"),
		"stdout":            str("標準出力（終了後のみ）"),
		"stderr":            str("標準エラー（終了後のみ）"),
		"stdout_truncated":  boolean("stdout が上限で切り詰められたか"),
		"stderr_truncated":  boolean("stderr が上限で切り詰められたか"),
		"message":           str("request_id が見つからないときの説明"),
	}
	executeProps := map[string]any{
		"requires_approval": boolean("人間の承認が必要か"),
		"auto_executed":     boolean("承認省略で即時実行されたか"),
		"deduplicated":      boolean("同じ client_request_id の既存要求を返したか"),
		"next_action":       str("次に呼ぶべき操作"),
		"notice":            str("補足（承認省略が無効化された理由など）"),
	}
	for k, v := range requestProps {
		executeProps[k] = v
	}
	for k, v := range errorProps {
		executeProps[k] = v
	}
	object := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	withErrors := func(props map[string]any) map[string]any {
		merged := map[string]any{}
		for k, v := range props {
			merged[k] = v
		}
		for k, v := range errorProps {
			merged[k] = v
		}
		return object(merged)
	}

	executeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"host":              str("接続先名（list_hosts の name）"),
			"command":           str("実行するコマンド"),
			"reason":            str("実行理由（承認者に表示されます）"),
			"agent_name":        str("要求元エージェント名。例: claude-desktop, codex-local（エージェント用トークンで認証した場合は省略可、指定するならトークンの名前と一致させる）"),
			"requested_by":      str("非推奨: agent_name の旧名"),
			"client_request_id": map[string]any{"type": "string", "maxLength": maxClientRequestIDLen, "description": "冪等キー。同じ agent_name と値での再送は新規実行せず既存の request_id を返します（24時間）"},
			"force_queue":       boolean("承認省略が許可されていても承認キューに入れる"),
			"timeout_seconds":   map[string]any{"type": "integer", "minimum": 1, "maximum": 3600, "description": "実行タイムアウト（省略時は接続先の設定）"},
		},
		"required":             []string{"host", "command"},
		"additionalProperties": false,
	}
	hostProps := map[string]any{
		"name":              str("接続先名"),
		"type":              map[string]any{"type": "string", "enum": []string{"ssh", "serial"}},
		"executable":        boolean("MCP から実行できるか（serial は常に false）"),
		"tags":              strArray,
		"description":       str("接続先の説明"),
		"connected":         boolean("ターミナルか実行中のコマンドで接続しているか"),
		"connected_since":   str("接続開始の日時"),
		"open_terminals":    map[string]any{"type": "integer"},
		"inflight":          map[string]any{"type": "integer"},
		"pending_approvals": map[string]any{"type": "integer"},
		"in_use":            boolean("serial: このアプリのターミナルで使用中か"),
	}
	return append([]map[string]any{
		{
			"name":        "list_hosts",
			"title":       "接続先一覧",
			"description": "ssh-gete に登録された接続先と、接続中・実行中・承認待ちの件数を返します。type=serial の接続先は MCP から実行できません。",
			"annotations": map[string]any{"title": "接続先一覧", "readOnlyHint": true, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"connected_only": boolean("接続中（serial は使用中）の接続先だけ返す"),
					"tag":            str("このタグを持つ接続先だけ返す"),
				},
				"additionalProperties": false,
			},
			"outputSchema": withErrors(map[string]any{
				"hosts": map[string]any{"type": "array", "items": object(hostProps)},
			}),
		},
		{
			"name":         "execute_command",
			"title":        "コマンド実行を要求",
			"description":  "SSH コマンドの実行を要求します。原則として人間の承認キューに入り state=pending と request_id を返します。結果は get_request_status で待って取得してください。エージェント用トークンで認証し承認省略を許可されたエージェントのコマンドは、許可されたリスク上限（既定 Medium）まで即時実行されます（High は人が GUI でリスク上限を High にした場合だけ）。即時実行が RequestTimeout を超えると state=running を返し、実行は続きます。同時実行の上限を超えると server_busy と retry_after_ms を返します。",
			"annotations":  map[string]any{"title": "コマンド実行を要求", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
			"inputSchema":  executeSchema,
			"outputSchema": object(executeProps),
		},
		{
			"name":         "request_command_execution",
			"title":        "コマンド実行を要求（旧名）",
			"description":  "非推奨: execute_command の別名です。入力も出力も execute_command と同じです。",
			"annotations":  map[string]any{"title": "コマンド実行を要求（旧名）", "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
			"inputSchema":  executeSchema,
			"outputSchema": object(executeProps),
		},
		{
			"name":        "get_request_status",
			"title":       "要求の状態を取得",
			"description": "request_id の状態と結果を返します。エージェント用トークンで認証した場合、ほかのエージェントの要求は not_found になります。wait_seconds を指定すると、いずれかの要求が終了状態（succeeded / failed / rejected など）になるかその秒数が経つまで待ってから返します。",
			"annotations": map[string]any{"title": "要求の状態を取得", "readOnlyHint": true, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 20},
					"wait_seconds":   map[string]any{"type": "integer", "minimum": 0, "maximum": 60, "description": "状態が変わるまで待つ最大秒数（既定 0）"},
					"include_output": boolean("stdout/stderr を含める（既定 true）"),
				},
				"required":             []string{"request_ids"},
				"additionalProperties": false,
			},
			"outputSchema": withErrors(map[string]any{
				"requests": map[string]any{"type": "array", "items": object(requestProps)},
			}),
		},
		{
			"name":        "get_command_history",
			"title":       "コマンド履歴",
			"description": "承認キューと実行履歴の要約（stdout なし）を新しい順に返します。既定では呼び出し元のエージェント（エージェント用トークンの名前、または agent_name）の分だけを返します。all_agents は Bearer 認証時のみ使えます。",
			"annotations": map[string]any{"title": "コマンド履歴", "readOnlyHint": true, "idempotentHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":       str("接続先名で絞り込み"),
					"state":      map[string]any{"type": "string", "enum": requestStates},
					"agent_name": str("呼び出し元のエージェント名（エージェント用トークンで認証した場合は省略可）"),
					"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": maxHistoryLimit, "description": "件数（既定 20）"},
					"all_agents": boolean("すべてのエージェントの履歴を返す（Bearer 認証時のみ）"),
				},
				"additionalProperties": false,
			},
			"outputSchema": withErrors(map[string]any{
				"requests": map[string]any{"type": "array", "items": object(requestProps)},
				"agent":    str("絞り込んだエージェント名（all_agents のときは *）"),
			}),
		},
	}, aiSessionTools()...)
}

// toolResult wraps a tool's output. structuredContent carries the object;
// the text block repeats it as JSON for clients without structured support.
func toolResult(value map[string]any, isError bool) any {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		payload = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	result := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(payload)},
		},
		"structuredContent": value,
	}
	if isError {
		result["isError"] = true
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", latestProtocolVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
