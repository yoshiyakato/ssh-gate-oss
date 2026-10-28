//go:build windows

package main

import (
	"log"
	"os"
	"syscall"
)

// attachParentConsole gives the Windows GUI binary (linked with
// -H windowsgui, so it starts without a console) the console of the shell
// that started it, so `ssh-gete.exe --headless` from cmd / PowerShell shows
// the banner and errors and receives Ctrl+C (phase6-headless.md §1.1).
// Standard handles that were redirected (to a file or a pipe) are kept.
// Without a parent console (started from Explorer) it does nothing.
func attachParentConsole() {
	stdout, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	stderr, _ := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	stdin, _ := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	redirected := func(h syscall.Handle) bool { return h != 0 && h != syscall.InvalidHandle }
	if redirected(stdout) && redirected(stderr) && redirected(stdin) {
		return
	}
	attach := syscall.NewLazyDLL("kernel32.dll").NewProc("AttachConsole")
	if attach.Find() != nil {
		return
	}
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	if r, _, _ := attach.Call(attachParentProcess); r == 0 {
		return
	}
	if !redirected(stdout) || !redirected(stderr) {
		if out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
			if !redirected(stdout) {
				os.Stdout = out
			}
			if !redirected(stderr) {
				os.Stderr = out
				log.SetOutput(out)
			}
		}
	}
	if !redirected(stdin) {
		if in, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
			os.Stdin = in
		}
	}
}
