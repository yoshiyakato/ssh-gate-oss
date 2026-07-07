package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("SSH_GETE_CONFIG_DIR", t.TempDir())
	return NewApp()
}

func TestMCPHTTPServerHealth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	app := newTestApp(t)
	app.mcp.ListenAddress = "127.0.0.1"
	app.mcp.Port = port
	app.mcp.Enabled = true

	if err := app.startMCPServer(); err != nil {
		t.Fatal(err)
	}
	defer app.shutdown(t.Context())

	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestMCPToolsList(t *testing.T) {
	app := newTestApp(t)
	defer app.shutdown(t.Context())
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	app.handleMCP(rec, req, app.mcp, app.mcpToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type: %T", resp.Result)
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) < 4 {
		t.Fatalf("tools not returned: %#v", result["tools"])
	}
}

func TestMCPRequestCommandExecutionQueues(t *testing.T) {
	app := newTestApp(t)
	defer app.shutdown(t.Context())
	body := []byte(`{
		"jsonrpc":"2.0",
		"id":2,
		"method":"tools/call",
		"params":{
			"name":"request_command_execution",
			"arguments":{
				"host":"ローカル確認用",
				"command":"sudo systemctl restart nginx",
				"reason":"restart requested",
				"agent_name":"codex-test"
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	app.handleMCP(rec, req, app.mcp, app.mcpToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if len(app.requests) != 1 {
		t.Fatalf("queued requests=%d", len(app.requests))
	}
	if app.requests[0].Risk != "High" || app.requests[0].Status != "承認待ち" {
		t.Fatalf("unexpected request: %+v", app.requests[0])
	}
	if app.requests[0].RequestedBy != "codex-test" {
		t.Fatalf("agent name not stored: %+v", app.requests[0])
	}
}

func TestMCPExecuteCommandQueuesLowRisk(t *testing.T) {
	app := newTestApp(t)
	defer app.shutdown(t.Context())
	body := []byte(`{
		"jsonrpc":"2.0",
		"id":3,
		"method":"tools/call",
		"params":{
			"name":"execute_command",
			"arguments":{
				"host":"ローカル確認用",
				"command":"whoami",
				"reason":"approval required",
				"agent_name":"codex-test"
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	app.handleMCP(rec, req, app.mcp, app.mcpToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if len(app.requests) != 1 {
		t.Fatalf("queued requests=%d", len(app.requests))
	}
	if app.requests[0].Command != "whoami" || app.requests[0].Risk != "Low" || app.requests[0].Status != "承認待ち" {
		t.Fatalf("unexpected request: %+v", app.requests[0])
	}
	if app.requests[0].RequestedBy != "codex-test" {
		t.Fatalf("agent name not stored: %+v", app.requests[0])
	}
}

func TestMCPCommandRequiresAgentName(t *testing.T) {
	app := newTestApp(t)
	defer app.shutdown(t.Context())
	body := []byte(`{
		"jsonrpc":"2.0",
		"id":4,
		"method":"tools/call",
		"params":{
			"name":"execute_command",
			"arguments":{
				"host":"ローカル確認用",
				"command":"whoami",
				"reason":"approval required"
			}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	app.handleMCP(rec, req, app.mcp, app.mcpToken)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Message != "agent_name is required" {
		t.Fatalf("unexpected rpc response: %+v", resp)
	}
	if len(app.GetInitialData().Requests) != 0 {
		t.Fatalf("anonymous command was queued")
	}
}

func TestMCPRejectsSerialConnection(t *testing.T) {
	app := newTestApp(t)
	defer app.shutdown(t.Context())
	app.mu.Lock()
	app.conns = append(app.conns, Connection{Name: "uart", Type: "Serial", SerialPort: "/dev/ttyUSB0", BaudRate: 115200})
	app.mu.Unlock()

	body := []byte(`{
		"jsonrpc":"2.0",
		"id":9,
		"method":"tools/call",
		"params":{
			"name":"execute_command",
			"arguments":{"host":"uart","command":"whoami","agent_name":"codex-test"}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	app.handleMCP(rec, req, app.mcp, app.mcpToken)

	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "serial") {
		t.Fatalf("expected serial rejection, got: %+v", resp)
	}
	if len(app.GetInitialData().Requests) != 0 {
		t.Fatalf("serial command should not be queued")
	}
}

func TestSQLitePersistsCommandRequests(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("SSH_GETE_CONFIG_DIR", configDir)

	app := NewApp()
	req := app.queueCommand("ローカル確認用", "sudo systemctl restart nginx", "restart requested", "local-agent", "High")
	app.updateRequestResult(req.ID, req.Command, "拒否", commandResult{}, nil)
	app.shutdown(t.Context())

	reloaded := NewApp()
	defer reloaded.shutdown(t.Context())
	if len(reloaded.requests) != 1 {
		t.Fatalf("persisted requests=%d", len(reloaded.requests))
	}
	if reloaded.requests[0].ID != req.ID || reloaded.requests[0].Status != "拒否" {
		t.Fatalf("unexpected persisted request: %+v", reloaded.requests[0])
	}
	if reloaded.dbPath != filepath.Join(configDir, "ssh-gete.sqlite") {
		t.Fatalf("unexpected db path: %s", reloaded.dbPath)
	}
}

// TestRapidCommandRequestsPersistDistinctly guards against the millisecond-ID
// collision that dropped history rows when an approval-bypassed agent fired
// commands in bursts: every queued request must keep a unique ID and survive a
// reload from SQLite (the upsert previously overwrote rows sharing an ID).
func TestRapidCommandRequestsPersistDistinctly(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("SSH_GETE_CONFIG_DIR", configDir)

	app := NewApp()
	const n = 50
	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		req := app.queueCommand("ローカル確認用", "whoami", "burst", "codex-local", "Low")
		if ids[req.ID] {
			t.Fatalf("duplicate request ID generated: %s", req.ID)
		}
		ids[req.ID] = true
	}
	app.shutdown(t.Context())

	reloaded := NewApp()
	defer reloaded.shutdown(t.Context())
	if len(reloaded.requests) != n {
		t.Fatalf("persisted requests=%d, want %d", len(reloaded.requests), n)
	}
}

func TestDeleteConnectionPersists(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("SSH_GETE_CONFIG_DIR", configDir)

	app := NewApp()
	result := app.DeleteConnection("ローカル確認用")
	if !result.OK {
		t.Fatalf("delete failed: %s", result.Message)
	}
	if len(app.GetInitialData().Connections) != 0 {
		t.Fatalf("connection not removed from memory")
	}
	app.shutdown(t.Context())

	reloaded := NewApp()
	defer reloaded.shutdown(t.Context())
	if len(reloaded.GetInitialData().Connections) != 0 {
		t.Fatalf("deleted connection was seeded again: %+v", reloaded.GetInitialData().Connections)
	}
}

func TestDeleteCommandRequestPersists(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("SSH_GETE_CONFIG_DIR", configDir)

	app := NewApp()
	req := app.queueCommand("新規接続先", "whoami", "delete test", "mcp-client", "Low")
	result := app.DeleteCommandRequest(req.ID)
	if !result.OK {
		t.Fatalf("delete failed: %s", result.Message)
	}
	if len(app.GetInitialData().Requests) != 0 {
		t.Fatalf("request not removed from memory")
	}
	app.shutdown(t.Context())

	reloaded := NewApp()
	defer reloaded.shutdown(t.Context())
	if len(reloaded.GetInitialData().Requests) != 0 {
		t.Fatalf("deleted request was loaded again: %+v", reloaded.GetInitialData().Requests)
	}
}

func TestAgentApprovalBypassPersists(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("SSH_GETE_CONFIG_DIR", configDir)

	app := NewApp()
	result := app.setAgentApprovalBypass("codex-local", true)
	if !result.OK {
		t.Fatalf("set bypass failed: %s", result.Message)
	}
	if !app.agentApprovalBypassEnabled("codex-local") {
		t.Fatalf("bypass not enabled in memory")
	}
	app.shutdown(t.Context())

	reloaded := NewApp()
	defer reloaded.shutdown(t.Context())
	if !reloaded.agentApprovalBypassEnabled("codex-local") {
		t.Fatalf("bypass not persisted")
	}
	data := reloaded.GetInitialData()
	if len(data.AgentPolicies) != 1 || data.AgentPolicies[0].AgentName != "codex-local" || !data.AgentPolicies[0].ApprovalBypass {
		t.Fatalf("unexpected agent policies: %+v", data.AgentPolicies)
	}
}
