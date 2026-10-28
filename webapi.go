package main

// webapi.go is the call layer of the headless Web admin (phase6-headless.md
// §3): POST /api/call/<Method> runs one public *App method, chosen from the
// allowlist webCallable, with the same JSON argument and result conversion
// Wails uses, so the unchanged frontend works through headless-shim.js.
//
// This file does not authenticate or check Origin / CSRF; web_guard.go and
// requireSession (web_auth.go) wrap this handler. It also does not know the
// admin_audit types: the integrator passes webCallHooks that adapt to
// sessionFrom and auditSink.
//
// Locking (design-v1 §5.0): no lock of this file is held while an App method
// runs. App methods are called from HTTP handler goroutines exactly as Wails
// calls them from its own goroutines.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// webCallMaxBody is the request body limit (§3.1, 413 above it). Enough
	// for a large SendTerminalInput paste.
	webCallMaxBody = 1 << 20
	// webCallPerSession is the per-session concurrent call limit (§3.2, 429).
	webCallPerSession = 64
	// webCallPathPrefix is where the handler is mounted.
	webCallPathPrefix = "/api/call/"
	// webCallTerminalTrack bounds the per-tab input byte counters.
	webCallTerminalTrack = 4096
	// webAuditTextMax bounds one user-supplied value in an audit detail.
	webAuditTextMax = 120
)

// callSpec classifies one allowed method (§3.3). Target and Detail build the
// audit fields from the raw arguments; they must never return secrets.
type callSpec struct {
	Audit  bool
	Target func(args []json.RawMessage) string
	Detail func(args []json.RawMessage) string
	// diff, when set, runs before the call (outside any lock) and names the
	// fields the call changes. Its output is appended to Detail.
	diff func(a *App, args []json.RawMessage) string
}

// webCallable is the allowlist: the 46 functions of
// frontend/wailsjs/go/main/App.js, i.e. every public *App method today.
var webCallable = map[string]callSpec{
	// Reads (not audited).
	"GetInitialData":            {},
	"GetMCPSettings":            {},
	"ListAgentApprovalBypasses": {},
	"ListAgentTokens":           {},
	"ListTerminalSessions":      {},
	"ListSerialPorts":           {},
	"GetApprovalTarget":         {},
	"GetHostKeyInfo":            {},
	"PlaintextBackups":          {},
	"ResizeTerminal":            {},
	// SendTerminalInput is counted per tab and reported on close.
	"SendTerminalInput": {},

	// Approvals.
	"ApproveCommand":               {Audit: true, Target: webArgStr(0), Detail: webCommandDigest(1)},
	"ApproveCommandConfirmed":      {Audit: true, Target: webArgStr(0), Detail: webCommandDigest(1)},
	"ApproveCommandAndBypassAgent": {Audit: true, Target: webArgStr(0), Detail: webCommandDigest(1)},
	"RejectCommand":                {Audit: true, Target: webArgStr(0)},
	"DeleteCommandRequest":         {Audit: true, Target: webArgStr(0)},

	// Connections.
	"SaveConnection":                   {Audit: true, Target: webConnTarget, Detail: webConnDetail, diff: webConnDiff},
	"DeleteConnection":                 {Audit: true, Target: webArgStr(0)},
	"DisconnectConnection":             {Audit: true, Target: webArgStr(0)},
	"TestConnection":                   {Audit: true, Target: webArgStr(0)},
	"TestConnectionWithPassphrase":     {Audit: true, Target: webArgStr(0)},
	"TestConnectionWithPassphraseSave": {Audit: true, Target: webArgStr(0), Detail: webArgBoolDetail(2, "save")},
	"TestConnectionConfig":             {Audit: true, Detail: webConnNameHost},
	"UseCredentialForSession":          {Audit: true, Target: webArgStr(0)},
	"ResetHostKey":                     {Audit: true, Target: webArgStr(0)},

	// MCP and agents. Tokens returned by these never reach the audit.
	"SaveMCPSettings":                {Audit: true, diff: webMCPDiff},
	"GenerateMCPToken":               {Audit: true},
	"DeleteMCPSettings":              {Audit: true},
	"IssueAgentToken":                {Audit: true, Target: webArgStr(0)},
	"IssueAgentTokenWithBypass":      {Audit: true, Target: webArgStr(0), Detail: webArgBoolDetail(1, "bypass")},
	"RevokeAgentToken":               {Audit: true, Target: webArgStr(0)},
	"RevokeAgentApprovalBypass":      {Audit: true, Target: webArgStr(0)},
	"RevokeAllAgentApprovalBypasses": {Audit: true},
	"DeletePlaintextBackups":         {Audit: true, Detail: webArgCount(0, "paths")},

	// Terminals.
	"StartTerminal":              {Audit: true, Target: webArgStr(0), Detail: webArgStrDetail(1, "connId")},
	"CloseTerminal":              {Audit: true, Target: webArgStr(0)},
	"CloseTerminalsByConnection": {Audit: true, Target: webArgStr(0)},
	"CloseAllTerminals":          {Audit: true},

	// AI sessions (REQ-016, phase7-ai-session.md §8): list, read-only
	// viewing and force close. There is no input method: humans cannot write
	// to a session from the GUI or the Web either.
	"ListAISessions":          {},
	"GetAISession":            {},
	"ReadAISessionRaw":        {},
	"GetAISessionLineContext": {},
	"GetAISessionSettings":    {},
	"CloseAISession":          {Audit: true, Target: webArgStr(0)},
	"CloseAllAISessions":      {Audit: true},
	"SaveAISessionSettings":   {Audit: true, diff: webAISessionSettingsDiff},
}

