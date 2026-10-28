package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The acceptance flow of REQ-016 at the backend: cd and env carry over,
// every line is classified and gated, output is readable by cursor.
func TestAISessionExecFlowCarriesState(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	id := e.open(t)

	cd := e.send(t, id, "cd /var/www")
	if cd.State != stateAutoSucceeded || cd.Effect != lineEffectCd || cd.Risk != "Low" || cd.RequiresApproval {
		t.Fatalf("cd = %+v", cd)
	}
	if cd.Cwd != "/var/www" || !cd.CwdPersisted {
		t.Fatalf("cd cwd = %q persisted=%v", cd.Cwd, cd.CwdPersisted)
	}
	env := e.send(t, id, "export LANG=C")
	if env.State != stateAutoSucceeded || env.Effect != lineEffectEnv {
		t.Fatalf("export = %+v", env)
	}
	pwd := e.send(t, id, "pwd")
	if pwd.State != stateAutoSucceeded || !strings.Contains(pwd.Output, "/var/www") {
		t.Fatalf("pwd = %+v", pwd)
	}
	plans := e.factory.last().Plans()
	last := plans[len(plans)-1]
	if last.Cwd != "/var/www" || last.Env["LANG"] != "C" {
		t.Fatalf("pwd ran with cwd=%q env=%v", last.Cwd, last.Env)
	}
	// export never reaches the remote side in exec mode (§1.1).
	if got := e.factory.last().Delivered(); !slices.Equal(got, []string{"cd /var/www", "pwd"}) {
		t.Fatalf("delivered = %v", got)
	}
	read, err := e.app.aiSessionMCPReadOutput(context.Background(), aiTestAgent, readOutputArgs{SessionID: id, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read.Text, "$ cd /var/www\n") || !strings.Contains(read.Text, "$ pwd\n/var/www\n") {
		t.Fatalf("read_output text = %q", read.Text)
	}
	if read.Cwd != "/var/www" || read.Line == nil || read.Line.RequestID != pwd.RequestID {
		t.Fatalf("read view = %+v", read)
	}
	// The line is a command_requests row tied to its session.
	req, _ := e.app.findRequest(pwd.RequestID)
	if req.SessionID != id || req.SessionSeq != 3 {
		t.Fatalf("request session = %q/%d", req.SessionID, req.SessionSeq)
	}
	view := requestView(req, true)
	if view["session_id"] != id || view["session_seq"] != 3 {
		t.Fatalf("requestView = %v", view)
	}
	closed, err := e.app.aiSessionMCPClose(context.Background(), aiTestAgent, id)
	if err != nil || !closed.Closed || closed.CloseReason != aiCloseAgent || closed.Lines != 3 {
		t.Fatalf("close = %+v %v", closed, err)
	}
	// Closed: send is refused, the tombstone can still be read.
	if err := e.sendErr(id, "ls"); aiErrCode(err) != aiErrSessionClosed {
		t.Fatalf("send after close = %v", err)
	}
	if _, err := e.app.aiSessionMCPReadOutput(context.Background(), aiTestAgent, readOutputArgs{SessionID: id, MaxBytes: 10}); err != nil {
		t.Fatalf("tombstone read: %v", err)
	}
}

// Without a bypass grant every line waits for a human, and nothing reaches
// the transport until it is approved (§3.5).
func TestAISessionPendingLineNeverDelivered(t *testing.T) {
	e := newAITestApp(t)
	id := e.open(t)
	view := e.send(t, id, "ls -la")
	if view.State != statePending || !view.RequiresApproval {
		t.Fatalf("send = %+v", view)
	}
	if got := e.factory.last().Delivered(); len(got) != 0 {
		t.Fatalf("pending line delivered: %v", got)
	}
	// One line at a time.
	if err := e.sendErr(id, "pwd"); aiErrCode(err) != aiErrSessionBusy {
		t.Fatalf("second line = %v", err)
	}
	if st := e.app.ListAISessions()[0].State; st != aiStatePendingApproval {
		t.Fatalf("state = %s", st)
	}
	// Editing at approval time is refused (§3.6 step 1).
	if r := e.app.ApproveCommand(view.RequestID, "rm -rf /"); r.OK || r.Code != "edit_not_allowed" {
		t.Fatalf("edited approve = %+v", r)
	}
	if got := e.factory.last().Delivered(); len(got) != 0 {
		t.Fatalf("edited line delivered: %v", got)
	}
	if r := e.app.ApproveCommand(view.RequestID, "ls -la"); !r.OK {
		t.Fatalf("approve = %+v", r)
	}
	if got := e.factory.last().Delivered(); !slices.Equal(got, []string{"ls -la"}) {
		t.Fatalf("delivered = %v", got)
	}
	if st := e.requestStatus(t, view.RequestID); st != stateSucceeded {
		t.Fatalf("status = %s", st)
	}
	// Approving twice does not deliver twice.
	if r := e.app.ApproveCommand(view.RequestID, ""); r.OK {
		t.Fatalf("second approve = %+v", r)
	}
	if n := len(e.factory.last().Delivered()); n != 1 {
		t.Fatalf("delivered %d times", n)
	}
}

func TestAISessionRejectedLineFreesSession(t *testing.T) {
	e := newAITestApp(t)
	id := e.open(t)
	view := e.send(t, id, "ls")
	if r := e.app.RejectCommand(view.RequestID); !r.OK {
		t.Fatalf("reject = %+v", r)
	}
	// The next send_line notices the decision (no callback needed, §3.6).
	next := e.send(t, id, "pwd")
	if next.State != statePending {
		t.Fatalf("next = %+v", next)
	}
	if got := e.factory.last().Delivered(); len(got) != 0 {
		t.Fatalf("delivered: %v", got)
	}
}

// Approve racing reject / close delivers at most once (§3.5, -race).
func TestAISessionApproveRacesDeliverAtMostOnce(t *testing.T) {
	for round := 0; round < 10; round++ {
		e := newAITestApp(t)
		id := e.open(t)
		view := e.send(t, id, "uptime")
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); e.app.ApproveCommand(view.RequestID, "") }()
		go func() { defer wg.Done(); e.app.RejectCommand(view.RequestID) }()
		go func() { defer wg.Done(); e.app.CloseAISession(id) }()
		wg.Wait()
		n := len(e.factory.last().Delivered())
		st := e.requestStatus(t, view.RequestID)
		if n > 1 {
			t.Fatalf("delivered %d times", n)
		}
		if n == 1 && st != stateSucceeded && st != stateFailed && st != stateInterrupted {
			t.Fatalf("delivered but status %s", st)
		}
		if n == 0 && st != stateRejected && st != stateWithdrawn && st != stateFailed {
			t.Fatalf("not delivered, status %s", st)
		}
	}
}

