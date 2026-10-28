package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestInstanceLock(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireInstanceLock(dir); !errors.Is(err, errHeadlessLocked) {
		t.Fatalf("second lock in the same process: %v", err)
	}
	release()
	release() // idempotent
	release2, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()

	// A file left behind by a crashed process (any content, even a live
	// PID such as our own) does not block: only a held OS lock does.
	path := filepath.Join(dir, "ssh-gete.lock")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release3, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("stale lock file blocked the start: %v", err)
	}
	if runtime.GOOS != "windows" {
		data, _ := os.ReadFile(path)
		if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
			t.Fatalf("lock file content %q", data)
		}
	}
	release3()
}

// The lock is held across processes and released by the OS when the holder
// dies (no stale lock after a crash).
func TestInstanceLockAcrossProcesses(t *testing.T) {
	if os.Getenv("SSH_GETE_LOCK_CHILD") != "" {
		if _, err := acquireInstanceLock(os.Getenv("SSH_GETE_LOCK_CHILD")); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("locked\n")
		select {} // hold until killed
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstanceLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), "SSH_GETE_LOCK_CHILD="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := out.Read(buf); err != nil || string(buf[:6]) != "locked" {
		_ = cmd.Process.Kill()
		t.Fatalf("child did not lock: %q %v", buf, err)
	}
	if _, err := acquireInstanceLock(dir); !errors.Is(err, errHeadlessLocked) {
		_ = cmd.Process.Kill()
		t.Fatalf("lock held by another process not seen: %v", err)
	}
	_ = cmd.Process.Kill() // a crash: no release
	_ = cmd.Wait()
	release, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("lock not released by the OS after the holder died: %v", err)
	}
	release()
}
