package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.bug.st/serial"
)

// fakePortError stands in for *serial.PortError, whose code field cannot be
// set outside the library.
type fakePortError struct{ code serial.PortErrorCode }

func (e fakePortError) Error() string              { return fmt.Sprintf("serial error %d", e.code) }
func (e fakePortError) Code() serial.PortErrorCode { return e.code }

type fakeRead struct {
	data []byte
	err  error
}

// fakeSerialPort is a scriptable serialPort. Read takes one fakeRead at a
// time from reads; closing reads, or Close, makes Read return PortClosed.
type fakeSerialPort struct {
	reads chan fakeRead

	mu         sync.Mutex
	written    bytes.Buffer
	modes      []serial.Mode
	ops        []string // "write-begin", "write-end", "drain", "setmode"
	setErr     error    // returned by the next SetMode, then cleared
	writeErr   error
	writeDelay time.Duration
	drainDur   time.Duration

	closed    atomic.Bool
	closeOnce sync.Once
	closedCh  chan struct{}
	// opsAfterClose counts Write / Drain / SetMode calls that started after
	// Close: on a real unix port they would hit a reused fd (N1).
	opsAfterClose atomic.Int32
	// closeDelay keeps Close running after the port is marked closed, so the
	// window between the fd close and the end of finishTerminal is wide.
	closeDelay time.Duration
}

func (f *fakeSerialPort) checkOpen() {
	if f.closed.Load() {
		f.opsAfterClose.Add(1)
	}
}

func newFakeSerialPort() *fakeSerialPort {
	return &fakeSerialPort{reads: make(chan fakeRead, 16), closedCh: make(chan struct{})}
}

func (f *fakeSerialPort) Read(p []byte) (int, error) {
	select {
	case r, ok := <-f.reads:
		if !ok {
			return 0, fakePortError{serial.PortClosed}
		}
		return copy(p, r.data), r.err
	case <-f.closedCh:
		return 0, fakePortError{serial.PortClosed}
	}
}

func (f *fakeSerialPort) op(name string) {
	f.mu.Lock()
	f.ops = append(f.ops, name)
	f.mu.Unlock()
}

func (f *fakeSerialPort) Write(p []byte) (int, error) {
	f.checkOpen()
	f.op("write-begin")
	f.mu.Lock()
	delay, err := f.writeDelay, f.writeErr
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "write-end")
	if err != nil {
		return 0, err
	}
	return f.written.Write(p)
}

func (f *fakeSerialPort) Drain() error {
	f.checkOpen()
	f.op("drain")
	f.mu.Lock()
	d := f.drainDur
	f.mu.Unlock()
	time.Sleep(d)
	return nil
}

func (f *fakeSerialPort) SetMode(mode *serial.Mode) error {
	f.checkOpen()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "setmode")
	f.modes = append(f.modes, *mode)
	err := f.setErr
	f.setErr = nil
	return err
}

func (f *fakeSerialPort) Close() error {
	f.closed.Store(true)
	f.closeOnce.Do(func() { close(f.closedCh) })
	time.Sleep(f.closeDelay)
	return nil
}

func (f *fakeSerialPort) Modes() []serial.Mode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]serial.Mode(nil), f.modes...)
}

func (f *fakeSerialPort) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// fakeSerialEnv records what the swapped package variables saw.
type fakeSerialEnv struct {
	mu        sync.Mutex
	opened    []string
	openModes []serial.Mode
	presence  []string
	symlinks  map[string]string
}

func (e *fakeSerialEnv) Opened() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.opened...)
}

func (e *fakeSerialEnv) PresenceCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.presence)
}

// withFakeSerial swaps openSerialPort / serialPortPresent /
// evalSerialSymlinks / serialRemovalProbes / serialGOOS for the test. Call it
// before newSerialTestApp so the app's cleanup (closing every tab and waiting
// for its goroutines) runs first.
func withFakeSerial(t *testing.T, goos string, open func(name string, mode *serial.Mode) (serialPort, error), present func(goos, name string) (bool, error)) *fakeSerialEnv {
	t.Helper()
	env := &fakeSerialEnv{symlinks: map[string]string{}}
	oldOpen, oldPresent, oldEval, oldProbes, oldGOOS := openSerialPort, serialPortPresent, evalSerialSymlinks, serialRemovalProbes, serialGOOS
	oldDrain, oldTTL := serialDrainTimeout, serialPresenceTTL
	openSerialPort = func(name string, mode *serial.Mode) (serialPort, error) {
		env.mu.Lock()
		env.opened = append(env.opened, name)
		env.openModes = append(env.openModes, *mode)
		env.mu.Unlock()
		return open(name, mode)
	}
	serialPortPresent = func(goos, name string) (bool, error) {
		env.mu.Lock()
		env.presence = append(env.presence, name)
		env.mu.Unlock()
		if present == nil {
			return true, nil
		}
		return present(goos, name)
	}
	evalSerialSymlinks = func(path string) (string, error) {
		env.mu.Lock()
		defer env.mu.Unlock()
		if target, ok := env.symlinks[path]; ok {
			if target == "" {
				return "", fs.ErrNotExist
			}
			return target, nil
		}
		return "", fs.ErrNotExist
	}
	serialRemovalProbes = []time.Duration{0, 0, 0, 0}
	serialGOOS = goos
	t.Cleanup(func() {
		openSerialPort, serialPortPresent, evalSerialSymlinks, serialRemovalProbes, serialGOOS = oldOpen, oldPresent, oldEval, oldProbes, oldGOOS
		serialDrainTimeout, serialPresenceTTL = oldDrain, oldTTL
	})
	return env
}

// presenceSeq returns the scripted results in order, then repeats the last.
func presenceSeq(results ...any) func(goos, name string) (bool, error) {
	var mu sync.Mutex
	i := 0
	return func(string, string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		r := results[min(i, len(results)-1)]
		i++
		if err, ok := r.(error); ok {
			return false, err
		}
		return r.(bool), nil
	}
}

func openFake(port *fakeSerialPort) func(string, *serial.Mode) (serialPort, error) {
	return func(string, *serial.Mode) (serialPort, error) { return port, nil }
}

func newSerialTestApp(t *testing.T, conns ...Connection) (*App, *eventRecorder) {
	t.Helper()
	a, rec := newTerminalTestApp(t, conns...)
	t.Cleanup(func() {
		a.CloseAllTerminals()
		a.terminalLoops.Wait()
	})
	return a, rec
}

func serialConn(id, name, port string) Connection {
	return Connection{ID: id, Name: name, Type: "Serial", SerialPort: port, BaudRate: 115200}
}

func countSessionsChanged(a *App) func() int {
	var n atomic.Int32
	a.onSessionsChanged = func() { n.Add(1) }
	return func() int { return int(n.Load()) }
}

func settingsOf(baud, data int, parity, stop string) serialSettings {
	return serialSettings{Baud: baud, DataBits: data, Parity: parity, StopBits: stop}
}

// ---- §9.1 pure functions ----

func TestSerialSettingsValidate(t *testing.T) {
	cases := []struct {
		name, goos string
		s          serialSettings
		field      string // "" = OK
	}{
		{"T1", "darwin", settingsOf(115200, 8, "none", "1"), ""},
		{"T2", "linux", settingsOf(9600, 7, "even", "1"), ""},
		{"T3", "windows", settingsOf(9600, 8, "odd", "2"), ""},
		{"T4", "darwin", settingsOf(49, 8, "none", "1"), "baudRate"},
		{"T5", "linux", settingsOf(4000001, 8, "none", "1"), "baudRate"},
		{"T6", "linux", settingsOf(250000, 8, "none", "1"), ""},
		{"T7", "windows", settingsOf(9600, 4, "none", "1"), "dataBits"},
		{"T8", "darwin", settingsOf(9600, 9, "none", "1"), "dataBits"},
		{"T9", "linux", settingsOf(9600, 8, "parity", "1"), "parity"},
		{"T10", "darwin", settingsOf(9600, 8, "mark", "1"), "parity"},
		{"T11", "openbsd", settingsOf(9600, 8, "space", "1"), "parity"},
		{"T12", "linux", settingsOf(9600, 8, "mark", "1"), ""},
		{"T13", "windows", settingsOf(9600, 8, "space", "1"), ""},
		{"T14", "linux", settingsOf(9600, 8, "none", "3"), "stopBits"},
		{"T15", "darwin", settingsOf(9600, 5, "none", "1.5"), "stopBits"},
		{"T16", "linux", settingsOf(9600, 5, "none", "1.5"), "stopBits"},
		{"T17", "windows", settingsOf(9600, 5, "none", "1.5"), ""},
		{"T18", "windows", settingsOf(9600, 8, "none", "1.5"), "stopBits"},
		{"T19", "windows", settingsOf(9600, 5, "none", "2"), "stopBits"},
		{"T20", "linux", settingsOf(9600, 5, "none", "2"), ""},
	}
	for _, c := range cases {
		err := validateSerialSettings(c.goos, c.s)
		if c.field == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
			continue
		}
		var me *serialModeError
		if !errors.As(err, &me) || me.Field != c.field || me.Message == "" {
			t.Errorf("%s: err = %#v, want field %s", c.name, err, c.field)
		}
	}
	// The V6 / V7 messages differ: darwin cannot do 1.5 at all.
	if err := validateSerialSettings("windows", settingsOf(9600, 8, "none", "1.5")); !strings.Contains(err.Error(), "データビット 5") {
		t.Errorf("V7 message = %q", err)
	}
}