func TestAISessionCloseWithdrawsPendingLine(t *testing.T) {
	e := newAITestApp(t)
	id := e.open(t)
	view := e.send(t, id, "ls")
	if r := e.app.CloseAISession(id); !r.OK {
		t.Fatalf("close = %+v", r)
	}
	if st := e.requestStatus(t, view.RequestID); st != stateWithdrawn {
		t.Fatalf("status = %s", st)
	}
	if r := e.app.ApproveCommand(view.RequestID, ""); r.OK {
		t.Fatalf("approve after close = %+v", r)
	}
	if got := e.factory.last().Delivered(); len(got) != 0 {
		t.Fatalf("delivered: %v", got)
	}
}

func TestAISessionSessionRulesOnlyAddApproval(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t) // agent bypass up to Medium
	id := e.open(t)
	// Medium is queued because the session cap is Low (§3.4).
	medium := e.send(t, id, "export FOO=1")
	if medium.Risk != "Medium" || medium.State != statePending {
		t.Fatalf("export FOO = %+v", medium)
	}
	e.app.RejectCommand(medium.RequestID)
	// High is always queued, bypass or not.
	high := e.send(t, id, "export PATH=/tmp")
	if high.Risk != "High" || high.State != statePending {
		t.Fatalf("export PATH = %+v", high)
	}
	e.app.RejectCommand(high.RequestID)
	for _, line := range []string{"alias ll=ls", "trap x INT", "nohup sleep 1", "tmux"} {
		v := e.send(t, id, line)
		if v.Risk != "High" || v.State != statePending {
			t.Fatalf("%q = %+v", line, v)
		}
		e.app.RejectCommand(v.RequestID)
	}
	if got := e.factory.last().Delivered(); len(got) != 0 {
		t.Fatalf("delivered: %v", got)
	}
	low := e.send(t, id, "ls")
	if low.State != stateAutoSucceeded {
		t.Fatalf("ls = %+v", low)
	}
}