// webDenied lists public *App methods deliberately not reachable from the
// Web, with the reason. Empty today: every public method is a GUI action.
// A new public method must be added to webCallable or here
// (TestWebCallableCoversExportedMethods); until then it is not callable.
var webDenied = map[string]string{}

// checkWebCallable validates the allowlist against t (reflect.TypeOf(&App{})):
// every allowed name must exist with a signature the call layer can convert,
// and no name may be both allowed and denied. Run at startup; an error
// refuses to start instead of panicking at the first call.
func checkWebCallable(t reflect.Type) error {
	var problems []string
	for _, name := range webSortedKeys(webCallable) {
		if _, denied := webDenied[name]; denied {
			problems = append(problems, name+": both allowed and denied")
			continue
		}
		m, ok := t.MethodByName(name)
		if !ok {
			problems = append(problems, name+": no such method")
			continue
		}
		if err := webCheckSignature(m.Type, 1); err != nil {
			problems = append(problems, name+": "+err.Error())
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("web call allowlist: %s", strings.Join(problems, "; "))
	}
	return nil
}

// unclassifiedWebMethods returns the exported methods of t that are neither
// allowed nor denied.
func unclassifiedWebMethods(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumMethod(); i++ {
		name := t.Method(i).Name
		_, allowed := webCallable[name]
		_, denied := webDenied[name]
		if !allowed && !denied {
			out = append(out, name)
		}
	}
	return out
}

var webCallErrType = reflect.TypeOf((*error)(nil)).Elem()

// webCheckSignature accepts the shapes Wails binds: any JSON-decodable
// inputs and 0, 1, or 2 outputs where a second output is an error. skip is
// the number of leading inputs that are not arguments (1 for the receiver of
// a method expression, 0 for a bound method value).
func webCheckSignature(ft reflect.Type, skip int) error {
	if ft.IsVariadic() {
		return errors.New("variadic methods are not supported")
	}
	for i := skip; i < ft.NumIn(); i++ {
		switch ft.In(i).Kind() {
		case reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Interface:
			return fmt.Errorf("argument %d has unsupported type %s", i-skip, ft.In(i))
		}
	}
	switch ft.NumOut() {
	case 0, 1:
	case 2:
		if ft.Out(1) != webCallErrType {
			return errors.New("second result must be error")
		}
	default:
		return errors.New("more than two results")
	}
	return nil
}

// webCallError is a call that never reached the method (or panicked in it):
// it maps to an HTTP status and a fixed message (§3.1).
type webCallError struct {
	status int
	msg    string
}

func (e *webCallError) Error() string { return e.msg }

var (
	errWebUnknownMethod = &webCallError{http.StatusNotFound, "unknown method"}
	errWebInternal      = &webCallError{http.StatusInternalServerError, "internal error"}
)

// callAppMethod runs the allowed method name on app with the JSON array body
// as arguments. status is 200 when the method ran: result is its value and
// callErr its returned error, if any. Otherwise status is 4xx/5xx and callErr
// holds the message for the {"error": ...} body.
func callAppMethod(app *App, name string, body []byte) (result any, callErr error, status int) {
	if _, ok := webCallable[name]; !ok || app == nil {
		return nil, errWebUnknownMethod, errWebUnknownMethod.status
	}
	args, err := webParseCallArgs(body)
	if err != nil {
		return nil, err, http.StatusBadRequest
	}
	return webInvokeMethod(reflect.ValueOf(app), name, args)
}

// webParseCallArgs reads the body as a JSON array of raw arguments. An empty
// body or null means no arguments.
func webParseCallArgs(body []byte) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var args []json.RawMessage
	if err := json.Unmarshal(body, &args); err != nil {
		return nil, errors.New("arguments must be a JSON array")
	}
	return args, nil
}

