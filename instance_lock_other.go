//go:build !unix && !windows

package main

// acquireInstanceLock falls back to the PID file where the OS has no lock
// that is released on a crash.
func acquireInstanceLock(dir string) (func(), error) { return acquireDataDirLock(dir) }
