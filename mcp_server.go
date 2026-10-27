package main

import (
	"bytes"
	"context"
	"encoding/json"
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

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type executeCommandArgs struct {
	Host            string `json:"host"`
	Command         string `json:"command"`
	Reason          string `json:"reason"`
	AgentName       string `json:"agent_name"`
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
}

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request, settings MCPSettings, token string) {
	if r.Method != http.MethodPost {
		// Stateless Streamable HTTP: no server-initiated SSE stream.
		w.Header().Set("Allow", "POST, OPTIONS")
		writeJSON(w, http.StatusMethodNotAllowed, rpcErrorResponse(nil, -32600, "POST only"))
		return
	}
	if settings.BearerEnabled && !checkBearer(r, token) {
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
	if len(req.ID) == 0 {
		// Notifications (and stray responses) get no JSON-RPC reply.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32600, "invalid request"))
		return
	}

	result, rpcErr := a.dispatchMCP(r.Context(), req)
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr})
}

func (a *App) handleAudit(w http.ResponseWriter, r *http.Request, settings MCPSettings, token string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, rpcErrorResponse(nil, -32600, "GET only"))
		return
	}
	if settings.BearerEnabled && !checkBearer(r, token) {
		writeUnauthorized(w)
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	a.mu.RLock()
	requests := cloneRequests(a.requests[:min(limit, len(a.requests))])
	agents := a.connectedAgentsLocked()
	a.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"requests":         requests,
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

func (a *App) dispatchMCP(ctx context.Context, req rpcRequest) (any, *rpcError) {
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
				"version": "0.2.0",
			},
			"instructions": "ssh-gete は人間の承認を挟む SSH ゲートウェイです。execute_command は原則として承認キューに入り、結果は get_request_status(request_ids, wait_seconds) で待って取得します。",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tool params"}
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		}
		return a.callMCPTool(ctx, params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (a *App) callMCPTool(ctx context.Context, params toolCallParams) (any, *rpcError) {
	switch params.Name {
	case "list_hosts":
		var args listHostsArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
		return toolResult(map[string]any{"hosts": a.listHostsForAgent(args)}, false), nil
	case "get_command_history":
		var args historyArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
		return toolResult(map[string]any{"requests": a.commandHistory(args)}, false), nil
	case "get_request_status":
		var args requestStatusArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
		if len(args.RequestIDs) == 0 || len(args.RequestIDs) > 20 {
			return nil, &rpcError{Code: -32602, Message: "request_ids must contain 1-20 ids"}
		}
		return toolResult(map[string]any{"requests": a.requestStatus(ctx, args)}, false), nil
	case "execute_command", "request_command_execution":
		var args executeCommandArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
		return a.executeCommandTool(ctx, args)
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool"}
	}
}

func (a *App) listHostsForAgent(args listHostsArgs) []map[string]any {
	a.mu.RLock()
	conns := cloneConnections(a.conns)
	pending := map[string]int{}
	inflight := map[string]int{}
	for _, req := range a.requests {
		switch req.Status {
		case statePending:
			pending[req.ConnectionID]++
		case stateRunning, stateAutoRunning:
			inflight[req.ConnectionID]++
		}
	}
	a.mu.RUnlock()
	terminals := a.terminalCounts()

	hosts := make([]map[string]any, 0, len(conns))
	for _, conn := range conns {
		if args.Tag != "" && !slices.Contains(conn.Tags, args.Tag) {
			continue
		}
		connected := terminals[conn.ID] > 0 || inflight[conn.ID] > 0
		if args.ConnectedOnly && !connected {
			continue
		}
		serial := conn.connectionType() == "Serial"
		entry := map[string]any{
			"name":        conn.Name,
			"type":        strings.ToLower(conn.connectionType()),
			"executable":  !serial,
			"tags":        conn.Tags,
			"description": conn.Description,
			"connected":   connected,
		}
		if !serial {
			entry["open_terminals"] = terminals[conn.ID]
			entry["inflight"] = inflight[conn.ID]
			entry["pending_approvals"] = pending[conn.ID]
		}
		hosts = append(hosts, entry)
	}
	return hosts
}

func (a *App) executeCommandTool(ctx context.Context, args executeCommandArgs) (any, *rpcError) {
	args.Host = strings.TrimSpace(args.Host)
	args.Command = strings.TrimSpace(args.Command)
	args.ClientRequestID = strings.TrimSpace(args.ClientRequestID)
	if args.Host == "" || args.Command == "" {
		return nil, &rpcError{Code: -32602, Message: "host and command are required"}
	}
	agentName := commandAgentName(args)
	if agentName == "" {
		return nil, &rpcError{Code: -32602, Message: "agent_name is required"}
	}
	if existing, ok := a.findRequestByClientID(agentName, args.ClientRequestID); ok {
		out := requestView(existing, true)
		out["deduplicated"] = true
		return toolResult(out, false), nil
	}

	conn, candidates, ok := a.resolveConnection(args.Host)
	if !ok {
		kind, message := "host_not_found", "host not found"
		if len(candidates) > 1 {
			kind, message = "ambiguous_host", "host matches several connections; use the connection name"
		} else {
			candidates = a.connectionNames(10)
		}
		return toolResult(map[string]any{"error_kind": kind, "message": message, "candidates": candidates}, true), nil
	}
	if conn.connectionType() == "Serial" {
		return toolResult(map[string]any{
			"error_kind": "serial_not_supported",
			"message":    "serial connections are interactive-only; not executable via MCP",
		}, true), nil
	}

	risk := classifyRisk(args.Command)
	if a.needsHumanApproval(conn, risk, args.Command, agentName, args.ForceQueue) {
		req, err := a.queueCommand(conn, args.Command, args.Reason, agentName, risk, args.ClientRequestID)
		if err != nil {
			return toolResult(map[string]any{"error_kind": "storage", "message": err.Error()}, true), nil
		}
		out := requestView(req, false)
		out["requires_approval"] = true
		out["auto_executed"] = false
		out["next_action"] = "call get_request_status with this request_id and wait_seconds to receive the result after human approval"
		return toolResult(out, false), nil
	}

	req, err := a.executeCommandAndLog(ctx, conn, args.Command, args.Reason, agentName, risk, args.ClientRequestID, args.TimeoutSeconds)
	if err != nil {
		return toolResult(map[string]any{"error_kind": "storage", "message": err.Error()}, true), nil
	}
	out := requestView(req, true)
	out["requires_approval"] = false
	out["auto_executed"] = true
	return toolResult(out, req.Status == stateAutoFailed), nil
}

// requestView is the agent-facing shape of a request.
func requestView(req CommandRequest, includeOutput bool) map[string]any {
	out := map[string]any{
		"request_id":        req.ID,
		"connection":        req.Host,
		"state":             req.Status,
		"risk":              req.Risk,
		"requested_command": req.Command,
		"requested_at":      req.RequestedAt,
	}
	if req.ExecutedCommand != "" {
		out["executed_command"] = req.ExecutedCommand
		out["modified_by_human"] = req.ExecutedCommand != req.Command
	}
	if req.DecidedBy != "" {
		out["decided_by"] = req.DecidedBy
		out["decided_at"] = req.DecidedAt
	}
	if req.ExitCode != nil {
		out["exit_code"] = *req.ExitCode
	}
	if req.ErrorKind != "" {
		out["error_kind"] = req.ErrorKind
		out["timed_out"] = req.ErrorKind == "timeout"
	}
	if req.FinishedAt != "" {
		out["finished_at"] = req.FinishedAt
		out["duration"] = req.Duration
	}
	if includeOutput && requestIsFinal(req.Status) {
		out["stdout"] = req.Stdout
		out["stderr"] = req.Stderr
		out["stdout_truncated"] = strings.HasSuffix(req.Stdout, "[ssh-gete: output truncated]")
		out["stderr_truncated"] = strings.HasSuffix(req.Stderr, "[ssh-gete: output truncated]")
	}
	return out
}

func requestIsFinal(state string) bool {
	switch state {
	case statePending, stateRunning, stateAutoRunning:
		return false
	}
	return true
}

// requestStatus returns the requested entries, long-polling up to
// wait_seconds while all of them are still open (pending/running).
func (a *App) requestStatus(ctx context.Context, args requestStatusArgs) []map[string]any {
	includeOutput := args.IncludeOutput == nil || *args.IncludeOutput
	wait := time.Duration(min(max(args.WaitSeconds, 0), 60)) * time.Second
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		a.mu.RLock()
		changed := a.requestsChanged
		views := make([]map[string]any, 0, len(args.RequestIDs))
		anyFinal := false
		for _, id := range args.RequestIDs {
			found := false
			for _, req := range a.requests {
				if req.ID == id {
					views = append(views, requestView(req, includeOutput))
					anyFinal = anyFinal || requestIsFinal(req.Status)
					found = true
					break
				}
			}
			if !found {
				views = append(views, map[string]any{"request_id": id, "error_kind": "not_found"})
				anyFinal = true
			}
		}
		a.mu.RUnlock()
		if anyFinal || wait == 0 {
			return views
		}
		select {
		case <-changed:
		case <-deadline.C:
			return views
		case <-ctx.Done():
			return views
		}
	}
}

