package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- allowlist ---

func TestWebCallableCoversExportedMethods(t *testing.T) {
	appType := reflect.TypeOf(&App{})
	if err := checkWebCallable(appType); err != nil {
		t.Fatal(err)
	}
	if missing := unclassifiedWebMethods(appType); len(missing) > 0 {
		t.Fatalf("public *App methods not classified for the Web admin (add each to webCallable or webDenied in webapi.go): %v", missing)
	}
	if got := len(webCallable) + len(webDenied); got != appType.NumMethod() {
		t.Fatalf("classified %d methods, *App has %d public methods", got, appType.NumMethod())
	}
}

func TestWebCallableMatchesAppJS(t *testing.T) {
	src, err := os.ReadFile("frontend/wailsjs/go/main/App.js")
	if err != nil {
		t.Skip("frontend/wailsjs not generated:", err)
	}
	var js []string
	for _, m := range regexp.MustCompile(`(?m)^export function (\w+)\(`).FindAllStringSubmatch(string(src), -1) {
		js = append(js, m[1])
	}
	allowed := webSortedKeys(webCallable)
	sort.Strings(js)
	if !reflect.DeepEqual(js, allowed) {
		t.Fatalf("App.js functions and webCallable differ\nApp.js:    %v\nallowlist: %v", js, allowed)
	}
}

func TestCheckWebCallableRejectsBadEntries(t *testing.T) {
	appType := reflect.TypeOf(&App{})

	webCallable["NoSuchMethod"] = callSpec{}
	err := checkWebCallable(appType)
	delete(webCallable, "NoSuchMethod")
	if err == nil || !strings.Contains(err.Error(), "NoSuchMethod: no such method") {
		t.Fatalf("missing method not reported: %v", err)
	}

	webDenied["GetInitialData"] = "test"
	err = checkWebCallable(appType)
	delete(webDenied, "GetInitialData")
	if err == nil || !strings.Contains(err.Error(), "both allowed and denied") {
		t.Fatalf("double classification not reported: %v", err)
	}

	if _, err := newRPCHandler(&App{}, webCallHooks{}); err != nil {
		t.Fatalf("newRPCHandler on the real allowlist: %v", err)
	}
}

func TestLifecycleMethodsAreNotCallable(t *testing.T) {
	for _, name := range []string{"startup", "shutdown", "stopMCPServer", "emitEvent", "saveConnection", "Startup", "", "../GetInitialData"} {
		_, err, status := callAppMethod(&App{}, name, []byte("[]"))
		if status != http.StatusNotFound || err == nil || err.Error() != "unknown method" {
			t.Errorf("%q: status %d err %v, want 404 unknown method", name, status, err)
		}
	}
}

// --- conversion (stand-in receiver covering every signature shape) ---

type webFakeItem struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	N    int      `json:"n,omitempty"`
}

type webFake struct {
	mu      sync.Mutex
	got     []any
	block   chan struct{}
	entered chan struct{}
}

func (f *webFake) record(v ...any) {
	f.mu.Lock()
	f.got = append(f.got, v...)
	f.mu.Unlock()
}

func (f *webFake) NoResult()                           { f.record("NoResult") }
func (f *webFake) Echo(s string, n int, b bool) string { f.record(s, n, b); return s }
func (f *webFake) Item(it webFakeItem) webFakeItem     { it.N++; return it }
func (f *webFake) NilSlice() []string                  { return nil }
func (f *webFake) Pair(fail bool) (webFakeItem, error) {
	if fail {
		return webFakeItem{ID: "x"}, errors.New("host key unknown")
	}
	return webFakeItem{ID: "ok"}, nil
}
func (f *webFake) OnlyErr(fail bool) error {
	if fail {
		return errors.New("boom")
	}
	return nil
}
func (f *webFake) Panics() string  { panic("secret detail in panic") }
func (f *webFake) Bad() (int, int) { return 1, 2 }

