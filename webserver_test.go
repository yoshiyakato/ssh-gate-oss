package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

const webTestIndex = `<!doctype html>
<html lang="ja">
<head>
  <meta charset="UTF-8">
  <title>ssh-gete</title>
  <script type="module" crossorigin src="/assets/index-abc.js"></script>
</head>
<body><header>x</header><div id="app"></div></body>
</html>`

func webTestFS() (fstest.MapFS, fstest.MapFS) {
	dist := fstest.MapFS{
		"index.html":       {Data: []byte(webTestIndex)},
		"assets/index.js":  {Data: []byte("console.log(1)")},
		"assets/index.css": {Data: []byte("body{}")},
	}
	admin := fstest.MapFS{
		"headless-shim.js": {Data: []byte("window.go={}")},
		"login.html":       {Data: []byte("<html><head></head><body>login</body></html>")},
		"login.js":         {Data: []byte("// login")},
		"login.css":        {Data: []byte("/* login */")},
	}
	return dist, admin
}

func TestInjectShim(t *testing.T) {
	out, err := injectShim([]byte(webTestIndex))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	shim := strings.Index(s, `<script src="/headless-shim.js"></script>`)
	head := strings.Index(s, "<head>")
	module := strings.Index(s, `type="module"`)
	if shim < 0 || shim < head || shim > module {
		t.Fatalf("shim position wrong:\n%s", s)
	}
	if strings.Count(s, "headless-shim.js") != 1 {
		t.Fatal("shim injected more than once")
	}
	if strings.Contains(s[shim:], `defer`) && strings.Contains(s[shim:shim+60], "defer") {
		t.Fatal("shim must be a classic synchronous script")
	}
	// <head lang=..> の属性つき、大文字
	out, err = injectShim([]byte("<HTML><HEAD data-x=1><title>t</title></HEAD></HTML>"))
	if err != nil || !strings.Contains(string(out), `<HEAD data-x=1>`+"\n"+`<script src="/headless-shim.js">`) {
		t.Fatalf("attr head: %v %s", err, out)
	}
	// <header> だけでは <head> とみなさない
	if _, err := injectShim([]byte("<html><body><header></header></body></html>")); err == nil {
		t.Fatal("index without <head> accepted")
	}
	if _, err := injectShim(out); err == nil {
		t.Fatal("double injection accepted")
	}
}

func TestStaticHandlerRequiresFiles(t *testing.T) {
	dist, admin := webTestFS()
	if _, err := newStaticHandler(fstest.MapFS{"index.html": {Data: []byte("<html><body></body></html>")}}, admin); err == nil {
		t.Fatal("index without head accepted")
	}
	if _, err := newStaticHandler(dist, fstest.MapFS{"login.html": admin["login.html"]}); err == nil {
		t.Fatal("missing shim accepted")
	}
	if _, err := newStaticHandler(fstest.MapFS{}, admin); err == nil {
		t.Fatal("missing index accepted")
	}
}

// --- サーバー全体 -------------------------------------------------------------

type fakeEvents struct {
	mu        sync.Mutex
	closed    []string
	shutdowns int32
}

func (e *fakeEvents) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, ": ok "+webSessionID(r)+"\n\n")
}
func (e *fakeEvents) closeSession(id string) {
	e.mu.Lock()
	e.closed = append(e.closed, id)
	e.mu.Unlock()
}
func (e *fakeEvents) shutdown()            { atomic.AddInt32(&e.shutdowns, 1) }
func (e *fakeEvents) shutdownCount() int32 { return atomic.LoadInt32(&e.shutdowns) }
func (e *fakeEvents) closedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.closed...)
}

type webFixture struct {
	t       *testing.T
	srv     *webServer
	base    string
	token   string
	events  *fakeEvents
	release chan struct{}
	inCall  chan struct{}
	client  *http.Client
}

