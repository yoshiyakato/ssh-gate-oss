package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.bug.st/serial"
)

// Serial line settings, port access, reservation keys and failure
// classification (docs/spec/phase4-serial-patch.md §2〜§5, §7.3〜§7.4).
// Everything that depends on the OS takes goos as a parameter so every case
// runs on any CI OS; production passes serialGOOS.

// serialSettings is the line configuration of a serial connection. Parity and
// StopBits are strings so the DB, JSON and UI share one vocabulary.
type serialSettings struct {
	Baud     int    // 50..4000000
	DataBits int    // 5..8
	Parity   string // "none" | "odd" | "even" | "mark" | "space"
	StopBits string // "1" | "1.5" | "2"
}

const (
	defaultSerialBaud     = 115200
	defaultSerialDataBits = 8
	defaultSerialParity   = "none"
	defaultSerialStopBits = "1"
	minSerialBaud         = 50
	maxSerialBaud         = 4_000_000

	// connState values only serial connections take (§7.4). The other
	// values live with the SessionSummary constants in app.go.
	connStateAvailable = "available"
	connStateMissing   = "missing"
)

// serialModeError is returned by validateSerialSettings; Field is the
// Connection json name the message is about.
type serialModeError struct {
	Field   string // "baudRate" | "dataBits" | "parity" | "stopBits"
	Message string
}

func (e *serialModeError) Error() string { return e.Message }

// serialSettingsOf reads the line settings of conn, filling the defaults.
func serialSettingsOf(conn Connection) serialSettings {
	s := serialSettings{
		Baud:     conn.BaudRate,
		DataBits: conn.DataBits,
		Parity:   strings.ToLower(strings.TrimSpace(conn.Parity)),
		StopBits: strings.TrimSpace(conn.StopBits),
	}
	if s.Baud <= 0 {
		s.Baud = defaultSerialBaud
	}
	if s.DataBits == 0 {
		s.DataBits = defaultSerialDataBits
	}
	if s.Parity == "" {
		s.Parity = defaultSerialParity
	}
	if s.StopBits == "" {
		s.StopBits = defaultSerialStopBits
	}
	return s
}

// validateSerialSettings applies §2.3 (first match wins). It must pass before
// any SetMode: on windows a rejected SetMode closes the port (L7). Returns nil
// or *serialModeError.
func validateSerialSettings(goos string, s serialSettings) error {
	fail := func(field, message string) error { return &serialModeError{Field: field, Message: message} }
	windows := goos == "windows"
	// darwin and the BSDs have no CMSPAR; only linux and windows do.
	markSpace := goos == "linux" || windows
	switch {
	case s.Baud < minSerialBaud || s.Baud > maxSerialBaud:
		return fail("baudRate", "ボーレートは 50〜4000000 の範囲で指定してください")
	case s.DataBits < 5 || s.DataBits > 8:
		return fail("dataBits", "データビットは 5〜8 で指定してください")
	}
	switch s.Parity {
	case "none", "odd", "even":
	case "mark", "space":
		if !markSpace {
			return fail("parity", "この OS ではマーク/スペースパリティを使えません")
		}
	default:
		return fail("parity", "パリティは none / odd / even / mark / space のいずれかです")
	}
	switch s.StopBits {
	case "1":
	case "2":
		if windows && s.DataBits == 5 {
			return fail("stopBits", "データビット 5 では 2 ストップビットを使えません（1.5 を選んでください）")
		}
	case "1.5":
		if !windows {
			return fail("stopBits", "この OS では 1.5 ストップビットを使えません")
		}
		if s.DataBits != 5 {
			return fail("stopBits", "1.5 ストップビットはデータビット 5 のときだけ使えます")
		}
	default:
		return fail("stopBits", "ストップビットは 1 / 1.5 / 2 のいずれかです")
	}
	return nil
}

// mode converts validated settings; unknown values fall back to the defaults
// instead of panicking. InitialStatusBits stays nil (library default
// DTR=RTS=true, unchanged from before).
func (s serialSettings) mode() *serial.Mode {
	m := &serial.Mode{BaudRate: s.Baud, DataBits: s.DataBits, Parity: serial.NoParity, StopBits: serial.OneStopBit}
	switch s.Parity {
	case "odd":
		m.Parity = serial.OddParity
	case "even":
		m.Parity = serial.EvenParity
	case "mark":
		m.Parity = serial.MarkParity
	case "space":
		m.Parity = serial.SpaceParity
	}
	switch s.StopBits {
	case "1.5":
		m.StopBits = serial.OnePointFiveStopBits
	case "2":
		m.StopBits = serial.TwoStopBits
	}
	return m
}