// Names from the allowlist, used to exercise the HTTP handler without a
// real App.
func (f *webFake) GetInitialData() string {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	return "data"
}
func (f *webFake) StartTerminal(id, connID string, cols, rows int) ActionResult {
	return ActionResult{OK: true, SessionID: id}
}
func (f *webFake) SendTerminalInput(id, data string) ActionResult { return ActionResult{OK: true} }
func (f *webFake) CloseTerminal(id string) ActionResult           { return ActionResult{OK: true} }
func (f *webFake) CloseTerminalsByConnection(connID string) ActionResult {
	return ActionResult{OK: true}
}
func (f *webFake) CloseAllTerminals() ActionResult { return ActionResult{OK: true} }
func (f *webFake) ApproveCommand(id, command string) ActionResult {
	return ActionResult{OK: false, Code: "risk_increased"}
}

func invokeFake(t *testing.T, f *webFake, name, body string) (any, error, int) {
	t.Helper()
	args, err := webParseCallArgs([]byte(body))
	if err != nil {
		return nil, err, http.StatusBadRequest
	}
	return webInvokeMethod(reflect.ValueOf(f), name, args)
}

func TestWebInvokeConversion(t *testing.T) {
	f := &webFake{}

	if res, err, st := invokeFake(t, f, "NoResult", ""); st != 200 || err != nil || res != nil {
		t.Fatalf("NoResult: %v %v %d", res, err, st)
	}
	if res, err, st := invokeFake(t, f, "Echo", `["a", 3, true]`); st != 200 || err != nil || res != "a" {
		t.Fatalf("Echo: %v %v %d", res, err, st)
	}
	// JS undefined becomes null in JSON.stringify: the zero value (as Wails).
	if res, _, st := invokeFake(t, f, "Echo", `[null, null, null]`); st != 200 || res != "" {
		t.Fatalf("Echo nulls: %v %d", res, st)
	}

	_, err, st := invokeFake(t, f, "Echo", `["a"]`)
	if st != 400 || err.Error() != "received 1 arguments to method 'main.App.Echo', expected 3" {
		t.Fatalf("arity: %v %d", err, st)
	}
	if _, err, st := invokeFake(t, f, "Echo", `["a", "three", true]`); st != 400 || !strings.Contains(err.Error(), "cannot unmarshal") {
		t.Fatalf("type mismatch: %v %d", err, st)
	}
	if _, err, st := invokeFake(t, f, "Echo", `{"a":1}`); st != 400 || err.Error() != "arguments must be a JSON array" {
		t.Fatalf("non-array: %v %d", err, st)
	}

	res, err, st := invokeFake(t, f, "Item", `[{"id":"c1","tags":["a"],"unknown":1}]`)
	if st != 200 || err != nil {
		t.Fatalf("Item: %v %d", err, st)
	}
	if got, _ := json.Marshal(res); string(got) != `{"id":"c1","tags":["a"],"n":1}` {
		t.Fatalf("Item JSON: %s", got)
	}

	res, _, _ = invokeFake(t, f, "NilSlice", "[]")
	if got, _ := json.Marshal(map[string]any{"result": res}); string(got) != `{"result":null}` {
		t.Fatalf("nil slice: %s", got)
	}

	if res, err, st := invokeFake(t, f, "Pair", "[false]"); st != 200 || err != nil || res.(webFakeItem).ID != "ok" {
		t.Fatalf("Pair ok: %v %v %d", res, err, st)
	}
	if res, err, st := invokeFake(t, f, "Pair", "[true]"); st != 200 || err == nil || err.Error() != "host key unknown" || res != nil {
		t.Fatalf("Pair error: %v %v %d", res, err, st)
	}
	if _, err, st := invokeFake(t, f, "OnlyErr", "[true]"); st != 200 || err == nil || err.Error() != "boom" {
		t.Fatalf("OnlyErr: %v %d", err, st)
	}
	if res, err, st := invokeFake(t, f, "OnlyErr", "[false]"); st != 200 || err != nil || res != nil {
		t.Fatalf("OnlyErr nil: %v %v %d", res, err, st)
	}
	if _, err, st := invokeFake(t, f, "Panics", "[]"); st != 500 || err.Error() != "internal error" {
		t.Fatalf("panic: %v %d", err, st)
	}
	if _, _, st := invokeFake(t, f, "Bad", "[]"); st != 500 {
		t.Fatalf("unsupported signature: %d", st)
	}
	if _, _, st := invokeFake(t, f, "Missing", "[]"); st != 404 {
		t.Fatalf("missing: %d", st)
	}
}

