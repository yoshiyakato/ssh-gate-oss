package main

const (
	windowWidth     = 1440
	windowHeight    = 960
	windowMinWidth  = 1000
	windowMinHeight = 560
)

// fitWindowSize returns the start size for a work area of workW x workH
// logical pixels. Wails centers the window in the work area, so a window
// taller than it starts with its title bar above the screen. A zero work area
// means unknown and keeps the wanted size.
func fitWindowSize(workW, workH, width, height, minW, minH int) (int, int) {
	if workW <= 0 || workH <= 0 {
		return width, height
	}
	return max(min(width, workW), minW), max(min(height, workH), minH)
}
