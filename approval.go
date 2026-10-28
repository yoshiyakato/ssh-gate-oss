package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const decidedByGUI = "gui"

var riskRank = map[string]int{"Low": 0, "Medium": 1, "High": 2}

func newCommandRequest(conn Connection, command string, reason string, requestedBy string, risk string, status string) CommandRequest {
	now := time.Now()
	seq := requestIDSeq.Add(1)
	req := CommandRequest{
		ID:           fmt.Sprintf("REQ-%d-%d", now.UnixMilli(), seq),
		ConnectionID: conn.ID,
		Host:         conn.Name,
		User:         conn.User,
		Command:      command,
		Reason:       reason,
		RequestedBy:  requestedBy,
		RequestedAt:  now.Format(time.RFC3339),
		Risk:         risk,
		Status:       status,
		Duration:     "-",
		TargetFP:     targetFingerprint(conn),
	}
	if status == statePending {
		req.ExpiresAt = now.Add(pendingRequestTTL).Format(time.RFC3339)
	}
	return req
}

// targetFingerprint identifies where a command would run (user@host:port).
// Serial connections have no SSH target and yield "".
func targetFingerprint(conn Connection) string {
	if conn.connectionType() == "Serial" || strings.TrimSpace(conn.Host) == "" {
		return ""
	}
	return conn.User + "@" + hostKeyAddress(conn)
}

// requestExpiresAt is when a pending request expires: ExpiresAt, or for rows
// stored without it, the request time + pendingRequestTTL.
func requestExpiresAt(req CommandRequest) time.Time {
	if t, err := time.Parse(time.RFC3339, req.ExpiresAt); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, req.RequestedAt); err == nil {
		return t.Add(pendingRequestTTL)
	}
	if millis, err := parseRequestIDMillis(req.ID); err == nil {
		return time.UnixMilli(millis).Add(pendingRequestTTL)
	}
	return time.Time{}
}