func TestSerialSettingsOfDefaults(t *testing.T) {
	if got := serialSettingsOf(Connection{Type: "Serial"}); got != settingsOf(115200, 8, "none", "1") {
		t.Fatalf("defaults = %+v", got)
	}
	if got := serialSettingsOf(Connection{Type: "Serial", Parity: " EVEN ", StopBits: " 2 "}); got.Parity != "even" || got.StopBits != "2" {
		t.Fatalf("trim/lower = %+v", got)
	}
}

func TestSerialSettingsModeAndLabel(t *testing.T) {
	cases := []struct {
		s     serialSettings
		mode  serial.Mode
		label string
	}{
		{settingsOf(115200, 8, "none", "1"), serial.Mode{BaudRate: 115200, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit}, "8N1"},
		{settingsOf(9600, 7, "even", "1"), serial.Mode{BaudRate: 9600, DataBits: 7, Parity: serial.EvenParity, StopBits: serial.OneStopBit}, "7E1"},
		{settingsOf(9600, 8, "odd", "2"), serial.Mode{BaudRate: 9600, DataBits: 8, Parity: serial.OddParity, StopBits: serial.TwoStopBits}, "8O2"},
		{settingsOf(1200, 5, "mark", "1.5"), serial.Mode{BaudRate: 1200, DataBits: 5, Parity: serial.MarkParity, StopBits: serial.OnePointFiveStopBits}, "5M1.5"},
		{settingsOf(4800, 6, "space", "1"), serial.Mode{BaudRate: 4800, DataBits: 6, Parity: serial.SpaceParity, StopBits: serial.OneStopBit}, "6S1"},
	}
	for _, c := range cases {
		m := c.s.mode()
		if m.InitialStatusBits != nil || m.BaudRate != c.mode.BaudRate || m.DataBits != c.mode.DataBits || m.Parity != c.mode.Parity || m.StopBits != c.mode.StopBits {
			t.Errorf("%+v.mode() = %+v, want %+v", c.s, *m, c.mode)
		}
		if got := c.s.label(); got != c.label {
			t.Errorf("%+v.label() = %q, want %q", c.s, got, c.label)
		}
	}
}

func TestSerialReservationKey(t *testing.T) {
	cases := []struct {
		name, goos, in string
		link           string // symlink target; "" = none, "!" = EvalSymlinks fails
		want           string
	}{
		{"N1", "darwin", "/dev/tty.usbserial-10", "", "/dev/cu.usbserial-10"},
		{"N2", "darwin", "/dev/cu.usbserial-10", "", "/dev/cu.usbserial-10"},
		{"N3", "darwin", " /dev/tty.usbmodem1101 ", "", "/dev/cu.usbmodem1101"},
		{"N4", "darwin", "/dev/ttys003", "", "/dev/ttys003"},
		{"N5", "windows", "COM3", "", "COM3"},
		{"N6", "windows", `\\.\com3`, "", "COM3"},
		{"N7", "windows", "com10", "", "COM10"},
		{"N8", "linux", "/dev/ttyUSB0", "", "/dev/ttyUSB0"},
		{"N9", "linux", "/dev/ttyusb0", "", "/dev/ttyusb0"},
		{"N10", "linux", "/dev/serial/by-id/usb-FTDI_FT232R-if00-port0", "/dev/ttyUSB0", "/dev/ttyUSB0"},
		{"N11", "darwin", "/dev/my-board", "/dev/tty.usbserial-10", "/dev/cu.usbserial-10"},
		{"N12", "linux", "/dev/serial/by-id/missing", "!", "/dev/serial/by-id/missing"},
		{"N13", "windows", `\\.\COM3`, "", "COM3"},
	}
	old := evalSerialSymlinks
	t.Cleanup(func() { evalSerialSymlinks = old })
	for _, c := range cases {
		calls := 0
		evalSerialSymlinks = func(path string) (string, error) {
			calls++
			switch c.link {
			case "":
				return path, nil // not a link
			case "!":
				return "", fs.ErrNotExist
			default:
				return c.link, nil
			}
		}
		if got := serialReservationKey(c.goos, c.in); got != c.want {
			t.Errorf("%s: serialReservationKey(%s, %q) = %q, want %q", c.name, c.goos, c.in, got, c.want)
		}
		if c.goos == "windows" && calls != 0 {
			t.Errorf("%s: EvalSymlinks called on windows", c.name)
		}
	}
}

func TestSerialOpenError(t *testing.T) {
	type in struct {
		code     serial.PortErrorCode
		hasCode  bool
		errno    syscall.Errno
		hasErrno bool
		notExist bool
	}
	pc := func(c serial.PortErrorCode) in { return in{code: c, hasCode: true} }
	en := func(e syscall.Errno) in { return in{errno: e, hasErrno: true} }
	cases := []struct {
		name, goos string
		in         in
		want       string
	}{
		{"O1", "darwin", pc(serial.PortBusy), "port_busy"},
		{"O2", "linux", pc(serial.PermissionDenied), "permission_denied"},
		{"O3", "darwin", en(13), "permission_denied"},
		{"O4", "windows", pc(serial.PortNotFound), "port_not_found"},
		{"O5", "linux", en(2), "port_not_found"},
		{"O6", "darwin", in{notExist: true}, "port_not_found"},
		{"O7", "linux", pc(serial.InvalidParity), "invalid_serial_mode"},
		{"O8", "darwin", pc(serial.InvalidSerialPort), "open_failed"},
		{"O9", "windows", en(5), "open_failed"},
		{"O10", "linux", in{}, "open_failed"},
	}
	for _, c := range cases {
		if got := serialOpenErrorFrom(c.goos, c.in.code, c.in.hasCode, c.in.errno, c.in.hasErrno, c.in.notExist); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// serialOpenError wires the extraction to the table.
	if code, msg := serialOpenError("linux", "/dev/ttyUSB0", fakePortError{serial.PermissionDenied}); code != "permission_denied" || !strings.Contains(msg, "dialout") {
		t.Errorf("linux permission: %s %q", code, msg)
	}
	if code, _ := serialOpenError("darwin", "/dev/cu.x", fmt.Errorf("x: %w", fs.ErrNotExist)); code != "port_not_found" {
		t.Errorf("wrapped ErrNotExist: %s", code)
	}
	if code, msg := serialOpenError("darwin", "/dev/cu.x", fakePortError{serial.PortBusy}); code != "port_busy" || !strings.Contains(msg, "/dev/cu.x") {
		t.Errorf("busy: %s %q", code, msg)
	}
	if code, _ := serialOpenError("linux", "/dev/x", errors.New("boom")); code != "open_failed" {
		t.Errorf("generic: %s", code)
	}
}

// ---- §9.2 presence ----

func TestDefaultSerialPortPresentUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	dir := t.TempDir()
	dev := filepath.Join(dir, "ttyUSB0")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := defaultSerialPortPresent("linux", dev); !ok || err != nil {
		t.Fatalf("existing = %v, %v", ok, err)
	}
	link := filepath.Join(dir, "by-id")
	if err := os.Symlink(dev, link); err != nil {
		t.Fatal(err)
	}
	if ok, err := defaultSerialPortPresent("linux", link); !ok || err != nil {
		t.Fatalf("link = %v, %v", ok, err)
	}
	if err := os.Remove(dev); err != nil {
		t.Fatal(err)
	}
	if ok, err := defaultSerialPortPresent("linux", dev); ok || err != nil {
		t.Fatalf("removed = %v, %v", ok, err)
	}
	if ok, err := defaultSerialPortPresent("linux", link); ok || err != nil {
		t.Fatalf("dangling link = %v, %v", ok, err)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "tty"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		if ok, err := defaultSerialPortPresent("linux", filepath.Join(locked, "tty")); ok || err == nil {
			t.Fatalf("unreadable dir = %v, %v; want an error", ok, err)
		}
	}
}

// ---- §9.3 classification ----

