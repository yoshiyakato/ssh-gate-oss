//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// acquireInstanceLock is the single-instance lock of §1.1 on Unix: an
// exclusive, non-blocking flock on <dir>/ssh-gete.lock, held for the life of
// the process. The kernel drops it when the process exits or crashes, so a
// stale lock (or a reused PID) can never block the next start. The file
// keeps the owner's PID for people reading it; it is not removed on release
// (removing a flock file races with a process that just opened it).
func acquireInstanceLock(dir string) (func(), error) {
	path := filepath.Join(dir, "ssh-gete.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			owner := "?"
			if data, rerr := os.ReadFile(path); rerr == nil && len(data) > 0 {
				owner = string(trimNL(data))
			}
			return nil, fmt.Errorf("%w（pid %s、%s）", errHeadlessLocked, owner, path)
		}
		return nil, fmt.Errorf("%s をロックできません: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = f.Truncate(0)
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
