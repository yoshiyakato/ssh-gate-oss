package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const decidedByGUI = "gui"

var riskRank = map[string]int{"Low": 0, "Medium": 1, "High": 2}

func newCommandRequest(conn Connection, command string, reason string, requestedBy string, risk string, status string) CommandRequest {
	now := time.Now()
	seq := requestIDSeq.Add(1)
	return CommandRequest{
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
	}
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

func (a *App) notifyRequestsChangedLocked() {
	close(a.requestsChanged)
	a.requestsChanged = make(chan struct{})
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
	approvedRisk := classifyRisk(command)
	if command != req.Command && riskRank[approvedRisk] > riskRank[req.Risk] && !allowRiskIncrease {
		return ActionResult{
			OK:      false,
			Code:    "risk_increased",
			Message: fmt.Sprintf("修正したコマンドのリスクが %s → %s に上がりました。内容を確認のうえ、再度承認してください", req.Risk, approvedRisk),
		}
	}
	agentName := strings.TrimSpace(req.RequestedBy)
	if grantBypass {
		if agentName == "" {
			return ActionResult{OK: false, Message: "承認省略にはエージェント名が必要です"}
		}
		if !a.GetMCPSettings().BypassAllowed {
			return ActionResult{OK: false, Code: "bearer_required", Message: "承認省略を使うには、先にMCP設定でBearerトークンを発行してください（名乗りだけでは本人確認ができないため）"}
		}
	}
	conn, connOK := a.connectionByID(req.ConnectionID)
	if !connOK {
		conn, connOK = a.findConnection(req.Host)
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

	bypassNote := ""
	if grantBypass {
		if result := a.setAgentApprovalBypass(agentName, true); result.OK {
			bypassNote = fmt.Sprintf("、%s の承認省略を許可しました", agentName)
		} else {
			bypassNote = "（承認省略の保存に失敗: " + result.Message + "）"
		}
	}

	if !connOK {
		a.finishRequest(running, stateFailed, commandResult{}, errors.New("対象ホストが接続先一覧にありません"))
		return ActionResult{OK: false, Message: "対象ホストが接続先一覧にありません" + bypassNote}
	}
	result, execErr := a.executeSSH(a.appCtx, conn, command, 0)
	if execErr != nil && !isExitError(execErr) {
		a.finishRequest(running, stateFailed, result, execErr)
		return ActionResult{OK: false, Message: fmt.Sprintf("%s の実行に失敗しました: %s%s", id, explainSSHError(conn, execErr), bypassNote)}
	}
	state := stateSucceeded
	if execErr != nil {
		state = stateFailed
	}
	a.finishRequest(running, state, result, execErr)
	return ActionResult{OK: true, Message: fmt.Sprintf("%s を承認し、実行しました%s", id, bypassNote)}
}

// finishRequest records the outcome of a running request. An execution
// stopped by app shutdown is recorded as interrupted whatever state the
// caller picked; one stopped by DisconnectConnection stays failed with
// ErrorKind "canceled".
func (a *App) finishRequest(running CommandRequest, state string, result commandResult, execErr error) CommandRequest {
	from := running.Status
	if errors.Is(execErr, errShuttingDown) {
		state = stateInterrupted
	}
	updated, err := a.transitionRequest(running.ID, []string{from}, func(r *CommandRequest) {
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
				r.Stderr += execErr.Error()
			}
		}
	})
	if err != nil {
		log.Printf("finish request %s: %v", running.ID, err)
	}
	return updated
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

func (a *App) DeleteCommandRequest(id string) ActionResult {
	id = strings.TrimSpace(id)
	req, ok := a.findRequest(id)
	if !ok {
		return ActionResult{OK: false, Message: "削除対象のコマンド履歴が見つかりません"}
	}
	if req.Status == stateRunning || req.Status == stateAutoRunning {
		return ActionResult{OK: false, Message: "実行中のリクエストは削除できません"}
	}
	if a.store != nil {
		if err := a.store.deleteCommandRequest(id); err != nil {
			return ActionResult{OK: false, Message: fmt.Sprintf("%s の削除に失敗しました: %v", id, err)}
		}
	}
	a.mu.Lock()
	next := make([]CommandRequest, 0, len(a.requests))
	for _, r := range a.requests {
		if r.ID != id {
			next = append(next, r)
		}
	}
	a.requests = next
	a.notifyRequestsChangedLocked()
	a.mu.Unlock()
	return ActionResult{OK: true, Message: fmt.Sprintf("%s をコマンド履歴から削除しました", id)}
}

// needsHumanApproval decides whether an agent request must wait for the GUI.
// Approval bypass only applies when the agent authenticated with a Bearer
// token (a self-declared agent_name alone proves nothing), and never to High
// risk or to commands covered by the RequireApproval* switches.
func (a *App) needsHumanApproval(conn Connection, risk string, command string, agentName string, forceQueue bool) bool {
	if forceQueue || !a.agentApprovalBypassEnabled(agentName) {
		return true
	}
	settings := a.GetMCPSettings()
	if !settings.BypassAllowed {
		return true
	}
	if risk == "High" {
		return true
	}
	if settings.RequireApprovalWriteOp && risk != "Low" {
		return true
	}
	if settings.RequireApprovalSudo && containsWord(command, "sudo") {
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

func containsWord(command string, word string) bool {
	for _, field := range strings.FieldsFunc(command, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ';' || r == '|' || r == '&' || r == '(' || r == ')' || r == '\n'
	}) {
		if field == word {
			return true
		}
	}
	return false
}

// executeCommandAndLog runs an approval-bypassed command. The request is
// persisted as auto_running before execution so a crash still leaves an
// audit record (recovered as interrupted on next start).
func (a *App) executeCommandAndLog(ctx context.Context, conn Connection, command string, reason string, requestedBy string, risk string, clientRequestID string, timeout int) (CommandRequest, error) {
	req := newCommandRequest(conn, command, reason, requestedBy, risk, stateAutoRunning)
	req.ExecutedCommand = command
	req.ApprovedRisk = risk
	req.DecidedBy = "bypass:" + requestedBy
	req.DecidedAt = req.RequestedAt
	req.ClientRequestID = clientRequestID
	if err := a.recordCommandRequest(req); err != nil {
		return req, err
	}
	result, execErr := a.executeSSH(ctx, conn, command, timeout)
	state := stateAutoSucceeded
	if execErr != nil {
		state = stateAutoFailed
	}
	return a.finishRequest(req, state, result, execErr), nil
}

func (a *App) queueCommand(conn Connection, command string, reason string, requestedBy string, risk string, clientRequestID string) (CommandRequest, error) {
	req := newCommandRequest(conn, command, reason, requestedBy, risk, statePending)
	req.ClientRequestID = clientRequestID
	return req, a.recordCommandRequest(req)
}

func (a *App) executeSSH(parent context.Context, conn Connection, command string, timeoutOverride int) (commandResult, error) {
	if conn.connectionType() == "Serial" {
		return commandResult{}, errors.New("serial connections are interactive-only")
	}
	timeout := timeoutSeconds(conn.CommandTimeout, 30)
	if timeoutOverride > 0 {
		timeout = time.Duration(min(timeoutOverride, 3600)) * time.Second
	}
	tracked, done := a.trackExec(parent, conn.ID)
	defer done()
	ctx, cancel := context.WithTimeout(tracked, timeout)
	defer cancel()
	a.mu.RLock()
	outputCap := a.mcp.MaxOutputKB * 1024
	strictHostKey := a.mcp.StrictHostKey
	conn.KeyPassphrase = a.sshPassphrases[conn.ID]
	a.mu.RUnlock()
	if outputCap <= 0 {
		outputCap = defaultOutputCap
	}
	res, err := runSSHCommand(ctx, conn, command, outputCap, strictHostKey)
	if err != nil {
		// Carry the cancel cause so finishRequest can tell a disconnect or
		// shutdown apart from a plain timeout or network error.
		// Shutdown wins whatever cause the tracked context carries: an MCP
		// call's r.Context() may have been cancelled by the server closing.
		if cause := context.Cause(tracked); errors.Is(cause, errDisconnected) || errors.Is(cause, errShuttingDown) {
			err = fmt.Errorf("%w: %w", cause, err)
		} else if errors.Is(context.Cause(a.appCtx), errShuttingDown) {
			err = fmt.Errorf("%w: %w", errShuttingDown, err)
		}
	}
	return res, err
}

func isExitError(err error) bool {
	var exitErr *ssh.ExitError
	return errors.As(err, &exitErr)
}

func classifyExecError(err error) string {
	var missing *passphraseMissingError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errDisconnected), errors.Is(err, errShuttingDown):
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