// webInvokeMethod is callAppMethod without the allowlist, on any receiver (tests
// use stand-in types to cover every signature shape).
func webInvokeMethod(recv reflect.Value, name string, args []json.RawMessage) (result any, callErr error, status int) {
	method := recv.MethodByName(name)
	if !method.IsValid() {
		return nil, errWebUnknownMethod, errWebUnknownMethod.status
	}
	ft := method.Type()
	if err := webCheckSignature(ft, 0); err != nil {
		log.Printf("web call %s: %v", name, err)
		return nil, errWebInternal, errWebInternal.status
	}
	// Same conversion and message as Wails' BoundMethod.ParseArgs.
	if len(args) != ft.NumIn() {
		return nil, fmt.Errorf("received %d arguments to method 'main.App.%s', expected %d", len(args), name, ft.NumIn()), http.StatusBadRequest
	}
	in := make([]reflect.Value, len(args))
	for i, raw := range args {
		ptr := reflect.New(ft.In(i))
		if err := json.Unmarshal(raw, ptr.Interface()); err != nil {
			return nil, err, http.StatusBadRequest
		}
		in[i] = ptr.Elem()
	}

	out, panicked := webCallRecovering(name, method, in)
	if panicked {
		return nil, errWebInternal, errWebInternal.status
	}
	switch len(out) {
	case 0:
		return nil, nil, http.StatusOK
	case 1:
		if ft.Out(0) == webCallErrType {
			if err, _ := out[0].Interface().(error); err != nil {
				return nil, err, http.StatusOK
			}
			return nil, nil, http.StatusOK
		}
		return out[0].Interface(), nil, http.StatusOK
	default:
		if err, _ := out[1].Interface().(error); err != nil {
			return nil, err, http.StatusOK
		}
		return out[0].Interface(), nil, http.StatusOK
	}
}

func webCallRecovering(name string, method reflect.Value, in []reflect.Value) (out []reflect.Value, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			// The stack goes to the log only, never to the response.
			log.Printf("web call %s panicked: %v\n%s", name, r, debug.Stack())
			out, panicked = nil, true
		}
	}()
	return method.Call(in), false
}

// webCallRecord is one audited call, handed to webCallHooks.Audit. The
// integrator converts it into an adminAuditEntry (Actor from the session,
// Action "call:<Method>").
type webCallRecord struct {
	At      time.Time
	Session string // session ID from webCallHooks.Session ("" when none)
	Actor   string // e.g. "web:<first 8 of session ID>"
	Remote  string // client IP from r.RemoteAddr
	Method  string
	Target  string
	Detail  string
	OK      bool
	Code    string // ActionResult.Code, or "error" / "bad_request" / "panic"
}

