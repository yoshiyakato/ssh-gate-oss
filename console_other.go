//go:build !windows

package main

// attachParentConsole is only needed by the Windows GUI binary; elsewhere
// the process already has the terminal it was started from.
func attachParentConsole() {}