// label is the usual short form: "8N1", "7E1", "5M1.5".
func (s serialSettings) label() string {
	initial := "N"
	if s.Parity != "" {
		initial = strings.ToUpper(s.Parity[:1])
	}
	return fmt.Sprintf("%d%s%s", s.DataBits, initial, s.StopBits)
}

// serialPort is the subset of serial.Port the terminal uses; tests swap in a
// fake.
type serialPort interface {
	io.ReadWriteCloser
	SetMode(mode *serial.Mode) error
	Drain() error
}

var (
	// serialGOOS is runtime.GOOS; tests swap it to run another OS's branch.
	serialGOOS = runtime.GOOS
	// openSerialPort opens a port. serial.Open returns a nil interface on
	// error, so the conversion never yields a non-nil serialPort holding nil.
	openSerialPort = func(name string, mode *serial.Mode) (serialPort, error) {
		p, err := serial.Open(name, mode)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	serialPortPresent   = defaultSerialPortPresent
	evalSerialSymlinks  = filepath.EvalSymlinks
	serialRemovalProbes = []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}
	serialDrainTimeout  = 500 * time.Millisecond
	// serialPresenceTTL bounds how often sessionSummaries stats a device;
	// serialPresenceInterval is the watchSerialPresence period.
	serialPresenceTTL      = 2 * time.Second
	serialPresenceInterval = 3 * time.Second
)

// serialReservationKey identifies the physical device behind a port name so
// /dev/tty.X and /dev/cu.X (darwin), COM3 and \\.\com3 (windows) and a
// /dev/serial/by-id symlink and its target (linux) share one reservation.
// The first step is the same rule as frontend/src/serialport.js.
func serialReservationKey(goos string, name string) string {
	key := normalizeSerialPort(goos, strings.TrimSpace(name))
	if goos == "windows" || key == "" {
		return key
	}
	if resolved, err := evalSerialSymlinks(key); err == nil && resolved != "" {
		key = normalizeSerialPort(goos, resolved)
	}
	return key
}

// defaultSerialPortPresent reports whether the device behind name still
// exists without opening it (opening toggles DTR and may reset the board).
func defaultSerialPortPresent(goos string, name string) (bool, error) {
	if goos == "windows" {
		ports, err := serial.GetPortsList() // registry SERIALCOMM; opens nothing
		if err != nil {
			return false, err
		}
		want := normalizeSerialPort(goos, name)
		for _, p := range ports {
			if normalizeSerialPort(goos, p) == want {
				return true, nil
			}
		}
		return false, nil
	}
	_, err := os.Stat(normalizeSerialPort(goos, name)) // follows symlinks
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// serialCodedError matches *serial.PortError (whose Code has a value
// receiver) and test fakes: PortError's code field is unexported, so a
// matching interface is the only way to fake PortClosed or PortBusy.
type serialCodedError interface {
	error
	Code() serial.PortErrorCode
}

// serialPortErrorCode extracts the library's error code, if err carries one.
func serialPortErrorCode(err error) (serial.PortErrorCode, bool) {
	var coded serialCodedError
	if errors.As(err, &coded) {
		return coded.Code(), true
	}
	return 0, false
}

func serialErrno(err error) (syscall.Errno, bool) {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno, true
	}
	return 0, false
}

// Errno values compared numerically so a goos other than the host's still
// classifies correctly.
const (
	errnoEPERM  = 1
	errnoENOENT = 2
	errnoEIO    = 5
	errnoENXIO  = 6
	errnoEACCES = 13
	errnoENODEV = 19

	winErrorAccessDenied       = 5
	winErrorInvalidHandle      = 6
	winErrorBadCommand         = 22
	winErrorGenFailure         = 31
	winErrorOperationAborted   = 995
	winErrorDeviceNotConnected = 1167
	winErrorDeviceRemoved      = 1617
)

