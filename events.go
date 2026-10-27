package main

import wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

const (
	terminalDataEvent    = "terminal:data"
	terminalExitEvent    = "terminal:exit"
	sessionsChangedEvent = "sessions:changed"
)

type terminalDataPayload struct {
	ID      string `json:"id"`
	Data    string `json:"data"` // base64
	Dropped int    `json:"dropped,omitempty"`
}

type terminalExitPayload struct {
	ID       string `json:"id"`
	Reason   string `json:"reason"` // user | remote_exit | conn_lost | device_removed | io_error
	ExitCode *int   `json:"exitCode,omitempty"`
	Message  string `json:"message"`
}

// emitEvent sends a Wails event, or hands it to the test hook a.emit.
func (a *App) emitEvent(name string, payload any) {
	if a.emit != nil {
		a.emit(name, payload)
		return
	}
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, name, payload)
	}
}

// sessionsChanged asks the sessions builder (a.onSessionsChanged) to publish
// the current sessions snapshot. Safe to call with no hook installed.
func (a *App) sessionsChanged() {
	if a.onSessionsChanged != nil {
		a.onSessionsChanged()
	}
}