// recordCommandRequest persists a new request, then publishes it in memory.
func (a *App) recordCommandRequest(req CommandRequest) error {
	if a.store != nil {
		if err := a.store.insertCommandRequest(req); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.requests = append([]CommandRequest{req}, a.requests...)
	a.notifyRequestsChangedLocked()
	a.mu.Unlock()
	return nil
}

// notifyRequestsChangedLocked wakes long-polls. Callers hold a.mu (write).
func (a *App) notifyRequestsChangedLocked() {
	a.reqCond.Broadcast()
}

// requestLookup is one entry of a status query: a copy of the request as it
// is now, or Found=false when the ID is unknown (or was deleted).
type requestLookup struct {
	ID      string
	Found   bool
	Request CommandRequest
}

// lookupRequests returns the requests with the given IDs, in order.
func (a *App) lookupRequests(ids []string) []requestLookup {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lookupRequestsLocked(ids)
}

func (a *App) lookupRequestsLocked(ids []string) []requestLookup {
	out := make([]requestLookup, len(ids))
	for i, id := range ids {
		out[i].ID = id
		for _, req := range a.requests {
			if req.ID == id {
				out[i].Found = true
				out[i].Request = req
				break
			}
		}
	}
	return out
}

// waitForRequests is the get_request_status long-poll. It returns at once
// when wait <= 0 or when any ID is unknown or already final; otherwise it
// blocks until any of them changes state, wait elapses or ctx is done
// (HTTP disconnect), and returns the entries as they are then.
func (a *App) waitForRequests(ctx context.Context, ids []string, wait time.Duration) []requestLookup {
	if wait <= 0 {
		return a.lookupRequests(ids)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	// Taking a.mu before broadcasting means the wake-up cannot slip in
	// between the ctx check below and reqCond.Wait.
	stop := context.AfterFunc(ctx, func() {
		a.mu.Lock()
		a.reqCond.Broadcast()
		a.mu.Unlock()
	})
	defer stop()

	a.mu.Lock()
	defer a.mu.Unlock()
	initial := a.lookupRequestsLocked(ids)
	for _, entry := range initial {
		if !entry.Found || requestIsFinal(entry.Request.Status) {
			return initial
		}
	}
	for {
		current := a.lookupRequestsLocked(ids)
		for i := range current {
			if current[i].Found != initial[i].Found || current[i].Request.Status != initial[i].Request.Status {
				return current
			}
		}
		if ctx.Err() != nil {
			return current
		}
		a.reqCond.Wait()
	}
}

// transitionRequest moves a request from one of `from` to a new state via
// mutate, atomically: the in-memory check and the SQLite compare-and-swap
// happen under a.mu, so two approvals (or approve vs reject) can never both
// win.
func (a *App) transitionRequest(id string, from []string, mutate func(*CommandRequest)) (CommandRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := -1
	for i := range a.requests {
		if a.requests[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return CommandRequest{}, errors.New("request not found")
	}
	current := a.requests[index]
	allowed := false
	for _, state := range from {
		if current.Status == state {
			allowed = true
			break
		}
	}
	if !allowed {
		return current, errStateConflict
	}
	next := current
	mutate(&next)
	if a.store != nil {
		if err := a.store.updateCommandRequestFrom(next, current.Status); err != nil {
			return current, err
		}
	}
	a.requests[index] = next
	a.notifyRequestsChangedLocked()
	return next, nil
}

// resolveRequestTarget finds the connection a request will run on. A request
// that recorded a connection_id runs only there: if that connection was
// deleted it is not re-pointed at whatever now has the same name. Rows from
// before connection ids fall back to the host name.
func (a *App) resolveRequestTarget(req CommandRequest) (Connection, string, bool) {
	if req.ConnectionID != "" {
		conn, ok := a.connectionByID(req.ConnectionID)
		if !ok {
			return Connection{}, "", false
		}
		return conn, "id", true
	}
	conn, ok := a.findConnection(req.Host)
	if !ok {
		return Connection{}, "", false
	}
	return conn, "name", true
}

// ApprovalTarget is what the approval screen shows before Approve: the
// connection the request resolves to now, and whether user@host:port moved
// since the agent asked.
type ApprovalTarget struct {
	RequestID     string      `json:"requestId"`
	Found         bool        `json:"found"`
	ResolvedBy    string      `json:"resolvedBy"`
	ConnID        string      `json:"connId"`
	Name          string      `json:"name"`
	Type          string      `json:"type"`
	RequestedHost string      `json:"requestedHost"`
	RequestedFP   string      `json:"requestedFp"`
	CurrentFP     string      `json:"currentFp"`
	FPChanged     bool        `json:"fpChanged"`
	Executable    bool        `json:"executable"`
	HostKey       HostKeyInfo `json:"hostKey"`
	Message       string      `json:"message"`
}

// GetApprovalTarget resolves a request's execution target for the approval
// screen. It does not touch the network.
func (a *App) GetApprovalTarget(id string) ApprovalTarget {
	id = strings.TrimSpace(id)
	req, ok := a.findRequest(id)
	if !ok {
		return ApprovalTarget{RequestID: id, Message: "リクエストが見つかりません"}
	}
	target := ApprovalTarget{RequestID: req.ID, RequestedHost: req.Host, RequestedFP: req.TargetFP}
	conn, resolvedBy, found := a.resolveRequestTarget(req)
	if !found {
		target.Message = "対象ホストが接続先一覧にありません（削除された可能性があります）"
		return target
	}
	target.Found = true
	target.ResolvedBy = resolvedBy
	target.ConnID = conn.ID
	target.Name = conn.Name
	target.Type = conn.connectionType()
	target.CurrentFP = targetFingerprint(conn)
	target.FPChanged = req.TargetFP != "" && req.TargetFP != target.CurrentFP
	target.Executable = target.Type != "Serial"
	switch {
	case !target.Executable:
		target.Message = "シリアルの接続先ではコマンドを実行できません"
	case target.FPChanged:
		target.Message = fmt.Sprintf("要求後に実行先が変わりました（%s → %s）", req.TargetFP, target.CurrentFP)
	}
	if target.Executable {
		if info, err := a.hostKeyFingerprint(conn.ID); err == nil {
			target.HostKey = info
		}
	}
	return target
}

func (a *App) ApproveCommand(id string, command string) ActionResult {
	return a.approve(id, command, false, false)
}

// ApproveCommandConfirmed approves a human-edited command whose risk is
// higher than the requested one, after the user confirmed it.
func (a *App) ApproveCommandConfirmed(id string, command string) ActionResult {
	return a.approve(id, command, true, false)
}

func (a *App) ApproveCommandAndBypassAgent(id string, command string) ActionResult {
	return a.approve(id, command, false, true)
}

func (a *App) approve(id string, command string, allowRiskIncrease bool, grantBypass bool) ActionResult {
	id = strings.TrimSpace(id)
	command = strings.TrimSpace(command)
	req, ok := a.findRequest(id)
	if !ok {
		return ActionResult{OK: false, Message: "承認対象のリクエストが見つかりません"}
	}
	if command == "" {
		command = req.Command
	}
	if req.Status != statePending {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s は既に処理済みです（%s）", id, req.Status), Code: "not_pending"}
	}
	if !a.clock().Before(requestExpiresAt(req)) {
		a.expireRequest(id)
		return ActionResult{OK: false, Message: fmt.Sprintf("%s は承認期限（%s）を過ぎたため失効しました", id, requestExpiresAt(req).Format(time.RFC3339)), Code: "expired"}
	}
	approvedRisk := classifyRisk(command)
	if command != req.Command && riskRank[approvedRisk] > riskRank[req.Risk] && !allowRiskIncrease {
		return ActionResult{
			OK:      false,
			Code:    "risk_increased",
			Message: fmt.Sprintf("修正したコマンドのリスクが %s → %s に上がりました。内容を確認のうえ、再度承認してください", req.Risk, approvedRisk),
		}
	}
	conn, _, connOK := a.resolveRequestTarget(req)
	// Serial connections are interactive-only: refuse before the request
	// leaves pending (phase4-serial-patch §7.5).
	if connOK && conn.connectionType() == "Serial" {
		return ActionResult{OK: false, Code: "serial_not_supported", Message: "シリアルの接続先では承認・実行できません。この要求は拒否してください"}
	}
	// A request is approved for the user@host:port the agent asked about. If
	// the connection was edited since, refuse; the agent has to ask again.
	if connOK && req.TargetFP != "" && targetFingerprint(conn) != req.TargetFP {
		return ActionResult{
			OK:      false,
			Code:    "target_changed",
			Message: fmt.Sprintf("要求後に実行先が変わったため承認できません（%s → %s）。拒否して、エージェントに再度依頼させてください", req.TargetFP, targetFingerprint(conn)),
		}
	}
	agentName := strings.TrimSpace(req.RequestedBy)
	if grantBypass {
		if agentName == "" {
			return ActionResult{OK: false, Message: "承認省略にはエージェント名が必要です"}
		}
		// The grant is only usable through the agent's own token; without
		// either kind of token there is no way to authenticate the agent.
		if !a.GetMCPSettings().BypassAllowed && !a.agentHasToken(agentName) {
			return ActionResult{OK: false, Code: "bearer_required", Pending: true, Message: "承認省略を使うには、先にMCP設定でBearerトークンまたはエージェント専用トークンを発行してください（名乗りだけでは本人確認ができないため）"}
		}
	}

	// Reserve the run before the request leaves pending so that a
	// DisconnectConnection or shutdown arriving between the transition and
	// the SSH start still cancels it (N2), and shutdown waits for it instead
	// of closing the store under it. The reservation is not counted as
	// running and emits no sessions:changed until the transition has
	// succeeded, so a refused approval never shows up as a running command
	// (merge audit #2). It is released after finishRequest.
	timeout := requestTimeoutOf(req)
	res, ok := a.reserveExec(a.appCtx, conn.ID)
	defer res.release()
	if !ok {
		return ActionResult{OK: false, Message: "終了処理中のため承認できません", Code: "shutting_down", Pending: true}
	}
	if cause := stopCauseOf(res.ctx); cause != nil {
		// Disconnected or shutting down before anything changed: the
		// request stays pending and nothing runs.
		if errors.Is(cause, errShuttingDown) {
			return ActionResult{OK: false, Message: "終了処理中のため承認できません", Code: "shutting_down", Pending: true}
		}
		return ActionResult{OK: false, Message: fmt.Sprintf("%s は承認と同時に切断されたため実行しませんでした（承認待ちのままです）", id), Code: "canceled", Pending: true}
	}

	running, err := a.transitionRequest(id, []string{statePending}, func(r *CommandRequest) {
		r.Status = stateRunning
		r.ExecutedCommand = command
		r.ApprovedRisk = approvedRisk
		r.DecidedBy = decidedByGUI
		r.DecidedAt = nowRFC3339()
		if connOK {
			r.ConnectionID = conn.ID
			r.Host = conn.Name
		}
	})
	if errors.Is(err, errStateConflict) {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s は既に処理済みです（%s）", id, running.Status), Code: "not_pending"}
	}
	if err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s の状態を更新できません: %v", id, err)}
	}
	res.activate()
	execCtx := res.ctx

	bypassNote, bypassState := "", ""
	if grantBypass {
		if result := a.setAgentApprovalBypass(agentName, true); result.OK {
			bypassNote, bypassState = fmt.Sprintf("、%s の承認省略を許可しました", agentName), "granted"
			if !a.agentHasToken(agentName) {
				bypassNote += "（エージェント専用トークンを発行するまでは有効になりません）"
				bypassState = "inactive"
			}
		} else {
			bypassNote, bypassState = "（承認省略の保存に失敗: "+result.Message+"）", "failed"
		}
	}
	// done attaches the bypass outcome so the UI can say it even when it
	// shows its own text for the code.
	done := func(r ActionResult) ActionResult {
		r.Bypass = bypassState
		return r
	}

	if !connOK {
		_, _ = a.finishRequest(running, stateFailed, commandResult{}, errors.New("対象ホストが接続先一覧にありません"))
		return done(ActionResult{OK: false, Message: "対象ホストが接続先一覧にありません" + bypassNote})
	}
	// A disconnect or shutdown that came after the transition: record it
	// and never start the command.
	if cause := stopCauseOf(execCtx); cause != nil {
		_, _ = a.finishRequest(running, stateFailed, commandResult{}, cause)
		if errors.Is(cause, errShuttingDown) {
			return done(ActionResult{OK: false, Message: fmt.Sprintf("%s は終了処理のため中断されました%s", id, bypassNote)})
		}
		return done(ActionResult{OK: false, Code: "canceled", Message: fmt.Sprintf("%s は切断されたため実行しませんでした%s", id, bypassNote)})
	}
	result, execErr := a.executeSSH(execCtx, conn, command, timeout, guiCredentialTimeout)
	if errors.Is(execErr, errShuttingDown) {
		// finishRequest records it as interrupted.
		_, _ = a.finishRequest(running, stateFailed, result, execErr)
		return done(ActionResult{OK: false, Message: fmt.Sprintf("%s は終了処理のため中断されました%s", id, bypassNote)})
	}
	if execErr != nil && !isExitError(execErr) {
		_, _ = a.finishRequest(running, stateFailed, result, execErr)
		return done(ActionResult{OK: false, Message: fmt.Sprintf("%s の実行に失敗しました: %s%s", id, explainSSHError(conn, execErr), bypassNote)})
	}
	state := stateSucceeded
	if execErr != nil {
		state = stateFailed
	}
	if _, err := a.finishRequest(running, state, result, execErr); err != nil {
		if r, ok := finalStateNotSaved(id, err); ok {
			r.Message += bypassNote
			return done(r)
		}
	}
	return done(ActionResult{OK: true, Message: fmt.Sprintf("%s を承認し、実行しました%s", id, bypassNote)})
}

// execErrorMarker starts the local execution error that finishRequest
// appends to stderr.
const execErrorMarker = "[ssh-gete error] "

// agentStderr is the stderr an agent may see. The appended local error can
// name the real host:port, host key fingerprints and local key paths, which
// list_hosts deliberately withholds, so it is replaced by a fixed message per
// error_kind. Rows written before the marker existed are handled by kind: for
// failures before the command ran, all of stderr is the local error.
func agentStderr(stderr string, errorKind string) string {
	if i := strings.LastIndex(stderr, execErrorMarker); i >= 0 {
		return stderr[:i] + execErrorMarker + agentErrorMessage(errorKind)
	}
	switch errorKind {
	case "connect", "auth", "credential_unavailable", "hostkey", "hostkey_unknown":
		return execErrorMarker + agentErrorMessage(errorKind)
	}
	return stderr
}

func agentErrorMessage(errorKind string) string {
	switch errorKind {
	case "connect":
		return "could not connect to the host"
	case "auth":
		return "SSH authentication failed"
	case "credential_unavailable":
		return "the saved password could not be read from the keychain; retry later or ask the human to unlock it"
	case "hostkey":
		return "host key verification failed: the presented key does not match the trusted key"
	case "hostkey_unknown":
		return "host key is not trusted yet; ask the human to connect once from the ssh-gete GUI"
	case "timeout":
		return "command timed out"
	case "canceled":
		return "command was canceled"
	}
	return "execution failed"
}

// finishRequest records the outcome of a running request. An execution
// stopped by app shutdown is recorded as interrupted whatever state the
// caller picked; one stopped by DisconnectConnection stays failed with
// ErrorKind "canceled".
//
// The outcome is always published (memory and UI) even when SQLite refuses
// it (audit #37): the error is then an *errFinalStateNotSaved, which callers
// report as a storage error instead of success.
func (a *App) finishRequest(running CommandRequest, state string, result commandResult, execErr error) (CommandRequest, error) {
	from := running.Status
	if errors.Is(execErr, errShuttingDown) {
		state = stateInterrupted
	}
	updated, err := a.finalizeRequest(running.ID, from, func(r *CommandRequest) {
		r.Status = state
		r.Stdout = result.Stdout
		r.Stderr = result.Stderr
		r.ExitCode = result.ExitCode
		r.FinishedAt = nowRFC3339()
		if result.Duration > 0 {
			r.Duration = result.Duration.Round(100 * time.Millisecond).String()
		}
		if execErr != nil {
			r.ErrorKind = classifyExecError(execErr)
			if !isExitError(execErr) {
				if r.Stderr != "" {
					r.Stderr += "\n"
				}
				// The detail (address, fingerprints, key paths) is for the GUI
				// and the DB; agentStderr replaces it for MCP.
				r.Stderr += execErrorMarker + execErr.Error()
			}
		}
	})
	if err != nil {
		log.Printf("finish request %s: %v", running.ID, err)
	}
	return updated, err
}

// finalStateNotSaved reports err as the GUI's storage error when the outcome
// of an approved run could not be written to SQLite (audit #37).
func finalStateNotSaved(id string, err error) (ActionResult, bool) {
	var notSaved *errFinalStateNotSaved
	if !errors.As(err, &notSaved) {
		return ActionResult{}, false
	}
	return ActionResult{OK: false, Code: "storage", Message: fmt.Sprintf("%s は実行しましたが、結果を保存できませんでした / %s ran but its result could not be saved: %v", id, id, notSaved.err)}, true
}

func (a *App) RejectCommand(id string) ActionResult {
	id = strings.TrimSpace(id)
	if id == "" {
		return ActionResult{OK: false, Message: "拒否対象のリクエストが選択されていません"}
	}
	updated, err := a.transitionRequest(id, []string{statePending}, func(r *CommandRequest) {
		r.Status = stateRejected
		r.DecidedBy = decidedByGUI
		r.DecidedAt = nowRFC3339()
	})
	if errors.Is(err, errStateConflict) {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s は既に処理済みです（%s）", id, updated.Status), Code: "not_pending"}
	}
	if err != nil {
		return ActionResult{OK: false, Message: "拒否対象のリクエストが見つかりません"}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s を拒否しました", id)}
}

// DeleteCommandRequest removes a request that is not running. The status
// check and the delete happen under a.mu and the DELETE is conditional on the
// status, like transitionRequest's CAS, so a delete racing an approval cannot
// drop a request that has just started running (its audit row would vanish).
func (a *App) DeleteCommandRequest(id string) ActionResult {
	id = strings.TrimSpace(id)
	a.mu.Lock()
	defer a.mu.Unlock()
	index := -1
	for i := range a.requests {
		if a.requests[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return ActionResult{OK: false, Message: "削除対象のコマンド履歴が見つかりません"}
	}
	if status := a.requests[index].Status; status == stateRunning || status == stateAutoRunning {
		return ActionResult{OK: false, Message: "実行中のリクエストは削除できません"}
	}
	if a.store != nil {
		err := a.store.deleteCommandRequestUnlessRunning(id)
		if errors.Is(err, errStateConflict) {
			// Running in SQLite, or already gone there; only the latter may
			// still be dropped from memory.
			if _, found, loadErr := a.store.loadCommandRequest(id); loadErr != nil || found {
				return ActionResult{OK: false, Message: "実行中のリクエストは削除できません"}
			}
			err = nil
		}
		if err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("%s の削除に失敗しました: %v", id, err)}
		}
	}
	a.requests = slices.Delete(slices.Clone(a.requests), index, index+1)
	a.notifyRequestsChangedLocked()
	return ActionResult{OK: true, Message: fmt.Sprintf("%s をコマンド履歴から削除しました", id)}
}

// needsHumanApproval decides whether an agent request must wait for the GUI.
// Approval bypass only applies when the caller authenticated with agentName's
// own token (neither a self-declared agent_name nor the shared token proves
// identity), the grant is unexpired and covers the risk and connection, and
// never to High risk or to commands covered by the RequireApproval* switches.
func (a *App) needsHumanApproval(conn Connection, risk string, command string, caller mcpCaller, agentName string, forceQueue bool) bool {
	if forceQueue || risk == "High" || !caller.mayBypassAs(agentName) {
		return true
	}
	if !a.agentBypassCovers(caller.Agent, conn, risk) {
		return true
	}
	settings := a.GetMCPSettings()
	if settings.RequireApprovalWriteOp && risk != "Low" {
		return true
	}
	if settings.RequireApprovalSudo && commandUsesSudo(command) {
		return true
	}
	if settings.RequireApprovalProd {
		for _, tag := range conn.Tags {
			tag = strings.ToLower(strings.TrimSpace(tag))
			if tag == "prod" || tag == "production" || tag == "本番" {
				return true
			}
		}
	}
	return false
}

func (a *App) queueCommand(conn Connection, command string, reason string, requestedBy string, risk string, clientRequestID string) (CommandRequest, error) {
	return a.queueCommandWithTimeout(conn, command, reason, requestedBy, risk, clientRequestID, 0)
}

// queueCommandWithTimeout queues a request whose approved run uses
// timeoutSeconds (1-3600; 0 = the connection's command timeout) (N11). The
// timeout is attached before the request is published, so no approval can
// see the request without it.
func (a *App) queueCommandWithTimeout(conn Connection, command string, reason string, requestedBy string, risk string, clientRequestID string, timeoutSeconds int) (CommandRequest, error) {
	req := newCommandRequest(conn, command, reason, requestedBy, risk, statePending)
	req.ClientRequestID = clientRequestID
	setRequestTimeout(&req, timeoutSeconds)
	return req, a.recordCommandRequest(req)
}

// setRequestTimeout attaches the agent's timeout_seconds (capped at 3600;
// <= 0 = the connection's command timeout) to a request before it is
// recorded. It is stored in command_requests.timeout_seconds, so it survives
// a restart.
func setRequestTimeout(req *CommandRequest, seconds int) {
	if seconds <= 0 {
		req.TimeoutSeconds = 0
		return
	}
	req.TimeoutSeconds = min(seconds, 3600)
}

// requestTimeoutOf is the timeout override (seconds, 0 = connection default)
// an approved run of req uses.
func requestTimeoutOf(req CommandRequest) int {
	return req.TimeoutSeconds
}

// execReservation is an execution registered in a.execs (so
// DisconnectConnection cancels it and shutdown's execWG waits for it) before
// its request becomes running. Until activate it is not added to
// runningTotal and emits no sessions:changed, so a reservation whose
// transition fails is never reported as a running command.
type execReservation struct {
	a      *App
	ctx    context.Context
	key    uint64
	cancel context.CancelCauseFunc
	stop   func() bool
	active atomic.Bool
	once   sync.Once
	held   bool
}

// reserveExec registers an execution without counting it (activate counts
// it; beginExec does both). ok is false once shutdown has begun; release
// must still be called (it is a no-op then).
func (a *App) reserveExec(parent context.Context, connID string) (*execReservation, bool) {
	ctx, cancel := context.WithCancelCause(parent)
	// Parents not derived from appCtx still stop on shutdown.
	stop := context.AfterFunc(a.appCtx, func() { cancel(errShuttingDown) })
	r := &execReservation{a: a, ctx: ctx, cancel: cancel, stop: stop}
	a.execMu.Lock()
	if a.execsClosed || a.appCtx.Err() != nil {
		a.execMu.Unlock()
		stop()
		cancel(errShuttingDown)
		return r, false
	}
	a.execSeq++
	r.key = a.execSeq
	a.execs[r.key] = runningExec{connID: connID, cancel: cancel}
	a.execWG.Add(1)
	a.execMu.Unlock()
	r.held = true
	return r, true
}

// activate counts the reserved run as running, once its request has become
// running.
func (r *execReservation) activate() {
	if !r.held || !r.active.CompareAndSwap(false, true) {
		return
	}
	r.a.execMu.Lock()
	if exec, found := r.a.execs[r.key]; found {
		exec.active = true
		r.a.execs[r.key] = exec
	}
	r.a.execMu.Unlock()
	r.a.runningTotal.Add(1)
	r.a.sessionsChanged()
}

// release ends the run (extra calls are ignored). An activated run is
// uncounted with a sessions:changed; a never-activated one leaves silently.
func (r *execReservation) release() {
	r.once.Do(func() {
		if !r.held {
			return
		}
		r.stop()
		r.a.execMu.Lock()
		delete(r.a.execs, r.key)
		r.a.execMu.Unlock()
		r.cancel(context.Canceled)
		wasActive := r.active.Swap(false)
		if wasActive {
			r.a.runningTotal.Add(-1)
		}
		r.a.execWG.Done()
		if wasActive {
			r.a.sessionsChanged()
		}
	})
}

// stopCauseOf returns why ssh-gete stopped ctx (disconnect, shutdown, gone
// client), or nil while it runs or for any other end.
func stopCauseOf(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if isStopCause(cause) {
		return cause
	}
	return nil
}

// credTimeout bounds the keychain read of a stored password
// (guiCredentialTimeout for a GUI approval, mcpCredentialTimeout for an
// agent's direct run).
func (a *App) executeSSH(parent context.Context, conn Connection, command string, timeoutOverride int, credTimeout time.Duration) (commandResult, error) {
	if conn.connectionType() == "Serial" {
		return commandResult{}, errors.New("serial connections are interactive-only")
	}
	credCtx, credCancel := context.WithTimeout(parent, credTimeout)
	conn, err := a.resolveCredential(credCtx, conn)
	credCancel()
	if err != nil {
		return commandResult{}, err
	}
	timeout := timeoutSeconds(conn.CommandTimeout, 30)
	if timeoutOverride > 0 {
		timeout = time.Duration(min(timeoutOverride, 3600)) * time.Second
	}
	// parent is the tracked context from beginExec: its cancel cause says why
	// a run was stopped.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	a.mu.RLock()
	outputCap := a.mcp.MaxOutputKB * 1024
	strictHostKey := a.mcp.StrictHostKey
	if conn.KeyPassphrase == "" {
		conn.KeyPassphrase = a.sshPassphrases[conn.ID]
	}
	a.mu.RUnlock()
	if outputCap <= 0 {
		outputCap = defaultOutputCap
	}
	res, err := runSSHCommand(ctx, conn, command, outputCap, strictHostKey)
	if err != nil && parent.Err() != nil && !isExitError(err) {
		// Carry the cancel cause so finishRequest can tell a disconnect, a
		// gone MCP client or shutdown apart from a plain timeout or network
		// error. Shutdown wins whatever cause the context carries.
		cause := context.Cause(parent)
		if errors.Is(context.Cause(a.appCtx), errShuttingDown) {
			cause = errShuttingDown
		}
		if isStopCause(cause) {
			err = fmt.Errorf("%w: %w", cause, err)
		}
	}
	return res, err
}

// isStopCause reports whether err is one of the reasons ssh-gete itself
// cancels an execution.
func isStopCause(err error) bool {
	return errors.Is(err, errDisconnected) || errors.Is(err, errShuttingDown) || errors.Is(err, errClientGone)
}

func isExitError(err error) bool {
	var exitErr *ssh.ExitError
	return errors.As(err, &exitErr)
}

func classifyExecError(err error) string {
	var missing *passphraseMissingError
	var credErr *credentialError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &credErr):
		return "credential_unavailable"
	case isStopCause(err):
		return "canceled"
	case isExitError(err):
		return "exit_nonzero"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &missing):
		return "auth"
	}
	if kind := hostKeyErrorKind(err); kind != "" {
		return kind
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "unable to authenticate"):
		return "auth"
	case strings.Contains(lower, "host key"), strings.Contains(lower, "knownhosts"):
		return "hostkey"
	}
	return "connect"
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