func TestClassifySerialIOError(t *testing.T) {
	type in struct {
		code     serial.PortErrorCode
		hasCode  bool
		errno    syscall.Errno
		hasErrno bool
		eof      bool
	}
	closed := in{code: serial.PortClosed, hasCode: true}
	en := func(e syscall.Errno) in { return in{errno: e, hasErrno: true} }
	cases := []struct {
		name, goos string
		in         in
		want       serialIOClass
	}{
		{"C1a", "linux", closed, serialIORemoved},
		{"C1b", "windows", closed, serialIORemoved},
		{"C2", "darwin", in{eof: true}, serialIORemoved},
		{"C3", "darwin", en(6), serialIORemoved},
		{"C4a", "darwin", en(5), serialIORemoved},
		{"C4b", "freebsd", en(19), serialIORemoved},
		{"C5a", "linux", en(5), serialIORemoved},
		{"C5b", "linux", en(19), serialIORemoved},
		{"C6a", "windows", en(1167), serialIORemoved},
		{"C6b", "windows", en(22), serialIORemoved},
		{"C6c", "windows", en(31), serialIORemoved},
		{"C6d", "windows", en(5), serialIORemoved},
		{"C6e", "windows", en(1617), serialIORemoved},
		{"C7a", "darwin", en(9), serialIOGeneric},
		{"C7b", "linux", en(11), serialIOGeneric},
		{"C7c", "windows", en(121), serialIOGeneric},
		{"C7d", "darwin", en(1167), serialIOGeneric},
		{"C7e", "linux", in{}, serialIOGeneric},
	}
	for _, c := range cases {
		if got := classifySerialIOErrorFrom(c.goos, c.in.code, c.in.hasCode, c.in.errno, c.in.hasErrno, c.in.eof); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if classifySerialIOError("linux", fakePortError{serial.PortClosed}) != serialIORemoved ||
		classifySerialIOError("darwin", fmt.Errorf("read: %w", syscall.Errno(6))) != serialIORemoved ||
		classifySerialIOError("linux", io.EOF) != serialIORemoved ||
		classifySerialIOError("linux", errors.New("x")) != serialIOGeneric {
		t.Error("classifySerialIOError does not extract like the table")
	}
}

func TestSerialEndReason(t *testing.T) {
	probeErr := errors.New("enumerate failed")
	cases := []struct {
		name, goos string
		err        error
		present    []any
		want       string
		calls      int
	}{
		{"R1", "linux", syscall.Errno(5), []any{false}, exitReasonDeviceRemoved, 1},
		{"R2", "linux", syscall.Errno(5), []any{true, true, false}, exitReasonDeviceRemoved, 3},
		{"R3", "linux", syscall.Errno(5), []any{true}, exitReasonIOError, 4},
		{"R4", "windows", syscall.Errno(1167), []any{probeErr}, exitReasonDeviceRemoved, 4},
		{"R5", "darwin", syscall.Errno(9), []any{false}, exitReasonDeviceRemoved, 1},
		{"R6", "darwin", syscall.Errno(9), []any{true}, exitReasonIOError, 1},
		{"R7", "darwin", syscall.Errno(9), []any{probeErr}, exitReasonIOError, 1},
		{"R8", "darwin", syscall.Errno(6), []any{probeErr, false}, exitReasonDeviceRemoved, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := withFakeSerial(t, c.goos, nil, presenceSeq(c.present...))
			reason, message := serialEndReason(c.goos, "/dev/x", c.err)
			if reason != c.want || !strings.Contains(message, "/dev/x") {
				t.Fatalf("reason = %s (%q), want %s", reason, message, c.want)
			}
			if got := env.PresenceCalls(); got != c.calls {
				t.Fatalf("present calls = %d, want %d", got, c.calls)
			}
		})
	}
}

// ---- §9.4 StartTerminal to close ----

func TestSerialOpenUsesSettings(t *testing.T) {
	port := newFakeSerialPort()
	env := withFakeSerial(t, "linux", openFake(port), nil)
	conn := serialConn("s1", "board", "/dev/ttyUSB0")
	conn.BaudRate, conn.DataBits, conn.Parity = 9600, 7, "even"
	a, _ := newSerialTestApp(t, conn)
	res := a.StartTerminal("TERM-p1", "s1", 80, 24)
	if !res.OK || !strings.Contains(res.Message, "9600 7E1") {
		t.Fatalf("start = %+v", res)
	}
	want := serial.Mode{BaudRate: 9600, DataBits: 7, Parity: serial.EvenParity, StopBits: serial.OneStopBit}
	if len(env.openModes) != 1 || env.openModes[0] != want {
		t.Fatalf("open modes = %+v", env.openModes)
	}
	infos := a.ListTerminalSessions()
	if len(infos) != 1 || infos[0].Baud != 9600 || infos[0].SerialMode != "7E1" || infos[0].Port != "/dev/ttyUSB0" {
		t.Fatalf("infos = %+v", infos)
	}
}

func TestSerialDarwinOpensNormalizedName(t *testing.T) {
	port := newFakeSerialPort()
	env := withFakeSerial(t, "darwin", openFake(port), nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/tty.usbserial-10"))
	mustStart(t, a, "TERM-n", "s1", 80, 24)
	if got := env.Opened(); len(got) != 1 || got[0] != "/dev/cu.usbserial-10" {
		t.Fatalf("opened = %v", got)
	}
	if infos := a.ListTerminalSessions(); infos[0].Port != "/dev/tty.usbserial-10" {
		t.Fatalf("port shown = %q, want as configured", infos[0].Port)
	}
}

func TestSerialInvalidModeRejectedBeforeOpen(t *testing.T) {
	env := withFakeSerial(t, "darwin", openFake(newFakeSerialPort()), nil)
	conn := serialConn("s1", "board", "/dev/cu.x")
	conn.Parity = "mark"
	a, _ := newSerialTestApp(t, conn)
	changed := countSessionsChanged(a)
	res := a.StartTerminal("TERM-p2", "s1", 80, 24)
	if res.OK || res.Code != "invalid_serial_mode" {
		t.Fatalf("start = %+v", res)
	}
	a.termMu.Lock()
	terms, owners := len(a.terms), len(a.serialOwners)
	a.termMu.Unlock()
	if len(env.Opened()) != 0 || terms != 0 || owners != 0 || changed() != 0 {
		t.Fatalf("opened=%v terms=%d owners=%d changed=%d", env.Opened(), terms, owners, changed())
	}
	missing := serialConn("s2", "none", "  ")
	a.mu.Lock()
	a.conns = append(a.conns, missing)
	a.mu.Unlock()
	if res := a.StartTerminal("TERM-p2b", "s2", 80, 24); res.Code != "invalid_serial_mode" {
		t.Fatalf("empty port = %+v", res)
	}
}

func TestSerialCuTtyDoubleOpenRejected(t *testing.T) {
	env := withFakeSerial(t, "darwin", openFake(newFakeSerialPort()), nil)
	a, _ := newSerialTestApp(t, serialConn("a", "board-A", "/dev/cu.usbserial-10"), serialConn("b", "board-B", "/dev/tty.usbserial-10"))
	mustStart(t, a, "TERM-a", "a", 80, 24)
	res := a.StartTerminal("TERM-b", "b", 80, 24)
	if res.Code != "port_in_use" || !strings.Contains(res.Message, "board-A") || !strings.Contains(res.Message, "同じデバイス") {
		t.Fatalf("second = %+v", res)
	}
	if len(env.Opened()) != 1 {
		t.Fatalf("opened = %v", env.Opened())
	}
}

func TestSerialSameConnSecondTabRejected(t *testing.T) {
	withFakeSerial(t, "linux", openFake(newFakeSerialPort()), nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-1", "s1", 80, 24)
	res := a.StartTerminal("TERM-2", "s1", 80, 24)
	if res.Code != "port_in_use" || strings.Contains(res.Message, "同じデバイス") {
		t.Fatalf("second = %+v", res)
	}
}

// gatedOpen blocks the open until release is closed.
func gatedOpen(port *fakeSerialPort) (open func(string, *serial.Mode) (serialPort, error), entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}, 4), make(chan struct{})
	open = func(string, *serial.Mode) (serialPort, error) {
		entered <- struct{}{}
		<-release
		return port, nil
	}
	return open, entered, release
}