func TestAISessionLineRejectedNeverQueued(t *testing.T) {
	e := newAITestApp(t)
	id := e.open(t)
	for line, reason := range map[string]string{
		"ls\nrm -rf /":            lineRejectMultiline,
		"cat <<EOF":               lineRejectHeredoc,
		"echo 'unclosed":          lineRejectUnbalancedQuote,
		"ls \\":                   lineRejectLineContinuation,
		"":                        lineRejectEmpty,
		"ls \x03":                 lineRejectControlChar,
		strings.Repeat("a", 5000): lineRejectTooLong,
	} {
		err := e.sendErr(id, line)
		me, ok := err.(*mcpAISessionErr)
		if !ok || me.Code != "line_rejected" || me.Extra["reject_reason"] != reason {
			t.Fatalf("%q: %v (%+v)", line, err, me)
		}
	}
	e.app.mu.RLock()
	n := len(e.app.requests)
	e.app.mu.RUnlock()
	if n != 0 {
		t.Fatalf("%d requests recorded for rejected lines", n)
	}
}

func TestAISessionIdempotentSend(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	id := e.open(t)
	wait := 5
	args := sendLineArgs{SessionID: id, Line: "echo hi", ClientRequestID: "same", WaitSeconds: &wait}
	first, err := e.app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller, args)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller, args)
	if err != nil || !again.Deduplicated || again.RequestID != first.RequestID {
		t.Fatalf("resend = %+v %v", again, err)
	}
	if n := len(e.factory.last().Delivered()); n != 1 {
		t.Fatalf("delivered %d times", n)
	}
	args.Line = "echo other"
	if _, err := e.app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller, args); aiErrCode(err) != "idempotency_conflict" {
		t.Fatalf("conflict = %v", err)
	}
}

func TestAISessionTargetChangedClosesSession(t *testing.T) {
	e := newAITestApp(t)
	id := e.open(t)
	view := e.send(t, id, "ls")
	conn := e.conn
	conn.Host = "10.0.0.10"
	conn.Credential = ""
	if r := e.app.SaveConnection(conn); !r.OK {
		t.Fatalf("save: %+v", r)
	}
	if r := e.app.ApproveCommand(view.RequestID, ""); r.OK || r.Code != "target_changed" {
		t.Fatalf("approve = %+v", r)
	}
	if got := e.factory.last().Delivered(); len(got) != 0 {
		t.Fatalf("delivered: %v", got)
	}
	if r := e.app.GetAISession(id); r.Session.CloseReason != aiCloseTargetChanged {
		t.Fatalf("session = %+v", r.Session)
	}
	if st := e.requestStatus(t, view.RequestID); st != stateWithdrawn {
		t.Fatalf("status = %s", st)
	}
}

