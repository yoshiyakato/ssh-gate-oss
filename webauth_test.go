package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type webTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func newWebTestClock() *webTestClock {
	return &webTestClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *webTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *webTestClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestAdminTokenCreateAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "admin-token")
	token, created, err := loadOrCreateAdminToken(path)
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	if !strings.HasPrefix(token, "sgadm_") || len(token) != len("sgadm_")+43 {
		t.Fatalf("token format: %q", token)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("token mode = %#o", info.Mode().Perm())
		}
		dinfo, _ := os.Stat(filepath.Dir(path))
		if dinfo.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %#o", dinfo.Mode().Perm())
		}
	}
	again, created, err := loadOrCreateAdminToken(path)
	if err != nil || created || again != token {
		t.Fatalf("reload: %v created=%v same=%v", err, created, again == token)
	}
	rotated, err := rotateAdminToken(path)
	if err != nil || rotated == token || !validAdminTokenValue(rotated) {
		t.Fatalf("rotate: %v", err)
	}
	reread, _, _ := loadOrCreateAdminToken(path)
	if reread != rotated {
		t.Fatal("rotated token not persisted")
	}
	// 一時ファイルが残っていないこと
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestAdminTokenRejectsWideModeAndBadFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin-token")
	if _, _, err := loadOrCreateAdminToken(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadOrCreateAdminToken(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("wide mode accepted: %v", err)
		}
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("mcp_abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateAdminToken(bad); !errors.Is(err, errAdminTokenFormat) {
		t.Fatalf("bad format: %v", err)
	}
}

type authFixture struct {
	t     *testing.T
	auth  *adminAuth
	clock *webTestClock
	token string
	path  string
	mu    sync.Mutex
	gone  []string
	audit []webAuditEvent
}

func newAuthFixture(t *testing.T, secure bool, policy adminSessionPolicy) *authFixture {
	t.Helper()
	f := &authFixture{t: t, clock: newWebTestClock(), path: filepath.Join(t.TempDir(), "admin-token")}
	tok, _, err := loadOrCreateAdminToken(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.token = tok
	a, err := newAdminAuth(f.path, policy, secure, 8788, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	a.audit = func(e webAuditEvent) { f.mu.Lock(); f.audit = append(f.audit, e); f.mu.Unlock() }
	a.onInvalidate(func(id string) { f.mu.Lock(); f.gone = append(f.gone, id); f.mu.Unlock() })
	f.auth = a
	return f
}

func (f *authFixture) login(token, remote string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"token": token})
	r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	r.RemoteAddr = remote + ":5555"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.auth.handleLogin(w, r)
	rememberSessionKey(w.Header(), w.Result().Cookies())
	return w
}

func (f *authFixture) protected(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	h := f.auth.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := sessionFrom(r.Context())
		if !ok || !strings.HasPrefix(s.Actor, "web:") || len(s.ID) != 64 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		addAdminCookie(r, c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies = %v", cs)
	}
	return cs[0]
}

func (f *authFixture) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.audit {
		out = append(out, e.Action)
	}
	return out
}

func TestAdminLoginSuccessAndCookieAttributes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		f := newAuthFixture(t, secure, adminSessionPolicy{})
		w := f.login(f.token, "127.0.0.1")
		if w.Code != http.StatusNoContent {
			t.Fatalf("secure=%v login = %d %s", secure, w.Code, w.Body)
		}
		c := sessionCookie(t, w)
		raw := w.Header().Get("Set-Cookie")
		if !strings.Contains(raw, "HttpOnly") || !strings.Contains(raw, "SameSite=Strict") || !strings.Contains(raw, "Path=/") {
			t.Fatalf("cookie attrs: %s", raw)
		}
		if strings.Contains(raw, "Max-Age") || strings.Contains(raw, "Domain") {
			t.Fatalf("cookie must be a host-only session cookie: %s", raw)
		}
		if secure {
			if c.Name != "__Host-ssh-gete-admin" || !strings.Contains(raw, "Secure") {
				t.Fatalf("secure cookie: %s", raw)
			}
		} else if c.Name != "ssh-gete-admin-8788" || strings.Contains(raw, "Secure") {
			t.Fatalf("plain cookie: %s", raw)
		}
		if strings.Contains(raw, f.token) {
			t.Fatal("cookie contains admin token")
		}
		if got := f.protected("/api/x", c); got.Code != 200 {
			t.Fatalf("protected with cookie = %d", got.Code)
		}
	}
}