func (a *App) commandHistory(args historyArgs) []map[string]any {
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 500)
	var connID string
	if args.Host != "" {
		if conn, ok := a.findConnection(args.Host); ok {
			connID = conn.ID
		}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := []map[string]any{}
	for _, req := range a.requests {
		if len(out) >= limit {
			break
		}
		if args.Host != "" && req.ConnectionID != connID && req.Host != args.Host {
			continue
		}
		if args.State != "" && req.Status != args.State {
			continue
		}
		if args.AgentName != "" && req.RequestedBy != args.AgentName {
			continue
		}
		out = append(out, requestView(req, false))
	}
	return out
}

func (a *App) connectionNames(limit int) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := []string{}
	for _, conn := range a.conns {
		if len(names) >= limit {
			break
		}
		if conn.connectionType() == "SSH" {
			names = append(names, conn.Name)
		}
	}
	return names
}

func commandAgentName(args executeCommandArgs) string {
	if agentName := strings.TrimSpace(args.AgentName); agentName != "" {
		return agentName
	}
	return strings.TrimSpace(args.RequestedBy)
}

func mcpTools() []map[string]any {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	executeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"host":              str("接続先名（list_hosts の name）"),
			"command":           str("実行するコマンド"),
			"reason":            str("実行理由（承認者に表示されます）"),
			"agent_name":        str("要求元エージェント名。例: claude-desktop, codex-local"),
			"client_request_id": str("冪等キー。同じ値での再送は新規実行せず既存の request_id を返します（24時間）"),
			"force_queue":       map[string]any{"type": "boolean", "description": "承認省略が許可されていても承認キューに入れる"},
			"timeout_seconds":   map[string]any{"type": "integer", "minimum": 1, "maximum": 3600, "description": "実行タイムアウト（省略時は接続先の設定）"},
		},
		"required":             []string{"host", "command", "agent_name"},
		"additionalProperties": false,
	}
	return []map[string]any{
		{
			"name":        "list_hosts",
			"title":       "接続先一覧",
			"description": "ssh-gete に登録された接続先と、接続中・実行中・承認待ちの件数を返します。type=serial の接続先は MCP から実行できません。",
			"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"connected_only": map[string]any{"type": "boolean"},
					"tag":            str("このタグを持つ接続先だけ返す"),
				},
				"additionalProperties": false,
			},
		},
		{
			"name":        "execute_command",
			"title":       "コマンド実行を要求",
			"description": "SSH コマンドの実行を要求します。原則として人間の承認キューに入り state=pending と request_id を返します。結果は get_request_status で待って取得してください。Bearer 認証済みで承認省略を許可されたエージェントの Low/Medium リスク コマンドのみ即時実行されます（High は常に承認必須）。",
			"annotations": map[string]any{"destructiveHint": true, "openWorldHint": true},
			"inputSchema": executeSchema,
		},
		{
			"name":        "request_command_execution",
			"title":       "コマンド実行を要求（旧名）",
			"description": "execute_command の別名です（非推奨）。",
			"annotations": map[string]any{"destructiveHint": true, "openWorldHint": true},
			"inputSchema": executeSchema,
		},
		{
			"name":        "get_request_status",
			"title":       "要求の状態を取得",
			"description": "request_id の状態と結果を返します。wait_seconds を指定すると、承認・実行が終わるかその秒数が経つまで待ってから返します。",
			"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 20},
					"wait_seconds":   map[string]any{"type": "integer", "minimum": 0, "maximum": 60},
					"include_output": map[string]any{"type": "boolean", "description": "stdout/stderr を含める（既定 true）"},
				},
				"required":             []string{"request_ids"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "get_command_history",
			"title":       "コマンド履歴",
			"description": "承認キューと実行履歴の要約（stdout なし）を新しい順に返します。",
			"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":       str("接続先名で絞り込み"),
					"state":      str("pending / running / succeeded / failed / rejected / interrupted / auto_succeeded / auto_failed"),
					"agent_name": str("要求元エージェントで絞り込み"),
					"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 500},
				},
				"additionalProperties": false,
			},
		},
	}
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

func classifyRisk(command string) string {
	lower := strings.ToLower(command)
	highPatterns := []string{
		" rm ", "rm -", "rm\t", "mkfs", "dd if=", "shutdown", "reboot", "halt",
		"systemctl restart", "systemctl stop", "service restart", "service stop",
		"iptables", "ufw ", "userdel", "passwd ", "chown -r", "chmod -r",
	}
	for _, pattern := range highPatterns {
		if strings.Contains(" "+lower+" ", pattern) {
			return "High"
		}
	}
	mediumPatterns := []string{
		"sudo ", "apt ", "apt-get ", "yum ", "dnf ", "brew ", "docker rm",
		"docker stop", "kubectl delete", "mv ", "cp -r", "truncate",
	}
	for _, pattern := range mediumPatterns {
		if strings.Contains(" "+lower+" ", pattern) {
			return "Medium"
		}
	}
	return "Low"
}
