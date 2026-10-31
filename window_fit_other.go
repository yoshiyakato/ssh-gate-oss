//go:build !windows

package main

// windowWorkArea is unknown here: the window keeps its wanted size.
func windowWorkArea() (int, int) { return 0, 0 }