func TestSerialReservedWhileConnecting(t *testing.T) {
	open, entered, release := gatedOpen(newFakeSerialPort())
	env := withFakeSerial(t, "linux", open, nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"), serialConn("s2", "other", "/dev/ttyUSB0"))
	first := make(chan ActionResult, 1)
	go func() { first <- a.StartTerminal("TERM-1", "s1", 80, 24) }()
	<-entered
	if res := a.StartTerminal("TERM-2", "s2", 80, 24); res.Code != "port_in_use" {
		t.Fatalf("second while connecting = %+v", res)
	}
	close(release)
	if res := <-first; !res.OK {
		t.Fatalf("first = %+v", res)
	}
	if len(env.Opened()) != 1 {
		t.Fatalf("opened = %v", env.Opened())
	}
}

func TestSerialReservationReleasedOnOpenFailure(t *testing.T) {
	port := newFakeSerialPort()
	var fail atomic.Bool
	fail.Store(true)
	withFakeSerial(t, "darwin", func(string, *serial.Mode) (serialPort, error) {
		if fail.Load() {
			return nil, fakePortError{serial.PortBusy}
		}
		return port, nil
	}, nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.x"))
	res := a.StartTerminal("TERM-1", "s1", 80, 24)
	if res.OK || res.Code != "port_busy" || !strings.Contains(res.Message, "screen") {
		t.Fatalf("first = %+v", res)
	}
	if rt := a.terminalRuntime()["s1"]; rt.LastError != res.Message {
		t.Fatalf("lastErr = %q", rt.LastError)
	}
	fail.Store(false)
	mustStart(t, a, "TERM-2", "s1", 80, 24)
	if rt := a.terminalRuntime()["s1"]; rt.LastError != "" {
		t.Fatalf("lastErr after success = %q", rt.LastError)
	}
}

func TestSerialReservationReleasedOnClose(t *testing.T) {
	withFakeSerial(t, "darwin", func(string, *serial.Mode) (serialPort, error) { return newFakeSerialPort(), nil }, nil)
	a, _ := newSerialTestApp(t, serialConn("a", "A", "/dev/cu.usbserial-10"), serialConn("b", "B", "/dev/tty.usbserial-10"))
	mustStart(t, a, "TERM-a", "a", 80, 24)
	a.CloseTerminal("TERM-a")
	mustStart(t, a, "TERM-b", "b", 80, 24)
}

func TestSerialWindowsAliases(t *testing.T) {
	withFakeSerial(t, "windows", openFake(newFakeSerialPort()), nil)
	a, _ := newSerialTestApp(t, serialConn("a", "A", "COM3"), serialConn("b", "B", `\\.\com3`))
	mustStart(t, a, "TERM-a", "a", 80, 24)
	if res := a.StartTerminal("TERM-b", "b", 80, 24); res.Code != "port_in_use" {
		t.Fatalf("alias = %+v", res)
	}
}

func TestSerialLinuxByIDSymlink(t *testing.T) {
	env := withFakeSerial(t, "linux", openFake(newFakeSerialPort()), nil)
	byID := "/dev/serial/by-id/usb-FTDI_FT232R-if00-port0"
	env.symlinks[byID] = "/dev/ttyUSB0"
	a, _ := newSerialTestApp(t, serialConn("a", "A", byID), serialConn("b", "B", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-a", "a", 80, 24)
	if res := a.StartTerminal("TERM-b", "b", 80, 24); res.Code != "port_in_use" {
		t.Fatalf("symlink = %+v", res)
	}
}

func TestSerialUnplugDeviceRemoved(t *testing.T) {
	port := newFakeSerialPort()
	withFakeSerial(t, "darwin", openFake(port), presenceSeq(false))
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.usbserial-10"))
	mustStart(t, a, "TERM-u", "s1", 80, 24)
	changed := countSessionsChanged(a)
	port.reads <- fakeRead{err: syscall.Errno(6)}
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-u")) > 0 })
	a.terminalLoops.Wait()
	exits := rec.exits("TERM-u")
	if len(exits) != 1 || exits[0].Reason != exitReasonDeviceRemoved || !strings.Contains(exits[0].Message, "/dev/cu.usbserial-10") {
		t.Fatalf("exits = %+v", exits)
	}
	if len(a.ListTerminalSessions()) != 0 {
		t.Fatal("tab still listed")
	}
	a.termMu.Lock()
	owners := len(a.serialOwners)
	lastErr := a.lastErr["s1"]
	a.termMu.Unlock()
	if owners != 0 || lastErr != exits[0].Message || changed() == 0 || !port.closed.Load() {
		t.Fatalf("owners=%d lastErr=%q changed=%d closed=%v", owners, lastErr, changed(), port.closed.Load())
	}
}

func TestSerialLinuxZeroReadIsRemoval(t *testing.T) {
	port := newFakeSerialPort()
	env := withFakeSerial(t, "linux", openFake(port), presenceSeq(true, true, false))
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-z", "s1", 80, 24)
	close(port.reads) // PortClosed, as the library reports a 0-byte read
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-z")) > 0 })
	if exits := rec.exits("TERM-z"); exits[0].Reason != exitReasonDeviceRemoved || env.PresenceCalls() != 3 {
		t.Fatalf("exits = %+v, presence calls %d", exits, env.PresenceCalls())
	}
}

func TestSerialIOErrorWhenStillPresent(t *testing.T) {
	port := newFakeSerialPort()
	withFakeSerial(t, "linux", openFake(port), nil)
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-io", "s1", 80, 24)
	port.reads <- fakeRead{err: syscall.Errno(5)}
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-io")) > 0 })
	if exits := rec.exits("TERM-io"); exits[0].Reason != exitReasonIOError {
		t.Fatalf("exits = %+v", exits)
	}
}

func TestSerialUserCloseNotMisclassified(t *testing.T) {
	port := newFakeSerialPort()
	env := withFakeSerial(t, "linux", openFake(port), nil)
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-c", "s1", 80, 24)
	a.CloseTerminal("TERM-c")
	a.terminalLoops.Wait()
	exits := rec.exits("TERM-c")
	if len(exits) != 1 || exits[0].Reason != exitReasonUser || env.PresenceCalls() != 0 {
		t.Fatalf("exits = %+v, presence calls %d", exits, env.PresenceCalls())
	}
}

func TestSerialWriteErrorClassified(t *testing.T) {
	port := newFakeSerialPort()
	port.writeErr = syscall.Errno(6)
	withFakeSerial(t, "darwin", openFake(port), presenceSeq(false))
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.x"))
	mustStart(t, a, "TERM-w", "s1", 80, 24)
	mustSend(t, a, "TERM-w", "hello")
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-w")) > 0 })
	a.terminalLoops.Wait()
	if exits := rec.exits("TERM-w"); len(exits) != 1 || exits[0].Reason != exitReasonDeviceRemoved {
		t.Fatalf("exits = %+v", exits)
	}
}

func TestSerialOutputBeforeErrorDelivered(t *testing.T) {
	port := newFakeSerialPort()
	withFakeSerial(t, "linux", openFake(port), nil)
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-o", "s1", 80, 24)
	port.reads <- fakeRead{data: []byte("abc")}
	port.reads <- fakeRead{err: syscall.Errno(5)}
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-o")) > 0 })
	rec.mu.Lock()
	dataAt, exitAt := -1, -1
	for i, e := range rec.events {
		if e.name == terminalDataEvent && dataAt < 0 {
			dataAt = i
		}
		if e.name == terminalExitEvent {
			exitAt = i
		}
	}
	rec.mu.Unlock()
	if rec.data("TERM-o") != "abc" || dataAt < 0 || dataAt > exitAt {
		t.Fatalf("data %q at %d, exit at %d", rec.data("TERM-o"), dataAt, exitAt)
	}
}

func TestSerialWritesReachPort(t *testing.T) {
	port := newFakeSerialPort()
	withFakeSerial(t, "linux", openFake(port), nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-in", "s1", 80, 24)
	mustSend(t, a, "TERM-in", "ab")
	mustSend(t, a, "TERM-in", "cd")
	waitUntil(t, "written", func() bool {
		port.mu.Lock()
		defer port.mu.Unlock()
		return port.written.String() == "abcd"
	})
}

// ---- §9.5 in-place apply ----

func openSerialTab(t *testing.T, goos string, conn Connection) (*App, *eventRecorder, *fakeSerialPort, string) {
	t.Helper()
	port := newFakeSerialPort()
	withFakeSerial(t, goos, openFake(port), nil)
	a, rec := newSerialTestApp(t, conn)
	id := "TERM-" + conn.ID
	mustStart(t, a, id, conn.ID, 80, 24)
	return a, rec, port, id
}

func TestApplySerialSettingsInPlace(t *testing.T) {
	a, _, port, _ := openSerialTab(t, "linux", serialConn("s1", "board", "/dev/ttyUSB0"))
	changed := countSessionsChanged(a)
	applied, err := a.applySerialSettings("s1", settingsOf(9600, 7, "even", "1"))
	if !applied || err != nil {
		t.Fatalf("apply = %v, %v", applied, err)
	}
	modes := port.Modes()
	if len(modes) != 1 || modes[0].BaudRate != 9600 || modes[0].DataBits != 7 || modes[0].Parity != serial.EvenParity {
		t.Fatalf("modes = %+v", modes)
	}
	infos := a.ListTerminalSessions()
	if port.closed.Load() || infos[0].Baud != 9600 || infos[0].SerialMode != "7E1" || changed() != 1 {
		t.Fatalf("closed=%v infos=%+v changed=%d", port.closed.Load(), infos, changed())
	}
}

func TestApplySerialSettingsSameIsNoop(t *testing.T) {
	a, _, port, _ := openSerialTab(t, "linux", serialConn("s1", "board", "/dev/ttyUSB0"))
	changed := countSessionsChanged(a)
	applied, err := a.applySerialSettings("s1", settingsOf(115200, 8, "none", "1"))
	if !applied || err != nil || len(port.Modes()) != 0 || changed() != 0 {
		t.Fatalf("apply = %v, %v; modes %v; changed %d", applied, err, port.Modes(), changed())
	}
}

func TestApplySerialSettingsNoTab(t *testing.T) {
	withFakeSerial(t, "linux", openFake(newFakeSerialPort()), nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	if applied, err := a.applySerialSettings("s1", settingsOf(9600, 8, "none", "1")); applied || err != nil {
		t.Fatalf("apply = %v, %v", applied, err)
	}
}

func TestApplySerialSettingsRejectsInvalid(t *testing.T) {
	a, _, port, _ := openSerialTab(t, "darwin", serialConn("s1", "board", "/dev/cu.x"))
	if applied, err := a.applySerialSettings("s1", settingsOf(9600, 8, "mark", "1")); applied || err == nil || len(port.Modes()) != 0 {
		t.Fatalf("apply = %v, %v; modes %v", applied, err, port.Modes())
	}
}

func TestApplySerialSettingsWaitsForWrites(t *testing.T) {
	a, _, port, id := openSerialTab(t, "linux", serialConn("s1", "board", "/dev/ttyUSB0"))
	port.mu.Lock()
	port.writeDelay = 50 * time.Millisecond
	port.mu.Unlock()
	mustSend(t, a, id, "x")
	waitUntil(t, "write begins", func() bool { return len(port.Ops()) > 0 })
	if _, err := a.applySerialSettings("s1", settingsOf(9600, 8, "none", "1")); err != nil {
		t.Fatal(err)
	}
	ops := strings.Join(port.Ops(), ",")
	if ops != "write-begin,write-end,drain,setmode" {
		t.Fatalf("ops = %s", ops)
	}
}

func TestApplySerialSettingsDrainTimeout(t *testing.T) {
	a, _, port, _ := openSerialTab(t, "linux", serialConn("s1", "board", "/dev/ttyUSB0"))
	port.mu.Lock()
	port.drainDur = time.Second
	port.mu.Unlock()
	serialDrainTimeout = 20 * time.Millisecond
	start := time.Now()
	if _, err := a.applySerialSettings("s1", settingsOf(9600, 8, "none", "1")); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond || len(port.Modes()) != 1 {
		t.Fatalf("elapsed %v, modes %v", elapsed, port.Modes())
	}
}

func TestApplySerialSettingsRollbackUnix(t *testing.T) {
	a, rec, port, id := openSerialTab(t, "linux", serialConn("s1", "board", "/dev/ttyUSB0"))
	port.mu.Lock()
	port.setErr = fakePortError{serial.InvalidSpeed}
	port.mu.Unlock()
	applied, err := a.applySerialSettings("s1", settingsOf(250000, 8, "none", "1"))
	if applied || err == nil {
		t.Fatalf("apply = %v, %v", applied, err)
	}
	modes := port.Modes()
	if len(modes) != 2 || modes[0].BaudRate != 250000 || modes[1].BaudRate != 115200 {
		t.Fatalf("modes = %+v, want [new, old]", modes)
	}
	infos := a.ListTerminalSessions()
	if len(infos) != 1 || infos[0].Baud != 115200 || infos[0].SerialMode != "8N1" || len(rec.exits(id)) != 0 {
		t.Fatalf("infos = %+v exits = %+v", infos, rec.exits(id))
	}
}

func TestApplySerialSettingsWindowsFailureCloses(t *testing.T) {
	a, rec, port, id := openSerialTab(t, "windows", serialConn("s1", "board", "COM3"))
	port.mu.Lock()
	port.setErr = fakePortError{serial.InvalidSerialPort}
	port.mu.Unlock()
	if _, err := a.applySerialSettings("s1", settingsOf(9600, 8, "none", "1")); err == nil {
		t.Fatal("want error")
	}
	a.terminalLoops.Wait()
	exits := rec.exits(id)
	if len(exits) != 1 || exits[0].Reason != exitReasonIOError || len(port.Modes()) != 1 {
		t.Fatalf("exits = %+v modes = %+v", exits, port.Modes())
	}
}

func TestApplySerialSettingsWhileConnecting(t *testing.T) {
	port := newFakeSerialPort()
	open, entered, release := gatedOpen(port)
	env := withFakeSerial(t, "linux", open, nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	done := make(chan ActionResult, 1)
	go func() { done <- a.StartTerminal("TERM-cn", "s1", 80, 24) }()
	<-entered
	if applied, err := a.applySerialSettings("s1", settingsOf(9600, 7, "even", "1")); !applied || err != nil {
		t.Fatalf("apply while connecting = %v, %v", applied, err)
	}
	close(release)
	if res := <-done; !res.OK || !strings.Contains(res.Message, "9600 7E1") {
		t.Fatalf("start = %+v", res)
	}
	if env.openModes[0].BaudRate != 115200 {
		t.Fatalf("open mode = %+v, want the old settings", env.openModes[0])
	}
	if modes := port.Modes(); len(modes) != 1 || modes[0].BaudRate != 9600 {
		t.Fatalf("modes = %+v", modes)
	}
	if infos := a.ListTerminalSessions(); infos[0].SerialMode != "7E1" || infos[0].Baud != 9600 {
		t.Fatalf("infos = %+v", infos)
	}
}

func TestApplySerialSettingsRace(t *testing.T) {
	for i := range 20 {
		port := newFakeSerialPort()
		withFakeSerial(t, "linux", openFake(port), presenceSeq(false))
		a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
		id := fmt.Sprintf("TERM-race-%d", i)
		mustStart(t, a, id, "s1", 80, 24)
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); _, _ = a.applySerialSettings("s1", settingsOf(9600, 8, "none", "1")) }()
		go func() { defer wg.Done(); _, _ = a.applySerialSettings("s1", settingsOf(19200, 7, "odd", "2")) }()
		go func() { defer wg.Done(); a.CloseTerminal(id) }()
		go func() { defer wg.Done(); port.reads <- fakeRead{err: syscall.Errno(5)} }()
		wg.Wait()
		waitUntil(t, "exit", func() bool { return len(rec.exits(id)) > 0 })
		a.terminalLoops.Wait()
		if exits := rec.exits(id); len(exits) != 1 {
			t.Fatalf("exits = %+v, want one", exits)
		}
	}
}

// ---- §7.3 afterSerialSave, §7.4 live state ----

func TestAfterSerialSave(t *testing.T) {
	base := serialConn("s1", "board", "/dev/ttyUSB0")
	base.DataBits, base.Parity, base.StopBits = 8, "none", "1"
	ok := ActionResult{OK: true, Message: "保存しました", ID: "s1"}
	even := base
	even.Parity = "even"

	t.Run("S2 applied", func(t *testing.T) {
		a, _, port, _ := openSerialTab(t, "linux", base)
		r := a.afterSerialSave(base, even, ok)
		if !r.OK || r.Code != "serial_mode_applied" || !strings.Contains(r.Message, "115200 8E1") {
			t.Fatalf("r = %+v", r)
		}
		if m := port.Modes(); len(m) != 1 || m[0].Parity != serial.EvenParity {
			t.Fatalf("modes = %+v", m)
		}
	})
	t.Run("S4 failure", func(t *testing.T) {
		a, _, port, id := openSerialTab(t, "linux", base)
		port.mu.Lock()
		port.setErr = errors.New("ioctl failed")
		port.mu.Unlock()
		r := a.afterSerialSave(base, even, ok)
		if !r.OK || r.Code != "serial_mode_not_applied" || !strings.Contains(r.Message, "再接続すると反映されます") {
			t.Fatalf("r = %+v", r)
		}
		if len(a.ListTerminalSessions()) != 1 || a.ListTerminalSessions()[0].ID != id {
			t.Fatal("tab closed on unix failure")
		}
	})
	t.Run("S5 windows failure", func(t *testing.T) {
		win := base
		win.SerialPort = "COM3"
		winEven := win
		winEven.Parity = "even"
		a, _, port, _ := openSerialTab(t, "windows", win)
		port.mu.Lock()
		port.setErr = errors.New("SetCommState failed")
		port.mu.Unlock()
		r := a.afterSerialSave(win, winEven, ok)
		if r.Code != "serial_mode_not_applied" || !strings.Contains(r.Message, "ターミナルは閉じられました") {
			t.Fatalf("r = %+v", r)
		}
	})
	t.Run("S1 S6 S7 S8", func(t *testing.T) {
		a, _, port, _ := openSerialTab(t, "linux", base)
		if r := a.afterSerialSave(base, base, ok); r.Code != "" || r.Message != ok.Message {
			t.Fatalf("S1 = %+v", r)
		}
		moved := base
		moved.SerialPort = "/dev/ttyUSB1"
		if r := a.afterSerialSave(base, moved, ok); r.Code != "reconnect_required" || !strings.Contains(r.Message, "/dev/ttyUSB0") {
			t.Fatalf("S7 = %+v", r)
		}
		ssh := Connection{ID: "s1", Name: "board", Type: "SSH", Host: "h", User: "u"}
		if r := a.afterSerialSave(base, ssh, ok); r.Code != "reconnect_required" {
			t.Fatalf("S8 = %+v", r)
		}
		if len(port.Modes()) != 0 {
			t.Fatalf("SetMode called: %+v", port.Modes())
		}
		other := serialConn("s9", "idle", "/dev/ttyUSB9")
		otherEven := other
		otherEven.Parity = "even"
		other.ID, otherEven.ID = "s9", "s9"
		if r := a.afterSerialSave(other, otherEven, ok); r.Code != "" {
			t.Fatalf("S6 = %+v", r)
		}
		if r := a.afterSerialSave(Connection{}, base, ok); r.Code != "" {
			t.Fatalf("new connection = %+v", r)
		}
	})
}

func TestSerialConnStates(t *testing.T) {
	var present atomic.Bool
	present.Store(true)
	withFakeSerial(t, "darwin", openFake(newFakeSerialPort()), func(string, string) (bool, error) { return present.Load(), nil })
	serialPresenceTTL = 0
	a, _ := newSerialTestApp(t, serialConn("a", "A", "/dev/cu.usbserial-10"), serialConn("b", "B", "/dev/tty.usbserial-10"),
		Connection{ID: "h", Name: "ssh", Type: "SSH"})
	conns := func() []Connection { a.mu.RLock(); defer a.mu.RUnlock(); return append([]Connection(nil), a.conns...) }
	if st := a.serialConnStates("darwin", conns()); st["a"] != connStateAvailable || st["b"] != connStateAvailable || st["h"] != "" {
		t.Fatalf("present = %v", st)
	}
	present.Store(false)
	if st := a.serialConnStates("darwin", conns()); st["a"] != connStateMissing {
		t.Fatalf("missing = %v", st)
	}
	present.Store(true)
	mustStart(t, a, "TERM-a", "a", 80, 24)
	if st := a.serialConnStates("darwin", conns()); st["b"] != connStateInUse || st["a"] != connStateAvailable {
		t.Fatalf("other holds the device = %v", st)
	}
	serialPortPresent = func(string, string) (bool, error) { return false, errors.New("probe") }
	a.presenceMu.Lock()
	a.presence = nil
	a.presenceMu.Unlock()
	if st := a.serialConnStates("darwin", conns()); st["a"] != connStateDisconnected {
		t.Fatalf("probe error = %v", st)
	}
}

func TestSerialPresenceCached(t *testing.T) {
	env := withFakeSerial(t, "linux", nil, nil)
	serialPresenceTTL = time.Hour
	a, _ := newSerialTestApp(t)
	for range 10 {
		got := a.serialPresence("linux", []string{"/dev/ttyUSB0", "/dev/ttyUSB1", "/dev/ttyUSB0"})
		if !got["/dev/ttyUSB0"] || !got["/dev/ttyUSB1"] {
			t.Fatalf("presence = %v", got)
		}
	}
	if n := env.PresenceCalls(); n != 2 {
		t.Fatalf("present calls = %d, want one per port", n)
	}
}

func TestSerialPresenceTickEmitsOnChange(t *testing.T) {
	var present atomic.Bool
	present.Store(true)
	withFakeSerial(t, "linux", nil, func(string, string) (bool, error) { return present.Load(), nil })
	serialPresenceTTL = 0
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	prev, changed := a.serialPresenceTick("linux", nil)
	if changed || !prev["/dev/ttyUSB0"] {
		t.Fatalf("first tick changed=%v prev=%v", changed, prev)
	}
	if prev, changed = a.serialPresenceTick("linux", prev); changed {
		t.Fatal("no change reported as change")
	}
	present.Store(false)
	if prev, changed = a.serialPresenceTick("linux", prev); !changed {
		t.Fatal("unplug not reported")
	}
	if _, changed = a.serialPresenceTick("linux", prev); changed {
		t.Fatal("still unplugged reported again")
	}
	empty, _ := newSerialTestApp(t)
	if next, changed := empty.serialPresenceTick("linux", nil); changed || next != nil {
		t.Fatalf("no serial connections: %v %v", next, changed)
	}
}

func TestSerialReplugClearsLastError(t *testing.T) {
	port := newFakeSerialPort()
	var present atomic.Bool
	present.Store(true)
	withFakeSerial(t, "darwin", openFake(port), func(string, string) (bool, error) { return present.Load(), nil })
	serialPresenceTTL = 0
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.usbserial-10"))
	prev, _ := a.serialPresenceTick("darwin", nil)
	mustStart(t, a, "TERM-r", "s1", 80, 24)
	present.Store(false)
	port.reads <- fakeRead{err: syscall.Errno(6)}
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-r")) > 0 })
	a.terminalLoops.Wait()
	if rec.exits("TERM-r")[0].Reason != exitReasonDeviceRemoved || a.terminalRuntime()["s1"].LastError == "" {
		t.Fatalf("exit = %+v", rec.exits("TERM-r"))
	}
	prev, _ = a.serialPresenceTick("darwin", prev)
	if a.terminalRuntime()["s1"].LastError == "" {
		t.Fatal("lastErr cleared while still unplugged")
	}
	present.Store(true)
	prev, changed := a.serialPresenceTick("darwin", prev)
	if !changed || a.terminalRuntime()["s1"].LastError != "" {
		t.Fatalf("replug: changed=%v lastErr=%q", changed, a.terminalRuntime()["s1"].LastError)
	}
	conns := []Connection{serialConn("s1", "board", "/dev/cu.usbserial-10")}
	if st := a.serialConnStates("darwin", conns); st["s1"] != connStateAvailable {
		t.Fatalf("state = %v", st)
	}
	if _, changed = a.serialPresenceTick("darwin", prev); changed {
		t.Fatal("steady state reported as change")
	}
}

// A key missing from prev (first tick after startup, a new or re-pointed
// port) is not a replug: an open failure recorded then must stay visible.
func TestSerialPresenceTickKeepsErrorWithoutReplug(t *testing.T) {
	withFakeSerial(t, "linux", nil, func(string, string) (bool, error) { return true, nil })
	serialPresenceTTL = 0
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	setErr := func() {
		a.termMu.Lock()
		a.ensureMaps()
		a.lastErr["s1"] = "permission denied"
		a.termMu.Unlock()
	}
	setErr()
	prev, changed := a.serialPresenceTick("linux", nil)
	if changed || a.terminalRuntime()["s1"].LastError == "" {
		t.Fatalf("first tick: changed=%v lastErr=%q", changed, a.terminalRuntime()["s1"].LastError)
	}
	if _, changed = a.serialPresenceTick("linux", prev); changed || a.terminalRuntime()["s1"].LastError == "" {
		t.Fatalf("steady present: changed=%v lastErr=%q", changed, a.terminalRuntime()["s1"].LastError)
	}
	// Re-pointed port: the previous round only knew another key.
	if _, changed = a.serialPresenceTick("linux", map[string]bool{"/dev/ttyUSB9": true}); a.terminalRuntime()["s1"].LastError == "" {
		t.Fatalf("new key cleared lastErr (changed=%v)", changed)
	}
}

func TestTestSerialConnectionDoesNotOpen(t *testing.T) {
	var present atomic.Bool
	present.Store(true)
	env := withFakeSerial(t, "darwin", openFake(newFakeSerialPort()), func(string, string) (bool, error) { return present.Load(), nil })
	a, _ := newSerialTestApp(t, serialConn("a", "A", "/dev/cu.usbserial-10"), serialConn("b", "B", "/dev/tty.usbserial-10"))
	if r := a.testSerialConnection(serialConn("a", "A", "/dev/cu.usbserial-10")); !r.OK || r.Code != "available" {
		t.Fatalf("available = %+v", r)
	}
	present.Store(false)
	if r := a.testSerialConnection(serialConn("a", "A", "/dev/cu.usbserial-10")); r.OK || r.Code != "missing" {
		t.Fatalf("missing = %+v", r)
	}
	if len(env.Opened()) != 0 {
		t.Fatalf("test opened the port: %v", env.Opened())
	}
	present.Store(true)
	mustStart(t, a, "TERM-a", "a", 80, 24)
	if r := a.testSerialConnection(serialConn("b", "B", "/dev/tty.usbserial-10")); !r.OK || r.Code != "in_use" || !strings.Contains(r.Message, "A") {
		t.Fatalf("in_use = %+v", r)
	}
	if r := a.testSerialConnection(serialConn("a", "A", "/dev/cu.usbserial-10")); r.Code != "in_use" {
		t.Fatalf("own tab = %+v", r)
	}
	if len(env.Opened()) != 1 {
		t.Fatalf("opened = %v", env.Opened())
	}
}

// ---- audit-2 N1: no port access after close ----

func TestSerialHandleRefusesAfterClose(t *testing.T) {
	port := newFakeSerialPort()
	h := &serialHandle{port: port, settings: settingsOf(115200, 8, "none", "1")}
	if _, err := h.Write([]byte("a")); err != nil {
		t.Fatalf("write before close: %v", err)
	}
	h.closePort()
	h.closePort() // idempotent
	if n, err := h.Write([]byte("rm -rf ~\n")); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after close = %d, %v", n, err)
	}
	if got := port.opsAfterClose.Load(); got != 0 {
		t.Fatalf("port touched after close %d times", got)
	}
	if port.written.String() != "a" {
		t.Fatalf("written = %q", port.written.String())
	}
}

// Input queued when the tab is closed must not reach the port once Close has
// run: on unix the fd number may already be a socket or the DB file.
func TestSerialCloseStopsQueuedInput(t *testing.T) {
	for round := 0; round < 5; round++ {
		port := newFakeSerialPort()
		port.writeDelay = 2 * time.Millisecond
		port.closeDelay = 30 * time.Millisecond
		withFakeSerial(t, "darwin", openFake(port), nil)
		a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.usbserial-10"))
		id := fmt.Sprintf("TERM-n1-%d", round)
		mustStart(t, a, id, "s1", 80, 24)
		for i := 0; i < 20; i++ {
			mustSend(t, a, id, "x")
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			a.CloseTerminal(id)
		}()
		var lateOK atomic.Int32
		go func() {
			defer wg.Done()
			// Keystrokes that race the close: none may be accepted once
			// the port is closed.
			for i := 0; i < 50; i++ {
				if port.closed.Load() {
					if r := a.SendTerminalInput(id, "rm -rf ~\n"); r.OK {
						lateOK.Add(1)
					}
				} else {
					_ = a.SendTerminalInput(id, "y")
				}
				time.Sleep(time.Millisecond)
			}
		}()
		wg.Wait()
		a.terminalLoops.Wait()
		if got := port.opsAfterClose.Load(); got != 0 {
			t.Fatalf("round %d: %d port operations after Close", round, got)
		}
		if got := lateOK.Load(); got != 0 {
			t.Fatalf("round %d: %d inputs accepted after Close", round, got)
		}
		if exits := rec.exits(id); len(exits) != 1 || exits[0].Reason != exitReasonUser {
			t.Fatalf("round %d: exits = %+v", round, exits)
		}
	}
}

// An in-place settings change racing the close must not SetMode / Drain a
// closed port.
func TestSerialApplyAfterCloseDoesNotTouchPort(t *testing.T) {
	port := newFakeSerialPort()
	withFakeSerial(t, "linux", openFake(port), nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	mustStart(t, a, "TERM-ap", "s1", 80, 24)
	a.termMu.Lock()
	ts := a.terms["TERM-ap"]
	a.termMu.Unlock()
	ts.serial.closePort()
	if ok, err := a.applyNow(ts, settingsOf(9600, 8, "none", "1")); ok || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("applyNow after close = %v, %v", ok, err)
	}
	if got := port.opsAfterClose.Load(); got != 0 {
		t.Fatalf("port touched after close %d times", got)
	}
}

// ---- unplug and replug within one watchSerialPresence interval ----

func TestSerialQuickReplugClearsLastError(t *testing.T) {
	port := newFakeSerialPort()
	var present atomic.Bool
	present.Store(true)
	withFakeSerial(t, "darwin", openFake(port), func(string, string) (bool, error) { return present.Load(), nil })
	serialPresenceTTL = 0
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.usbserial-10"))
	prev, _ := a.serialPresenceTick("darwin", nil)
	mustStart(t, a, "TERM-q", "s1", 80, 24)
	present.Store(false)
	port.reads <- fakeRead{err: syscall.Errno(6)}
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-q")) > 0 })
	a.terminalLoops.Wait()
	if rec.exits("TERM-q")[0].Reason != exitReasonDeviceRemoved || a.terminalRuntime()["s1"].LastError == "" {
		t.Fatalf("exit = %+v", rec.exits("TERM-q"))
	}
	// Plugged back in before the next tick: no tick ever saw it missing.
	present.Store(true)
	prev, changed := a.serialPresenceTick("darwin", prev)
	if !changed || a.terminalRuntime()["s1"].LastError != "" {
		t.Fatalf("quick replug: changed=%v lastErr=%q", changed, a.terminalRuntime()["s1"].LastError)
	}
	conns := []Connection{serialConn("s1", "board", "/dev/cu.usbserial-10")}
	if st := a.serialConnStates("darwin", conns); st["s1"] != connStateAvailable {
		t.Fatalf("state = %v", st)
	}
	if _, changed = a.serialPresenceTick("darwin", prev); changed {
		t.Fatal("steady state reported as change")
	}
}

// A presence result cached before the removal says nothing about a replug.
func TestSerialQuickReplugIgnoresStaleCache(t *testing.T) {
	port := newFakeSerialPort()
	var present atomic.Bool
	present.Store(true)
	withFakeSerial(t, "darwin", openFake(port), func(string, string) (bool, error) { return present.Load(), nil })
	serialPresenceTTL = time.Hour
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.usbserial-10"))
	prev, _ := a.serialPresenceTick("darwin", nil) // caches present
	mustStart(t, a, "TERM-st", "s1", 80, 24)
	present.Store(false)
	port.reads <- fakeRead{err: syscall.Errno(6)}
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-st")) > 0 })
	a.terminalLoops.Wait()
	if _, _ = a.serialPresenceTick("darwin", prev); a.terminalRuntime()["s1"].LastError == "" {
		t.Fatal("stale cached presence cleared device_removed")
	}
	// Once the cache expires, a fresh probe that sees the device clears it.
	present.Store(true)
	serialPresenceTTL = 0
	if _, changed := a.serialPresenceTick("darwin", prev); !changed || a.terminalRuntime()["s1"].LastError != "" {
		t.Fatalf("fresh probe: changed=%v lastErr=%q", changed, a.terminalRuntime()["s1"].LastError)
	}
}

// A newer error that replaced device_removed is not cleared by the mark.
func TestSerialGoneMarkOnlyForSameError(t *testing.T) {
	port := newFakeSerialPort()
	var present atomic.Bool
	present.Store(true)
	withFakeSerial(t, "darwin", openFake(port), func(string, string) (bool, error) { return present.Load(), nil })
	serialPresenceTTL = 0
	a, rec := newSerialTestApp(t, serialConn("s1", "board", "/dev/cu.usbserial-10"))
	prev, _ := a.serialPresenceTick("darwin", nil)
	mustStart(t, a, "TERM-g", "s1", 80, 24)
	present.Store(false)
	port.reads <- fakeRead{err: syscall.Errno(6)}
	waitUntil(t, "exit", func() bool { return len(rec.exits("TERM-g")) > 0 })
	a.terminalLoops.Wait()
	present.Store(true)
	a.termMu.Lock()
	a.lastErr["s1"] = "permission denied"
	a.termMu.Unlock()
	if _, _ = a.serialPresenceTick("darwin", prev); a.terminalRuntime()["s1"].LastError != "permission denied" {
		t.Fatalf("lastErr = %q", a.terminalRuntime()["s1"].LastError)
	}
}

// ---- legacy rows that kept the port name in Host ----

func TestSerialLegacyHostPort(t *testing.T) {
	port := newFakeSerialPort()
	env := withFakeSerial(t, "darwin", openFake(port), nil)
	legacy := Connection{ID: "old", Name: "old board", Type: "Serial", Host: " /dev/tty.usbserial-OLD ", BaudRate: 9600}
	a, _ := newSerialTestApp(t, legacy)
	mustStart(t, a, "TERM-legacy", "old", 80, 24)
	if got := env.Opened(); len(got) != 1 || got[0] != "/dev/cu.usbserial-OLD" {
		t.Fatalf("opened = %v", got)
	}
	infos := a.ListTerminalSessions()
	if len(infos) != 1 || infos[0].Port != "/dev/tty.usbserial-OLD" {
		t.Fatalf("infos = %+v", infos)
	}
	if r := a.testSerialConnection(legacy); r.Code != "in_use" {
		t.Fatalf("test = %+v", r)
	}
	if st := a.serialConnStates("darwin", []Connection{legacy}); st["old"] == connStateMissing {
		t.Fatalf("state = %v", st)
	}
	// SerialPort wins when both are set; SSH rows never use Host as a port.
	if got := serialPortOf(Connection{Type: "Serial", Host: "/dev/a", SerialPort: "/dev/b"}); got != "/dev/b" {
		t.Fatalf("serialPortOf = %q", got)
	}
	if got := serialPortOf(Connection{Type: "SSH", Host: "web-01"}); got != "" {
		t.Fatalf("ssh serialPortOf = %q", got)
	}
	if r := a.StartTerminal("TERM-none", "none", 80, 24); r.OK {
		t.Fatalf("unknown connection = %+v", r)
	}
}

func TestSerialEmptyPortStillRejected(t *testing.T) {
	withFakeSerial(t, "darwin", openFake(newFakeSerialPort()), nil)
	a, _ := newSerialTestApp(t, Connection{ID: "e", Name: "empty", Type: "Serial"})
	if r := a.StartTerminal("TERM-e", "e", 80, 24); r.OK || r.Code != "invalid_serial_mode" {
		t.Fatalf("empty port = %+v", r)
	}
}

// ---- §9.6 A11 / M2 ----

// TestSaveConnectionNoDeadlock (A11): SaveConnection, opening and closing the
// tab, in-place apply and the read paths that take a.mu and termMu run in
// parallel. If any path held a.mu while waiting for termMu (or the other way
// round) this would hang; the watchdog fails the test with a goroutine dump.
// Run it with -race -count=20.
func TestSaveConnectionNoDeadlock(t *testing.T) {
	withFakeSerial(t, "linux", func(string, *serial.Mode) (serialPort, error) { return newFakeSerialPort(), nil }, nil)
	app := newTestApp(t)
	conn := addTestConnection(t, app, Connection{Name: "uart", Type: "Serial", SerialPort: "/dev/ttyUSB3", BaudRate: 115200})
	t.Cleanup(func() {
		app.CloseAllTerminals()
		app.terminalLoops.Wait()
	})

	const rounds = 30
	parities := []string{"none", "even", "odd"}
	var wg sync.WaitGroup
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				f(i)
			}
		}()
	}
	run(func(i int) { // save: in-place apply on the open tab, or nothing
		edit := conn
		edit.Parity = parities[i%len(parities)]
		edit.BaudRate = []int{9600, 115200}[i%2]
		if r := app.SaveConnection(edit); !r.OK {
			t.Errorf("save %d: %+v", i, r)
		}
	})
	run(func(i int) { // save a rename, which also rewrites a.conns
		edit := conn
		edit.Description = fmt.Sprintf("round %d", i)
		_ = app.SaveConnection(edit)
	})
	run(func(i int) { // open / close the tab
		id := fmt.Sprintf("TERM-nd-%d", i)
		if r := app.StartTerminal(id, conn.ID, 80, 24); r.OK {
			_ = app.SendTerminalInput(id, "x")
			app.CloseTerminal(id)
		}
	})
	run(func(i int) { // open a second tab and close everything by connection
		if r := app.StartTerminal(fmt.Sprintf("TERM-nd2-%d", i), conn.ID, 80, 24); r.OK {
			app.CloseTerminalsByConnection(conn.ID)
		}
	})
	run(func(i int) {
		_, _ = app.applySerialSettings(conn.ID, settingsOf([]int{19200, 115200}[i%2], 8, "none", "1"))
	})
	run(func(int) {
		_ = app.ListTerminalSessions()
		_ = app.GetInitialData()
	})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		// t.Fatalf, or a panic here, would run the cleanups, which need the
		// same locks and hang. Dump every goroutine and crash the process
		// from a goroutine tRunner does not recover.
		buf := make([]byte, 1<<22)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "%s\n", buf[:n])
		go panic("TestSaveConnectionNoDeadlock: SaveConnection / tab open-close / apply deadlocked")
		select {}
	}
	// The app is still usable afterwards.
	if r := app.SaveConnection(conn); !r.OK {
		t.Fatalf("save after the race: %+v", r)
	}
}