// serialOpenErrorFrom maps an open failure to its StartTerminal code (§3.4).
func serialOpenErrorFrom(goos string, code serial.PortErrorCode, hasCode bool, errno syscall.Errno, hasErrno bool, notExist bool) string {
	unix := goos != "windows"
	switch {
	case hasCode && code == serial.PortBusy:
		return "port_busy"
	case hasCode && code == serial.PermissionDenied,
		unix && hasErrno && (errno == errnoEACCES || errno == errnoEPERM):
		return "permission_denied"
	case hasCode && code == serial.PortNotFound,
		notExist,
		unix && hasErrno && (errno == errnoENOENT || errno == errnoENXIO || errno == errnoENODEV):
		return "port_not_found"
	case hasCode && (code == serial.InvalidSpeed || code == serial.InvalidDataBits ||
		code == serial.InvalidParity || code == serial.InvalidStopBits):
		return "invalid_serial_mode"
	default:
		return "open_failed"
	}
}

// serialOpenError returns the code and user-facing message for an open
// failure of portName (as configured).
func serialOpenError(goos string, portName string, err error) (code string, message string) {
	pcode, hasCode := serialPortErrorCode(err)
	errno, hasErrno := serialErrno(err)
	code = serialOpenErrorFrom(goos, pcode, hasCode, errno, hasErrno, errors.Is(err, fs.ErrNotExist))
	switch code {
	case "port_busy":
		message = fmt.Sprintf("%s は他のアプリ（screen / minicom / Arduino IDE など）が使用中です", portName)
	case "permission_denied":
		if goos == "linux" {
			message = fmt.Sprintf("%s を開く権限がありません（dialout グループへの追加が必要な場合があります）", portName)
		} else {
			message = fmt.Sprintf("%s を開く権限がありません", portName)
		}
	case "port_not_found":
		message = fmt.Sprintf("%s が見つかりません。ケーブルの接続を確認してください", portName)
	case "invalid_serial_mode":
		message = fmt.Sprintf("%s の通信設定がこの OS / デバイスで使えません: %v", portName, err)
	default:
		if hasCode && pcode == serial.InvalidSerialPort {
			message = fmt.Sprintf("%s はシリアルデバイスとして開けません: %v", portName, err)
		} else {
			message = fmt.Sprintf("%s を開けません: %v", portName, err)
		}
	}
	return code, message
}

// serialStartError carries a serial open failure's code to failTerminalStart.
type serialStartError struct {
	code string
	err  error
}

func (e *serialStartError) Error() string { return e.err.Error() }
func (e *serialStartError) Unwrap() error { return e.err }

type serialIOClass int

const (
	serialIOGeneric serialIOClass = iota // no sign of removal
	serialIORemoved                      // what an unplug usually looks like
)

// classifySerialIOErrorFrom is §5.3's table on already-extracted facts.
func classifySerialIOErrorFrom(goos string, code serial.PortErrorCode, hasCode bool, errno syscall.Errno, hasErrno bool, isEOF bool) serialIOClass {
	if (hasCode && code == serial.PortClosed) || isEOF {
		return serialIORemoved
	}
	if !hasErrno {
		return serialIOGeneric
	}
	switch goos {
	case "windows":
		switch errno {
		case winErrorAccessDenied, winErrorInvalidHandle, winErrorBadCommand, winErrorGenFailure,
			winErrorDeviceNotConnected, winErrorDeviceRemoved, winErrorOperationAborted:
			return serialIORemoved
		}
	default: // darwin, linux, *bsd share these values
		switch errno {
		case errnoEIO, errnoENXIO, errnoENODEV:
			return serialIORemoved
		}
	}
	return serialIOGeneric
}

func classifySerialIOError(goos string, err error) serialIOClass {
	code, hasCode := serialPortErrorCode(err)
	errno, hasErrno := serialErrno(err)
	return classifySerialIOErrorFrom(goos, code, hasCode, errno, hasErrno, errors.Is(err, io.EOF))
}

// serialEndReason decides device_removed vs io_error after a read/write
// failure. It sleeps between probes and must be called without locks.
func serialEndReason(goos string, portName string, err error) (reason string, message string) {
	class := classifySerialIOError(goos, err)
	probes := serialRemovalProbes
	if class == serialIOGeneric && len(probes) > 1 {
		probes = probes[:1] // no sign of removal: check once, now
	}
	removed := fmt.Sprintf("%s が外れました。ケーブルを挿し直してから再接続してください", portName)
	probeFailed := 0
	for _, d := range probes {
		if d > 0 {
			time.Sleep(d)
		}
		present, perr := serialPortPresent(goos, portName)
		if perr != nil {
			probeFailed++
			continue
		}
		if !present {
			return exitReasonDeviceRemoved, removed
		}
	}
	if class == serialIORemoved && len(probes) > 0 && probeFailed == len(probes) {
		// The ports cannot be listed at all (registry unreadable, ...):
		// trust the error's sign.
		return exitReasonDeviceRemoved, removed
	}
	return exitReasonIOError, fmt.Sprintf("%s の通信でエラーが発生しました: %v", portName, err)
}