// webCallHooks connects the handler to the rest of the Web server without
// depending on its types.
type webCallHooks struct {
	// Session returns the caller's session ID and audit actor (the request
	// already passed requireSession). Concurrency is limited per session ID.
	Session func(r *http.Request) (id, actor string)
	// Audit records an audited call. It must not block (§6.9).
	Audit func(rec webCallRecord)
	// Now is the clock for records; time.Now when nil.
	Now func() time.Time
	// RequireSession makes a request without a session (Session returns "")
	// answer 401 before anything else runs. requireSession already guards
	// the route; this keeps the call layer closed if it is ever mounted
	// without it (fail closed, §6.5).
	RequireSession bool
}

// webCallHooksFor wires the handler to webauth.go: the session comes from
// sessionFrom and each record becomes a webAuditEvent with Action
// "call:<Method>" passed to audit (webServerOptions.Audit).
func webCallHooksFor(audit func(webAuditEvent)) webCallHooks {
	hooks := webCallHooks{
		RequireSession: true,
		Session: func(r *http.Request) (string, string) {
			s, ok := sessionFrom(r.Context())
			if !ok {
				return "", ""
			}
			return s.ID, s.Actor
		},
	}
	if audit != nil {
		hooks.Audit = func(rec webCallRecord) {
			audit(webAuditEvent{
				Actor:  rec.Actor,
				Remote: rec.Remote,
				Action: "call:" + rec.Method,
				Target: rec.Target,
				Detail: rec.Detail,
				OK:     rec.OK,
				Code:   rec.Code,
			})
		}
	}
	return hooks
}

func webFirst(a, _ string) string { return a }

// webCallHandler serves POST /api/call/<Method>.
type webCallHandler struct {
	app   *App
	hooks webCallHooks
	recv  reflect.Value

	mu       sync.Mutex
	stopped  bool
	inflight map[string]int
	calls    sync.WaitGroup
	// tabs counts SendTerminalInput bytes per terminal tab (guarded by mu).
	tabs map[string]*webTabInput
}

type webTabInput struct {
	connID string
	bytes  int64
}

// newRPCHandler returns the handler for webCallPathPrefix. It returns an
// error when the allowlist does not match *App (checkWebCallable).
func newRPCHandler(app *App, hooks webCallHooks) (*webCallHandler, error) {
	if err := checkWebCallable(reflect.TypeOf(app)); err != nil {
		return nil, err
	}
	return &webCallHandler{
		app:      app,
		hooks:    hooks,
		recv:     reflect.ValueOf(app),
		inflight: map[string]int{},
		tabs:     map[string]*webTabInput{},
	}, nil
}

// StopIntake makes every later call answer 503 (shutdown step 1, §8).
func (h *webCallHandler) StopIntake() {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
}

