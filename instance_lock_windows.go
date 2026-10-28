//go:build windows

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

// acquireInstanceLock is the single-instance lock of §1.1 on Windows: the
// lock file is opened with share mode 0, so no other process can open it
// while this one runs. Windows closes the handle when the process exits or
// crashes, so a stale file never blocks the next start (the PID-file
// fallback could not tell a dead PID from a live one here). The file keeps
// the owner's PID for people reading it.
func acquireInstanceLock(dir string) (func(), error) {
	path := filepath.Join(dir, "ssh-gete.lock")
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing: this is the lock
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		const errorSharingViolation = syscall.Errno(32)
		const errorLockViolation = syscall.Errno(33)
		if errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation) {
			return nil, fmt.Errorf("%w（%s）", errHeadlessLocked, path)
		}
		return nil, fmt.Errorf("%s をロックできません: %w", path, err)
	}
	f := os.NewFile(uintptr(h), path)
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\r\n"), 0)
	}
	var once sync.Once
	return func() {
		once.Do(func() { _ = f.Close() })
	}, nil
}
