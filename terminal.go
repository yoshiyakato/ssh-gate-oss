package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
	"golang.org/x/crypto/ssh"
)

// Output coalescing and input limits. Package variables so tests can shrink
// or stretch them.
var (
	terminalFlushInterval = 16 * time.Millisecond
	terminalFlushBytes    = 32 * 1024
	terminalPendingCap    = 4 * 1024 * 1024
	terminalInputTimeout  = 2 * time.Second
	// terminalInputFailGrace is how long a failed stdin write waits for the
	// session's own end (remote_exit / conn_lost from session.Wait, after the
	// output copies finish) before the pump reports io_error itself.
	terminalInputFailGrace = 3 * time.Second
)

const (
	terminalInputQueue = 64
	terminalInputMax   = 64 * 1024
	terminalMaxSize    = 1000

	terminalStatusConnecting = "connecting"
	terminalStatusOpen       = "open"

	exitReasonUser          = "user"
	exitReasonRemoteExit    = "remote_exit"
	exitReasonConnLost      = "conn_lost"
	exitReasonDeviceRemoved = "device_removed"
	exitReasonIOError       = "io_error"
)

// terminalIDPattern matches the frontend-issued tab id (TERM-<uuid>).
var terminalIDPattern = regexp.MustCompile(`^TERM-[A-Za-z0-9-]{1,64}$`)

// terminalState is embedded in App; the maps are guarded by termMu.
type terminalState struct {
	termMu       sync.Mutex
	terms        map[string]*terminalSession // key: session id
	serialOwners map[string]string           // serialReservationKey -> session id
	lastErr      map[string]string           // connID -> last abnormal end / failed start
	// serialGone remembers, per connID, a device_removed lastErr (recorded
	// only after a probe found the device absent). Any presence probe
	// that starts after `at` and finds the device means it was plugged back
	// in, even when watchSerialPresence never saw it missing (an unplug and
	// replug inside one serialPresenceInterval).
	serialGone map[string]serialGoneMark

	// terminalLoops counts the input pumps and serial read loops, so tests
	// can wait for them before restoring package variables.
	terminalLoops sync.WaitGroup
	serialPresenceCache
}

// ensureMaps lazily creates the maps (NewApp does not know about them).
// Callers hold termMu.
func (s *terminalState) ensureMaps() {
	if s.terms == nil {
		s.terms = map[string]*terminalSession{}
	}
	if s.serialOwners == nil {
		s.serialOwners = map[string]string{}
	}
	if s.lastErr == nil {
		s.lastErr = map[string]string{}
	}
	if s.serialGone == nil {
		s.serialGone = map[string]serialGoneMark{}
	}
}

// serialGoneMark is one serialGone entry; it only counts while lastErr still
// holds message.
type serialGoneMark struct {
	message string
	at      time.Time
}

// setSerialGoneLocked records that lastErr[connID] = message means the
// device was absent. Callers hold termMu.
func (s *terminalState) setSerialGoneLocked(connID, message string) {
	s.serialGone[connID] = serialGoneMark{message: message, at: time.Now()}
}

// terminalRuntime aggregates the live tabs of one connection.
type terminalRuntime struct {
	Open       int
	Connecting int
	Since      time.Time // earliest startedAt among open tabs; zero if none
	LastError  string
}

// TerminalInfo is the frontend-visible view of one live terminal tab.
type TerminalInfo struct {
	ID        string `json:"id"`
	ConnID    string `json:"connId"`
	ConnName  string `json:"connName"`
	Kind      string `json:"kind"`           // "ssh" | "serial"
	Port      string `json:"port,omitempty"` // serial only, as configured (not normalized)
	Baud      int    `json:"baud,omitempty"` // serial only
	StartedAt string `json:"startedAt"`      // RFC3339
	Status    string `json:"status"`         // "connecting" | "open"
	// SerialMode is the line format in effect, e.g. "8N1" (serial only). It
	// changes when settings are applied in place.
	SerialMode string `json:"serialMode,omitempty"`
}

// terminalSession is one tab. SSH and serial both build one; it owns its
// ssh.Client / serial port, and only close (via closer) may release them.
// Fields below the line are guarded by termMu.
type terminalSession struct {
	id, connID, connName, kind, port string
	normPort                         string        // serial only; serialReservationKey, key in serialOwners
	serial                           *serialHandle // serial only
	baud                             int           // guarded by termMu (applySerialSettings changes it)
	startedAt                        time.Time
	cancel                           context.CancelFunc
	// resizeMu serializes the PTY window changes of this tab, so the size
	// applied last is the latest one ResizeTerminal stored (#38, N22).
	resizeMu    sync.Mutex
	input       chan []byte
	out         *outputBuffer
	closeOnce   sync.Once
	doneOnce    sync.Once
	done        chan struct{} // closed once the tab is gone
	stdinWriter io.Writer     // set before open; only pumpTerminalInput writes

	status     string
	cols, rows int // latest size; applied after the PTY exists
	reason     string
	resize     func(cols, rows int) error
	closer     func()
}