// findRequestByClientID returns an existing request with the same idempotency
// key from the same agent within the last 24 hours.
func (a *App) findRequestByClientID(agentName string, clientRequestID string) (CommandRequest, bool) {
	if clientRequestID == "" {
		return CommandRequest{}, false
	}
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, req := range a.requests {
		if req.RequestedBy != agentName || req.ClientRequestID != clientRequestID {
			continue
		}
		if millis, err := parseRequestIDMillis(req.ID); err == nil && millis >= cutoff {
			return req, true
		}
	}
	return CommandRequest{}, false
}

// expireRequest moves a pending request past its deadline to expired.
func (a *App) expireRequest(id string) {
	_, err := a.transitionRequest(id, []string{statePending}, func(r *CommandRequest) {
		r.Status = stateExpired
		r.DecidedAt = nowRFC3339()
		r.FinishedAt = r.DecidedAt
	})
	if err != nil && !errors.Is(err, errStateConflict) {
		log.Printf("expire request %s: %v", id, err)
	}
}

// expirePendingRequests expires every pending request past its deadline.
func (a *App) expirePendingRequests() {
	now := a.clock()
	a.mu.RLock()
	var overdue []string
	for _, req := range a.requests {
		if req.Status == statePending && !now.Before(requestExpiresAt(req)) {
			overdue = append(overdue, req.ID)
		}
	}
	a.mu.RUnlock()
	for _, id := range overdue {
		a.expireRequest(id)
	}
}

// expireAgentPolicies switches off lapsed grants, in SQLite first, then in
// memory. Lapsed grants are already ignored at decision time
// (policyActiveAt); this keeps the stored state and the list in line.
func (a *App) expireAgentPolicies() {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	now := a.clock()
	if a.store != nil {
		if _, err := a.store.expireAgentPolicies(now); err != nil {
			log.Printf("expire approval bypass: %v", err)
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, policy := range a.agentPolicies {
		// A grant renewed meanwhile is active again and left alone.
		if policy.ApprovalBypass && !policyActiveAt(policy, now) {
			policy.ApprovalBypass = false
			policy.UpdatedAt = now.Format(time.RFC3339)
			a.agentPolicies[name] = policy
		}
	}
}

func (a *App) expireOverdue() {
	a.expirePendingRequests()
	a.expireAgentPolicies()
}

// runExpirySweeper expires requests and grants until ctx is cancelled.
func (a *App) runExpirySweeper(ctx context.Context) {
	defer a.wg.Done()
	ticker := time.NewTicker(expirySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.expireOverdue()
		}
	}
}