// serialPresenceCache memoizes serialPortPresent per reservation key for
// serialPresenceTTL. presenceMu is a leaf lock: serialPortPresent is never
// called while holding it.
type serialPresenceCache struct {
	presenceMu sync.Mutex
	presence   map[string]serialPresenceEntry
}

type serialPresenceEntry struct {
	present bool
	known   bool // false when the probe failed
	at      time.Time
}

// serialPresence returns, per reservation key of ports, whether the device
// exists. Keys whose probe failed are absent.
func (a *App) serialPresence(goos string, ports []string) map[string]bool {
	wanted := map[string]bool{}
	for _, p := range ports {
		if key := serialReservationKey(goos, p); key != "" {
			wanted[key] = true
		}
	}
	out := make(map[string]bool, len(wanted))
	var stale []string
	now := time.Now()
	a.presenceMu.Lock()
	for key := range wanted {
		e, ok := a.presence[key]
		if ok && now.Sub(e.at) < serialPresenceTTL {
			if e.known {
				out[key] = e.present
			}
			continue
		}
		stale = append(stale, key)
	}
	a.presenceMu.Unlock()
	if len(stale) == 0 {
		return out
	}
	fresh := make(map[string]serialPresenceEntry, len(stale))
	for _, key := range stale {
		present, err := serialPortPresent(goos, key)
		fresh[key] = serialPresenceEntry{present: present, known: err == nil, at: time.Now()}
		if err == nil {
			out[key] = present
		}
	}
	a.presenceMu.Lock()
	if a.presence == nil {
		a.presence = map[string]serialPresenceEntry{}
	}
	maps.Copy(a.presence, fresh)
	a.presenceMu.Unlock()
	return out
}

// serialConnStates gives, for each serial connection in conns, the connState
// to use once Shells / Connecting / LastError have not decided it (§7.4):
// in_use when another connection's tab holds the same device, then
// available / missing / disconnected (probe failed). Call without locks.
func (a *App) serialConnStates(goos string, conns []Connection) map[string]string {
	keys := map[string]string{} // connID -> key
	var ports []string
	for _, conn := range conns {
		if conn.connectionType() != "Serial" {
			continue
		}
		key := serialReservationKey(goos, conn.SerialPort)
		keys[conn.ID] = key
		if key != "" {
			ports = append(ports, key)
		}
	}
	if len(keys) == 0 {
		return map[string]string{}
	}
	holders := map[string]map[string]bool{} // key -> connIDs with a tab on it
	a.termMu.Lock()
	for _, ts := range a.terms {
		if ts.kind == "serial" && ts.normPort != "" {
			if holders[ts.normPort] == nil {
				holders[ts.normPort] = map[string]bool{}
			}
			holders[ts.normPort][ts.connID] = true
		}
	}
	a.termMu.Unlock()
	present := a.serialPresence(goos, ports)
	out := make(map[string]string, len(keys))
	for connID, key := range keys {
		if key == "" {
			out[connID] = connStateMissing
			continue
		}
		other := false
		for holder := range holders[key] {
			if holder != connID {
				other = true
			}
		}
		p, known := present[key]
		switch {
		case other:
			out[connID] = connStateInUse
		case !known:
			out[connID] = connStateDisconnected
		case p:
			out[connID] = connStateAvailable
		default:
			out[connID] = connStateMissing
		}
	}
	return out
}