// serialHandle owns the open port of one serial tab.
type serialHandle struct {
	// port is set once, before the tab turns open; read only after that.
	port serialPort
	// applyMu serializes applyNow calls of this tab. Taken before termMu and
	// writeMu, never while holding either.
	applyMu sync.Mutex
	// writeMu is a leaf lock: it is held only around port.Write, port.Drain
	// and port.SetMode, so input and an in-place SetMode never overlap.
	writeMu sync.Mutex
	// closed is guarded by writeMu. Once set, no Write / Drain / SetMode
	// reaches the port: go.bug.st/serial's unix Write and ioctls use the raw
	// fd without checking that the port is open, so after Close they would
	// hit whatever file or socket reused the fd number (N1).
	closed   bool
	settings serialSettings  // guarded by termMu: what the port is set to
	pending  *serialSettings // guarded by termMu: applied while connecting
}

// errSerialPortClosed is returned by a serialHandle after closePort.
var errSerialPortClosed = io.ErrClosedPipe

// Write is the serial tab's stdin; only pumpTerminalInput calls it.
func (h *serialHandle) Write(p []byte) (int, error) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if h.closed {
		return 0, errSerialPortClosed
	}
	return h.port.Write(p)
}

// closePort marks the handle closed under writeMu (waiting for a write or
// SetMode in progress, which still uses the valid fd) and then closes the
// port. Every later Write / applyNow fails with errSerialPortClosed. Safe to
// call more than once.
func (h *serialHandle) closePort() {
	h.writeMu.Lock()
	already := h.closed
	h.closed = true
	h.writeMu.Unlock()
	if !already && h.port != nil {
		_ = h.port.Close()
	}
}

func (t *terminalSession) markDone() {
	t.doneOnce.Do(func() { close(t.done) })
}

// outputBuffer coalesces remote output: it flushes up to terminalFlushInterval
// after the first pending byte, or as soon as terminalFlushBytes are pending.
// At most terminalPendingCap bytes wait; beyond that the oldest are dropped and
// counted. mu is a leaf lock: emit is always called without it.
type outputBuffer struct {
	mu      sync.Mutex
	buf     []byte
	dropped int
	closed  bool

	wake    chan struct{}
	full    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	emit    func(data []byte, dropped int)
}

func newOutputBuffer(emit func(data []byte, dropped int)) *outputBuffer {
	b := &outputBuffer{
		wake:    make(chan struct{}, 1),
		full:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		emit:    emit,
	}
	go b.run()
	return b
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return n, nil
	}
	limit := terminalPendingCap
	if len(p) >= limit {
		b.dropped += len(b.buf) + len(p) - limit
		b.buf = append(b.buf[:0], p[len(p)-limit:]...)
	} else {
		if over := len(b.buf) + len(p) - limit; over > 0 {
			copy(b.buf, b.buf[over:])
			b.buf = b.buf[:len(b.buf)-over]
			b.dropped += over
		}
		b.buf = append(b.buf, p...)
	}
	pending := len(b.buf)
	b.mu.Unlock()
	signal(b.wake)
	if pending >= terminalFlushBytes {
		signal(b.full)
	}
	return n, nil
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// pending reports the unsent byte count (tests).
func (b *outputBuffer) pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

func (b *outputBuffer) run() {
	defer close(b.stopped)
	for {
		select {
		case <-b.wake:
		case <-b.stop:
			b.drain()
			return
		}
		timer := time.NewTimer(terminalFlushInterval)
		select {
		case <-timer.C:
		case <-b.full:
		case <-b.stop:
		}
		timer.Stop()
		b.drain()
		select {
		case <-b.full:
		default:
		}
	}
}

// drain sends everything pending in chunks of at most terminalFlushBytes.
func (b *outputBuffer) drain() {
	for {
		b.mu.Lock()
		if len(b.buf) == 0 && b.dropped == 0 {
			b.mu.Unlock()
			return
		}
		size := min(len(b.buf), terminalFlushBytes)
		chunk := append([]byte(nil), b.buf[:size]...)
		rest := copy(b.buf, b.buf[size:])
		b.buf = b.buf[:rest]
		dropped := b.dropped
		b.dropped = 0
		b.mu.Unlock()
		b.emit(chunk, dropped)
	}
}

// close stops accepting output and returns once the rest has been sent.
func (b *outputBuffer) close() {
	b.mu.Lock()
	already := b.closed
	b.closed = true
	b.mu.Unlock()
	if !already {
		close(b.stop)
	}
	<-b.stopped
}