func TestAdminLoginFailureAndUnauthenticated(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	other, _ := newAdminTokenValue()
	if w := f.login("", "127.0.0.1"); w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
		t.Fatalf("empty login = %d", w.Code) // token も password も無い
	}
	for _, tok := range []string{other, f.token + "x", "Bearer " + f.token} {
		if w := f.login(tok, "127.0.0.1"); w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
			t.Fatalf("login %q = %d", tok, w.Code)
		}
		f.auth.limiter.succeed("127.0.0.1") // 遅延を避ける
	}
	if w := f.protected("/api/call/X"); w.Code != http.StatusUnauthorized {
		t.Fatalf("api without session = %d", w.Code)
	}
	w := f.protected("/settings?tab=1")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2Fsettings%3Ftab%3D1" {
		t.Fatalf("page without session = %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := f.protected("/"); w.Header().Get("Location") != "/login" {
		t.Fatalf("root redirect = %q", w.Header().Get("Location"))
	}
	if w := f.protected("/api/x", &http.Cookie{Name: "ssh-gete-admin-8788", Value: "forged"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie = %d", w.Code)
	}
	for _, e := range f.audit {
		if strings.Contains(e.Detail+e.Target+e.Code, f.token) || strings.Contains(e.Detail+e.Target+e.Code, other) {
			t.Fatal("token leaked to audit")
		}
	}
	if acts := f.actions(); len(acts) == 0 || acts[0] != "login_fail" {
		t.Fatalf("audit = %v", acts)
	}
}

func TestAdminSessionIdleAndMaxExpiry(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{IdleMinutes: 10, MaxHours: 1})
	c := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	f.clock.Add(9 * time.Minute)
	if f.protected("/api/x", c).Code != 200 {
		t.Fatal("expired too early")
	}
	f.clock.Add(9 * time.Minute) // 最終利用から9分
	if f.protected("/api/x", c).Code != 200 {
		t.Fatal("touch did not extend idle")
	}
	f.clock.Add(10 * time.Minute)
	if f.protected("/api/x", c).Code != http.StatusUnauthorized {
		t.Fatal("idle expiry not enforced")
	}
	if len(f.gone) != 1 {
		t.Fatalf("invalidate not fired: %v", f.gone)
	}

	c = sessionCookie(t, f.login(f.token, "127.0.0.1"))
	for i := 0; i < 6; i++ {
		f.clock.Add(9 * time.Minute)
		if f.protected("/api/x", c).Code != 200 {
			t.Fatalf("step %d expired", i)
		}
	}
	f.clock.Add(9 * time.Minute) // 作成から63分
	if f.protected("/api/x", c).Code != http.StatusUnauthorized {
		t.Fatal("max lifetime not enforced")
	}
}

func TestAdminSweepFiresInvalidate(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{IdleMinutes: 5})
	f.login(f.token, "127.0.0.1")
	f.clock.Add(6 * time.Minute)
	f.auth.sweep()
	if f.auth.sessionCount() != 0 || len(f.gone) != 1 {
		t.Fatalf("sweep: count=%d gone=%v", f.auth.sessionCount(), f.gone)
	}
}

func TestAdminLoginRotatesSessionID(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	c1 := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	c2 := sessionCookie(t, f.login(f.token, "127.0.0.1", c1))
	if c1.Value == c2.Value {
		t.Fatal("session id not renewed")
	}
	if f.protected("/api/x", c1).Code != http.StatusUnauthorized {
		t.Fatal("old session still valid after re-login")
	}
	if f.protected("/api/x", c2).Code != 200 {
		t.Fatal("new session invalid")
	}
}

func TestAdminMaxSessionsEvictsOldest(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{MaxSessions: 2})
	c1 := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	f.clock.Add(time.Second)
	c2 := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	f.clock.Add(time.Second)
	c3 := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	if f.auth.sessionCount() != 2 {
		t.Fatalf("count = %d", f.auth.sessionCount())
	}
	if f.protected("/api/x", c1).Code != http.StatusUnauthorized || f.protected("/api/x", c2).Code != 200 || f.protected("/api/x", c3).Code != 200 {
		t.Fatal("wrong session evicted")
	}
	if len(f.gone) != 1 || f.gone[0] != adminSessionKey(c1.Value) {
		t.Fatalf("gone = %v", f.gone)
	}
}

