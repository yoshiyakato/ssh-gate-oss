package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type executeCommandArgs struct {
	Host           string `json:"host"`
	Command        string `json:"command"`
	Reason         string `json:"reason"`
	AgentName      string `json:"agent_name"`
	RequestedBy    string `json:"requested_by"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ForceQueue     bool   `json:"force_queue"`
}

func (a *App) startMCPServer() error {
	a.mu.RLock()
	settings := a.mcp
	token := a.mcpToken
	a.mu.RUnlock()

	if !settings.Enabled {
		a.setMCPStatus("停止中", "")
		return nil
	}
	addr := net.JoinHostPort(settings.ListenAddress, strconv.Itoa(settings.Port))
	if addr == "" {
		addr = defaultMCPAddr
	}

	mux := http.NewServeMux()
	mux.HandleFunc(settings.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"name":             appName,
			"mcp":              settings.MCPPath,
			"address":          addr,
			"connected_agents": a.connectedAgentCount(),
		})
	})
	mux.HandleFunc(settings.MCPPath, func(w http.ResponseWriter, r *http.Request) {
		a.handleMCP(w, r, settings, token)
	})
	mux.HandleFunc(settings.AuditPath, func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"requests":         cloneRequests(a.requests),
			"connected_agents": a.connectedAgentsLocked(),
		})
	})

	a.mcpServer = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       time.Duration(settings.RequestTimeout) * time.Second,
		MaxHeaderBytes:    settings.MaxBodyKB * 1024,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		a.setMCPStatus("起動中", "")
		log.Printf("%s MCP server listening on http://%s%s", appName, addr, settings.MCPPath)
		if err := a.mcpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			a.setMCPStatus("エラー", err.Error())
			log.Printf("mcp server stopped: %v", err)
		}
	}()
	return nil
}

func (a *App) restartMCPServer() error {
	if a.mcpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.mcpServer.Shutdown(ctx)
		a.mcpServer = nil
	}
	a.mu.RLock()
	enabled := a.mcp.Enabled
	a.mu.RUnlock()
	if !enabled {
		a.setMCPStatus("停止中", "")
		return nil
	}
	return a.startMCPServer()
}

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request, settings MCPSettings, token string) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost")
		w.Header().Set("Access-Control-Allow-Headers", "content-type, authorization")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.rememberMCPAgent(r)
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32600, Message: "POST only"},
		})
		return
	}
	if settings.BearerEnabled && (token == "" || r.Header.Get("Authorization") != "Bearer "+token) {
		writeJSON(w, http.StatusUnauthorized, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32001, Message: "unauthorized"},
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(settings.MaxBodyKB)*1024)
	defer r.Body.Close()
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "invalid json"},
		})
		return
	}
	if req.ID == nil && strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := a.dispatchMCP(req)
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
		Error:   rpcErr,
	})
}

func (a *App) rememberMCPAgent(r *http.Request) {
	key := r.Header.Get("User-Agent")
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		key = host + " " + key
	}
	if strings.TrimSpace(key) == "" {
		key = r.RemoteAddr
	}
	a.mu.Lock()
	if a.mcpAgents == nil {
		a.mcpAgents = map[string]time.Time{}
	}
	a.mcpAgents[key] = time.Now()
	a.mcp.ConnectedAgents = a.connectedAgentsLocked()
	a.mu.Unlock()
}

func (a *App) connectedAgentCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.connectedAgentsLocked()
}

func (a *App) dispatchMCP(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		protocolVersion := "2025-06-18"
		var params initializeParams
		if err := json.Unmarshal(req.Params, &params); err == nil && strings.TrimSpace(params.ProtocolVersion) != "" {
			protocolVersion = strings.TrimSpace(params.ProtocolVersion)
		}
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    appName,
				"version": "0.1.0",
			},
		}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tool params"}
		}
		return a.callMCPTool(params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func (a *App) callMCPTool(params toolCallParams) (any, *rpcError) {
	switch params.Name {
	case "list_hosts":
		a.mu.RLock()
		defer a.mu.RUnlock()
		hosts := make([]map[string]any, 0, len(a.conns))
		for _, conn := range a.conns {
			hosts = append(hosts, map[string]any{
				"name":        conn.Name,
				"type":        conn.connectionType(),
				"host":        conn.Host,
				"port":        conn.Port,
				"user":        conn.User,
				"auth_method": conn.AuthMethod,
				"tags":        conn.Tags,
				"status":      conn.Status,
			})
		}
		return textToolResult(hosts), nil
	case "get_command_history":
		a.mu.RLock()
		defer a.mu.RUnlock()
		return textToolResult(cloneRequests(a.requests)), nil
	case "request_command_execution":
		var args executeCommandArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
		if strings.TrimSpace(args.Host) == "" || strings.TrimSpace(args.Command) == "" {
			return nil, &rpcError{Code: -32602, Message: "host and command are required"}
		}
		agentName := commandAgentName(args)
		if agentName == "" {
			return nil, &rpcError{Code: -32602, Message: "agent_name is required"}
		}
		conn, ok := a.findConnection(args.Host)
		if !ok {
			return nil, &rpcError{Code: -32602, Message: "host not found"}
		}
		if conn.connectionType() == "Serial" {
			return nil, &rpcError{Code: -32602, Message: "serial connections are interactive-only; not executable via MCP"}
		}
		risk := classifyRisk(args.Command)
		if a.agentApprovalBypassEnabled(agentName) {
			req := a.executeCommandAndLog(conn, args.Command, args.Reason, agentName, risk)
			return textToolResult(map[string]any{
				"auto_executed": true,
				"request_id":    req.ID,
				"risk":          risk,
				"status":        req.Status,
				"stdout":        req.Stdout,
				"stderr":        req.Stderr,
				"duration":      req.Duration,
			}), nil
		}
		req := a.queueCommand(args.Host, args.Command, args.Reason, agentName, risk)
		return textToolResult(map[string]any{
			"queued":     true,
			"request_id": req.ID,
			"risk":       risk,
			"status":     req.Status,
		}), nil
	case "execute_command":
		var args executeCommandArgs
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid arguments"}
		}
		return a.executeCommandTool(args)
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool"}
	}
}

func (a *App) executeCommandTool(args executeCommandArgs) (any, *rpcError) {
	args.Host = strings.TrimSpace(args.Host)
	args.Command = strings.TrimSpace(args.Command)
	if args.Host == "" || args.Command == "" {
		return nil, &rpcError{Code: -32602, Message: "host and command are required"}
	}
	agentName := commandAgentName(args)
	if agentName == "" {
		return nil, &rpcError{Code: -32602, Message: "agent_name is required"}
	}

	risk := classifyRisk(args.Command)
	conn, ok := a.findConnection(args.Host)
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "host not found"}
	}
	if conn.connectionType() == "Serial" {
		return nil, &rpcError{Code: -32602, Message: "serial connections are interactive-only; not executable via MCP"}
	}
	if a.agentApprovalBypassEnabled(agentName) {
		req := a.executeCommandAndLog(conn, args.Command, args.Reason, agentName, risk)
		return textToolResult(map[string]any{
			"auto_executed": true,
			"request_id":    req.ID,
			"risk":          risk,
			"status":        req.Status,
			"stdout":        req.Stdout,
			"stderr":        req.Stderr,
			"duration":      req.Duration,
		}), nil
	}
	req := a.queueCommand(args.Host, args.Command, args.Reason, agentName, risk)
	return textToolResult(map[string]any{
		"queued":     true,
		"request_id": req.ID,
		"risk":       risk,
		"reason":     "approval_required",
	}), nil
}

func commandAgentName(args executeCommandArgs) string {
	if agentName := strings.TrimSpace(args.AgentName); agentName != "" {
		return agentName
	}
	return strings.TrimSpace(args.RequestedBy)
}

func mcpTools() []map[string]any {
	stringSchema := map[string]any{"type": "string"}
	return []map[string]any{
		{
			"name":        "list_hosts",
			"description": "ssh-geteに登録されているSSH接続先を一覧します。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "execute_command",
			"description": "コマンド実行要求を承認キューへ登録します。MCP経由ではLowリスクでも直接SSH実行しません。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":    stringSchema,
					"command": stringSchema,
					"reason":  stringSchema,
					"agent_name": map[string]any{
						"type":        "string",
						"description": "要求元エージェント名。例: codex-local, claude-desktop, local-agent",
					},
				},
				"required": []string{"host", "command", "agent_name"},
			},
		},
		{
			"name":        "request_command_execution",
			"description": "コマンド実行を承認キューへ登録します。実行はGUIで承認後に行います。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":       stringSchema,
					"command":    stringSchema,
					"reason":     stringSchema,
					"agent_name": map[string]any{"type": "string", "description": "要求元エージェント名"},
				},
				"required": []string{"host", "command", "agent_name"},
			},
		},
		{
			"name":        "get_command_history",
			"description": "承認キューとコマンド履歴を取得します。",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func textToolResult(value any) any {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		payload = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	return map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": string(payload),
			},
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", "2025-06-18")
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
