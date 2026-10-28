package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAISessionBackend is a scripted mcpAISessionBackend. It keeps a tiny
// exec-mode model (cwd carried by cd) so the open → send → read → close flow
// can be checked end to end at the MCP layer.
type fakeAISessionBackend struct {
	mu       sync.Mutex
	disabled bool
	sessions map[string]*fakeAISession
	nextID   int
	calls    map[string]int
	// openDelay and the concurrency counters check the idempotency lock.
	openDelay   time.Duration
	opening     int32
	maxOpening  int32
	byClientReq map[string]string
	// errFor makes the named method fail.
	errFor map[string]error
	// listExtra is appended to every list answer (filter checks).
	listExtra []mcpAISessionListItem
	// sendState overrides the state of the next send_line answer.
	sendState string
	lastSend  sendLineArgs
	lastRead  readOutputArgs
	lastOpen  openSessionArgs
	readCtx   context.Context
}

type fakeAISession struct {
	id, owner, conn string
	cwd             string
	out             strings.Builder
	lines           int
	closed          bool
	reason          string
}

func newFakeAISessionBackend() *fakeAISessionBackend {
	return &fakeAISessionBackend{
		sessions:    map[string]*fakeAISession{},
		calls:       map[string]int{},
		byClientReq: map[string]string{},
		errFor:      map[string]error{},
	}
}

// fakeAISessionID builds a well-formed ID from n.
func fakeAISessionID(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	suffix := make([]byte, 6)
	for i := len(suffix) - 1; i >= 0; i-- {
		suffix[i] = alphabet[n%32]
		n /= 32
	}
	return "AIS-" + strings.Repeat("A", 20) + string(suffix)
}

func (f *fakeAISessionBackend) record(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	return f.errFor[name]
}

func (f *fakeAISessionBackend) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeAISessionBackend) aiSessionMCPEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.disabled
}

func (f *fakeAISessionBackend) aiSessionMCPLookup(id string) (mcpAISessionRef, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["lookup"]++
	s, ok := f.sessions[id]
	if !ok {
		return mcpAISessionRef{}, false
	}
	return mcpAISessionRef{ID: s.id, Owner: s.owner, Closed: s.closed, CloseReason: s.reason}, true
}