// StartTerminal opens a new tab with the frontend-issued id for the saved
// connection connID, dispatching to SSH or serial. Other tabs are untouched.
func (a *App) StartTerminal(id string, connID string, cols int, rows int) ActionResult {
	if !terminalIDPattern.MatchString(id) {
		return ActionResult{OK: false, Code: "invalid_id", Message: "ターミナルIDが不正です", SessionID: id}
	}
	conn, ok := a.connectionByID(connID)
	if !ok {
		return ActionResult{OK: false, Code: "not_found", Message: "対象の接続先が見つかりません", SessionID: id, ID: connID}
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	cols, rows = min(cols, terminalMaxSize), min(rows, terminalMaxSize)

	a.mu.RLock()
	strictHostKey := a.mcp.StrictHostKey
	conn.KeyPassphrase = a.sshPassphrases[conn.ID]
	a.mu.RUnlock()

	ctx, cancel := context.WithCancel(context.Background())
	ts := &terminalSession{
		id:        id,
		connID:    conn.ID,
		connName:  conn.Name,
		kind:      "ssh",
		startedAt: time.Now(),
		cancel:    cancel,
		input:     make(chan []byte, terminalInputQueue),
		done:      make(chan struct{}),
		status:    terminalStatusConnecting,
		cols:      cols,
		rows:      rows,
	}
	if conn.connectionType() == "Serial" {
		ts.kind = "serial"
		ts.port = serialPortOf(conn)
		if ts.port == "" {
			cancel()
			return ActionResult{OK: false, Code: "invalid_serial_mode", Message: "シリアルポートが設定されていません", SessionID: id, ID: conn.ID}
		}
		// Validate before reserving or opening: on windows a rejected mode
		// costs the port (phase4-serial-patch §2.3, L7).
		settings := serialSettingsOf(conn)
		if err := validateSerialSettings(serialGOOS, settings); err != nil {
			cancel()
			return ActionResult{OK: false, Code: "invalid_serial_mode", Message: err.Error(), SessionID: id, ID: conn.ID}
		}
		ts.normPort = serialReservationKey(serialGOOS, ts.port) // stats the path: outside termMu
		ts.baud = settings.Baud
		ts.serial = &serialHandle{settings: settings}
	}

	a.termMu.Lock()
	a.ensureMaps()
	if _, exists := a.terms[id]; exists {
		a.termMu.Unlock()
		cancel()
		return ActionResult{OK: false, Code: "duplicate_id", Message: "同じIDのターミナルが既にあります", SessionID: id, ID: conn.ID}
	}
	if ts.kind == "serial" {
		if owner, held := a.serialOwners[ts.normPort]; held {
			message := fmt.Sprintf("%s は別のターミナルで使用中です", ts.port)
			if other := a.terms[owner]; other != nil {
				message = fmt.Sprintf("%s は「%s」のターミナルで使用中です", ts.port, other.connName)
				if other.port != ts.port {
					message += fmt.Sprintf("（%s と同じデバイス）", other.port)
				}
			}
			a.termMu.Unlock()
			cancel()
			return ActionResult{OK: false, Code: "port_in_use", Message: message, SessionID: id, ID: conn.ID}
		}
		a.serialOwners[ts.normPort] = id
	}
	a.terms[id] = ts
	a.termMu.Unlock()
	ts.out = newOutputBuffer(func(data []byte, dropped int) {
		a.emitEvent(terminalDataEvent, terminalDataPayload{ID: id, Data: base64.StdEncoding.EncodeToString(data), Dropped: dropped})
	})
	a.sessionsChanged()

	var err error
	var message string
	if ts.kind == "serial" {
		message, err = a.openSerialTerminal(ctx, ts)
	} else {
		message, err = a.openSSHTerminal(ctx, ts, conn, strictHostKey)
	}
	if err != nil {
		return a.failTerminalStart(ctx, ts, conn, message, err)
	}
	if ts.kind == "serial" {
		a.termMu.Lock()
		settings := ts.serial.settings
		a.termMu.Unlock()
		return ActionResult{OK: true, Message: fmt.Sprintf("%s @ %d %s に接続しました", ts.port, settings.Baud, settings.label()), SessionID: id, ID: conn.ID}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%s に接続しました", conn.Name), SessionID: id, ID: conn.ID}
}

// errTerminalCanceled marks a start aborted by CloseTerminal while connecting.
var errTerminalCanceled = errors.New("terminal start canceled")

// failTerminalStart drops the placeholder and reservation. No terminal:exit is
// sent: the tab never opened.
func (a *App) failTerminalStart(ctx context.Context, ts *terminalSession, conn Connection, message string, err error) ActionResult {
	canceled := errors.Is(err, errTerminalCanceled) || ctx.Err() != nil
	var serialErr *serialStartError
	isSerialErr := errors.As(err, &serialErr)
	ts.cancel()
	a.termMu.Lock()
	a.ensureMaps()
	if !canceled && ts.reason == exitReasonUser {
		canceled = true
	}
	a.dropTerminalLocked(ts)
	if !canceled {
		a.lastErr[ts.connID] = message
	}
	a.termMu.Unlock()
	ts.out.close()
	ts.markDone()
	a.sessionsChanged()

	result := ActionResult{OK: false, Message: message, SessionID: ts.id, ID: conn.ID}
	var missing *passphraseMissingError
	var credErr *credentialError
	switch {
	case canceled:
		result.Code = "canceled"
		result.Message = "接続を中止しました"
	case errors.As(err, &missing):
		result.Code = "needs_passphrase"
	case errors.As(err, &credErr):
		result.Code = credErr.code
	case isSerialErr:
		result.Code = serialErr.code
	default:
		result.Code = "connect_failed"
	}
	return result
}

// dropTerminalLocked removes ts from the maps if it is still registered.
// Callers hold termMu.
func (a *App) dropTerminalLocked(ts *terminalSession) {
	if a.terms[ts.id] == ts {
		delete(a.terms, ts.id)
	}
	if ts.normPort != "" && a.serialOwners[ts.normPort] == ts.id {
		delete(a.serialOwners, ts.normPort)
	}
}

// commitTerminal flips a connecting tab to open, unless it was closed while
// connecting. It returns the size to apply if it changed meanwhile.
func (a *App) commitTerminal(ts *terminalSession, resize func(cols, rows int) error, closer func()) (cols, rows int, ok bool) {
	a.termMu.Lock()
	if ts.reason == exitReasonUser || a.terms[ts.id] != ts {
		a.termMu.Unlock()
		return 0, 0, false
	}
	ts.status = terminalStatusOpen
	ts.startedAt = time.Now()
	ts.resize = resize
	ts.closer = closer
	cols, rows = ts.cols, ts.rows
	a.ensureMaps()
	delete(a.lastErr, ts.connID)
	a.terminalLoops.Add(1)
	a.termMu.Unlock()
	go func() {
		defer a.terminalLoops.Done()
		a.pumpTerminalInput(ts)
	}()
	a.sessionsChanged()
	return cols, rows, true
}

// openSSHTerminal dials a dedicated client and starts a PTY shell. On error it
// returns the user-facing message.
func (a *App) openSSHTerminal(ctx context.Context, ts *terminalSession, conn Connection, strictHostKey bool) (string, error) {
	// The keychain read is cancelled with the tab (CloseTerminal while it
	// waits for an unlock dialog).
	credCtx, credCancel := context.WithTimeout(ctx, guiCredentialTimeout)
	conn, err := a.resolveCredential(credCtx, conn)
	credCancel()
	if err != nil {
		if ctx.Err() != nil {
			return "", errTerminalCanceled
		}
		return explainSSHError(conn, err), err
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, timeoutSeconds(conn.ConnectTimeout, 10))
	client, err := openSSHClient(dialCtx, conn, strictHostKey)
	dialCancel()
	if err != nil {
		if ctx.Err() != nil {
			return "", errTerminalCanceled
		}
		return explainSSHError(conn, err), err
	}
	// Closing the client aborts NewSession/RequestPty/Shell if the tab is
	// closed while they run.
	stopWatch := context.AfterFunc(ctx, func() { _ = client.Close() })
	fail := func(prefix string, err error) (string, error) {
		stopWatch()
		_ = client.Close()
		if ctx.Err() != nil {
			return "", errTerminalCanceled
		}
		return prefix + err.Error(), err
	}

	session, err := newSessionContext(ctx, client, connectTimeout(conn))
	if err != nil {
		return fail("セッションを開けません: ", err)
	}
	a.termMu.Lock()
	cols, rows := ts.cols, ts.rows
	a.termMu.Unlock()
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		return fail("PTYの確保に失敗しました: ", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return fail("標準入力を開けません: ", err)
	}
	// Wire output before Shell so the MOTD is not lost; the frontend has
	// already registered the id.
	session.Stdout = ts.out
	session.Stderr = ts.out
	if err := session.Shell(); err != nil {
		return fail("シェルを起動できません: ", err)
	}
	if !stopWatch() {
		_ = client.Close()
		return "", errTerminalCanceled
	}

	ts.stdinWriter = stdin
	_, _, ok := a.commitTerminal(ts,
		func(cols, rows int) error { return session.WindowChange(rows, cols) },
		func() {
			_ = session.Close()
			_ = client.Close()
		})
	if !ok {
		_ = session.Close()
		_ = client.Close()
		return "", errTerminalCanceled
	}
	// A ResizeTerminal may have stored a size while connecting. Read it
	// under resizeMu so a concurrent ResizeTerminal cannot be overtaken by
	// this older value.
	ts.resizeMu.Lock()
	a.termMu.Lock()
	nowCols, nowRows := ts.cols, ts.rows
	a.termMu.Unlock()
	if nowCols != cols || nowRows != rows {
		_ = session.WindowChange(nowRows, nowCols)
	}
	ts.resizeMu.Unlock()

	go func() {
		err := session.Wait()
		var exitErr *ssh.ExitError
		switch {
		case err == nil:
			code := 0
			a.finishTerminal(ts, exitReasonRemoteExit, &code, fmt.Sprintf("%s のセッションが終了しました", ts.connName))
		case errors.As(err, &exitErr):
			code := exitErr.ExitStatus()
			a.finishTerminal(ts, exitReasonRemoteExit, &code, fmt.Sprintf("%s のセッションが終了しました", ts.connName))
		default:
			a.finishTerminal(ts, exitReasonConnLost, nil, fmt.Sprintf("%s との接続が切れました", ts.connName))
		}
	}()
	return "", nil
}

// openSerialTerminal opens the reserved port with the full line settings and
// starts its read loop. A failure returns a *serialStartError carrying the
// StartTerminal code.
func (a *App) openSerialTerminal(ctx context.Context, ts *terminalSession) (string, error) {
	h := ts.serial
	a.termMu.Lock()
	settings := h.settings
	a.termMu.Unlock()
	// The open is a blocking syscall that ctx cannot interrupt (O_NDELAY makes
	// it return at once in practice); a cancel is honored once it returns.
	port, err := openSerialPort(normalizeSerialPort(serialGOOS, ts.port), settings.mode())
	if err != nil {
		if ctx.Err() != nil {
			return "", errTerminalCanceled
		}
		code, message := serialOpenError(serialGOOS, ts.port, err)
		return message, &serialStartError{code: code, err: err}
	}
	if ctx.Err() != nil {
		_ = port.Close()
		return "", errTerminalCanceled
	}
	// Written before commitTerminal takes termMu, so whoever sees the tab
	// open (under termMu) also sees the port.
	h.port = port
	ts.stdinWriter = h
	if _, _, ok := a.commitTerminal(ts, nil, h.closePort); !ok {
		h.closePort()
		return "", errTerminalCanceled
	}
	a.terminalLoops.Add(1)
	go func() {
		defer a.terminalLoops.Done()
		a.serialReadLoop(ts)
	}()
	a.termMu.Lock()
	pending := h.pending
	h.pending = nil
	a.termMu.Unlock()
	if pending != nil {
		_, _ = a.applyNow(ts, *pending) // on failure the open settings stay
	}
	return "", nil
}

// serialReadLoop streams the port into the tab until a read fails, then
// decides between device_removed and io_error.
func (a *App) serialReadLoop(ts *terminalSession) {
	h := ts.serial
	buf := make([]byte, 4096)
	for {
		n, err := h.port.Read(buf)
		if n > 0 {
			_, _ = ts.out.Write(buf[:n])
		}
		if err == nil {
			continue // only a read timeout returns 0, nil; none is set
		}
		a.endSerialTerminal(ts, err)
		return
	}
}

// endSerialTerminal ends a serial tab after a read or write error. A failure
// caused by our own Close is ignored: finishTerminal and closeTerminalSession
// set the reason before the closer runs. Called without locks (it sleeps
// while probing for the device).
func (a *App) endSerialTerminal(ts *terminalSession, err error) {
	if a.terminalClosing(ts) {
		return
	}
	reason, message := serialEndReason(serialGOOS, ts.port, err)
	a.finishTerminal(ts, reason, nil, message)
}

func (a *App) terminalClosing(ts *terminalSession) bool {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	return ts.reason != ""
}

// applySerialSettings pushes new line settings to the serial tab of connID
// without reopening the port (no DTR toggle, so the board is not reset). It
// reports whether a tab took them; a connecting tab gets them right after
// its open. Call without locks (SaveConnection calls it after a.mu).
func (a *App) applySerialSettings(connID string, s serialSettings) (applied bool, err error) {
	if err := validateSerialSettings(serialGOOS, s); err != nil {
		return false, err
	}
	a.termMu.Lock()
	a.ensureMaps()
	var ts *terminalSession
	for _, t := range a.terms {
		// The port reservation allows one serial tab per device, hence per
		// connection: the first match is the only one.
		if t.connID == connID && t.kind == "serial" && t.serial != nil && t.reason == "" {
			ts = t
			break
		}
	}
	if ts == nil {
		a.termMu.Unlock()
		return false, nil
	}
	if ts.status == terminalStatusConnecting {
		cp := s
		ts.serial.pending = &cp
		a.termMu.Unlock()
		return true, nil
	}
	a.termMu.Unlock()
	return a.applyNow(ts, s)
}

// applyNow drains queued output and sets the new mode. On unix a failed
// SetMode is rolled back (a special baud can leave termios half-changed); on
// windows the library has already closed the port, so the tab ends with
// io_error.
func (a *App) applyNow(ts *terminalSession, s serialSettings) (bool, error) {
	h := ts.serial
	h.applyMu.Lock()
	a.termMu.Lock()
	prev := h.settings
	a.termMu.Unlock()
	if prev == s {
		h.applyMu.Unlock()
		return true, nil
	}
	goos := serialGOOS
	h.writeMu.Lock()
	if h.closed {
		// The tab is closing: the fd may already belong to something else.
		h.writeMu.Unlock()
		h.applyMu.Unlock()
		return false, errSerialPortClosed
	}
	drainWithTimeout(h.port, serialDrainTimeout)
	err := h.port.SetMode(s.mode())
	if err != nil && goos != "windows" {
		_ = h.port.SetMode(prev.mode())
	}
	h.writeMu.Unlock()
	if err == nil {
		a.termMu.Lock()
		h.settings = s
		ts.baud = s.Baud
		a.termMu.Unlock()
	}
	h.applyMu.Unlock()
	if err != nil {
		if goos == "windows" {
			a.finishTerminal(ts, exitReasonIOError, nil, fmt.Sprintf("%s の通信設定を変更できず、ポートが閉じられました: %v", ts.port, err))
		}
		return false, err
	}
	a.sessionsChanged()
	return true, nil
}

// drainWithTimeout waits for queued output to leave, but not for ever: a
// removed device may never finish draining.
func drainWithTimeout(p serialPort, d time.Duration) {
	done := make(chan struct{})
	go func() {
		_ = p.Drain()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// pumpTerminalInput is the single writer to the session's stdin, so input
// arrives in the order SendTerminalInput accepted it.
func (a *App) pumpTerminalInput(ts *terminalSession) {
	for {
		select {
		case <-ts.done:
			return
		case data := <-ts.input:
			if _, err := ts.stdinWriter.Write(data); err != nil {
				if ts.kind == "serial" {
					// Right after an unplug the write side often fails first
					// (ENXIO / EIO): classify it like a read failure.
					a.endSerialTerminal(ts, err)
					return
				}
				// A write usually fails because the remote side just closed
				// (typing ahead after `exit`). Let the session's own end win:
				// it reports remote_exit with the exit code after every
				// remaining output byte is flushed. Only a writer that fails
				// while the session stays up becomes io_error.
				timer := time.NewTimer(terminalInputFailGrace)
				defer timer.Stop()
				select {
				case <-ts.done:
				case <-timer.C:
					a.finishTerminal(ts, exitReasonIOError, nil, "入力の送信に失敗しました: "+err.Error())
				}
				return
			}
		}
	}
}

// finishTerminal ends an open tab exactly once: closer, final flush, map
// removal, then terminal:exit and sessions:changed outside every lock. A
// reason of user set earlier by CloseTerminal always wins.
func (a *App) finishTerminal(ts *terminalSession, reason string, exitCode *int, message string) {
	ts.closeOnce.Do(func() {
		a.termMu.Lock()
		if ts.reason == "" {
			ts.reason = reason
		}
		if ts.reason != reason {
			reason, exitCode, message = ts.reason, nil, ""
		}
		closer := ts.closer
		a.termMu.Unlock()

		if closer != nil {
			closer()
		}
		ts.cancel()
		ts.out.close()

		a.termMu.Lock()
		a.ensureMaps()
		a.dropTerminalLocked(ts)
		switch {
		case reason == exitReasonUser:
			delete(a.lastErr, ts.connID)
		case reason == exitReasonRemoteExit && exitCode != nil && *exitCode == 0:
		default:
			if !a.hasOpenTerminalLocked(ts.connID) {
				a.lastErr[ts.connID] = message
				if reason == exitReasonDeviceRemoved {
					a.setSerialGoneLocked(ts.connID, message)
				}
			}
		}
		a.termMu.Unlock()
		ts.markDone()

		a.emitEvent(terminalExitEvent, terminalExitPayload{ID: ts.id, Reason: reason, ExitCode: exitCode, Message: message})
		a.sessionsChanged()
	})
}

func (a *App) hasOpenTerminalLocked(connID string) bool {
	for _, t := range a.terms {
		if t.connID == connID && t.status == terminalStatusOpen {
			return true
		}
	}
	return false
}

// closeTerminalSession closes one tab with reason user. A connecting tab is
// canceled instead (StartTerminal then returns canceled, and no exit is sent).
func (a *App) closeTerminalSession(ts *terminalSession) {
	a.termMu.Lock()
	if ts.reason == "" {
		ts.reason = exitReasonUser
	}
	connecting := ts.status == terminalStatusConnecting
	a.termMu.Unlock()
	if connecting {
		ts.cancel()
		return
	}
	a.finishTerminal(ts, exitReasonUser, nil, "")
}

// SendTerminalInput queues user keystrokes (UTF-8) for the tab's stdin.
func (a *App) SendTerminalInput(id string, data string) ActionResult {
	if len(data) > terminalInputMax {
		return ActionResult{OK: false, Code: "input_too_large", Message: "一度に送れる入力が大きすぎます", SessionID: id}
	}
	a.termMu.Lock()
	ts := a.terms[id]
	// A tab that is closing (reason set before its closer runs) takes no
	// more input: it would only be dropped, or written after the close (N1).
	closing := ts != nil && ts.reason != ""
	a.termMu.Unlock()
	if ts == nil || closing {
		return ActionResult{OK: false, Code: "no_session", Message: "ターミナルが接続されていません", SessionID: id}
	}
	timer := time.NewTimer(terminalInputTimeout)
	defer timer.Stop()
	select {
	case ts.input <- []byte(data):
		return ActionResult{OK: true}
	case <-ts.done:
		return ActionResult{OK: false, Code: "no_session", Message: "ターミナルが接続されていません", SessionID: id}
	case <-timer.C:
		return ActionResult{OK: false, Code: "input_timeout", Message: "入力の送信が詰まっています", SessionID: id}
	}
}

// ResizeTerminal updates the remote PTY window size. While connecting the
// size is kept and applied once the PTY exists; serial ignores it.
func (a *App) ResizeTerminal(id string, cols int, rows int) ActionResult {
	if cols < 1 || cols > terminalMaxSize || rows < 1 || rows > terminalMaxSize {
		return ActionResult{OK: false, Code: "invalid_size", Message: "ターミナルのサイズが不正です", SessionID: id}
	}
	a.termMu.Lock()
	ts := a.terms[id]
	if ts == nil {
		a.termMu.Unlock()
		return ActionResult{OK: false, Code: "no_session", Message: "ターミナルが接続されていません", SessionID: id}
	}
	ts.cols, ts.rows = cols, rows
	a.termMu.Unlock()
	// Calls may arrive on separate goroutines in any order. Under resizeMu,
	// apply whatever size is stored now, so the last window change applied
	// is always the latest stored size.
	ts.resizeMu.Lock()
	defer ts.resizeMu.Unlock()
	a.termMu.Lock()
	resize := ts.resize
	cols, rows = ts.cols, ts.rows
	a.termMu.Unlock()
	if resize == nil {
		return ActionResult{OK: true}
	}
	if err := resize(cols, rows); err != nil {
		return ActionResult{OK: false, Message: "サイズ変更に失敗しました: " + err.Error(), SessionID: id}
	}
	return ActionResult{OK: true}
}

// CloseTerminal closes one tab. Unknown ids are fine (idempotent).
func (a *App) CloseTerminal(id string) ActionResult {
	a.termMu.Lock()
	ts := a.terms[id]
	a.termMu.Unlock()
	if ts != nil {
		a.closeTerminalSession(ts)
	}
	return ActionResult{OK: true, Message: "切断しました", SessionID: id}
}

// CloseTerminalsByConnection closes every tab of one connection.
func (a *App) CloseTerminalsByConnection(connID string) ActionResult {
	n := a.closeTerminalsForConnection(connID)
	return ActionResult{OK: true, Message: fmt.Sprintf("%d 件のターミナルを閉じました", n), ID: connID}
}

// CloseAllTerminals closes every tab (shutdown).
func (a *App) CloseAllTerminals() ActionResult {
	a.termMu.Lock()
	all := make([]*terminalSession, 0, len(a.terms))
	for _, ts := range a.terms {
		all = append(all, ts)
	}
	a.termMu.Unlock()
	for _, ts := range all {
		a.closeTerminalSession(ts)
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("%d 件のターミナルを閉じました", len(all))}
}

// ListTerminalSessions returns the live tabs, so a reloaded frontend can
// rebuild them.
func (a *App) ListTerminalSessions() []TerminalInfo {
	return a.listTerminalInfos()
}

// closeTerminalsForConnection closes the tabs attached to a connection (on
// disconnect or delete) and returns how many, connecting ones included.
func (a *App) closeTerminalsForConnection(connID string) int {
	a.termMu.Lock()
	a.ensureMaps()
	var matched []*terminalSession
	for _, ts := range a.terms {
		if ts.connID == connID {
			matched = append(matched, ts)
		}
	}
	delete(a.lastErr, connID)
	a.termMu.Unlock()
	for _, ts := range matched {
		a.closeTerminalSession(ts)
	}
	return len(matched)
}

// terminalRuntime aggregates tabs and the last error per connection ID.
func (a *App) terminalRuntime() map[string]terminalRuntime {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	out := map[string]terminalRuntime{}
	for _, ts := range a.terms {
		rt := out[ts.connID]
		switch ts.status {
		case terminalStatusOpen:
			rt.Open++
			if rt.Since.IsZero() || ts.startedAt.Before(rt.Since) {
				rt.Since = ts.startedAt
			}
		case terminalStatusConnecting:
			rt.Connecting++
		}
		out[ts.connID] = rt
	}
	for connID, message := range a.lastErr {
		rt := out[connID]
		rt.LastError = message
		out[connID] = rt
	}
	return out
}

// listTerminalInfos snapshots the tabs, oldest first. Names are looked up
// after termMu is released so a rename shows up.
func (a *App) listTerminalInfos() []TerminalInfo {
	a.termMu.Lock()
	infos := make([]TerminalInfo, 0, len(a.terms))
	started := make(map[string]time.Time, len(a.terms))
	for _, ts := range a.terms {
		info := TerminalInfo{
			ID:        ts.id,
			ConnID:    ts.connID,
			ConnName:  ts.connName,
			Kind:      ts.kind,
			StartedAt: ts.startedAt.UTC().Format(time.RFC3339),
			Status:    ts.status,
		}
		if ts.kind == "serial" {
			info.Port = ts.port
			info.Baud = ts.baud
			if ts.serial != nil {
				info.SerialMode = ts.serial.settings.label()
			}
		}
		infos = append(infos, info)
		started[ts.id] = ts.startedAt
	}
	a.termMu.Unlock()

	for i := range infos {
		if conn, ok := a.connectionByID(infos[i].ConnID); ok {
			infos[i].ConnName = conn.Name
		}
	}
	sort.Slice(infos, func(i, j int) bool {
		ti, tj := started[infos[i].ID], started[infos[j].ID]
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return infos[i].ID < infos[j].ID
	})
	return infos
}

// terminalCounts returns the number of open interactive tabs per connection
// ID (used by list_hosts).
func (a *App) terminalCounts() map[string]int {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	counts := map[string]int{}
	for _, ts := range a.terms {
		if ts.status == terminalStatusOpen {
			counts[ts.connID]++
		}
	}
	return counts
}

// serialPortInUse reports whether one of our serial tabs (connecting or open)
// belongs to connID.
func (a *App) serialPortInUse(connID string) bool {
	if connID == "" {
		return false
	}
	a.termMu.Lock()
	defer a.termMu.Unlock()
	for _, ts := range a.terms {
		if ts.kind == "serial" && ts.connID == connID {
			return true
		}
	}
	return false
}

// normalizeSerialPort maps the names one device can go by to a single key:
// on darwin /dev/tty.X and /dev/cu.X are the same device; on Windows `\\.\`
// is optional and names are case-insensitive.
func normalizeSerialPort(goos string, name string) string {
	name = strings.TrimSpace(name)
	switch goos {
	case "darwin":
		if rest, ok := strings.CutPrefix(name, "/dev/tty."); ok {
			return "/dev/cu." + rest
		}
		return name
	case "windows":
		name = strings.TrimPrefix(name, `\\.\`)
		return strings.ToUpper(name)
	default:
		return name
	}
}

// ListSerialPorts enumerates the serial ports currently available on the host.
func (a *App) ListSerialPorts() []string {
	ports, err := serial.GetPortsList()
	if err != nil || ports == nil {
		return []string{}
	}
	return ports
}

// testSerialConnection checks that the port exists without opening it:
// opening toggles DTR, which resets many boards, and fails with EBUSY while
// our own terminal holds the port. Code is in_use / available / missing.
func (a *App) testSerialConnection(conn Connection) ActionResult {
	port := serialPortOf(conn)
	if port == "" {
		return ActionResult{OK: false, Message: "シリアルポートが設定されていません", ID: conn.ID}
	}
	goos := serialGOOS
	if a.serialPortInUse(conn.ID) {
		return ActionResult{OK: true, Code: "in_use", Message: fmt.Sprintf("%s は使用中です（このアプリのターミナルで接続中）", port), ID: conn.ID}
	}
	if _, owner, held := a.serialOwnerOf(goos, port); held {
		return ActionResult{OK: true, Code: "in_use", Message: fmt.Sprintf("%s は使用中です（%s のターミナルで接続中）", port, owner), ID: conn.ID}
	}
	present, err := serialPortPresent(goos, port)
	if err != nil {
		return ActionResult{OK: false, Message: fmt.Sprintf("%s を確認できません: %v", port, err), ID: conn.ID}
	}
	if present {
		message := fmt.Sprintf("%s を検出しました（利用可能）", port)
		a.recordConnectionTest(conn, "ok", message)
		return ActionResult{OK: true, Code: "available", Message: message, ID: conn.ID}
	}
	message := fmt.Sprintf("%s が見つかりません。ケーブルの接続を確認してください", port)
	a.recordConnectionTest(conn, "failed", message)
	return ActionResult{OK: false, Code: "missing", Message: message, ID: conn.ID}
}

var _ io.Writer = (*outputBuffer)(nil)
