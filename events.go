package main

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

// emitEvent hands the event to the test hook a.emit, or to platformEmit
// (the Web event hub when startup got withWebHub, else the Wails runtime).
func (a *App) emitEvent(name string, payload any) {
	if a.emit != nil {
		a.emit(name, payload)
		return
	}
	if a.ctx != nil {
		platformEmit(a.ctx, name, payload)
	}
}

// sessionsChanged asks the sessions builder (a.onSessionsChanged) to publish
// the current sessions snapshot. Safe to call with no hook installed.
func (a *App) sessionsChanged() {
	if a.onSessionsChanged != nil {
		a.onSessionsChanged()
	}
}