// serialPresenceTick is one round of watchSerialPresence. prev maps
// reservation keys to presence from the previous round (nil on the first).
// A device that is present now but was missing in the previous round has been
// plugged back in: the lastErr of the serial connections on it is cleared so
// their state can become available again. changed reports whether the UI
// should be told.
func (a *App) serialPresenceTick(goos string, prev map[string]bool) (next map[string]bool, changed bool) {
	a.mu.RLock()
	type serialConn struct{ id, port string }
	var conns []serialConn
	for _, conn := range a.conns {
		if conn.connectionType() == "Serial" {
			conns = append(conns, serialConn{conn.ID, conn.SerialPort})
		}
	}
	a.mu.RUnlock()
	if len(conns) == 0 {
		return prev, false
	}
	keyOf := make(map[string]string, len(conns))
	ports := make([]string, 0, len(conns))
	for _, c := range conns {
		keyOf[c.id] = serialReservationKey(goos, c.port)
		ports = append(ports, c.port)
	}
	next = a.serialPresence(goos, ports)
	changed = prev != nil && !maps.Equal(prev, next)

	cleared := false
	a.termMu.Lock()
	a.ensureMaps()
	for _, c := range conns {
		key := keyOf[c.id]
		// Only a real absent -> present transition counts as plugged back in.
		// A key missing from prev (first tick, new or re-pointed port) says
		// nothing about the device, so an open failure such as
		// permission_denied or port_busy stays visible.
		if p, ok := prev[key]; !next[key] || !ok || p {
			continue
		}
		if _, ok := a.lastErr[c.id]; ok {
			delete(a.lastErr, c.id)
			cleared = true
		}
	}
	a.termMu.Unlock()
	return next, changed || cleared
}

// watchSerialPresence polls the serial devices every serialPresenceInterval
// and publishes sessions:changed when one is plugged or unplugged. It returns
// when ctx is done. Start it from startup (counted in a.wg).
func (a *App) watchSerialPresence(ctx context.Context) {
	ticker := time.NewTicker(serialPresenceInterval)
	defer ticker.Stop()
	var prev map[string]bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var changed bool
		prev, changed = a.serialPresenceTick(serialGOOS, prev)
		if changed {
			a.sessionsChanged()
		}
	}
}

// serialOwnerOf returns the tab (connecting or open) holding the device
// behind port, if any.
func (a *App) serialOwnerOf(goos string, port string) (sessionID string, connName string, ok bool) {
	key := serialReservationKey(goos, port)
	if key == "" {
		return "", "", false
	}
	a.termMu.Lock()
	defer a.termMu.Unlock()
	a.ensureMaps()
	owner, held := a.serialOwners[key]
	if !held {
		return "", "", false
	}
	if ts := a.terms[owner]; ts != nil {
		connName = ts.connName
	}
	return owner, connName, true
}

// afterSerialSave is §7.3's table: after a successful SaveConnection
// (called without a.mu), apply changed line settings to the open serial tab
// in place, or tell the UI that the tab still runs the old port / type.
// prev is the saved row before the update (zero ID for a new connection).
func (a *App) afterSerialSave(prev Connection, next Connection, r ActionResult) ActionResult {
	if !r.OK || prev.ID == "" || next.ID == "" {
		return r
	}
	prevSerial, nextSerial := prev.connectionType() == "Serial", next.connectionType() == "Serial"
	if !prevSerial && !nextSerial {
		return r
	}
	rt := a.terminalRuntime()[next.ID]
	hasTab := rt.Open+rt.Connecting > 0
	if !hasTab {
		return r // S6
	}
	if prevSerial != nextSerial { // S8
		r.Message += "（開いているターミナルは変更前の設定のままです）"
		r.Code = "reconnect_required"
		return r
	}
	goos := serialGOOS
	if serialReservationKey(goos, prev.SerialPort) != serialReservationKey(goos, next.SerialPort) { // S7
		r.Message += fmt.Sprintf("（開いているターミナルは旧ポート %s のままです。再接続すると反映されます）", prev.SerialPort)
		r.Code = "reconnect_required"
		return r
	}
	before, after := serialSettingsOf(prev), serialSettingsOf(next)
	if before == after { // S1
		return r
	}
	applied, err := a.applySerialSettings(next.ID, after)
	switch {
	case err != nil && goos == "windows": // S5
		r.Message += fmt.Sprintf("（開いているターミナルには適用できませんでした: %v。ターミナルは閉じられました）", err)
		r.Code = "serial_mode_not_applied"
	case err != nil: // S4
		r.Message += fmt.Sprintf("（開いているターミナルには適用できませんでした: %v。再接続すると反映されます）", err)
		r.Code = "serial_mode_not_applied"
	case applied: // S2, S3
		r.Message += fmt.Sprintf("（開いているターミナルに %d %s を適用しました）", after.Baud, after.label())
		r.Code = "serial_mode_applied"
	}
	return r
}