// TestMCPNeverReturnsTerminalOutput (M2): what a serial tab prints, and what
// is typed into it, never reaches an MCP agent through any tool.
func TestMCPNeverReturnsTerminalOutput(t *testing.T) {
	const marker = "secret-xyz"
	port := newFakeSerialPort()
	withFakeSerial(t, "linux", openFake(port), nil)
	isolateEnv(t)
	t.Setenv("SSH_GETE_NO_SEED", "1") // only the two rows below
	app := NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	rec := &eventRecorder{}
	app.emit = rec.emit
	conn := addTestConnection(t, app, Connection{Name: "uart", Type: "Serial", SerialPort: "/dev/ttyUSB4", BaudRate: 115200,
		Description: "board console", Tags: []string{"lab"}})
	ssh := addTestConnection(t, app, Connection{Name: "web", Host: "10.9.9.9", User: "u"})
	pending := queueOn(t, app, ssh, "uptime", "agent-m2")
	t.Cleanup(func() {
		app.CloseAllTerminals()
		app.terminalLoops.Wait()
	})

	id := "TERM-m2"
	if r := app.StartTerminal(id, conn.ID, 80, 24); !r.OK {
		t.Fatalf("open: %+v", r)
	}
	port.reads <- fakeRead{data: []byte("login: " + marker + "\r\n")}
	mustSend(t, app, id, "echo "+marker+"\r")
	waitUntil(t, "output reaches the tab", func() bool { return strings.Contains(rec.data(id), marker) })
	waitUntil(t, "input reaches the port", func() bool {
		port.mu.Lock()
		defer port.mu.Unlock()
		return strings.Contains(port.written.String(), marker)
	})

	calls := []struct{ name, args string }{
		{"list_hosts", `{}`},
		{"list_hosts", `{"connected_only":true}`},
		{"list_hosts", `{"tag":"lab"}`},
		{"execute_command", `{"host":"uart","command":"cat /proc/version","agent_name":"agent-m2"}`},
		{"request_command_execution", `{"host":"uart","command":"cat /proc/version","agent_name":"agent-m2"}`},
		{"get_request_status", `{"request_ids":["` + pending.ID + `"]}`},
		{"get_command_history", `{"agent_name":"agent-m2"}`},
		{"get_command_history", `{"host":"uart","agent_name":"agent-m2"}`},
	}
	seen := map[string]bool{}
	for _, c := range calls {
		resp := app.serveTest(t, mcpCall{body: toolCall(c.name, c.args)})
		if resp.Code != 200 {
			t.Fatalf("%s: status=%d body=%s", c.name, resp.Code, resp.Body.String())
		}
		body := resp.Body.String()
		if !strings.Contains(body, `"result"`) {
			t.Fatalf("%s %s: no result: %s", c.name, c.args, body)
		}
		if strings.Contains(body, marker) {
			t.Fatalf("%s %s returned terminal output: %s", c.name, c.args, body)
		}
		seen[c.name] = true
	}
	// The open serial tab is visible to list_hosts (so the tool did look at
	// the terminal state), just never its contents.
	out, _ := callTool(t, app, "list_hosts", `{"connected_only":true}`)
	if hosts, _ := out["hosts"].([]any); len(hosts) != 1 {
		t.Fatalf("connected hosts = %v, want the open serial tab", out["hosts"])
	}
	for _, method := range []string{"tools/list", "initialize"} {
		resp := app.serveTest(t, mcpCall{body: `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{}}`})
		if strings.Contains(resp.Body.String(), marker) {
			t.Fatalf("%s returned terminal output", method)
		}
		seen[method] = true
	}
	for _, name := range []string{"list_hosts", "execute_command", "request_command_execution", "get_request_status", "get_command_history"} {
		if !seen[name] {
			t.Fatalf("tool %s not called", name)
		}
	}
}