// WaitCalls waits for running calls to finish, up to timeout. It reports
// whether they all finished.
func (h *webCallHandler) WaitCalls(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		h.calls.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (h *webCallHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		webWriteCallError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.hooks.RequireSession && (h.hooks.Session == nil || webFirst(h.hooks.Session(r)) == "") {
		webWriteCallError(w, http.StatusUnauthorized, "login required")
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, webCallPathPrefix)
	spec, known := webCallable[name]
	if !ok || !known {
		webWriteCallError(w, errWebUnknownMethod.status, errWebUnknownMethod.msg)
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		webWriteCallError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	session, actor := "", ""
	if h.hooks.Session != nil {
		session, actor = h.hooks.Session(r)
	}
	release, status := h.admit(session)
	if status != 0 {
		msg := "too many concurrent calls"
		if status == http.StatusServiceUnavailable {
			msg = "shutting down"
		}
		webWriteCallError(w, status, msg)
		return
	}
	defer release()

	body, err := webReadCallBody(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			webWriteCallError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			webWriteCallError(w, http.StatusBadRequest, "could not read request body")
		}
		return
	}

	args, err := webParseCallArgs(body)
	if err != nil {
		h.audit(r, session, actor, name, spec, nil, nil, "", false, "bad_request")
		webWriteCallError(w, http.StatusBadRequest, err.Error())
		return
	}
	var diff string
	if spec.Audit && spec.diff != nil && webValidArgCount(h.recv, name, args) {
		diff = spec.diff(h.app, args)
	}

	result, callErr, status := webInvokeMethod(h.recv, name, args)
	h.trackTerminal(name, args, result, status)
	if spec.Audit {
		ok, code := webCallOutcome(result, callErr, status)
		h.audit(r, session, actor, name, spec, args, result, webJoinDetail(diff, h.closeDetail(name, args, status)), ok, code)
	}

	switch {
	case status != http.StatusOK:
		webWriteCallError(w, status, callErr.Error())
	case callErr != nil:
		webWriteCallJSON(w, http.StatusOK, map[string]any{"error": callErr.Error()})
	default:
		webWriteCallJSON(w, http.StatusOK, map[string]any{"result": result})
	}
}

// admit reserves a call slot. status is 503 after StopIntake and 429 when
// the session already has webCallPerSession calls running.
func (h *webCallHandler) admit(session string) (release func(), status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil, http.StatusServiceUnavailable
	}
	if h.inflight[session] >= webCallPerSession {
		return nil, http.StatusTooManyRequests
	}
	h.inflight[session]++
	h.calls.Add(1)
	return func() {
		h.mu.Lock()
		if h.inflight[session]--; h.inflight[session] <= 0 {
			delete(h.inflight, session)
		}
		h.mu.Unlock()
		h.calls.Done()
	}, 0
}

func webReadCallBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.ContentLength > webCallMaxBody {
		return nil, &http.MaxBytesError{Limit: webCallMaxBody}
	}
	reader := http.MaxBytesReader(w, r.Body, webCallMaxBody)
	defer reader.Close()
	return io.ReadAll(reader)
}

func webValidArgCount(recv reflect.Value, name string, args []json.RawMessage) bool {
	m := recv.MethodByName(name)
	return m.IsValid() && m.Type().NumIn() == len(args)
}

// webCallOutcome maps a call result to the audit's ok / code.
func webCallOutcome(result any, callErr error, status int) (bool, string) {
	switch {
	case status == http.StatusBadRequest:
		return false, "bad_request"
	case status != http.StatusOK:
		return false, "panic"
	case callErr != nil:
		return false, "error"
	}
	if ar, ok := result.(ActionResult); ok {
		return ar.OK, ar.Code
	}
	return true, ""
}

func (h *webCallHandler) audit(r *http.Request, session, actor, name string, spec callSpec, args []json.RawMessage, result any, extra string, ok bool, code string) {
	if h.hooks.Audit == nil || !spec.Audit {
		return
	}
	now := time.Now
	if h.hooks.Now != nil {
		now = h.hooks.Now
	}
	rec := webCallRecord{
		At:      now(),
		Session: session,
		Actor:   actor,
		Remote:  webCallRemoteIP(r.RemoteAddr),
		Method:  name,
		OK:      ok,
		Code:    code,
	}
	if args != nil {
		if spec.Target != nil {
			rec.Target = spec.Target(args)
		}
		if spec.Detail != nil {
			rec.Detail = spec.Detail(args)
		}
	}
	if ar, isAR := result.(ActionResult); isAR && rec.Target == "" && ar.ID != "" {
		rec.Target = webAuditText(ar.ID) // e.g. the ID of a newly saved connection
	}
	rec.Detail = webJoinDetail(rec.Detail, extra)
	h.hooks.Audit(rec)
}