func TestAdminLogout(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	c := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	r := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	f.auth.handleLogout(w, r)
	if w.Code != http.StatusNoContent || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout = %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if f.protected("/api/x", c).Code != http.StatusUnauthorized || len(f.gone) != 1 {
		t.Fatal("session survived logout")
	}
}

func TestAdminRotateInvalidatesAll(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	c1 := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	c2 := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	h := f.auth.requireSession(http.HandlerFunc(f.auth.handleRotate))
	r := httptest.NewRequest(http.MethodPost, "/api/admin/rotate-token", nil)
	addAdminCookie(r, c1)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate = %d", w.Code)
	}
	var resp struct{ Token string }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !validAdminTokenValue(resp.Token) || resp.Token == f.token {
		t.Fatalf("rotate token: %q", resp.Token)
	}
	if f.protected("/api/x", c1).Code != 401 || f.protected("/api/x", c2).Code != 401 {
		t.Fatal("sessions survived rotation")
	}
	if f.login(f.token, "127.0.0.1").Code != 401 {
		t.Fatal("old token still works")
	}
	if f.login(resp.Token, "127.0.0.1").Code != 204 {
		t.Fatal("new token does not work")
	}
	onDisk, _, _ := loadOrCreateAdminToken(f.path)
	if onDisk != resp.Token {
		t.Fatal("rotated token not on disk")
	}
}

