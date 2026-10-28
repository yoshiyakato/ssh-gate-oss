//go:build !headless

package main

import wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

// Only main.go and *_wails.go may import the Wails runtime (phase6 §2.2).
func init() { wailsEmit = wruntime.EventsEmit }
