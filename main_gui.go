//go:build !headless

package main

import (
	"fmt"
	"os"
)

// runCLIFromGUI is the GUI build's command line (phase6-headless.md §1.1).
// With no headless flag it reports handled=false and main opens the window
// as before, ignoring whatever arguments the OS passed. With --headless (or
// a one-shot flag such as --check-config) it parses strictly and runs the
// headless mode in this process; Wails is never started, so nothing calls
// the Wails runtime.
//
// The Windows GUI binary has no console, so before printing anything it
// attaches to the parent's console (attachParentConsole, console_windows.go;
// a no-op elsewhere). The headless-only build is a console program and needs
// none. The window path never attaches, so double-clicking shows no console.
func runCLIFromGUI(args []string) (code int, handled bool) {
	opts, err := parseCLI(args, true)
	if err != nil {
		attachParentConsole()
		fmt.Fprintf(os.Stderr, "ssh-gete: %v\n\n%s", err, cliUsage())
		return exitUsage, true
	}
	if !opts.wantsHeadless() {
		return 0, false
	}
	attachParentConsole()
	return runHeadless(opts, embeddedDist()), true
}