func TestAdminTokenFileChangedExternally(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	c := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	if changed, err := f.auth.checkTokenFile(); err != nil || changed {
		t.Fatalf("unchanged file: %v %v", changed, err)
	}
	newTok, err := rotateAdminToken(f.path)
	if err != nil {
		t.Fatal(err)
	}
	// 同じ秒の中で書き換えても mtime と大きさが同じことがあるので、mtime をずらす
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(f.path, future, future)
	changed, err := f.auth.checkTokenFile()
	if err != nil || !changed {
		t.Fatalf("external rotation not detected: %v %v", changed, err)
	}
	if f.protected("/api/x", c).Code != 401 {
		t.Fatal("session survived external rotation")
	}
	if f.login(newTok, "127.0.0.1").Code != 204 {
		t.Fatal("new token rejected")
	}
	// 壊れたファイルは受け入れず、今のトークンのまま続ける
	if err := os.WriteFile(f.path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.checkTokenFile(); err == nil {
		t.Fatal("garbage token file accepted")
	}
	if f.login(newTok, "127.0.0.2").Code != 204 {
		t.Fatal("current token rejected after bad file")
	}
}

func TestAdminSessionPolicyValidate(t *testing.T) {
	bad := []adminSessionPolicy{{IdleMinutes: 4}, {IdleMinutes: 721}, {MaxHours: 169}, {MaxSessions: -1}}
	for _, p := range bad {
		if p.validate() == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	if (adminSessionPolicy{IdleMinutes: 5, MaxHours: 168, MaxSessions: 1}).validate() != nil {
		t.Fatal("rejected valid policy")
	}
}

// limiterProbe は check して、取れた枠をすぐ返す（結果は数えない）。
func limiterProbe(l *loginLimiter, remote string) (time.Duration, error) {
	at, d, err := l.check(remote)
	at.release()
	return d, err
}

func TestLoginLimiterDelayGrowth(t *testing.T) {
	clock := newWebTestClock()
	l := newLoginLimiter(clock.Now)
	want := []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		at, d, err := l.check("192.0.2.1")
		if err != nil || d != w {
			t.Fatalf("attempt %d: delay %v err %v, want %v", i, d, err, w)
		}
		at.fail()
	}
	l.succeed("192.0.2.1")
	if d, err := limiterProbe(l, "192.0.2.1"); d != 0 || err != nil {
		t.Fatalf("after success: %v %v", d, err)
	}
}

func TestLoginLimiterRemoteLock(t *testing.T) {
	clock := newWebTestClock()
	l := newLoginLimiter(clock.Now)
	for i := 0; i < 10; i++ {
		l.fail("192.0.2.1")
	}
	_, err := limiterProbe(l, "192.0.2.1")
	var locked *loginLockedError
	if !errors.As(err, &locked) || locked.Scope != "remote" || locked.RetryAfter != 15*time.Minute {
		t.Fatalf("not locked: %v", err)
	}
	if _, err := limiterProbe(l, "192.0.2.2"); err != nil {
		t.Fatal("other remote locked")
	}
	clock.Add(15 * time.Minute)
	if d, err := limiterProbe(l, "192.0.2.1"); err != nil || d != 0 {
		t.Fatalf("lock not lifted: %v %v", d, err)
	}
	// 15分の窓の外の失敗は数えない
	for i := 0; i < 9; i++ {
		l.fail("192.0.2.3")
	}
	clock.Add(16 * time.Minute)
	l.fail("192.0.2.3")
	if _, err := limiterProbe(l, "192.0.2.3"); err != nil {
		t.Fatal("old failures counted")
	}
}

func TestLoginLimiterGlobalLockSparesLoopback(t *testing.T) {
	clock := newWebTestClock()
	l := newLoginLimiter(clock.Now)
	for i := 0; i < 100; i++ {
		l.fail("198.51.100." + string(rune('0'+i%10)) + string(rune('0'+i/10)))
	}
	var locked *loginLockedError
	if _, err := limiterProbe(l, "203.0.113.9"); !errors.As(err, &locked) || locked.Scope != "global" {
		t.Fatalf("global lock not applied: %v", err)
	}
	if _, err := limiterProbe(l, "127.0.0.1"); err != nil {
		t.Fatalf("loopback blocked by global lock: %v", err)
	}
	if _, err := limiterProbe(l, "::1"); err != nil {
		t.Fatalf("::1 blocked by global lock: %v", err)
	}
	clock.Add(15 * time.Minute)
	if _, err := limiterProbe(l, "203.0.113.9"); err != nil {
		t.Fatal("global lock not lifted")
	}
}

func TestLoginLimiterDelaySlots(t *testing.T) {
	l := newLoginLimiter(nil)
	for i := 0; i < 16; i++ {
		if !l.enterDelay() {
			t.Fatalf("slot %d refused", i)
		}
	}
	if l.enterDelay() {
		t.Fatal("17th slot granted")
	}
	l.leaveDelay()
	if !l.enterDelay() {
		t.Fatal("slot not released")
	}
}

func TestAdminLoginLockedReturns429(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	for i := 0; i < 10; i++ {
		f.auth.limiter.fail("192.0.2.7")
	}
	w := f.login(f.token, "192.0.2.7")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "900" {
		t.Fatalf("locked login = %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if acts := f.actions(); len(acts) != 1 || acts[0] != "login_locked" {
		t.Fatalf("audit = %v", acts)
	}
}

func TestAdminLoginAppliesDelay(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	f.auth.limiter.fail("192.0.2.8") // 次は 250ms 待つ
	start := time.Now()
	if w := f.login(f.token, "192.0.2.8"); w.Code != 204 {
		t.Fatalf("login = %d", w.Code)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("delay not applied")
	}
}

// --- 管理パスワード ----------------------------------------------------------

func (f *authFixture) loginPassword(pw, remote string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"password": pw})
	r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	r.RemoteAddr = remote + ":5555"
	w := httptest.NewRecorder()
	f.auth.handleLogin(w, r)
	rememberSessionKey(w.Header(), w.Result().Cookies())
	return w
}