func (f *fakeAISessionBackend) aiSessionMCPOpen(ctx context.Context, owner string, caller mcpCaller, args openSessionArgs) (mcpAISessionOpenView, error) {
	if err := f.record("open"); err != nil {
		return mcpAISessionOpenView{}, err
	}
	n := atomic.AddInt32(&f.opening, 1)
	defer atomic.AddInt32(&f.opening, -1)
	for {
		old := atomic.LoadInt32(&f.maxOpening)
		if n <= old || atomic.CompareAndSwapInt32(&f.maxOpening, old, n) {
			break
		}
	}
	// Lookup and creation are not atomic on purpose: the MCP layer's key
	// lock is what makes concurrent retries open one session.
	f.mu.Lock()
	existing, dup := f.byClientReq[owner+"/"+args.ClientRequestID]
	f.mu.Unlock()
	time.Sleep(f.openDelay)
	if dup && args.ClientRequestID != "" {
		return mcpAISessionOpenView{SessionID: existing, Connection: args.Host, Mode: args.Mode, Shell: args.Shell, State: "idle", Cwd: "/home/u", Deduplicated: true}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastOpen = args
	f.nextID++
	id := fakeAISessionID(f.nextID)
	f.sessions[id] = &fakeAISession{id: id, owner: owner, conn: args.Host, cwd: "/home/u"}
	if args.ClientRequestID != "" {
		f.byClientReq[owner+"/"+args.ClientRequestID] = id
	}
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	return mcpAISessionOpenView{
		SessionID: id, Connection: args.Host, Mode: args.Mode, Shell: args.Shell, State: "idle", Cwd: "/home/u",
		OpenedAt: formatAISessionTime(now), IdleTimeoutS: 600, ExpiresAt: formatAISessionTime(now.Add(time.Hour)),
		Limits: mcpAISessionLimits{PerAgent: 3, Total: 16, RingBytes: 1 << 20},
	}, nil
}

func (f *fakeAISessionBackend) aiSessionMCPSendLine(ctx context.Context, owner string, caller mcpCaller, args sendLineArgs) (mcpAISessionSendView, error) {
	if err := f.record("send"); err != nil {
		return mcpAISessionSendView{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSend = args
	s := f.sessions[args.SessionID]
	if s == nil || s.owner != owner {
		return mcpAISessionSendView{}, newMCPAISessionErr("session_not_found", "gone", nil)
	}
	s.lines++
	before := uint64(s.out.Len())
	view := mcpAISessionSendView{
		RequestID: fmt.Sprintf("REQ-%d", s.lines), SessionID: s.id, Seq: s.lines, Risk: "Low",
		Effect: "none", SessionState: "idle", CursorBefore: before,
	}
	if state := f.sendState; state != "" {
		view.State = state
		view.RequiresApproval = state == statePending
		if state == statePending {
			view.SessionState = "pending_approval"
		}
		return view, nil
	}
	exit := 0
	output := ""
	switch {
	case strings.HasPrefix(args.Line, "cd "):
		s.cwd = strings.TrimSpace(strings.TrimPrefix(args.Line, "cd "))
		view.Effect = "cd"
		view.CwdPersisted = true
		view.RiskReasons = []string{"cd_builtin"}
	case args.Line == "pwd":
		output = s.cwd + "\n"
	default:
		output = "ran " + args.Line + "\n"
	}
	s.out.WriteString("$ " + args.Line + "\n" + output)
	after := uint64(s.out.Len())
	view.State = stateAutoSucceeded
	view.ExitCode = &exit
	view.Output = output
	view.Cwd = s.cwd
	view.CursorAfter = &after
	return view, nil
}

func (f *fakeAISessionBackend) aiSessionMCPReadOutput(ctx context.Context, owner string, args readOutputArgs) (mcpAISessionReadView, error) {
	if err := f.record("read"); err != nil {
		return mcpAISessionReadView{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRead = args
	f.readCtx = ctx
	s := f.sessions[args.SessionID]
	text := s.out.String()
	since := uint64(0)
	if args.SinceCursor != nil {
		since = *args.SinceCursor
	}
	if since > uint64(len(text)) {
		return mcpAISessionReadView{}, newMCPAISessionErr("invalid_cursor", "since_cursor is past the end", nil)
	}
	chunk := text[since:]
	if len(chunk) > args.MaxBytes {
		chunk = chunk[:args.MaxBytes]
	}
	state := "idle"
	if s.closed {
		state = "closed"
	}
	return mcpAISessionReadView{
		Text: chunk, StartCursor: since, Cursor: since + uint64(len(chunk)), EndCursor: uint64(len(text)),
		More: since+uint64(len(chunk)) < uint64(len(text)), SessionState: state, CloseReason: s.reason, Cwd: s.cwd,
	}, nil
}

func (f *fakeAISessionBackend) aiSessionMCPInterrupt(ctx context.Context, owner string, sessionID string) (mcpAISessionInterruptView, error) {
	if err := f.record("interrupt"); err != nil {
		return mcpAISessionInterruptView{}, err
	}
	return mcpAISessionInterruptView{SessionID: sessionID}, nil
}

func (f *fakeAISessionBackend) aiSessionMCPClose(ctx context.Context, owner string, sessionID string) (mcpAISessionCloseView, error) {
	if err := f.record("close"); err != nil {
		return mcpAISessionCloseView{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[sessionID]
	already := s.closed
	if !s.closed {
		s.closed = true
		s.reason = "agent_closed"
	}
	return mcpAISessionCloseView{SessionID: s.id, Closed: true, CloseReason: s.reason, Lines: s.lines, OutputBytes: uint64(s.out.Len()), AlreadyClosed: already}, nil
}

func (f *fakeAISessionBackend) aiSessionMCPList(owner string, includeClosed bool) ([]mcpAISessionListItem, error) {
	if err := f.record("list"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mcpAISessionListItem
	ids := make([]string, 0, len(f.sessions))
	for id := range f.sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		s := f.sessions[id]
		// Deliberately returns every owner: the MCP layer must filter.
		state := "idle"
		if s.closed {
			state = "closed"
		}
		out = append(out, mcpAISessionListItem{Owner: s.owner, SessionID: s.id, Connection: s.conn, Mode: "exec", State: state, Cwd: s.cwd, Lines: s.lines, CloseReason: s.reason})
	}
	return append(out, f.listExtra...), nil
}

var _ mcpAISessionBackend = (*fakeAISessionBackend)(nil)

// aisCall runs one session tool through callAISessionToolWith and returns
// structuredContent, isError and the JSON-RPC error.
func aisCall(t *testing.T, app *App, backend mcpAISessionBackend, caller mcpCaller, name string, args string) (map[string]any, bool, *rpcError) {
	t.Helper()
	out, rpcErr, handled := app.callAISessionToolWith(context.Background(), backend, caller, toolCallParams{Name: name, Arguments: json.RawMessage(args)})
	if !handled {
		t.Fatalf("%s not handled", name)
	}
	if rpcErr != nil {
		return nil, false, rpcErr
	}
	// Round-trip through JSON the way the HTTP answer would.
	payload, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
		t.Fatalf("%s: content block missing: %s", name, payload)
	}
	return resp.StructuredContent, resp.IsError, nil
}

func mustAIS(t *testing.T, app *App, backend mcpAISessionBackend, caller mcpCaller, name string, args string) map[string]any {
	t.Helper()
	out, isErr, rpcErr := aisCall(t, app, backend, caller, name, args)
	if rpcErr != nil {
		t.Fatalf("%s %s: rpc error %+v", name, args, rpcErr)
	}
	if isErr {
		t.Fatalf("%s %s: tool error %v", name, args, out)
	}
	return out
}

func wantAISError(t *testing.T, app *App, backend mcpAISessionBackend, caller mcpCaller, name string, args string, code string) map[string]any {
	t.Helper()
	out, isErr, rpcErr := aisCall(t, app, backend, caller, name, args)
	if rpcErr != nil {
		t.Fatalf("%s %s: rpc error %+v, want %s", name, args, rpcErr, code)
	}
	if !isErr || out["error_kind"] != code {
		t.Fatalf("%s %s: got isError=%v %v, want %s", name, args, isErr, out, code)
	}
	return out
}

func wantInvalidParams(t *testing.T, app *App, backend mcpAISessionBackend, caller mcpCaller, name string, args string) {
	t.Helper()
	_, _, rpcErr := aisCall(t, app, backend, caller, name, args)
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("%s %s: got %+v, want invalid params", name, args, rpcErr)
	}
}

var aisAgent = mcpCaller{Agent: "claude-code"}

func openFakeSession(t *testing.T, app *App, backend *fakeAISessionBackend, caller mcpCaller) string {
	t.Helper()
	out := mustAIS(t, app, backend, caller, "open_session", `{"host":"web-01","reason":"調査"}`)
	id, _ := out["session_id"].(string)
	if !isAISessionID(id) {
		t.Fatalf("bad session id %q", id)
	}
	return id
}

func TestAISessionToolsListSchema(t *testing.T) {
	tools := aiSessionTools()
	want := []string{"open_session", "send_line", "read_output", "interrupt", "close_session", "list_sessions"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	// mcpTools() ends with aiSessionTools(); the names must not collide with
	// the tools before them.
	existing := map[string]bool{}
	all := mcpTools()
	for _, tool := range all[:len(all)-len(tools)] {
		existing[tool["name"].(string)] = true
	}
	for i, tool := range all[len(all)-len(tools):] {
		if tool["name"] != want[i] {
			t.Fatalf("mcpTools() does not end with the AI session tools: %v", tool["name"])
		}
	}
	argTypes := map[string]any{
		"open_session": openSessionArgs{}, "send_line": sendLineArgs{}, "read_output": readOutputArgs{},
		"interrupt": sessionIDArgs{}, "close_session": sessionIDArgs{}, "list_sessions": listSessionsArgs{},
	}
	viewTypes := map[string]any{
		"open_session": mcpAISessionOpenView{}, "send_line": mcpAISessionSendView{}, "read_output": mcpAISessionReadView{},
		"interrupt": mcpAISessionInterruptView{}, "close_session": mcpAISessionCloseView{},
	}
	for i, tool := range tools {
		name := tool["name"].(string)
		if name != want[i] {
			t.Fatalf("tool %d = %s, want %s", i, name, want[i])
		}
		if existing[name] {
			t.Fatalf("%s collides with an existing tool", name)
		}
		if !isAISessionTool(name) {
			t.Fatalf("%s not routed", name)
		}
		for _, key := range []string{"title", "description", "annotations", "inputSchema", "outputSchema"} {
			if tool[key] == nil {
				t.Fatalf("%s: %s missing", name, key)
			}
		}
		if !strings.Contains(tool["description"].(string), "エージェント用トークン") || !strings.Contains(tool["description"].(string), "GUI で有効") {
			t.Fatalf("%s: description must say token and GUI enable are needed", name)
		}
		input := tool["inputSchema"].(map[string]any)
		if input["additionalProperties"] != false || input["type"] != "object" {
			t.Fatalf("%s: inputSchema must be a closed object", name)
		}
		props := input["properties"].(map[string]any)
		if _, ok := props["agent_name"]; ok {
			t.Fatalf("%s: agent_name must not be accepted", name)
		}
		// inputSchema and the decoded struct agree field for field.
		fields := jsonFieldNames(reflect.TypeOf(argTypes[name]))
		for _, f := range fields {
			if _, ok := props[f]; !ok {
				t.Fatalf("%s: arg %s missing from inputSchema", name, f)
			}
		}
		if len(fields) != len(props) {
			t.Fatalf("%s: inputSchema has %d properties, struct %d", name, len(props), len(fields))
		}
		if required, ok := input["required"].([]string); ok {
			for _, r := range required {
				if _, ok := props[r]; !ok {
					t.Fatalf("%s: required %s not a property", name, r)
				}
			}
		}
		output := tool["outputSchema"].(map[string]any)
		outProps := output["properties"].(map[string]any)
		for _, key := range []string{"error_kind", "message", "close_reason"} {
			if _, ok := outProps[key]; !ok {
				t.Fatalf("%s: outputSchema lacks error field %s", name, key)
			}
		}
		if view, ok := viewTypes[name]; ok {
			for _, f := range jsonFieldNames(reflect.TypeOf(view)) {
				if _, ok := outProps[f]; !ok {
					t.Fatalf("%s: view field %s missing from outputSchema", name, f)
				}
			}
		}
		ann := tool["annotations"].(map[string]any)
		readOnly := name == "read_output" || name == "list_sessions"
		if ann["readOnlyHint"] != readOnly {
			t.Fatalf("%s: readOnlyHint=%v", name, ann["readOnlyHint"])
		}
		if !readOnly && ann["destructiveHint"] != true {
			t.Fatalf("%s: destructiveHint must be true", name)
		}
	}
	// list_sessions items cover the list view.
	listOut := tools[5]["outputSchema"].(map[string]any)["properties"].(map[string]any)["sessions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	for _, f := range jsonFieldNames(reflect.TypeOf(mcpAISessionListItem{})) {
		if _, ok := listOut[f]; !ok {
			t.Fatalf("list item field %s missing from outputSchema", f)
		}
	}
	// The whole list marshals (tools/list is JSON).
	if _, err := json.Marshal(append(mcpTools(), tools...)); err != nil {
		t.Fatal(err)
	}
}

func jsonFieldNames(typ reflect.Type) []string {
	var names []string
	for i := range typ.NumField() {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
}

func TestAISessionToolNotHandledForOtherNames(t *testing.T) {
	app := newTestApp(t)
	for _, name := range []string{"execute_command", "list_hosts", "get_request_status", "nope", ""} {
		_, _, handled := app.callAISessionToolWith(context.Background(), newFakeAISessionBackend(), aisAgent, toolCallParams{Name: name, Arguments: json.RawMessage(`{}`)})
		if handled {
			t.Fatalf("%q handled by the session tools", name)
		}
	}
}

func TestAISessionToolsDisabledWithoutBackend(t *testing.T) {
	// The real App has no session backend until the session code lands: the
	// tools must answer ai_session_disabled, not crash or run anything.
	app := newTestApp(t)
	for _, name := range []string{"open_session", "send_line", "read_output", "interrupt", "close_session", "list_sessions"} {
		args := `{}`
		switch name {
		case "open_session":
			args = `{"host":"web-01","reason":"x"}`
		case "send_line":
			args = `{"session_id":"` + fakeAISessionID(1) + `","line":"ls","client_request_id":"c1"}`
		case "read_output", "interrupt", "close_session":
			args = `{"session_id":"` + fakeAISessionID(1) + `"}`
		}
		out, rpcErr, handled := app.callAISessionTool(context.Background(), aisAgent, toolCallParams{Name: name, Arguments: json.RawMessage(args)})
		if !handled || rpcErr != nil {
			t.Fatalf("%s: handled=%v rpc=%+v", name, handled, rpcErr)
		}
		sc := out.(map[string]any)["structuredContent"].(map[string]any)
		if sc["error_kind"] != "ai_session_disabled" {
			t.Fatalf("%s: %v", name, sc)
		}
	}
}

func TestAISessionToolsDisabledSetting(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	backend.disabled = true
	wantAISError(t, app, backend, aisAgent, "open_session", `{"host":"web-01","reason":"x"}`, "ai_session_disabled")
	wantAISError(t, app, backend, aisAgent, "list_sessions", `{}`, "ai_session_disabled")
	if backend.count("open") != 0 || backend.count("list") != 0 {
		t.Fatal("backend reached while disabled")
	}
}

func TestAISessionToolsRequireAgentToken(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	id := openFakeSession(t, app, backend, aisAgent)
	for _, caller := range []mcpCaller{{}, {Shared: true}, {Agent: "   "}} {
		for name, args := range map[string]string{
			"open_session":  `{"host":"web-01","reason":"x"}`,
			"send_line":     `{"session_id":"` + id + `","line":"ls","client_request_id":"c1"}`,
			"read_output":   `{"session_id":"` + id + `"}`,
			"interrupt":     `{"session_id":"` + id + `"}`,
			"close_session": `{"session_id":"` + id + `"}`,
			"list_sessions": `{}`,
		} {
			wantAISError(t, app, backend, caller, name, args, "agent_token_required")
		}
	}
	if backend.count("open") != 1 || backend.count("send") != 0 || backend.count("close") != 0 || backend.count("list") != 0 {
		t.Fatalf("backend reached without an agent token: %v", backend.calls)
	}
}

func TestAISessionToolsRejectNamedAgent(t *testing.T) {
	// The owner comes from the token, never from an argument.
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	wantInvalidParams(t, app, backend, aisAgent, "open_session", `{"host":"web-01","reason":"x","agent_name":"other"}`)
	wantInvalidParams(t, app, backend, aisAgent, "list_sessions", `{"agent_name":"other"}`)
	wantInvalidParams(t, app, backend, aisAgent, "send_line", `{"session_id":"x","line":"ls","client_request_id":"c","requested_by":"other"}`)
}

func TestAISessionArgumentValidation(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	id := openFakeSession(t, app, backend, aisAgent)
	long := strings.Repeat("あ", mcpAISMaxReasonRunes+1)
	crid := strings.Repeat("c", maxClientRequestIDLen+1)
	for _, c := range []struct{ name, args string }{
		{"open_session", `{"reason":"x"}`},
		{"open_session", `{"host":"web-01"}`},
		{"open_session", `{"host":"web-01","reason":"   "}`},
		{"open_session", `{"host":"web-01","reason":"` + long + `"}`},
		{"open_session", `{"host":"web-01","reason":"x","mode":"pty"}`},
		{"open_session", `{"host":"web-01","reason":"x","shell":"zsh"}`},
		{"open_session", `{"host":"web-01","reason":"x","client_request_id":"` + crid + `"}`},
		{"open_session", `{"host":1,"reason":"x"}`},
		{"open_session", `[]`},
		{"send_line", `{"line":"ls","client_request_id":"c"}`},
		{"send_line", `{"session_id":"` + id + `","line":"ls"}`},
		{"send_line", `{"session_id":"` + id + `","line":"ls","client_request_id":"  "}`},
		{"send_line", `{"session_id":"` + id + `","line":"ls","client_request_id":"` + crid + `"}`},
		{"send_line", `{"session_id":"` + id + `","line":"ls","client_request_id":"c","wait_seconds":61}`},
		{"send_line", `{"session_id":"` + id + `","line":"ls","client_request_id":"c","wait_seconds":-1}`},
		{"send_line", `{"session_id":"` + id + `","line":"ls","client_request_id":"c","timeout_seconds":3601}`},
		{"send_line", `{"session_id":"` + id + `","line":"ls","client_request_id":"c","reason":"` + long + `"}`},
		{"read_output", `{}`},
		{"read_output", `{"session_id":"` + id + `","since_cursor":-1}`},
		{"read_output", `{"session_id":"` + id + `","max_bytes":65537}`},
		{"read_output", `{"session_id":"` + id + `","max_bytes":-5}`},
		{"read_output", `{"session_id":"` + id + `","wait_seconds":31}`},
		{"interrupt", `{}`},
		{"close_session", `{"session_id":"` + id + `","extra":1}`},
		{"list_sessions", `{"include_closed":"yes"}`},
	} {
		wantInvalidParams(t, app, backend, aisAgent, c.name, c.args)
	}
	if backend.count("send") != 0 || backend.count("read") != 0 || backend.count("interrupt") != 0 || backend.count("close") != 0 {
		t.Fatalf("invalid arguments reached the backend: %v", backend.calls)
	}

	// Defaults and normalization reach the backend.
	mustAIS(t, app, backend, aisAgent, "open_session", `{"host":"  web-01 ","reason":" 調査 ","client_request_id":" k "}`)
	if got := backend.lastOpen; got.Host != "web-01" || got.Reason != "調査" || got.Mode != "exec" || got.Shell != "sh" || got.ClientRequestID != "k" {
		t.Fatalf("open args = %+v", got)
	}
	mustAIS(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"  ls  ","client_request_id":"c"}`)
	if got := backend.lastSend; got.Line != "  ls  " || got.waitDuration() != 10*time.Second || got.WaitSeconds == nil {
		t.Fatalf("send args = %+v (the line must not be trimmed)", got)
	}
	mustAIS(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"ls","client_request_id":"c2","wait_seconds":0}`)
	if backend.lastSend.waitDuration() != 0 {
		t.Fatal("wait_seconds=0 must not wait")
	}
	mustAIS(t, app, backend, aisAgent, "read_output", `{"session_id":"`+id+`"}`)
	if got := backend.lastRead; got.MaxBytes != mcpAISDefaultReadMaxBytes || got.SinceCursor != nil {
		t.Fatalf("read args = %+v", got)
	}
	// null arguments are an empty object.
	out, rpcErr, _ := app.callAISessionToolWith(context.Background(), backend, aisAgent, toolCallParams{Name: "list_sessions", Arguments: json.RawMessage(`null`)})
	if rpcErr != nil || out == nil {
		t.Fatalf("null arguments: %+v", rpcErr)
	}
}

func TestAISessionIDShapeAndOwner(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	id := openFakeSession(t, app, backend, aisAgent)
	lookups := backend.count("lookup")

	// Malformed IDs (a human tab's TERM- ID included) are not looked up.
	for _, bad := range []string{"TERM-1", "TERM-" + strings.Repeat("A", 26), strings.ToLower(id), id + "A", id[:len(id)-1], "AIS-" + strings.Repeat("1", 26), "ais-x"} {
		for _, name := range []string{"read_output", "interrupt", "close_session"} {
			wantAISError(t, app, backend, aisAgent, name, `{"session_id":"`+bad+`"}`, "session_not_found")
		}
		wantAISError(t, app, backend, aisAgent, "send_line", `{"session_id":"`+bad+`","line":"ls","client_request_id":"c"}`, "session_not_found")
	}
	if backend.count("lookup") != lookups {
		t.Fatal("malformed IDs were looked up")
	}
	// Well-formed but unknown.
	wantAISError(t, app, backend, aisAgent, "read_output", `{"session_id":"`+fakeAISessionID(999)+`"}`, "session_not_found")

	// Another agent's session.
	other := mcpCaller{Agent: "codex"}
	for name, args := range map[string]string{
		"send_line":     `{"session_id":"` + id + `","line":"ls","client_request_id":"c"}`,
		"read_output":   `{"session_id":"` + id + `"}`,
		"interrupt":     `{"session_id":"` + id + `"}`,
		"close_session": `{"session_id":"` + id + `"}`,
	} {
		wantAISError(t, app, backend, other, name, args, "not_owner")
	}
	if backend.count("send") != 0 || backend.count("read") != 0 || backend.count("interrupt") != 0 || backend.count("close") != 0 {
		t.Fatalf("another agent reached the backend: %v", backend.calls)
	}
	// Owner names are compared exactly.
	wantAISError(t, app, backend, mcpCaller{Agent: "Claude-Code"}, "read_output", `{"session_id":"`+id+`"}`, "not_owner")
}

func TestAISessionClosedSession(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	id := openFakeSession(t, app, backend, aisAgent)
	mustAIS(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"pwd","client_request_id":"c1"}`)
	closed := mustAIS(t, app, backend, aisAgent, "close_session", `{"session_id":"`+id+`"}`)
	if closed["closed"] != true || closed["close_reason"] != "agent_closed" || closed["already_closed"] != nil {
		t.Fatalf("close = %v", closed)
	}
	// Closing again answers the reason, not an error.
	again := mustAIS(t, app, backend, aisAgent, "close_session", `{"session_id":"`+id+`"}`)
	if again["close_reason"] != "agent_closed" || again["already_closed"] != true {
		t.Fatalf("second close = %v", again)
	}
	out := wantAISError(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"ls","client_request_id":"c2"}`, "session_closed")
	if out["close_reason"] != "agent_closed" {
		t.Fatalf("session_closed lacks close_reason: %v", out)
	}
	wantAISError(t, app, backend, aisAgent, "interrupt", `{"session_id":"`+id+`"}`, "session_closed")
	// The tombstone can still be read.
	read := mustAIS(t, app, backend, aisAgent, "read_output", `{"session_id":"`+id+`"}`)
	if read["session_state"] != "closed" || !strings.Contains(read["text"].(string), "/home/u") {
		t.Fatalf("tombstone read = %v", read)
	}
	if backend.count("send") != 1 {
		t.Fatal("a line reached a closed session")
	}
}

func TestAISessionFlowCarriesCwd(t *testing.T) {
	// open → send(cd) → send(pwd) → read → close, as the acceptance criteria
	// describe, at the MCP layer.
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	opened := mustAIS(t, app, backend, aisAgent, "open_session", `{"host":"web-01","reason":"nginx を調べる"}`)
	id := opened["session_id"].(string)
	if opened["state"] != "idle" || opened["mode"] != "exec" || opened["output_cursor"] != float64(0) {
		t.Fatalf("open = %v", opened)
	}
	if limits := opened["limits"].(map[string]any); limits["per_agent"] != float64(3) || limits["total"] != float64(16) {
		t.Fatalf("limits = %v", limits)
	}
	cd := mustAIS(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"cd /var/www","client_request_id":"c1"}`)
	if cd["effect"] != "cd" || cd["cwd_persisted"] != true || cd["cwd"] != "/var/www" || cd["state"] != stateAutoSucceeded {
		t.Fatalf("cd = %v", cd)
	}
	if _, ok := cd["next_action"]; ok {
		t.Fatalf("a finished, untruncated line needs no next_action: %v", cd)
	}
	pwd := mustAIS(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"pwd","client_request_id":"c2"}`)
	if pwd["output"] != "/var/www\n" || pwd["exit_code"] != float64(0) {
		t.Fatalf("pwd = %v", pwd)
	}
	if reasons, ok := pwd["risk_reasons"].([]any); !ok || len(reasons) != 0 {
		t.Fatalf("risk_reasons must be an array: %v", pwd["risk_reasons"])
	}
	read := mustAIS(t, app, backend, aisAgent, "read_output", fmt.Sprintf(`{"session_id":"%s","since_cursor":%v}`, id, cd["cursor_before"]))
	if !strings.Contains(read["text"].(string), "$ pwd\n/var/www\n") || read["more"] != false {
		t.Fatalf("read = %v", read)
	}
	// Reading on from cursor returns nothing new.
	tail := mustAIS(t, app, backend, aisAgent, "read_output", fmt.Sprintf(`{"session_id":"%s","since_cursor":%v}`, id, read["cursor"]))
	if tail["text"] != "" {
		t.Fatalf("tail = %v", tail)
	}
	wantAISError(t, app, backend, aisAgent, "read_output", fmt.Sprintf(`{"session_id":"%s","since_cursor":99999}`, id), "invalid_cursor")
	list := mustAIS(t, app, backend, aisAgent, "list_sessions", `{}`)
	sessions := list["sessions"].([]any)
	if len(sessions) != 1 || sessions[0].(map[string]any)["cwd"] != "/var/www" || sessions[0].(map[string]any)["lines"] != float64(2) {
		t.Fatalf("list = %v", list)
	}
	closed := mustAIS(t, app, backend, aisAgent, "close_session", `{"session_id":"`+id+`"}`)
	if closed["lines"] != float64(2) {
		t.Fatalf("close = %v", closed)
	}
}

func TestAISessionSendLineStates(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	id := openFakeSession(t, app, backend, aisAgent)
	args := func(crid string) string {
		return `{"session_id":"` + id + `","line":"systemctl restart nginx","client_request_id":"` + crid + `"}`
	}

	backend.sendState = statePending
	pending := mustAIS(t, app, backend, aisAgent, "send_line", args("p"))
	if pending["requires_approval"] != true || !strings.Contains(pending["next_action"].(string), "get_request_status") {
		t.Fatalf("pending = %v", pending)
	}
	backend.sendState = stateAutoRunning
	running := mustAIS(t, app, backend, aisAgent, "send_line", args("r"))
	if !strings.Contains(running["next_action"].(string), "read_output") {
		t.Fatalf("running = %v", running)
	}
	// An auto-executed line that failed is isError, like execute_command.
	for _, state := range []string{stateAutoFailed, stateInterrupted} {
		backend.sendState = state
		out, isErr, rpcErr := aisCall(t, app, backend, aisAgent, "send_line", args(state))
		if rpcErr != nil || !isErr || out["state"] != state || out["request_id"] == nil {
			t.Fatalf("%s: isError=%v %v", state, isErr, out)
		}
	}
}

func TestAISessionBackendErrors(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	id := openFakeSession(t, app, backend, aisAgent)

	backend.errFor["send"] = newMCPAISessionErr("session_busy", "a line is awaiting approval", map[string]any{"request_id": "REQ-1", "state": statePending, "error_kind": "spoofed", "message": "spoofed"})
	busy := wantAISError(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"ls","client_request_id":"c"}`, "session_busy")
	if busy["request_id"] != "REQ-1" || busy["state"] != statePending || busy["message"] != "a line is awaiting approval" {
		t.Fatalf("busy = %v", busy)
	}
	backend.errFor["send"] = fmt.Errorf("wrapped: %w", newMCPAISessionErr("line_rejected", "heredoc", map[string]any{"reject_reason": "heredoc"}))
	rejected := wantAISError(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"cat <<EOF","client_request_id":"c"}`, "line_rejected")
	if rejected["reject_reason"] != "heredoc" {
		t.Fatalf("rejected = %v", rejected)
	}
	// Unknown errors never leak their text (dial errors carry addresses).
	backend.errFor["send"] = errors.New("dial tcp 10.0.0.5:22: connection refused")
	internal := wantAISError(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"ls","client_request_id":"c"}`, "internal")
	if strings.Contains(fmt.Sprint(internal), "10.0.0.5") {
		t.Fatalf("error detail leaked: %v", internal)
	}
	// An undocumented code is reported as internal.
	backend.errFor["send"] = newMCPAISessionErr("made_up", "x", nil)
	wantAISError(t, app, backend, aisAgent, "send_line", `{"session_id":"`+id+`","line":"ls","client_request_id":"c"}`, "internal")
	backend.errFor["open"] = fmt.Errorf("stop: %w", errShuttingDown)
	wantAISError(t, app, backend, aisAgent, "open_session", `{"host":"web-01","reason":"x"}`, "shutting_down")
	backend.errFor["open"] = newMCPAISessionErr("connect_failed", "host key is not trusted", map[string]any{"connect_error_kind": "hostkey_unknown"})
	cf := wantAISError(t, app, backend, aisAgent, "open_session", `{"host":"web-01","reason":"x"}`, "connect_failed")
	if cf["connect_error_kind"] != "hostkey_unknown" {
		t.Fatalf("connect_failed = %v", cf)
	}
	backend.errFor["open"] = newMCPAISessionErr("session_limit", "too many sessions", map[string]any{"scope": "per_agent"})
	wantAISError(t, app, backend, aisAgent, "open_session", `{"host":"web-01","reason":"x"}`, "session_limit")
	backend.errFor["interrupt"] = newMCPAISessionErr("interrupt_rate_limited", "once per second", nil)
	wantAISError(t, app, backend, aisAgent, "interrupt", `{"session_id":"`+id+`"}`, "interrupt_rate_limited")
	backend.errFor["list"] = errors.New("db locked")
	wantAISError(t, app, backend, aisAgent, "list_sessions", `{}`, "internal")
	// Every documented code is accepted.
	for _, code := range mcpAISessionErrorCodes {
		sc := aiSessionError(code, "m", nil).(map[string]any)["structuredContent"].(map[string]any)
		if sc["error_kind"] != code {
			t.Fatalf("%s reported as %v", code, sc["error_kind"])
		}
	}
}

func TestAISessionListFiltersOwnerAndHumanTabs(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	mine := openFakeSession(t, app, backend, aisAgent)
	openFakeSession(t, app, backend, mcpCaller{Agent: "codex"})
	closedID := openFakeSession(t, app, backend, aisAgent)
	mustAIS(t, app, backend, aisAgent, "close_session", `{"session_id":"`+closedID+`"}`)
	backend.listExtra = []mcpAISessionListItem{
		{Owner: aisAgent.Agent, SessionID: "TERM-3", Connection: "web-01", State: "idle"},
		{Owner: aisAgent.Agent, SessionID: "not-an-id", State: "idle"},
	}
	ids := func(out map[string]any) []string {
		var got []string
		for _, s := range out["sessions"].([]any) {
			entry := s.(map[string]any)
			if _, leaked := entry["Owner"]; leaked {
				t.Fatalf("owner leaked: %v", entry)
			}
			got = append(got, entry["session_id"].(string))
		}
		return got
	}
	if got := ids(mustAIS(t, app, backend, aisAgent, "list_sessions", `{}`)); !slices.Equal(got, []string{mine}) {
		t.Fatalf("list = %v, want only %s", got, mine)
	}
	if got := ids(mustAIS(t, app, backend, aisAgent, "list_sessions", `{"include_closed":true}`)); !slices.Equal(got, []string{mine, closedID}) {
		t.Fatalf("list with closed = %v", got)
	}
	// An empty list is an array, not null.
	empty := mustAIS(t, app, backend, mcpCaller{Agent: "nobody"}, "list_sessions", `{}`)
	if s, ok := empty["sessions"].([]any); !ok || len(s) != 0 {
		t.Fatalf("empty list = %v", empty)
	}
}

func TestAISessionReadOutputLongPollSlots(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	id := openFakeSession(t, app, backend, aisAgent)
	taken := 0
	for app.mcpRuntime.acquireWaiter() {
		taken++
	}
	defer func() {
		for range taken {
			app.mcpRuntime.releaseWaiter()
		}
	}()
	out := wantAISError(t, app, backend, aisAgent, "read_output", `{"session_id":"`+id+`","wait_seconds":5}`, "server_busy")
	if out["retry_after_ms"] == nil {
		t.Fatalf("server_busy lacks retry_after_ms: %v", out)
	}
	// Without a wait no slot is needed.
	mustAIS(t, app, backend, aisAgent, "read_output", `{"session_id":"`+id+`"}`)
	// A slot taken for a long-poll is given back.
	app.mcpRuntime.releaseWaiter()
	taken--
	mustAIS(t, app, backend, aisAgent, "read_output", `{"session_id":"`+id+`","wait_seconds":1}`)
	if !app.mcpRuntime.acquireWaiter() {
		t.Fatal("read_output did not release its waiter slot")
	}
	taken++
	if backend.lastRead.waitDuration() != time.Second {
		t.Fatalf("wait = %v", backend.lastRead.waitDuration())
	}
	if backend.readCtx.Err() == nil {
		t.Fatal("the long-poll context must end with the call")
	}
}

func TestAISessionCursorKeepsUint64Precision(t *testing.T) {
	app := newTestApp(t)
	backend := &bigCursorBackend{fakeAISessionBackend: newFakeAISessionBackend()}
	id := openFakeSession(t, app, backend.fakeAISessionBackend, aisAgent)
	out, rpcErr, _ := app.callAISessionToolWith(context.Background(), backend, aisAgent, toolCallParams{Name: "read_output", Arguments: json.RawMessage(`{"session_id":"` + id + `","since_cursor":18446744073709551000}`)})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	payload, _ := json.Marshal(out)
	if !strings.Contains(string(payload), `"cursor":18446744073709551615`) {
		t.Fatalf("cursor lost precision: %s", payload)
	}
	if backend.since != 18446744073709551000 {
		t.Fatalf("since_cursor decoded as %d", backend.since)
	}
}

type bigCursorBackend struct {
	*fakeAISessionBackend
	since uint64
}

func (b *bigCursorBackend) aiSessionMCPReadOutput(ctx context.Context, owner string, args readOutputArgs) (mcpAISessionReadView, error) {
	b.since = *args.SinceCursor
	return mcpAISessionReadView{Cursor: 1<<64 - 1, SessionState: "idle"}, nil
}

func TestAISessionOpenIdempotencyKeySerializes(t *testing.T) {
	app := newTestApp(t)
	backend := newFakeAISessionBackend()
	backend.openDelay = 20 * time.Millisecond
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, rpcErr, _ := app.callAISessionToolWith(context.Background(), backend, aisAgent, toolCallParams{Name: "open_session", Arguments: json.RawMessage(`{"host":"web-01","reason":"x","client_request_id":"same"}`)})
			if rpcErr != nil {
				t.Error(rpcErr)
				return
			}
			ids[i] = out.(map[string]any)["structuredContent"].(map[string]any)["session_id"].(string)
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("one client_request_id opened several sessions: %v", ids)
		}
	}
	if backend.maxOpening != 1 {
		t.Fatalf("opens with one key overlapped (%d)", backend.maxOpening)
	}
	// Different agents with the same key are independent.
	other := mustAIS(t, app, backend, mcpCaller{Agent: "codex"}, "open_session", `{"host":"web-01","reason":"x","client_request_id":"same"}`)
	if other["session_id"] == ids[0] {
		t.Fatal("client_request_id shared across agents")
	}
}

func TestAISessionIDPattern(t *testing.T) {
	// The ID shape the lifecycle code must generate: "AIS-" + base32 of 16
	// random bytes without padding.
	for _, id := range []string{"AIS-" + strings.Repeat("A", 26), "AIS-ABCDEFGHIJKLMNOPQRSTUVWX27"[:30]} {
		if !isAISessionID(id) {
			t.Fatalf("%s rejected", id)
		}
	}
	for _, id := range []string{"", "AIS-", "AIS-" + strings.Repeat("A", 25), "AIS-" + strings.Repeat("A", 27), "AIS-" + strings.Repeat("0", 26), " AIS-" + strings.Repeat("A", 26), "AIS-" + strings.Repeat("A", 25) + "\n"} {
		if isAISessionID(id) {
			t.Fatalf("%q accepted", id)
		}
	}
}