func startWebFixture(t *testing.T, mutate func(*webServerOptions)) *webFixture {
	t.Helper()
	dist, admin := webTestFS()
	f := &webFixture{t: t, events: &fakeEvents{}, release: make(chan struct{}), inCall: make(chan struct{}, 8)}
	opts := webServerOptions{
		Listen:  "127.0.0.1:0",
		DataDir: t.TempDir(),
		Dist:    dist,
		Admin:   admin,
		RPC: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/Slow") {
				f.inCall <- struct{}{}
				<-f.release
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			webauthJSON(w, 200, map[string]any{"result": map[string]any{"path": r.URL.Path, "len": len(body), "session": webSessionID(r)}})
		}),
		Events: f.events,
		Env: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			webauthJSON(w, 200, map[string]string{"platform": runtime.GOOS})
		}),
		Logf: t.Logf,
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := newWebServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	f.srv = srv
	f.base = strings.TrimSuffix(srv.URL(), "/")
	tok, _, err := loadOrCreateAdminToken(srv.AdminTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	f.token = tok
	jar, _ := cookiejar.New(nil)
	f.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if srv.tlsConfig != nil {
		pool := x509.NewCertPool()
		pool.AddCert(srv.tlsConfig.Certificates[0].Leaf)
		f.client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true}
	}
	withSessionKey(f.client)
	return f
}