// trackTerminal keeps the per-tab input byte counters (§3.3: input is not
// audited per call; the total goes into the close record).
func (h *webCallHandler) trackTerminal(name string, args []json.RawMessage, result any, status int) {
	if status != http.StatusOK {
		return
	}
	ar, _ := result.(ActionResult)
	switch name {
	case "StartTerminal":
		if !ar.OK || len(args) < 2 {
			return
		}
		id := webArgRaw(args, 0)
		h.mu.Lock()
		if len(h.tabs) >= webCallTerminalTrack {
			for k := range h.tabs { // drop an arbitrary stale counter
				delete(h.tabs, k)
				break
			}
		}
		h.tabs[id] = &webTabInput{connID: webArgRaw(args, 1)}
		h.mu.Unlock()
	case "SendTerminalInput":
		if !ar.OK || len(args) < 2 {
			return
		}
		id, data := webArgRaw(args, 0), webArgRaw(args, 1)
		h.mu.Lock()
		if tab := h.tabs[id]; tab != nil {
			tab.bytes += int64(len(data))
		}
		h.mu.Unlock()
	}
}

// closeDetail removes the counters of the tabs a Close* call closed and
// returns "input_bytes=N" for the audit.
func (h *webCallHandler) closeDetail(name string, args []json.RawMessage, status int) string {
	if status != http.StatusOK {
		return ""
	}
	var total int64
	h.mu.Lock()
	defer h.mu.Unlock()
	switch name {
	case "CloseTerminal":
		id := webArgRaw(args, 0)
		if tab := h.tabs[id]; tab != nil {
			total = tab.bytes
			delete(h.tabs, id)
		}
	case "CloseTerminalsByConnection":
		connID := webArgRaw(args, 0)
		for id, tab := range h.tabs {
			if tab.connID == connID {
				total += tab.bytes
				delete(h.tabs, id)
			}
		}
	case "CloseAllTerminals":
		for id, tab := range h.tabs {
			total += tab.bytes
			delete(h.tabs, id)
		}
	default:
		return ""
	}
	return "input_bytes=" + strconv.FormatInt(total, 10)
}

// handleEnv serves GET /api/env: the Wails Environment() shape the frontend
// uses to pick serial port examples (§5.3). The OS is the server's.
func handleEnv(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		webWriteCallError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	webWriteCallJSON(w, http.StatusOK, map[string]string{
		"buildType": "production",
		"platform":  runtime.GOOS,
		"arch":      runtime.GOARCH,
	})
}

func webWriteCallError(w http.ResponseWriter, status int, msg string) {
	webWriteCallJSON(w, status, map[string]string{"error": msg})
}

func webWriteCallJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		log.Printf("web call: encode response: %v", err)
		status = http.StatusInternalServerError
		body = []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func webCallRemoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// --- audit field builders (§3.3 "監査に残す引数"). Never secrets. ---

func webArgRaw(args []json.RawMessage, i int) string {
	if i >= len(args) {
		return ""
	}
	var s string
	if json.Unmarshal(args[i], &s) != nil {
		return ""
	}
	return s
}

// webAuditText trims and bounds a user-supplied value and quotes it when it
// contains spaces, quotes or control characters.
func webAuditText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > webAuditTextMax {
		s = strings.ToValidUTF8(s[:webAuditTextMax], "") + "…"
	}
	if strings.ContainsAny(s, " \t\r\n\"=,") || strconv.Quote(s) != `"`+s+`"` {
		return strconv.Quote(s)
	}
	return s
}

func webArgStr(i int) func([]json.RawMessage) string {
	return func(args []json.RawMessage) string { return webAuditText(webArgRaw(args, i)) }
}

func webArgStrDetail(i int, key string) func([]json.RawMessage) string {
	return func(args []json.RawMessage) string { return key + "=" + webAuditText(webArgRaw(args, i)) }
}

func webArgBoolDetail(i int, key string) func([]json.RawMessage) string {
	return func(args []json.RawMessage) string {
		var b bool
		if i < len(args) {
			_ = json.Unmarshal(args[i], &b)
		}
		return key + "=" + strconv.FormatBool(b)
	}
}

