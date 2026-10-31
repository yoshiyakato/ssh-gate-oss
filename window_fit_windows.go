package main

import (
	"syscall"
	"unsafe"
)

// windowWorkArea returns the primary monitor's work area (without the
// taskbar) in logical pixels, or zeros when it cannot be read.
func windowWorkArea() (int, int) {
	const spiGetWorkArea = 0x0030
	var work struct{ Left, Top, Right, Bottom int32 }
	user32 := syscall.NewLazyDLL("user32.dll")
	ok, _, _ := user32.NewProc("SystemParametersInfoW").Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&work)), 0)
	if ok == 0 {
		return 0, 0
	}
	dpi := 96
	if proc := user32.NewProc("GetDpiForSystem"); proc.Find() == nil {
		if v, _, _ := proc.Call(); v != 0 {
			dpi = int(v)
		}
	}
	return int(work.Right-work.Left) * 96 / dpi, int(work.Bottom-work.Top) * 96 / dpi
}
