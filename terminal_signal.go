package main

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"go.bug.st/serial"
	"golang.org/x/crypto/ssh"
)

// REQ-017: signals from the terminal toolbar.
//
// Three ways to deliver one, picked by the sig name:
//   - control characters (INT_CHAR ...): queued on ts.input like keystrokes,
//     so they keep their order with input that has already reached ts.input.
//     Input still waiting in the frontend (pendingInput) is not covered, so
//     the toolbar sends them through xterm instead (signals.js
//     createSignalSender); this path is for callers without a tab entry;
//   - SSH "signal" channel requests (TERM, KILL, HUP, INT): sent with
//     want_reply=true so a server that refuses (or never answers) is reported
//     as signal_unsupported instead of failing silently;
//   - serial BREAK: serial.Port.Break after draining queued output, serialized
//     with writes by writeMu.
//
// Lock rules (design-v1 §5.0): termMu is only held to look the tab up; the
// signaler registry has its own leaf lock (signalers.mu) that is never held
// together with termMu, and no network or port call runs under either.

var (
	// terminalSignalTimeout bounds the wait for the server's reply to a
	// signal request. A var so tests can shorten it.
	terminalSignalTimeout = 3 * time.Second
	// terminalBreakDuration is how long the serial line is held in BREAK.
	terminalBreakDuration = 250 * time.Millisecond
)

type terminalSignalMethod int

const (
	signalByChar terminalSignalMethod = iota
	signalBySSHRequest
	signalBySerialBreak
)

type terminalSignalSpec struct {
	method terminalSignalMethod
	char   string // signalByChar
	name   string // signalBySSHRequest: RFC 4254 §6.9 name, without "SIG"
	label  string // for messages
	kinds  []string
}

