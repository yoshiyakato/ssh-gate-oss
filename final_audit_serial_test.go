package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// Final audit #5: a serial write in progress does not hold up the close of
// the tab or an in-place settings change, and the fd is closed only after
// the write in progress has left it.
func TestSerialCloseDoesNotWaitForInflightWrite(t *testing.T) {
	a, _, port, id := openSerialTab(t, "linux", serialConn("s1", "board", "/dev/ttyUSB0"))
	port.mu.Lock()
	port.writeDelay = 2 * time.Second
	port.mu.Unlock()
	mustSend(t, a, id, "x")
	waitUntil(t, "write begins", func() bool { return len(port.Ops()) > 0 })

	start := time.Now()
	a.CloseTerminal(id)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("CloseTerminal took %s behind an in-flight write", elapsed)
	}
	if port.closed.Load() {
		t.Fatal("fd closed while the write was still on it")
	}
	waitUntil(t, "port closed after the write", func() bool { return port.closed.Load() })
	a.terminalLoops.Wait()
	if got := port.opsAfterClose.Load(); got != 0 {
		t.Fatalf("%d port operations after Close", got)
	}
}

// Final audit #5: a long paste at a low baud rate is written in pieces, so
// a settings change waits for one piece, not the whole paste.
func TestSerialApplyDoesNotWaitForWholePaste(t *testing.T) {
	conn := serialConn("s1", "board", "/dev/ttyUSB0")
	conn.BaudRate = 1200 // 12-byte pieces
	a, _, port, id := openSerialTab(t, "linux", conn)
	port.mu.Lock()
	port.writeDelay = 50 * time.Millisecond
	port.mu.Unlock()
	paste := make([]byte, 600) // 50 pieces, about 2.5 s in total
	for i := range paste {
		paste[i] = 'a'
	}
	mustSend(t, a, id, string(paste))
	waitUntil(t, "write begins", func() bool { return len(port.Ops()) > 0 })

	start := time.Now()
	if applied, err := a.applySerialSettings("s1", settingsOf(9600, 8, "none", "1")); !applied || err != nil {
		t.Fatalf("apply = %v, %v", applied, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("applySerialSettings waited %s for the paste", elapsed)
	}
	waitUntil(t, "paste written", func() bool {
		port.mu.Lock()
		defer port.mu.Unlock()
		return port.written.Len() == len(paste)
	})
}

// Final audit #6: settings saved while the tab was connecting do not
// override newer settings applied in place right after it turned open.
func TestSerialStalePendingDoesNotOverrideNewerApply(t *testing.T) {
	port := newFakeSerialPort()
	open, entered, release := gatedOpen(port)
	withFakeSerial(t, "linux", open, nil)
	a, _ := newSerialTestApp(t, serialConn("s1", "board", "/dev/ttyUSB0"))
	started := make(chan ActionResult, 1)
	go func() { started <- a.StartTerminal("TERM-p", "s1", 80, 24) }()
	<-entered
	if applied, err := a.applySerialSettings("s1", settingsOf(9600, 8, "none", "1")); !applied || err != nil {
		t.Fatalf("pending apply = %v, %v", applied, err)
	}
	// The next sessions:changed comes from commitTerminal, after the tab is
	// open and before openSerialTerminal reads pending.
	// A flag, not sync.Once: applyNow emits sessions:changed itself.
	var fired atomic.Bool
	newer := make(chan error, 1)
	a.onSessionsChanged = func() {
		if fired.CompareAndSwap(false, true) {
			_, err := a.applySerialSettings("s1", settingsOf(57600, 8, "none", "1"))
			newer <- err
		}
	}
	close(release)
	if res := <-started; !res.OK {
		t.Fatalf("start = %+v", res)
	}
	if err := <-newer; err != nil {
		t.Fatalf("newer apply: %v", err)
	}
	modes := port.Modes()
	if len(modes) == 0 || modes[len(modes)-1].BaudRate != 57600 {
		t.Fatalf("SetMode order = %+v, want 57600 last", modes)
	}
	for _, info := range a.ListTerminalSessions() {
		if info.ID == "TERM-p" && info.Baud != 57600 {
			t.Fatalf("tab baud = %d, want 57600", info.Baud)
		}
	}
}