// AI tools never see human tabs, and other agents' sessions are not theirs.
func TestAISessionIsolation(t *testing.T) {
	e := newAITestApp(t)
	id := e.open(t)
	// A human terminal id is never an AI session: the backend answers
	// session_not_found (the MCP layer already refuses the id shape).
	if err := e.sendErr("TERM-1", "ls"); aiErrCode(err) != aiErrSessionNotFound {
		t.Fatalf("TERM id = %v, want %s", err, aiErrSessionNotFound)
	}
	if got := e.factory.last().Delivered(); len(got) != 0 {
		t.Fatalf("delivered: %v", got)
	}
	if r := e.app.SendTerminalInput(id, "ls\n"); r.OK {
		t.Fatalf("SendTerminalInput on an AI session = %+v", r)
	}
	issueAgentToken(t, e.app, "codex")
	other := mcpCaller{Agent: "codex"}
	wait := 0
	_, err := e.app.aiSessionMCPSendLine(context.Background(), "codex", other, sendLineArgs{SessionID: id, Line: "ls", ClientRequestID: "x", WaitSeconds: &wait})
	if aiErrCode(err) != aiErrNotOwner {
		t.Fatalf("other agent = %v", err)
	}
	if list, _ := e.app.aiSessionMCPList("codex", true); len(list) != 0 {
		t.Fatalf("codex sees %v", list)
	}
	if list, _ := e.app.aiSessionMCPList(aiTestAgent, false); len(list) != 1 || list[0].SessionID != id {
		t.Fatalf("owner sees %v", list)
	}
	// Through the MCP layer: shared token / no token are refused.
	for _, caller := range []mcpCaller{{}, {Shared: true}} {
		out, rpcErr, handled := e.app.callAISessionTool(context.Background(), caller, toolCallParams{Name: "list_sessions", Arguments: json.RawMessage(`{}`)})
		if !handled || rpcErr != nil || !strings.Contains(mustJSON(t, out), "agent_token_required") {
			t.Fatalf("caller %+v: %s", caller, mustJSON(t, out))
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAISessionDisconnectAndDeleteClose(t *testing.T) {
	e := newAITestApp(t)
	id := e.open(t)
	r := e.app.DisconnectConnection(e.conn.ID)
	if !r.OK || !strings.Contains(r.Message, "AI セッション 1 件") {
		t.Fatalf("disconnect = %+v", r)
	}
	if s := e.app.GetAISession(id).Session; s.CloseReason != aiCloseDisconnected {
		t.Fatalf("after disconnect: %+v", s)
	}
	id2 := e.open(t)
	if r := e.app.DeleteConnection(e.conn.ID); !r.OK {
		t.Fatalf("delete = %+v", r)
	}
	if s := e.app.GetAISession(id2).Session; s.CloseReason != aiCloseConnDeleted {
		t.Fatalf("after delete: %+v", s)
	}
}

func TestAISessionInterruptStopsRunningLine(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	id := e.open(t)
	wait := 0
	view, err := e.app.aiSessionMCPSendLine(context.Background(), aiTestAgent, aiTestCaller,
		sendLineArgs{SessionID: id, Line: "tail -f log", ClientRequestID: "t1", WaitSeconds: &wait})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", func() bool { return !e.factory.last().Ready() })
	iv, err := e.app.aiSessionMCPInterrupt(context.Background(), aiTestAgent, id)
	if err != nil || !iv.Interrupted || iv.Line == nil || iv.Line.RequestID != view.RequestID {
		t.Fatalf("interrupt = %+v %v", iv, err)
	}
	waitFor(t, "line done", func() bool { return e.requestStatus(t, view.RequestID) == stateAutoFailed })
	// Rate limit.
	if _, err := e.app.aiSessionMCPInterrupt(context.Background(), aiTestAgent, id); aiErrCode(err) != "interrupt_rate_limited" {
		t.Fatalf("second interrupt = %v", err)
	}
}

func TestAISessionDisabledAndLimits(t *testing.T) {
	e := newAITestApp(t)
	settings := e.app.GetAISessionSettings()
	settings.PerAgent = 1
	if r := e.app.SaveAISessionSettings(settings); !r.OK {
		t.Fatal(r)
	}
	id := e.open(t)
	_, err := e.app.aiSessionMCPOpen(context.Background(), aiTestAgent, aiTestCaller, openSessionArgs{Host: e.conn.Name, Reason: "x", Mode: "exec", Shell: "sh"})
	if me, ok := err.(*mcpAISessionErr); !ok || me.Code != aiErrSessionLimit || me.Extra["scope"] != "per_agent" {
		t.Fatalf("second open = %v", err)
	}
	_, err = e.app.aiSessionMCPOpen(context.Background(), aiTestAgent, aiTestCaller, openSessionArgs{Host: e.conn.Name, Reason: "x", Mode: "shell", Shell: "sh"})
	if aiErrCode(err) != aiErrShellNotAllowed {
		t.Fatalf("shell open = %v", err)
	}
	settings.Enabled = false
	if r := e.app.SaveAISessionSettings(settings); !r.OK {
		t.Fatal(r)
	}
	if s := e.app.GetAISession(id).Session; s.CloseReason != aiCloseDisabled {
		t.Fatalf("after disable: %+v", s)
	}
	out, _, _ := e.app.callAISessionTool(context.Background(), aiTestCaller, toolCallParams{Name: "list_sessions", Arguments: json.RawMessage(`{}`)})
	if !strings.Contains(mustJSON(t, out), "ai_session_disabled") {
		t.Fatalf("disabled list = %s", mustJSON(t, out))
	}
}

// The full MCP path: tools/call JSON in, structuredContent out, and
// get_request_status sees the session.
func TestAISessionMCPRoundTrip(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	call := func(name, args string) map[string]any {
		t.Helper()
		out, rpcErr, handled := e.app.callAISessionTool(context.Background(), aiTestCaller, toolCallParams{Name: name, Arguments: json.RawMessage(args)})
		if !handled || rpcErr != nil {
			t.Fatalf("%s: %v", name, rpcErr)
		}
		var res struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		}
		if err := json.Unmarshal([]byte(mustJSON(t, out)), &res); err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("%s error: %v", name, res.StructuredContent)
		}
		return res.StructuredContent
	}
	open := call("open_session", `{"host":"web-01","reason":"nginx の設定を調べる"}`)
	id := open["session_id"].(string)
	if open["mode"] != "exec" || open["cwd"] != "/home/tester" {
		t.Fatalf("open = %v", open)
	}
	call("send_line", `{"session_id":"`+id+`","line":"cd /etc/nginx","client_request_id":"a"}`)
	sent := call("send_line", `{"session_id":"`+id+`","line":"pwd","client_request_id":"b"}`)
	if !strings.Contains(sent["output"].(string), "/etc/nginx") {
		t.Fatalf("pwd = %v", sent)
	}
	status, _ := e.app.requestStatusTool(context.Background(), aiTestCaller, requestStatusArgs{RequestIDs: []string{sent["request_id"].(string)}})
	if !strings.Contains(mustJSON(t, status), id) {
		t.Fatalf("get_request_status lacks session_id: %s", mustJSON(t, status))
	}
	read := call("read_output", `{"session_id":"`+id+`","since_cursor":0}`)
	if !strings.Contains(read["text"].(string), "$ pwd") {
		t.Fatalf("read = %v", read)
	}
	list := call("list_sessions", `{}`)
	if n := len(list["sessions"].([]any)); n != 1 {
		t.Fatalf("list = %v", list)
	}
	closed := call("close_session", `{"session_id":"`+id+`"}`)
	if closed["close_reason"] != "agent_closed" {
		t.Fatalf("close = %v", closed)
	}
	again := call("close_session", `{"session_id":"`+id+`"}`)
	if again["already_closed"] != true {
		t.Fatalf("second close = %v", again)
	}
}

// Deleting an open session's line is refused; it is audit.
func TestAISessionLineDeleteRefusedWhileOpen(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	id := e.open(t)
	v := e.send(t, id, "ls")
	if r := e.app.DeleteCommandRequest(v.RequestID); r.OK || r.Code != "ai_session_open" {
		t.Fatalf("delete = %+v", r)
	}
	e.app.CloseAISession(id)
	if r := e.app.DeleteCommandRequest(v.RequestID); !r.OK {
		t.Fatalf("delete after close = %+v", r)
	}
}

func TestAISessionLineContextForApprovalScreen(t *testing.T) {
	e := newAITestApp(t)
	e.grantBypass(t)
	id := e.open(t)
	e.send(t, id, "cd /srv")
	e.send(t, id, "export LANG=C")
	v := e.send(t, id, "systemctl restart nginx")
	if v.State != statePending {
		t.Fatalf("restart = %+v", v)
	}
	ctx := e.app.GetAISessionLineContext(v.RequestID)
	if !ctx.Found || ctx.SessionID != id || ctx.Cwd != "/srv" || ctx.Editable || ctx.Agent != aiTestAgent {
		t.Fatalf("context = %+v", ctx)
	}
	if len(ctx.PreviousLines) == 0 || ctx.PreviousLines[0].Line != "cd /srv" {
		t.Fatalf("previous = %+v", ctx.PreviousLines)
	}
	_ = time.Second
}