var terminalSignals = map[string]terminalSignalSpec{
	"INT_CHAR":  {method: signalByChar, char: "\x03", label: "Ctrl-C", kinds: []string{"ssh", "serial"}},
	"TSTP_CHAR": {method: signalByChar, char: "\x1a", label: "Ctrl-Z", kinds: []string{"ssh", "serial"}},
	"QUIT_CHAR": {method: signalByChar, char: "\x1c", label: `Ctrl-\`, kinds: []string{"ssh", "serial"}},
	"EOF_CHAR":  {method: signalByChar, char: "\x04", label: "Ctrl-D", kinds: []string{"ssh", "serial"}},
	"TERM":      {method: signalBySSHRequest, name: "TERM", label: "SIGTERM", kinds: []string{"ssh"}},
	"KILL":      {method: signalBySSHRequest, name: "KILL", label: "SIGKILL", kinds: []string{"ssh"}},
	"HUP":       {method: signalBySSHRequest, name: "HUP", label: "SIGHUP", kinds: []string{"ssh"}},
	"INT":       {method: signalBySSHRequest, name: "INT", label: "SIGINT", kinds: []string{"ssh"}},
	"BREAK":     {method: signalBySerialBreak, label: "BREAK", kinds: []string{"serial"}},
}

// TerminalSignalNames lists the accepted sig values (for the frontend menu).
func (a *App) TerminalSignalNames() []string {
	return []string{"INT_CHAR", "TSTP_CHAR", "QUIT_CHAR", "EOF_CHAR", "TERM", "KILL", "HUP", "INT", "BREAK"}
}

// terminalSignalSender is the part of *ssh.Session the signal request needs.
type terminalSignalSender interface {
	SendRequest(name string, wantReply bool, payload []byte) (bool, error)
}

// The real types satisfy the interfaces the wiring relies on.
var (
	_ terminalSignalSender = (*ssh.Session)(nil)
	_ serialBreaker        = serial.Port(nil)
)

type terminalSignaler struct {
	sender terminalSignalSender
	busy   bool // a request is waiting for its reply; guarded by signalers.mu
}

// signalers maps an SSH tab to its session. mu is a leaf lock.
var signalers = struct {
	mu sync.Mutex
	m  map[*terminalSession]*terminalSignaler
}{m: map[*terminalSession]*terminalSignaler{}}

// registerTerminalSignaler makes ts's SSH session reachable for signal
// requests. openSSHTerminal calls it once the shell is up (see the wiring
// notes of REQ-017); the entry is dropped when the tab is gone.
func registerTerminalSignaler(ts *terminalSession, sender terminalSignalSender) {
	signalers.mu.Lock()
	signalers.m[ts] = &terminalSignaler{sender: sender}
	signalers.mu.Unlock()
	go func() {
		<-ts.done
		signalers.mu.Lock()
		delete(signalers.m, ts)
		signalers.mu.Unlock()
	}()
}

func signalResult(ok bool, code, message, id string) ActionResult {
	return ActionResult{OK: ok, Code: code, Message: message, SessionID: id}
}

// SendTerminalSignal sends one signal to the tab id. sig is one of
// TerminalSignalNames.
func (a *App) SendTerminalSignal(id string, sig string) ActionResult {
	spec, ok := terminalSignals[sig]
	if !ok {
		return signalResult(false, "invalid_signal", "不明なシグナルです: "+sig, id)
	}
	a.termMu.Lock()
	ts := a.terms[id]
	var kind, status, reason string
	if ts != nil {
		kind, status, reason = ts.kind, ts.status, ts.reason
	}
	a.termMu.Unlock()
	if ts == nil || reason != "" {
		return signalResult(false, "no_session", "ターミナルが接続されていません", id)
	}
	if !slices.Contains(spec.kinds, kind) {
		return signalResult(false, "signal_not_applicable", fmt.Sprintf("%s はこの種類のターミナル（%s）には送れません", spec.label, kind), id)
	}
	switch spec.method {
	case signalByChar:
		return a.queueTerminalControl(ts, spec)
	case signalBySSHRequest:
		if status != terminalStatusOpen {
			return signalResult(false, "not_ready", "ターミナルはまだ接続中です", id)
		}
		return a.sendSSHSignal(ts, spec)
	default:
		if status != terminalStatusOpen {
			return signalResult(false, "not_ready", "ターミナルはまだ接続中です", id)
		}
		return sendSerialBreak(ts)
	}
}

// queueTerminalControl puts the control character on the tab's input queue,
// the same channel SendTerminalInput uses, so ordering with typed input holds.
// A connecting tab queues it like typed input.
func (a *App) queueTerminalControl(ts *terminalSession, spec terminalSignalSpec) ActionResult {
	timer := time.NewTimer(terminalInputTimeout)
	defer timer.Stop()
	select {
	case <-ts.done:
		return signalResult(false, "no_session", "ターミナルが接続されていません", ts.id)
	default:
	}
	select {
	case ts.input <- []byte(spec.char):
		return signalResult(true, "", spec.label+" を送信しました", ts.id)
	case <-ts.done:
		return signalResult(false, "no_session", "ターミナルが接続されていません", ts.id)
	case <-timer.C:
		return signalResult(false, "input_timeout", "入力の送信が詰まっています", ts.id)
	}
}

type signalReply struct {
	ok  bool
	err error
}

// sendSSHSignal sends an RFC 4254 "signal" request with want_reply, waiting
// at most terminalSignalTimeout. ssh.Session.Signal sends want_reply=false,
// which would hide a refusal, hence SendRequest.
func (a *App) sendSSHSignal(ts *terminalSession, spec terminalSignalSpec) ActionResult {
	signalers.mu.Lock()
	entry := signalers.m[ts]
	if entry == nil {
		signalers.mu.Unlock()
		return signalResult(false, "signal_unavailable", "このターミナルではシグナルを送れません", ts.id)
	}
	if entry.busy {
		signalers.mu.Unlock()
		return signalResult(false, "signal_busy", "前のシグナルの応答を待っています", ts.id)
	}
	entry.busy = true
	sender := entry.sender
	signalers.mu.Unlock()

	payload := ssh.Marshal(&struct{ Signal string }{spec.name})
	replies := make(chan signalReply, 1)
	go func() {
		// A server that never replies keeps this goroutine (and busy) until
		// the channel closes; x/crypto then returns an error.
		ok, err := sender.SendRequest("signal", true, payload)
		signalers.mu.Lock()
		entry.busy = false
		signalers.mu.Unlock()
		replies <- signalReply{ok, err}
	}()

	timer := time.NewTimer(terminalSignalTimeout)
	defer timer.Stop()
	select {
	case r := <-replies:
		switch {
		case r.err != nil:
			select {
			case <-ts.done:
				return signalResult(false, "no_session", "ターミナルが接続されていません", ts.id)
			default:
			}
			return signalResult(false, "signal_failed", fmt.Sprintf("%s を送れませんでした: %v", spec.label, r.err), ts.id)
		case !r.ok:
			return signalResult(false, "signal_unsupported", fmt.Sprintf("サーバーが %s を受け付けませんでした（signal リクエストに未対応の可能性があります）", spec.label), ts.id)
		}
		return signalResult(true, "", spec.label+" を送信しました", ts.id)
	case <-ts.done:
		return signalResult(false, "no_session", "ターミナルが接続されていません", ts.id)
	case <-timer.C:
		return signalResult(false, "signal_unsupported", fmt.Sprintf("サーバーから %s への応答がありません（signal リクエストに未対応の可能性があります）", spec.label), ts.id)
	}
}

// serialBreaker is satisfied by go.bug.st/serial's Port (v1.7.1 has Break).
type serialBreaker interface {
	Break(time.Duration) error
}

// sendSerialBreak holds the line in BREAK for terminalBreakDuration. It takes
// writeMu so the break does not cut into a write or a SetMode, and drains
// first: a returned write may still sit in the tty/UART FIFO, and TIOCSBRK
// (SetCommBreak on windows) asserts the break at once without waiting for it.
// The drain is bounded like applyNow's, since a removed device may never
// finish.
func sendSerialBreak(ts *terminalSession) ActionResult {
	h := ts.serial
	if h == nil || h.port == nil {
		return signalResult(false, "break_unsupported", "このポートでは BREAK を送れません", ts.id)
	}
	breaker, ok := h.port.(serialBreaker)
	if !ok {
		return signalResult(false, "break_unsupported", "このポートでは BREAK を送れません", ts.id)
	}
	h.writeMu.Lock()
	drainWithTimeout(h.port, serialDrainTimeout)
	err := breaker.Break(terminalBreakDuration)
	h.writeMu.Unlock()
	if err != nil {
		select {
		case <-ts.done:
			return signalResult(false, "no_session", "ターミナルが接続されていません", ts.id)
		default:
		}
		return signalResult(false, "break_failed", "BREAK を送れませんでした: "+err.Error(), ts.id)
	}
	return signalResult(true, "", "BREAK を送信しました", ts.id)
}