func (f *authFixture) changePassword(cookie *http.Cookie, current, pw string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"current": current, "password": pw})
	h := f.auth.requireSession(http.HandlerFunc(f.auth.handlePassword))
	r := httptest.NewRequest(http.MethodPost, "/api/admin/password", strings.NewReader(string(body)))
	r.RemoteAddr = "127.0.0.1:5555"
	if cookie != nil {
		addAdminCookie(r, cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdminPasswordHashFormat(t *testing.T) {
	phc, err := hashAdminPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(phc, "correct horse") {
		t.Fatalf("phc = %q", phc)
	}
	h, err := parseAdminPasswordHash(phc)
	if err != nil {
		t.Fatal(err)
	}
	if !h.verify("correct horse battery") || h.verify("correct horse batterY") || h.verify("") {
		t.Fatal("verify mismatch")
	}
	other, _ := hashAdminPassword("correct horse battery")
	if other == phc {
		t.Fatal("salt must differ")
	}
	for _, bad := range []string{
		"", "$2a$12$abc", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=999999999,t=2,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5", // memory too large
		"$argon2id$v=19$m=19456,t=99,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=19456,t=2,p=1$!!$a2V5a2V5a2V5a2V5a2V5",
	} {
		if _, err := parseAdminPasswordHash(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestValidateNewAdminPassword(t *testing.T) {
	ok := []string{"twelve chars", "パスワードは十二文字以上です", strings.Repeat("a", 1024)}
	for _, pw := range ok {
		if err := validateNewAdminPassword(pw); err != nil {
			t.Fatalf("%q: %v", pw, err)
		}
	}
	tok, _ := newAdminTokenValue()
	bad := []string{"", "short", "elevenchars", " leading-space-pw", "trailing-space-pw ", "has\nnewline-inside", "tab\there-is-bad",
		strings.Repeat("a", 1025), string([]byte{0xff, 0xfe, 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k'}), tok}
	for _, pw := range bad {
		if err := validateNewAdminPassword(pw); err == nil {
			t.Fatalf("accepted %q", pw)
		}
	}
}

func TestReadNewAdminPassword(t *testing.T) {
	cases := []struct {
		in          string
		interactive bool
		want        string
		wantErr     bool
	}{
		{"pw-twelve-chars\n", false, "pw-twelve-chars", false},
		{"pw-twelve-chars", false, "pw-twelve-chars", false},
		{"pw-twelve-chars\r\npw-twelve-chars\r\n", false, "pw-twelve-chars", false},
		{"pw-twelve-chars\nother\n", false, "", true},
		{"pw-twelve-chars\npw-twelve-chars\n", true, "pw-twelve-chars", false},
		{"pw-twelve-chars\n", true, "", true}, // 端末では確認が必須
		{"", false, "", true},
		{"\n", false, "", true},
	}
	for _, c := range cases {
		var prompts []string
		got, err := readNewAdminPassword(strings.NewReader(c.in), c.interactive, func(s string) { prompts = append(prompts, s) })
		if (err != nil) != c.wantErr || got != c.want {
			t.Fatalf("%q interactive=%v: got %q err %v", c.in, c.interactive, got, err)
		}
		if c.interactive && len(prompts) != 2 || !c.interactive && len(prompts) != 0 {
			t.Fatalf("%q: prompts %v", c.in, prompts)
		}
	}
}

func TestAdminPasswordLoginFlow(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	const pw = "a long admin password"
	// 未設定のあいだはパスワードでは入れない
	if w := f.loginPassword(pw, "127.0.0.1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("password before set = %d", w.Code)
	}
	f.auth.limiter.succeed("127.0.0.1")
	// token と password の両方・どちらも無しは 400
	r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"x","password":"y"}`))
	w := httptest.NewRecorder()
	f.auth.handleLogin(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("both = %d", w.Code)
	}

	// 管理トークンでログインし、Web からパスワードを設定する（初回の手順）
	c := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	if w := f.changePassword(c, "", pw); w.Code != http.StatusForbidden {
		t.Fatalf("set without current = %d", w.Code)
	}
	f.auth.limiter.succeed("127.0.0.1")
	if w := f.changePassword(c, f.token, "short"); w.Code != http.StatusBadRequest {
		t.Fatalf("weak password = %d", w.Code)
	}
	if w := f.changePassword(c, f.token, pw); w.Code != http.StatusNoContent {
		t.Fatalf("set = %d %s", w.Code, w.Body)
	}
	// 自分を含む全セッションが無効になる
	if got := f.protected("/api/x", c); got.Code != http.StatusUnauthorized {
		t.Fatalf("session survived password change: %d", got.Code)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(f.path), adminPasswordFileName))
	if err != nil || strings.Contains(string(data), pw) || !strings.HasPrefix(string(data), "$argon2id$") {
		t.Fatalf("password file: %v %q", err, data)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(filepath.Dir(f.path), adminPasswordFileName))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %#o", info.Mode().Perm())
		}
	}

	// パスワードでログインできる。トークンも使える（復旧手段）
	w = f.loginPassword(pw, "127.0.0.1")
	if w.Code != http.StatusNoContent {
		t.Fatalf("password login = %d", w.Code)
	}
	c2 := sessionCookie(t, w)
	if w := f.login(f.token, "127.0.0.1"); w.Code != http.StatusNoContent {
		t.Fatalf("token after password = %d", w.Code)
	}
	if w := f.loginPassword(pw+"x", "127.0.0.1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", w.Code)
	}
	f.auth.limiter.succeed("127.0.0.1")

	// 変更には今のパスワード（かトークン）が要る
	c2 = sessionCookie(t, f.loginPassword(pw, "127.0.0.1"))
	if w := f.changePassword(c2, "not the password", "another long password"); w.Code != http.StatusForbidden {
		t.Fatalf("change with wrong current = %d", w.Code)
	}
	f.auth.limiter.succeed("127.0.0.1")
	if w := f.changePassword(c2, pw, ""); w.Code != http.StatusNoContent {
		t.Fatalf("clear = %d", w.Code)
	}
	if w := f.loginPassword(pw, "127.0.0.1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("password after clear = %d", w.Code)
	}
	for _, e := range f.audit {
		if strings.Contains(e.Detail+e.Target+e.Code, pw) {
			t.Fatal("password leaked to audit")
		}
	}
	acts := strings.Join(f.actions(), ",")
	if !strings.Contains(acts, "password_change") || !strings.Contains(acts, "login_fail") {
		t.Fatalf("audit = %s", acts)
	}
}

func TestAdminPasswordFailuresCountTowardLock(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	if err := setAdminPasswordFile(filepath.Join(filepath.Dir(f.path), adminPasswordFileName), "a long admin password", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.checkPasswordFile(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < loginRemoteMaxFails; i++ {
		f.auth.limiter.fail("192.0.2.9")
	}
	if w := f.loginPassword("a long admin password", "192.0.2.9"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked remote = %d", w.Code)
	}
}

func TestAdminPasswordFileChangedExternally(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	pwPath := filepath.Join(filepath.Dir(f.path), adminPasswordFileName)
	c := sessionCookie(t, f.login(f.token, "127.0.0.1"))
	if changed, err := f.auth.checkPasswordFile(); changed || err != nil {
		t.Fatalf("no change: %v %v", changed, err)
	}
	if err := setAdminPasswordFile(pwPath, "set from the cli ok", nil); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.auth.checkPasswordFile(); !changed || err != nil {
		t.Fatalf("change not seen: %v %v", changed, err)
	}
	if got := f.protected("/api/x", c); got.Code != http.StatusUnauthorized {
		t.Fatalf("session survived: %d", got.Code)
	}
	if w := f.loginPassword("set from the cli ok", "127.0.0.1"); w.Code != http.StatusNoContent {
		t.Fatalf("login with new password = %d", w.Code)
	}
	// 壊れたファイルはパスワードでのログインを止める（トークンは使える）
	if err := os.WriteFile(pwPath, []byte("garbage-but-longer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.checkPasswordFile(); err == nil {
		t.Fatal("garbage accepted")
	}
	if w := f.loginPassword("set from the cli ok", "127.0.0.1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("login with broken file = %d", w.Code)
	}
	f.auth.limiter.succeed("127.0.0.1")
	if w := f.login(f.token, "127.0.0.1"); w.Code != http.StatusNoContent {
		t.Fatalf("token with broken password file = %d", w.Code)
	}
	// 消すと未設定に戻る
	if err := os.Remove(pwPath); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.auth.checkPasswordFile(); !changed || err != nil {
		t.Fatalf("remove not seen: %v %v", changed, err)
	}
	if f.auth.passwordSet() {
		t.Fatal("password still set")
	}
}

func TestAdminPasswordFileRefusesWideMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not used on Windows")
	}
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "admin-token")
	if _, _, err := loadOrCreateAdminToken(tokenPath); err != nil {
		t.Fatal(err)
	}
	pwPath := filepath.Join(dir, adminPasswordFileName)
	if err := setAdminPasswordFile(pwPath, "a long admin password", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pwPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newAdminAuth(tokenPath, adminSessionPolicy{}, false, 8788, nil); err == nil {
		t.Fatal("0644 password file accepted")
	}
}