func webArgCount(i int, key string) func([]json.RawMessage) string {
	return func(args []json.RawMessage) string {
		var items []json.RawMessage
		if i < len(args) {
			_ = json.Unmarshal(args[i], &items)
		}
		return key + "=" + strconv.Itoa(len(items))
	}
}

// webCommandDigest records the command's sha256 and length, not its text.
func webCommandDigest(i int) func([]json.RawMessage) string {
	return func(args []json.RawMessage) string {
		cmd := webArgRaw(args, i)
		sum := sha256.Sum256([]byte(cmd))
		return "command_sha256=" + hex.EncodeToString(sum[:]) + " command_len=" + strconv.Itoa(len(cmd))
	}
}

func webArgConnection(args []json.RawMessage) Connection {
	var c Connection
	if len(args) > 0 {
		_ = json.Unmarshal(args[0], &c)
	}
	return c
}

func webConnTarget(args []json.RawMessage) string {
	return webAuditText(webArgConnection(args).ID)
}

func webConnDetail(args []json.RawMessage) string {
	c := webArgConnection(args)
	return "name=" + webAuditText(c.Name) + " type=" + webAuditText(c.connectionType()) + " host=" + webAuditText(c.Host)
}

func webConnNameHost(args []json.RawMessage) string {
	c := webArgConnection(args)
	return "name=" + webAuditText(c.Name) + " host=" + webAuditText(c.Host)
}

// Fields whose values are secrets: only their names are recorded, and only
// when the request sets them.
var webSecretFields = map[string]bool{"credential": true, "keyPassphrase": true, "tokenInput": true}

// Fields the UI echoes back but the save ignores or derives.
var webDerivedFields = map[string]bool{
	"credentialSet": true, "lastTestResult": true, "lastTestAt": true, "lastTestMessage": true,
	"tokenPreview": true, "tokenConfigured": true, "tokenFromEnv": true, "configPath": true,
	"status": true, "lastError": true, "connectedAgents": true, "localURL": true,
	"lanAddress": true, "lanURL": true, "lanReachable": true, "bypassAllowed": true,
}

// webChangedFields compares the JSON object in raw with current and returns
// "changed=a,b" (sorted), or "new" when there is no current value.
func webChangedFields(raw json.RawMessage, current any, exists bool) string {
	var incoming map[string]json.RawMessage
	if json.Unmarshal(raw, &incoming) != nil {
		return ""
	}
	var old map[string]json.RawMessage
	if exists {
		if b, err := json.Marshal(current); err == nil {
			_ = json.Unmarshal(b, &old)
		}
	}
	var changed []string
	for key, value := range incoming {
		if webDerivedFields[key] || key == "id" {
			continue
		}
		if webSecretFields[key] {
			var s string
			if json.Unmarshal(value, &s) == nil && s != "" {
				changed = append(changed, key)
			}
			continue
		}
		if !exists || !webJSONEqual(value, old[key]) {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	prefix := "changed="
	if !exists {
		prefix = "new fields="
	}
	return prefix + strings.Join(changed, ",")
}

func webJSONEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil {
		return false
	}
	if len(b) == 0 {
		b = json.RawMessage("null")
	}
	if json.Unmarshal(b, &bv) != nil {
		return false
	}
	// Treat a missing / null value like the zero value the save stores.
	if webJSONZero(av) && webJSONZero(bv) {
		return true
	}
	return reflect.DeepEqual(av, bv)
}

func webJSONZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func webConnDiff(a *App, args []json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	id := strings.TrimSpace(webArgConnection(args).ID)
	var current Connection
	exists := false
	if id != "" {
		current, exists = a.connectionByID(id)
	}
	return webChangedFields(args[0], current, exists)
}

func webMCPDiff(a *App, args []json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	return webChangedFields(args[0], a.GetMCPSettings(), true)
}

func webAISessionSettingsDiff(a *App, args []json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	return webChangedFields(args[0], a.GetAISessionSettings(), true)
}

func webJoinDetail(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func webSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
