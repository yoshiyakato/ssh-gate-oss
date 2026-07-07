package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"go.bug.st/serial"
	"golang.org/x/crypto/ssh"
)

// Wails event names shared by every terminal transport (SSH, serial, ...).
const (
	terminalDataEvent = "terminal:data"
	terminalExitEvent = "terminal:exit"
)

// terminalSession is transport-agnostic: it only needs a way to push user input,
// resize the remote view (optional), and tear everything down. SSH and serial
// both build one of these and reuse SendTerminalInput / ResizeTerminal /
// CloseTerminal unchanged.
type terminalSession struct {
	kind   string
	label  string
	stdin  io.Writer
	resize func(cols, rows int) error
	closer func()
}

func (t *terminalSession) closeQuiet() {
	if t == nil || t.closer == nil {
		return
	}
	t.closer()
}

// ptyEmitter forwards remote output to the frontend xterm via Wails events.
// Bytes are base64-encoded so non-UTF8 output survives JSON event payloads.
type ptyEmitter struct {
	app *App
}

func (e *ptyEmitter) Write(p []byte) (int, error) {
	if e.app != nil && e.app.ctx != nil {
		wruntime.EventsEmit(e.app.ctx, terminalDataEvent, base64.StdEncoding.EncodeToString(p))
	}
	return len(p), nil
}

func (a *App) emitTerminalExit(message string) {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, terminalExitEvent, message)
	}
}

// setTerminalSession swaps in a new active session, closing any previous one.
func (a *App) setTerminalSession(ts *terminalSession) {
	a.termMu.Lock()
	old := a.term
	a.term = ts
	a.termMu.Unlock()
	if old != nil {
		old.closeQuiet()
	}
}

// StartTerminal opens an interactive terminal for the named connection and
// dispatches to SSH or serial based on the connection's type. Any existing
// terminal session is closed first.
func (a *App) StartTerminal(name string) ActionResult {
	conn, ok := a.findConnection(name)
	if !ok {
		return ActionResult{OK: false, Message: "対象の接続先が見つかりません"}
	}
	if conn.connectionType() == "Serial" {
		return a.startSerialSession(conn)
	}
	return a.startSSHSession(conn)
}

// startSSHSession opens an interactive PTY shell over SSH and streams output.
func (a *App) startSSHSession(conn Connection) ActionResult {
	a.mu.RLock()
	strictHostKey := a.mcp.StrictHostKey
	conn.KeyPassphrase = a.sshPassphrases[conn.Name]
	a.mu.RUnlock()

	// The context only bounds the dial + handshake; the client persists after.
	dialCtx, cancel := context.WithTimeout(context.Background(), timeoutSeconds(conn.ConnectTimeout, 10))
	client, err := openSSHClient(dialCtx, conn, strictHostKey)
	cancel()
	if err != nil {
		return ActionResult{OK: false, Message: explainSSHError(conn, err)}
	}

	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return ActionResult{OK: false, Message: "セッションを開けません: " + err.Error()}
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		_ = session.Close()
		_ = client.Close()
		return ActionResult{OK: false, Message: "PTYの確保に失敗しました: " + err.Error()}
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()
		return ActionResult{OK: false, Message: "標準入力を開けません: " + err.Error()}
	}

	emitter := &ptyEmitter{app: a}
	session.Stdout = emitter
	session.Stderr = emitter

	if err := session.Shell(); err != nil {
		_ = session.Close()
		_ = client.Close()
		return ActionResult{OK: false, Message: "シェルを起動できません: " + err.Error()}
	}

	ts := &terminalSession{
		kind:  "ssh",
		label: conn.Name,
		stdin: stdin,
		resize: func(cols, rows int) error {
			return session.WindowChange(rows, cols)
		},
		closer: func() {
			_ = session.Close()
			_ = client.Close()
		},
	}
	a.setTerminalSession(ts)

	go func() {
		_ = session.Wait()
		a.termMu.Lock()
		current := a.term == ts
		if current {
			a.term = nil
		}
		a.termMu.Unlock()
		ts.closeQuiet()
		if current {
			a.emitTerminalExit(fmt.Sprintf("%s のセッションが終了しました", conn.Name))
		}
	}()

	return ActionResult{OK: true, Message: fmt.Sprintf("%s に接続しました", conn.Name)}
}

// ListSerialPorts enumerates the serial ports currently available on the host.
func (a *App) ListSerialPorts() []string {
	ports, err := serial.GetPortsList()
	if err != nil || ports == nil {
		return []string{}
	}
	return ports
}

// startSerialSession opens the connection's serial port and streams its bytes
// to the frontend xterm, reusing the same session plumbing as SSH.
func (a *App) startSerialSession(conn Connection) ActionResult {
	portName := conn.SerialPort
	if portName == "" {
		portName = conn.Host // tolerate device path stored in host
	}
	if portName == "" {
		return ActionResult{OK: false, Message: "シリアルポートが設定されていません"}
	}
	baud := conn.BaudRate
	if baud <= 0 {
		baud = 115200
	}
	port, err := serial.Open(portName, &serial.Mode{BaudRate: baud})
	if err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s を開けません: %v", portName, err)}
	}

	label := fmt.Sprintf("%s @ %d", portName, baud)
	ts := &terminalSession{
		kind:   "serial",
		label:  label,
		stdin:  port,
		resize: nil,
		closer: func() { _ = port.Close() },
	}
	a.setTerminalSession(ts)

	emitter := &ptyEmitter{app: a}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := port.Read(buf)
			if n > 0 {
				_, _ = emitter.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
		a.termMu.Lock()
		current := a.term == ts
		if current {
			a.term = nil
		}
		a.termMu.Unlock()
		ts.closeQuiet()
		if current {
			a.emitTerminalExit(fmt.Sprintf("%s を切断しました", label))
		}
	}()

	return ActionResult{OK: true, Message: fmt.Sprintf("%s に接続しました", label)}
}

// SendTerminalInput writes user keystrokes (UTF-8) to the active session's stdin.
func (a *App) SendTerminalInput(data string) ActionResult {
	a.termMu.Lock()
	ts := a.term
	a.termMu.Unlock()
	if ts == nil {
		return ActionResult{OK: false, Message: "ターミナルが接続されていません"}
	}
	if _, err := ts.stdin.Write([]byte(data)); err != nil {
		return ActionResult{OK: false, Message: "入力の送信に失敗しました: " + err.Error()}
	}
	return ActionResult{OK: true, Message: ""}
}

// ResizeTerminal updates the remote PTY window size (no-op for transports
// without a resize concept, e.g. serial).
func (a *App) ResizeTerminal(cols int, rows int) ActionResult {
	a.termMu.Lock()
	ts := a.term
	a.termMu.Unlock()
	if ts == nil {
		return ActionResult{OK: false, Message: "ターミナルが接続されていません"}
	}
	if ts.resize == nil {
		return ActionResult{OK: true, Message: ""}
	}
	if err := ts.resize(cols, rows); err != nil {
		return ActionResult{OK: false, Message: "サイズ変更に失敗しました: " + err.Error()}
	}
	return ActionResult{OK: true, Message: ""}
}

// CloseTerminal tears down the active session, if any.
func (a *App) CloseTerminal() ActionResult {
	a.termMu.Lock()
	ts := a.term
	a.term = nil
	a.termMu.Unlock()
	if ts != nil {
		ts.closeQuiet()
	}
	return ActionResult{OK: true, Message: "切断しました"}
}