// do は shim と同じ形の要求を送る（Origin・CSRF ヘッダ・JSON）。mut で崩す。
func (f *webFixture) do(method, path, body string, mut func(*http.Request)) *http.Response {
	f.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.base+path, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Origin", f.base)
	req.Header.Set(webCSRFHeader, "1")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if mut != nil {
		mut(req)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (f *webFixture) login() {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"token": f.token})
	if resp := f.do("POST", "/api/login", string(body), nil); resp.StatusCode != 204 {
		f.t.Fatalf("login = %d", resp.StatusCode)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestWebServerLoginFlowAndRoutes(t *testing.T) {
	f := startWebFixture(t, nil)
	// 未ログイン
	if resp := f.do("GET", "/", "", func(r *http.Request) { r.Header.Del("Origin") }); resp.StatusCode != 303 || resp.Header.Get("Location") != "/login" {
		t.Fatalf("page unauth = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := f.do("POST", "/api/call/GetInitialData", "[]", nil); resp.StatusCode != 401 {
		t.Fatalf("api unauth = %d", resp.StatusCode)
	}
	if resp := f.do("GET", "/headless-shim.js", "", nil); resp.StatusCode != 303 {
		t.Fatalf("shim must need session: %d", resp.StatusCode)
	}
	// セッション不要のもの
	if resp := f.do("GET", "/login", "", nil); resp.StatusCode != 200 || !strings.Contains(readBody(t, resp), "login") {
		t.Fatalf("login page = %d", resp.StatusCode)
	}
	if resp := f.do("GET", "/login-assets/login.js", "", nil); resp.StatusCode != 200 {
		t.Fatalf("login asset = %d", resp.StatusCode)
	}
	if resp := f.do("GET", "/login-assets/headless-shim.js", "", nil); resp.StatusCode != 404 {
		t.Fatalf("shim via login-assets = %d", resp.StatusCode)
	}

	f.login()
	resp := f.do("GET", "/", "", nil)
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `<script src="/headless-shim.js"></script>`) {
		t.Fatalf("index = %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index headers: %v", resp.Header)
	}
	if resp := f.do("GET", "/headless-shim.js", "", nil); resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("shim = %d %v", resp.StatusCode, resp.Header)
	}
	if resp := f.do("GET", "/assets/index.js", "", nil); resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "private, max-age=86400" {
		t.Fatalf("asset = %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if resp := f.do("GET", "/assets/", "", nil); resp.StatusCode != 404 {
		t.Fatalf("directory listing = %d", resp.StatusCode)
	}
	if resp := f.do("GET", "/nope.txt", "", nil); resp.StatusCode != 404 {
		t.Fatalf("missing = %d", resp.StatusCode)
	}
	resp = f.do("POST", "/api/call/GetInitialData", "[]", nil)
	body = readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"/api/call/GetInitialData"`) || strings.Contains(body, `"session":""`) {
		t.Fatalf("call = %d %s", resp.StatusCode, body)
	}
	if resp := f.do("GET", "/api/env", "", nil); resp.StatusCode != 200 {
		t.Fatalf("env = %d", resp.StatusCode)
	}
	if resp := f.do("GET", "/api/events", "", nil); resp.StatusCode != 200 || !strings.Contains(readBody(t, resp), ": ok ") {
		t.Fatalf("events = %d", resp.StatusCode)
	}
	resp = f.do("GET", "/api/session", "", nil)
	var sess map[string]any
	_ = json.Unmarshal([]byte(readBody(t, resp)), &sess)
	if sess["authenticated"] != true || sess["expiresAt"] == nil {
		t.Fatalf("session = %v", sess)
	}
	if resp := f.do("GET", "/api/admin/audit", "", nil); resp.StatusCode != 404 {
		t.Fatalf("unconfigured audit = %d", resp.StatusCode)
	}
	// ログアウトでイベント層のセッションが閉じる
	if resp := f.do("POST", "/api/logout", "{}", nil); resp.StatusCode != 204 {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	if len(f.events.closedIDs()) != 1 {
		t.Fatalf("closeSession not called: %v", f.events.closedIDs())
	}
	if resp := f.do("POST", "/api/call/GetInitialData", "[]", nil); resp.StatusCode != 401 {
		t.Fatalf("after logout = %d", resp.StatusCode)
	}
}

func TestWebServerGuards(t *testing.T) {
	f := startWebFixture(t, nil)
	f.login()
	port := strconv.Itoa(f.srv.Port())
	cases := []struct {
		name string
		mut  func(*http.Request)
		want int
	}{
		{"dns rebinding host", func(r *http.Request) { r.Host = "evil.example:" + port }, 403},
		{"wrong port host", func(r *http.Request) { r.Host = "127.0.0.1:1" }, 403},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:5173") }, 403},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"no origin on POST", func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"no csrf header", func(r *http.Request) { r.Header.Del(webCSRFHeader) }, 403},
		{"wrong csrf header", func(r *http.Request) { r.Header.Set(webCSRFHeader, "0") }, 403},
		{"text/plain", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"cross-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"same-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403},
		{"authorization", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }, 400},
		{"authorization no csrf", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer x")
			r.Header.Del(webCSRFHeader)
		}, 400},
		{"ok", nil, 200},
		// localhost の Host とオリジンは検査を通る（Cookie は 127.0.0.1 のものなので 401 で止まる）
		{"localhost origin passes guard", func(r *http.Request) { r.Header.Set("Origin", "http://localhost:"+port); r.Host = "localhost:" + port }, 401},
	}
	for _, c := range cases {
		resp := f.do("POST", "/api/call/GetInitialData", "[]", c.mut)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, resp.StatusCode, c.want, readBody(t, resp))
		}
		if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: security headers missing", c.name)
		}
	}
	// 本文の上限（1 MiB）
	big := "[\"" + strings.Repeat("a", webAPIBodyLimit) + "\"]"
	if resp := f.do("POST", "/api/call/Echo", big, nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("body limit: %d", resp.StatusCode)
	}
	// GET の API でも CSRF ヘッダは必須（EventSource 型の取り込みを防ぐ）
	if resp := f.do("GET", "/api/events", "", func(r *http.Request) { r.Header.Del(webCSRFHeader) }); resp.StatusCode != 403 {
		t.Errorf("events without csrf = %d", resp.StatusCode)
	}
	// ログインも同じ検査を通る
	body, _ := json.Marshal(map[string]string{"token": f.token})
	if resp := f.do("POST", "/api/login", string(body), func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); resp.StatusCode != 403 {
		t.Errorf("cross-origin login = %d", resp.StatusCode)
	}
}

func TestWebServerSecurityHeaders(t *testing.T) {
	f := startWebFixture(t, nil)
	resp := f.do("GET", "/login", "", nil)
	h := resp.Header
	want := map[string]string{
		"Content-Security-Policy":      webContentSecurityPolicy,
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), usb=(), serial=()",
		"Cache-Control":                "no-store",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
	if !strings.Contains(webContentSecurityPolicy, "script-src 'self';") || strings.Contains(webContentSecurityPolicy, "script-src 'self' 'unsafe-inline'") {
		t.Error("CSP must not allow inline scripts")
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS on plain HTTP")
	}
}

func TestWebServerStopIntakeAndWaitCalls(t *testing.T) {
	f := startWebFixture(t, nil)
	f.login()
	done := make(chan int, 1)
	go func() {
		resp := f.do("POST", "/api/call/Slow", "[]", nil)
		done <- resp.StatusCode
	}()
	<-f.inCall
	f.srv.StopIntake()
	f.srv.StopIntake() // 2回目は何もしない
	if f.events.shutdownCount() != 1 {
		t.Fatalf("events.shutdown called %d times", f.events.shutdownCount())
	}
	if resp := f.do("POST", "/api/call/GetInitialData", "[]", nil); resp.StatusCode != 503 {
		t.Fatalf("call after StopIntake = %d", resp.StatusCode)
	}
	body, _ := json.Marshal(map[string]string{"token": f.token})
	if resp := f.do("POST", "/api/login", string(body), nil); resp.StatusCode != 503 {
		t.Fatalf("login after StopIntake = %d", resp.StatusCode)
	}
	if f.srv.WaitCalls(50 * time.Millisecond) {
		t.Fatal("WaitCalls returned true while a call is running")
	}
	close(f.release)
	if !f.srv.WaitCalls(2 * time.Second) {
		t.Fatal("WaitCalls timed out")
	}
	if code := <-done; code != 200 {
		t.Fatalf("in-flight call = %d", code)
	}
	if err := f.srv.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", strings.TrimPrefix(f.base, "http://"), 200*time.Millisecond); err == nil {
		t.Fatal("still listening after Close")
	}
}

func TestWebServerRotateViaAPI(t *testing.T) {
	f := startWebFixture(t, nil)
	f.login()
	resp := f.do("POST", "/api/admin/rotate-token", "{}", nil)
	var out struct{ Token string }
	_ = json.Unmarshal([]byte(readBody(t, resp)), &out)
	if resp.StatusCode != 200 || !validAdminTokenValue(out.Token) {
		t.Fatalf("rotate = %d", resp.StatusCode)
	}
	if resp := f.do("GET", "/api/session", "", nil); resp.StatusCode != 401 {
		t.Fatalf("own session survived rotation: %d", resp.StatusCode)
	}
	f.token = out.Token
	f.login()
}

func TestWebServerWatchesTokenFile(t *testing.T) {
	f := startWebFixture(t, func(o *webServerOptions) { o.TokenCheckEvery = 20 * time.Millisecond })
	f.login()
	if _, err := rotateAdminToken(f.srv.AdminTokenPath()); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(3 * time.Second)
	_ = os.Chtimes(f.srv.AdminTokenPath(), future, future)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.do("GET", "/api/session", "", nil).StatusCode == 401 {
			if len(f.events.closedIDs()) == 0 {
				t.Fatal("SSE sessions not closed on rotation")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("external rotation not picked up")
}

func TestWebServerSelfSignedTLS(t *testing.T) {
	f := startWebFixture(t, func(o *webServerOptions) { o.TLSMode = "self-signed" })
	if !strings.HasPrefix(f.base, "https://") || len(strings.Split(f.srv.TLSFingerprint(), ":")) != 32 {
		t.Fatalf("base=%s fp=%s", f.base, f.srv.TLSFingerprint())
	}
	body, _ := json.Marshal(map[string]string{"token": f.token})
	resp := f.do("POST", "/api/login", string(body), nil)
	if resp.StatusCode != 204 {
		t.Fatalf("tls login = %d", resp.StatusCode)
	}
	sc := resp.Header.Get("Set-Cookie")
	if !strings.HasPrefix(sc, "__Host-ssh-gete-admin=") || !strings.Contains(sc, "Secure") {
		t.Fatalf("tls cookie = %q", sc)
	}
	// Loopback without an https publicURL: no HSTS (it would cover every
	// http://localhost:* site for a year; review finding 3).
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS sent on a loopback listener without a publicURL")
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("HTTP/2 not negotiated: %s", resp.Proto)
	}
	if resp := f.do("GET", "/api/session", "", nil); resp.StatusCode != 200 {
		t.Fatalf("tls session = %d", resp.StatusCode)
	}
}

func TestLoadOrCreateSelfSigned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "web-tls")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	hosts := []string{"192.0.2.10", "gate.example.lan", "localhost", "127.0.0.1", "::1"}
	cert, fp, err := loadOrCreateSelfSigned(dir, hosts, now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if !webCertCovers(leaf, hosts) || leaf.NotAfter.Sub(now) < 396*24*time.Hour {
		t.Fatalf("SAN/validity: %v %v %v", leaf.DNSNames, leaf.IPAddresses, leaf.NotAfter)
	}
	if leaf.PublicKeyAlgorithm != x509.ECDSA {
		t.Fatalf("key algo = %v", leaf.PublicKeyAlgorithm)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, "key.pem"))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("key mode %#o", info.Mode().Perm())
		}
	}
	// 再利用
	_, fp2, err := loadOrCreateSelfSigned(dir, hosts, now.Add(24*time.Hour))
	if err != nil || fp2 != fp {
		t.Fatalf("not reused: %v", err)
	}
	// SAN が足りなければ作り直す
	_, fp3, err := loadOrCreateSelfSigned(dir, append(hosts, "198.51.100.1"), now)
	if err != nil || fp3 == fp {
		t.Fatalf("not regenerated for new SAN: %v", err)
	}
	// 期限の30日前を過ぎたら作り直す
	_, fp4, err := loadOrCreateSelfSigned(dir, append(hosts, "198.51.100.1"), now.Add(370*24*time.Hour))
	if err != nil || fp4 == fp3 {
		t.Fatalf("not renewed near expiry: %v", err)
	}
	// 鍵の権限が広ければ作り直す
	if runtime.GOOS != "windows" {
		_ = os.Chmod(filepath.Join(dir, "key.pem"), 0o644)
		_, fp5, err := loadOrCreateSelfSigned(dir, append(hosts, "198.51.100.1"), now.Add(370*24*time.Hour))
		if err != nil || fp5 == fp4 {
			t.Fatalf("wide key reused: %v", err)
		}
	}
}

func TestWebTLSHosts(t *testing.T) {
	hosts := webTLSHosts("192.0.2.10:8788", "https://gate.example.lan:8788")
	for _, want := range []string{"192.0.2.10", "gate.example.lan", "localhost", "127.0.0.1", "::1"} {
		found := false
		for _, h := range hosts {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Errorf("SAN missing %s: %v", want, hosts)
		}
	}
	for _, h := range webTLSHosts("0.0.0.0:8788", "") {
		if h == "0.0.0.0" {
			t.Error("unspecified address in SAN")
		}
	}
}

func TestValidateWebListen(t *testing.T) {
	mcp := MCPSettings{Enabled: true, ListenAddress: "127.0.0.1", Port: 8787}
	adminTok, _ := newAdminTokenValue()
	adminHash := adminTokenHashHex(adminTok)
	cases := []struct {
		name string
		opts webServerOptions
		mcp  MCPSettings
		tok  string
		ok   bool
	}{
		{"loopback plain", webServerOptions{Listen: "127.0.0.1:8788"}, mcp, "", true},
		{"default listen", webServerOptions{}, mcp, "", true},
		{"localhost plain", webServerOptions{Listen: "localhost:8788"}, mcp, "", true},
		{"ipv6 loopback plain", webServerOptions{Listen: "[::1]:8788"}, mcp, "", true},
		{"loopback tls", webServerOptions{Listen: "127.0.0.1:8788", TLSMode: "self-signed"}, mcp, "", true},
		{"any plain", webServerOptions{Listen: "0.0.0.0:8788"}, mcp, "", false},
		{"any6 plain", webServerOptions{Listen: "[::]:8788"}, mcp, "", false},
		{"lan plain", webServerOptions{Listen: "192.0.2.10:8788", TLSMode: "off"}, mcp, "", false},
		{"lan self-signed", webServerOptions{Listen: "0.0.0.0:8788", TLSMode: "self-signed"}, mcp, "", true},
		{"lan files", webServerOptions{Listen: "0.0.0.0:8788", TLSMode: "files", TLSCert: "c", TLSKey: "k"}, mcp, "", true},
		{"files without paths", webServerOptions{Listen: "0.0.0.0:8788", TLSMode: "files"}, mcp, "", false},
		{"bad tls mode", webServerOptions{Listen: "127.0.0.1:8788", TLSMode: "on"}, mcp, "", false},
		{"loopback + https proxy", webServerOptions{Listen: "127.0.0.1:8788", PublicURL: "https://gate.example.lan"}, mcp, "", true},
		{"loopback + http lan publicURL", webServerOptions{Listen: "127.0.0.1:8788", PublicURL: "http://gate.example.lan"}, mcp, "", false},
		{"loopback + http loopback publicURL", webServerOptions{Listen: "127.0.0.1:8788", PublicURL: "http://127.0.0.1:8788"}, mcp, "", true},
		{"tls + http publicURL", webServerOptions{Listen: "127.0.0.1:8788", TLSMode: "self-signed", PublicURL: "http://localhost:8788"}, mcp, "", false},
		{"publicURL with path", webServerOptions{Listen: "127.0.0.1:8788", PublicURL: "https://gate.example.lan/admin"}, mcp, "", false},
		{"hostname listen", webServerOptions{Listen: "gate.example.lan:8788"}, mcp, "", false},
		{"missing host", webServerOptions{Listen: ":8788"}, mcp, "", false},
		{"bad port", webServerOptions{Listen: "127.0.0.1:99999"}, mcp, "", false},
		{"same as mcp", webServerOptions{Listen: "127.0.0.1:8787"}, mcp, "", false},
		{"localhost same as mcp", webServerOptions{Listen: "localhost:8787"}, mcp, "", false},
		{"any overlaps mcp", webServerOptions{Listen: "0.0.0.0:8787", TLSMode: "self-signed"}, mcp, "", false},
		{"mcp disabled same port", webServerOptions{Listen: "127.0.0.1:8787"}, MCPSettings{ListenAddress: "127.0.0.1", Port: 8787}, "", true},
		{"different ip same port", webServerOptions{Listen: "127.0.0.2:8787"}, mcp, "", true},
		{"token reuse", webServerOptions{Listen: "127.0.0.1:8788"}, mcp, adminTok, false},
		{"different mcp token", webServerOptions{Listen: "127.0.0.1:8788"}, mcp, "mcp-token", true},
	}
	for _, c := range cases {
		err := validateWebListen(c.opts, c.mcp, c.tok, adminHash)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestNewWebServerRefusesUnsafeListen(t *testing.T) {
	dist, admin := webTestFS()
	_, err := newWebServer(webServerOptions{Listen: "0.0.0.0:0", DataDir: t.TempDir(), Dist: dist, Admin: admin})
	if err == nil || !strings.Contains(err.Error(), "--tls-self-signed") {
		t.Fatalf("plain LAN listen accepted: %v", err)
	}
}

func TestWebOrigins(t *testing.T) {
	got := webOrigins("127.0.0.1:8788", "", 8788, false)
	want := []string{"http://127.0.0.1:8788", "http://localhost:8788", "http://[::1]:8788"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("origins = %v", got)
	}
	got = webOrigins("192.0.2.10:8788", "", 8788, true)
	if got[len(got)-1] != "https://192.0.2.10:8788" || !strings.HasPrefix(got[0], "https://") {
		t.Fatalf("lan origins = %v", got)
	}
	got = webOrigins("127.0.0.1:8788", "https://gate.example.lan:9443/", 8788, false)
	if len(got) != 1 || got[0] != "https://gate.example.lan:9443" {
		t.Fatalf("publicURL origins = %v", got)
	}
}

func TestWebServerPublicURLBehindProxy(t *testing.T) {
	f := startWebFixture(t, func(o *webServerOptions) { o.PublicURL = "https://gate.example.lan" })
	// プロキシ経由: Host と Origin は publicURL。Cookie は Secure なので平文の
	// テストクライアントのジャーには残らない。ここでは検査の通り方だけ見る。
	pub := "https://gate.example.lan"
	body, _ := json.Marshal(map[string]string{"token": f.token})
	req, _ := http.NewRequest("POST", "http://"+f.srv.listener.Addr().String()+"/api/login", strings.NewReader(string(body)))
	req.Host = "gate.example.lan"
	req.Header.Set("Origin", pub)
	req.Header.Set(webCSRFHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || !strings.Contains(resp.Header.Get("Set-Cookie"), "__Host-ssh-gete-admin=") {
		t.Fatalf("proxy login = %d %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
	if f.srv.URL() != pub+"/" {
		t.Fatalf("URL = %s", f.srv.URL())
	}
	// ループバックのオリジンは publicURL があると通らない
	local := "http://" + f.srv.listener.Addr().String()
	req2, _ := http.NewRequest("POST", local+"/api/login", strings.NewReader(string(body)))
	req2.Header.Set("Origin", local)
	req2.Header.Set(webCSRFHeader, "1")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 403 {
		t.Fatalf("loopback origin with publicURL = %d", resp2.StatusCode)
	}
}

func TestWebServerOptionsFromConfig(t *testing.T) {
	cfg := WebConfig{Listen: "0.0.0.0:9000", PublicURL: "https://x", TLS: WebTLSConfig{Mode: "files", Cert: "c", Key: "k"}, AdminTokenFile: "/t", Session: SessionConfig{IdleMinutes: 10, MaxHours: 2}, MaxSessions: 4}
	o := webServerOptionsFromConfig(cfg, "/data")
	if o.Listen != cfg.Listen || o.TLSMode != "files" || o.TLSCert != "c" || o.AdminTokenFile != "/t" || o.DataDir != "/data" || o.Session != (adminSessionPolicy{10, 2, 4}) {
		t.Fatalf("options = %+v", o)
	}
}

func TestWebServerOptionsPasswordFile(t *testing.T) {
	if o := webServerOptionsFromConfig(WebConfig{}, "/data"); o.AdminPasswordFile != filepath.Join("/data", "admin-password") {
		t.Fatalf("default password file = %q", o.AdminPasswordFile)
	}
	if o := webServerOptionsFromConfig(WebConfig{AdminPasswordFile: "/etc/pw"}, "/data"); o.AdminPasswordFile != "/etc/pw" {
		t.Fatalf("configured password file = %q", o.AdminPasswordFile)
	}
}

// 初回の手順（トークンでログイン→パスワードを設定→パスワードでログイン）を
// 本物の HTTP サーバーで通す。CSRF・Origin の検査も効いていること。
func TestWebServerPasswordSetupFlow(t *testing.T) {
	pwPath := filepath.Join(t.TempDir(), "pw", "admin-password")
	f := startWebFixture(t, func(o *webServerOptions) { o.AdminPasswordFile = pwPath })
	const pw = "the web admin password"
	set, _ := json.Marshal(map[string]string{"current": f.token, "password": pw})
	if resp := f.do("POST", "/api/admin/password", string(set), nil); resp.StatusCode != 401 {
		t.Fatalf("set without session = %d", resp.StatusCode)
	}
	f.login()
	if resp := f.do("POST", "/api/admin/password", string(set), func(r *http.Request) { r.Header.Del(webCSRFHeader) }); resp.StatusCode != 403 {
		t.Fatalf("set without csrf = %d", resp.StatusCode)
	}
	if resp := f.do("POST", "/api/admin/password", string(set), func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); resp.StatusCode != 403 {
		t.Fatalf("set cross-origin = %d", resp.StatusCode)
	}
	resp := f.do("GET", "/api/session", "", nil)
	if body := readBody(t, resp); !strings.Contains(body, `"passwordSet":false`) {
		t.Fatalf("session before = %s", body)
	}
	if resp := f.do("POST", "/api/admin/password", string(set), nil); resp.StatusCode != 204 {
		t.Fatalf("set = %d %s", resp.StatusCode, readBody(t, resp))
	}
	if resp := f.do("GET", "/api/session", "", nil); resp.StatusCode != 401 {
		t.Fatalf("session must end after the change: %d", resp.StatusCode)
	}
	login, _ := json.Marshal(map[string]string{"password": pw})
	if resp := f.do("POST", "/api/login", string(login), nil); resp.StatusCode != 204 {
		t.Fatalf("password login = %d", resp.StatusCode)
	}
	resp = f.do("GET", "/api/session", "", nil)
	if body := readBody(t, resp); !strings.Contains(body, `"passwordSet":true`) {
		t.Fatalf("session after = %s", body)
	}
	if data, err := os.ReadFile(pwPath); err != nil || strings.Contains(string(data), pw) {
		t.Fatalf("password file: %v", err)
	}
	// Bearer でパスワードの変更は呼べない（エージェントのトークンを持ち込ませない）
	if resp := f.do("POST", "/api/admin/password", string(set), func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.token) }); resp.StatusCode != 400 {
		t.Fatalf("with Authorization = %d", resp.StatusCode)
	}
}

// §6.5 の残りの半分: 管理トークンも管理の Cookie も MCP の認証には使えない。
func TestMCPRejectsAdminCredentials(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	defer app.shutdown(context.Background())
	adminToken, err := newAdminTokenValue()
	if err != nil {
		t.Fatal(err)
	}
	const shared = "mcp-shared-token-value-0123456789"
	settings := MCPSettings{BearerEnabled: true}
	req := func(mut func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		mut(r)
		return r
	}
	cases := map[string]func(*http.Request){
		"admin token as Bearer": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) },
		"admin cookie (secure)": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: adminCookieSecureName, Value: "session-value"})
		},
		"admin cookie (plain)": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: adminCookiePlainName + "8788", Value: "session-value"})
		},
		"csrf header only": func(r *http.Request) { r.Header.Set(webCSRFHeader, "1") },
	}
	for name, mut := range cases {
		if _, ok := app.authenticateMCP(req(mut), settings, shared); ok {
			t.Fatalf("%s accepted by MCP", name)
		}
	}
	if _, ok := app.authenticateMCP(req(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+shared) }), settings, shared); !ok {
		t.Fatal("the shared MCP token must still work")
	}
	// 同じ値を使い回す設定は起動を拒否する（§6.1）
	if err := validateWebListen(webServerOptions{Listen: "127.0.0.1:8788"}, MCPSettings{}, adminToken, adminTokenHashHex(adminToken)); err == nil {
		t.Fatal("MCP token equal to admin token accepted")
	}
}