// --- HTTP handler ---

type webAuditLog struct {
	mu   sync.Mutex
	recs []webCallRecord
}

func (l *webAuditLog) add(r webCallRecord) {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
}

func (l *webAuditLog) all() []webCallRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]webCallRecord(nil), l.recs...)
}

func (l *webAuditLog) last(t *testing.T, method string) webCallRecord {
	t.Helper()
	recs := l.all()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Method == method {
			return recs[i]
		}
	}
	t.Fatalf("no audit record for %s in %+v", method, recs)
	return webCallRecord{}
}

func fakeCallHandler(f *webFake, log *webAuditLog) *webCallHandler {
	return &webCallHandler{
		recv: reflect.ValueOf(f),
		hooks: webCallHooks{
			Session: func(r *http.Request) (string, string) {
				id := r.Header.Get("X-Test-Session")
				return id, "web:" + id
			},
			Audit: log.add,
		},
		inflight: map[string]int{},
		tabs:     map[string]*webTabInput{},
	}
}

func postCall(t *testing.T, h http.Handler, method, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webCallPathPrefix+method, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.7:51234"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeCall(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %q", rec.Code, rec.Body.String())
	}
	return out
}

func newWebTestHandler(t *testing.T) (*App, *webCallHandler, *webAuditLog) {
	t.Helper()
	app := newTestApp(t)
	log := &webAuditLog{}
	h, err := newRPCHandler(app, webCallHooks{
		Session: func(r *http.Request) (string, string) { return "sess-1", "web:sess-1" },
		Audit:   log.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, h, log
}

func TestWebCallHTTPShapes(t *testing.T) {
	_, h, log := newWebTestHandler(t)

	rec := postCall(t, h, "GetInitialData", "[]")
	if rec.Code != 200 {
		t.Fatalf("GetInitialData: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q", ct)
	}
	var data struct {
		Result DashboardData `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(log.all()) != 0 {
		t.Fatalf("reads must not be audited: %+v", log.all())
	}

	// Method that returns (value, error): error goes in the body with 200.
	rec = postCall(t, h, "GetHostKeyInfo", `["no-such-connection"]`)
	body := decodeCall(t, rec)
	if rec.Code != 200 {
		t.Fatalf("GetHostKeyInfo: %d %s", rec.Code, rec.Body)
	}
	if _, hasErr := body["error"]; !hasErr {
		if _, hasRes := body["result"]; !hasRes {
			t.Fatalf("GetHostKeyInfo: neither result nor error: %s", rec.Body)
		}
	}

	cases := []struct {
		name, method, body string
		ctype              string
		want               int
		msg                string
	}{
		{"unknown", "NoSuchThing", "[]", "", 404, "unknown method"},
		{"private", "shutdown", "[]", "", 404, "unknown method"},
		{"arity", "GetApprovalTarget", "[]", "", 400, "received 0 arguments to method 'main.App.GetApprovalTarget', expected 1"},
		{"type", "GetApprovalTarget", "[1]", "", 400, ""},
		{"not array", "GetInitialData", `"x"`, "", 400, "arguments must be a JSON array"},
		{"content type", "GetInitialData", "[]", "text/plain", 415, ""},
	}
	for _, c := range cases {
		hdr := []string{}
		if c.ctype != "" {
			hdr = []string{"Content-Type", c.ctype}
		}
		rec := postCall(t, h, c.method, c.body, hdr...)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
			continue
		}
		var e struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error == "" || (c.msg != "" && e.Error != c.msg) {
			t.Errorf("%s: error %q, want %q", c.name, e.Error, c.msg)
		}
	}

	req := httptest.NewRequest(http.MethodGet, webCallPathPrefix+"GetInitialData", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 405 || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("GET: %d allow %q", rr.Code, rr.Header().Get("Allow"))
	}

	big := `["` + strings.Repeat("a", webCallMaxBody) + `"]`
	if rec := postCall(t, h, "GetApprovalTarget", big); rec.Code != 413 {
		t.Fatalf("large body: %d", rec.Code)
	}
	// Chunked body without Content-Length also stops at the limit.
	req = httptest.NewRequest(http.MethodPost, webCallPathPrefix+"GetApprovalTarget", struct{ *strings.Reader }{strings.NewReader(big)})
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 413 {
		t.Fatalf("large chunked body: %d", rr.Code)
	}

	h.StopIntake()
	if rec := postCall(t, h, "GetInitialData", "[]"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "shutting down") {
		t.Fatalf("after StopIntake: %d %s", rec.Code, rec.Body)
	}
	if !h.WaitCalls(time.Second) {
		t.Fatal("WaitCalls with nothing running")
	}
}

func TestWebCallAuditNeverHoldsSecrets(t *testing.T) {
	app, h, log := newWebTestHandler(t)

	// New connection with a password: target is the new ID, the password and
	// passphrase never appear.
	const password = "hunter2-very-secret"
	conn := Connection{Name: "prod web", Type: "SSH", Host: "127.0.0.1", Port: 1, User: "ops", AuthMethod: "パスワード保管", Credential: password, KeyPassphrase: "pp-secret"}
	argsJSON, _ := json.Marshal([]any{conn})
	rec := postCall(t, h, "SaveConnection", string(argsJSON))
	var saved struct{ Result ActionResult }
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil || !saved.Result.OK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	r := log.last(t, "SaveConnection")
	if r.Target != saved.Result.ID || r.Session != "sess-1" || r.Actor != "web:sess-1" || r.Remote != "192.0.2.7" || !r.OK {
		t.Fatalf("save audit: %+v (id %s)", r, saved.Result.ID)
	}
	if !strings.Contains(r.Detail, `name="prod web"`) || !strings.Contains(r.Detail, "host=127.0.0.1") || !strings.Contains(r.Detail, "credential") {
		t.Fatalf("save detail: %q", r.Detail)
	}

	// Update: only changed fields are named.
	stored, ok := app.connectionByID(saved.Result.ID)
	if !ok {
		t.Fatal("saved connection missing")
	}
	stored.Description = "changed"
	stored.Port = 2222
	argsJSON, _ = json.Marshal([]any{stored})
	postCall(t, h, "SaveConnection", string(argsJSON))
	r = log.last(t, "SaveConnection")
	if !strings.Contains(r.Detail, "changed=description,port") {
		t.Fatalf("update detail: %q", r.Detail)
	}

	// Approval: command digest only.
	const command = "rm -rf /srv/very-secret-path"
	argsJSON, _ = json.Marshal([]any{"req-1", command})
	postCall(t, h, "ApproveCommand", string(argsJSON))
	r = log.last(t, "ApproveCommand")
	if r.Target != "req-1" || !strings.Contains(r.Detail, "command_sha256=") || !strings.Contains(r.Detail, "command_len=28") {
		t.Fatalf("approve audit: %+v", r)
	}

	// MCP: tokenInput's value and the generated token stay out.
	settings := app.GetMCPSettings()
	settings.TokenInput = "tok-input-secret-value"
	argsJSON, _ = json.Marshal([]any{settings})
	postCall(t, h, "SaveMCPSettings", string(argsJSON))
	r = log.last(t, "SaveMCPSettings")
	if !strings.Contains(r.Detail, "tokenInput") {
		t.Fatalf("mcp detail: %q", r.Detail)
	}
	rec = postCall(t, h, "GenerateMCPToken", "[]")
	var gen struct{ Result ActionResult }
	_ = json.Unmarshal(rec.Body.Bytes(), &gen)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token response must be no-store")
	}

	rec = postCall(t, h, "TestConnectionWithPassphrase", `["`+saved.Result.ID+`", "pass-secret"]`)
	_ = rec
	postCall(t, h, "UseCredentialForSession", `["`+saved.Result.ID+`", "session-secret"]`)

	secrets := []string{password, "pp-secret", command, "tok-input-secret-value", "pass-secret", "session-secret"}
	if gen.Result.Token != "" {
		secrets = append(secrets, gen.Result.Token)
	}
	for _, rec := range log.all() {
		line, _ := json.Marshal(rec)
		for _, s := range secrets {
			if bytes.Contains(line, []byte(s)) {
				t.Fatalf("audit record leaks %q: %s", s, line)
			}
		}
	}
}

func TestWebCallTerminalInputBytes(t *testing.T) {
	log := &webAuditLog{}
	h := fakeCallHandler(&webFake{}, log)

	postCall(t, h, "StartTerminal", `["tab-1","conn-a",80,24]`)
	postCall(t, h, "StartTerminal", `["tab-2","conn-a",80,24]`)
	postCall(t, h, "StartTerminal", `["tab-3","conn-b",80,24]`)
	postCall(t, h, "SendTerminalInput", `["tab-1","hello"]`)
	postCall(t, h, "SendTerminalInput", `["tab-1","!!"]`)
	postCall(t, h, "SendTerminalInput", `["tab-2","abc"]`)
	postCall(t, h, "SendTerminalInput", `["tab-3","xy"]`)

	for _, r := range log.all() {
		if r.Method == "SendTerminalInput" {
			t.Fatal("input must not be audited per call")
		}
	}
	if r := log.last(t, "StartTerminal"); r.Target != "tab-3" || r.Detail != "connId=conn-b" {
		t.Fatalf("start audit: %+v", r)
	}

	postCall(t, h, "CloseTerminal", `["tab-1"]`)
	if r := log.last(t, "CloseTerminal"); r.Detail != "input_bytes=7" {
		t.Fatalf("close detail %q", r.Detail)
	}
	postCall(t, h, "CloseTerminalsByConnection", `["conn-a"]`)
	if r := log.last(t, "CloseTerminalsByConnection"); r.Target != "conn-a" || r.Detail != "input_bytes=3" {
		t.Fatalf("close by conn: %+v", r)
	}
	postCall(t, h, "CloseAllTerminals", `[]`)
	if r := log.last(t, "CloseAllTerminals"); r.Detail != "input_bytes=2" {
		t.Fatalf("close all: %+v", r)
	}
	if len(h.tabs) != 0 {
		t.Fatalf("counters left: %v", h.tabs)
	}

	// ActionResult.Code reaches the audit.
	postCall(t, h, "ApproveCommand", `["r","ls"]`)
	if r := log.last(t, "ApproveCommand"); r.OK || r.Code != "risk_increased" {
		t.Fatalf("approve outcome: %+v", r)
	}
}

func TestWebCallConcurrencyAndDrain(t *testing.T) {
	f := &webFake{block: make(chan struct{}), entered: make(chan struct{}, webCallPerSession)}
	h := fakeCallHandler(f, &webAuditLog{})

	var wg sync.WaitGroup
	codes := make(chan int, webCallPerSession)
	for i := 0; i < webCallPerSession; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- postCall(t, h, "GetInitialData", "[]", "X-Test-Session", "s1").Code
		}()
	}
	for i := 0; i < webCallPerSession; i++ {
		<-f.entered
	}
	if rec := postCall(t, h, "GetInitialData", "[]", "X-Test-Session", "s1"); rec.Code != 429 {
		t.Fatalf("over the session limit: %d", rec.Code)
	}
	// Another session is not affected.
	f.entered = make(chan struct{}, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		codes2 := postCall(t, h, "GetInitialData", "[]", "X-Test-Session", "s2").Code
		if codes2 != 200 {
			t.Errorf("other session: %d", codes2)
		}
	}()
	<-f.entered

	h.StopIntake()
	if h.WaitCalls(50 * time.Millisecond) {
		t.Fatal("WaitCalls returned true with calls running")
	}
	close(f.block)
	if !h.WaitCalls(5 * time.Second) {
		t.Fatal("calls did not drain")
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Fatalf("blocked call ended with %d", c)
		}
	}
	if len(h.inflight) != 0 {
		t.Fatalf("inflight left: %v", h.inflight)
	}
}

func TestWebHandleEnv(t *testing.T) {
	rec := httptest.NewRecorder()
	handleEnv(rec, httptest.NewRequest(http.MethodGet, "/api/env", nil))
	var env struct{ BuildType, Platform, Arch string }
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != 200 {
		t.Fatalf("env: %d %s", rec.Code, rec.Body)
	}
	if env.BuildType != "production" || env.Platform == "" || env.Arch == "" {
		t.Fatalf("env: %+v", env)
	}
	rec = httptest.NewRecorder()
	handleEnv(rec, httptest.NewRequest(http.MethodPost, "/api/env", nil))
	if rec.Code != 405 {
		t.Fatalf("POST env: %d", rec.Code)
	}
}

func TestWebAuditText(t *testing.T) {
	cases := map[string]string{
		"plain":       "plain",
		" spaced ":    "spaced",
		"a b":         `"a b"`,
		"x=1":         `"x=1"`,
		"line\nbreak": `"line\nbreak"`,
		"日本語":         "日本語",
	}
	for in, want := range cases {
		if got := webAuditText(in); got != want {
			t.Errorf("webAuditText(%q) = %q, want %q", in, got, want)
		}
	}
	long := webAuditText(strings.Repeat("あ", 100))
	if !strings.HasSuffix(long, "…") || len(long) > webAuditTextMax+10 {
		t.Fatalf("truncation: %q", long)
	}
}

func TestWebCallHooksFor(t *testing.T) {
	var got []webAuditEvent
	hooks := webCallHooksFor(func(e webAuditEvent) { got = append(got, e) })
	h := fakeCallHandler(&webFake{}, &webAuditLog{})
	h.hooks = hooks

	req := httptest.NewRequest(http.MethodPost, webCallPathPrefix+"ApproveCommand", strings.NewReader(`["r1","ls"]`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:4000"
	req = req.WithContext(withAdminSession(req.Context(), adminSession{ID: "abcdef0123", Actor: "web:abcdef01"}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(got) != 1 {
		t.Fatalf("events: %+v", got)
	}
	e := got[0]
	if e.Action != "call:ApproveCommand" || e.Actor != "web:abcdef01" || e.Remote != "127.0.0.1" || e.Target != "r1" || e.OK || e.Code != "risk_increased" {
		t.Fatalf("event: %+v", e)
	}
}

// TestWebCallRequiresSessionFailClosed: with the real hooks, a request that
// reached the handler without an admin session (a route mounted without
// requireSession) is refused before the method runs and is not audited.
func TestWebCallRequiresSessionFailClosed(t *testing.T) {
	var got []webAuditEvent
	f := &webFake{}
	h := fakeCallHandler(f, &webAuditLog{})
	h.hooks = webCallHooksFor(func(e webAuditEvent) { got = append(got, e) })

	for _, name := range []string{"ApproveCommand", "GetInitialData", "NoSuchMethod"} {
		req := httptest.NewRequest(http.MethodPost, webCallPathPrefix+name, strings.NewReader(`["r1","ls"]`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sga_agent-token") // a Bearer never stands in for a session
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without session: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(got) != 0 {
		t.Fatalf("refused calls were audited or run: %+v", got)
	}

	// The same handler serves a request that carries a session.
	req := httptest.NewRequest(http.MethodPost, webCallPathPrefix+"ApproveCommand", strings.NewReader(`["r1","ls"]`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withAdminSession(req.Context(), adminSession{ID: "abcdef0123", Actor: "web:abcdef01"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(got) != 1 {
		t.Fatalf("with session: %d %s, audit %+v", rec.Code, rec.Body, got)
	}
}
