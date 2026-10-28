package main

// Regression tests for the REQ-015 adversarial review (webauth.go /
// webserver.go / app.go fixes). Each test names the finding it covers.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// Finding 1: a reservation is taken at check time, so attempts in flight
// count toward the per-IP and global limits before any failure is recorded.
func TestLoginLimiterReservesInFlightAttempts(t *testing.T) {
	l := newLoginLimiter(newWebTestClock().Now)
	var held []*loginAttempt
	for i := 0; i < loginRemoteMaxFails; i++ {
		at, d, err := l.check("192.0.2.7")
		if err != nil {
			t.Fatalf("attempt %d refused: %v", i, err)
		}
		if want := loginDelay(i); d != want {
			t.Fatalf("attempt %d delay %v, want %v (counted from fails+inflight)", i, d, want)
		}
		held = append(held, at)
	}
	var locked *loginLockedError
	if _, _, err := l.check("192.0.2.7"); !errors.As(err, &locked) || locked.Scope != "remote" {
		t.Fatalf("11th concurrent attempt admitted: %v", err)
	}
	held[0].release()
	held[0].release() // a second release is a no-op
	at, _, err := l.check("192.0.2.7")
	if err != nil {
		t.Fatalf("slot not returned: %v", err)
	}
	held[0] = at
	for _, at := range held {
		at.fail()
	}
	if _, _, err := l.check("192.0.2.7"); !errors.As(err, &locked) || locked.RetryAfter != loginLockDuration {
		t.Fatalf("10 failures did not lock: %v", err)
	}

	// Global: 100 in flight from different addresses block the 101st.
	g := newLoginLimiter(newWebTestClock().Now)
	for i := 0; i < loginGlobalMaxFails; i++ {
		if _, _, err := g.check(testIP(i)); err != nil {
			t.Fatalf("global attempt %d refused: %v", i, err)
		}
	}
	if _, _, err := g.check("203.0.113.200"); !errors.As(err, &locked) || locked.Scope != "global" {
		t.Fatalf("101st concurrent attempt admitted: %v", err)
	}
	if at, _, err := g.check("127.0.0.1"); err != nil {
		t.Fatalf("loopback refused by the global in-flight limit: %v", err)
	} else {
		at.release()
	}
}

func testIP(i int) string {
	return "198.51." + itoa(100+i/250) + "." + itoa(1+i%250)
}

func itoa(n int) string { return strconv.Itoa(n) }

func setFixturePassword(t *testing.T, f *authFixture, pw string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.path), adminPasswordFileName)
	if err := setAdminPasswordFile(path, pw, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.usePasswordFile(path); err != nil {
		t.Fatal(err)
	}
}

// Finding 1 (the reported scenario): a burst of concurrent password guesses
// from one address is checked at most 10 times, and the correct password
// sent with the burst does not get through once the address is locked.
func TestAdminLoginConcurrentBurstIsLimited(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	f.auth.limiter.delayFor = func(int) time.Duration { return 0 }
	const pw = "the correct admin password"
	setFixturePassword(t, f, pw)

	const n = 120
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			guess := "wrong guess number " + itoa(i)
			if i == n-1 {
				guess = pw
			}
			codes[i] = f.loginPassword(guess, "192.0.2.7").Code
		}(i)
	}
	close(start)
	wg.Wait()
	count := map[int]int{}
	for _, c := range codes {
		count[c]++
	}
	if count[http.StatusUnauthorized] > loginRemoteMaxFails {
		t.Fatalf("%d wrong passwords were checked (limit %d): %v", count[http.StatusUnauthorized], loginRemoteMaxFails, count)
	}
	if count[http.StatusUnauthorized]+count[http.StatusNoContent]+count[http.StatusTooManyRequests] != n {
		t.Fatalf("unexpected codes: %v", count)
	}
	if count[http.StatusTooManyRequests] < n-loginRemoteMaxFails {
		t.Fatalf("too few 429: %v", count)
	}
	// After the burst the address is locked even for the right password
	// (unless it was among the admitted ones and the lock was not reached).
	if count[http.StatusUnauthorized] == loginRemoteMaxFails {
		if c := f.loginPassword(pw, "192.0.2.7").Code; c != http.StatusTooManyRequests {
			t.Fatalf("locked address logged in: %d", c)
		}
	}
}

// Finding 1: the global rule holds against a concurrent burst from many
// addresses (each address only once, so the per-IP rule never applies).
func TestAdminLoginConcurrentBurstGlobalLimit(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	f.auth.limiter.delayFor = func(int) time.Duration { return 0 }
	setFixturePassword(t, f, "the correct admin password")
	const n = 160
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = f.loginPassword("wrong guess number "+itoa(i), testIP(i)).Code
		}(i)
	}
	close(start)
	wg.Wait()
	checked := 0
	for _, c := range codes {
		if c == http.StatusUnauthorized {
			checked++
		}
	}
	if checked > loginGlobalMaxFails {
		t.Fatalf("%d guesses were checked (global limit %d)", checked, loginGlobalMaxFails)
	}
}

// Finding 1: a request whose body never arrives does not hold a slot, so
// header-only connections cannot pass check() in advance.
func TestAdminLoginBadBodyTakesNoSlot(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	for i := 0; i < 3*loginRemoteMaxFails; i++ {
		if w := f.loginRaw(`{"token":`, "192.0.2.9"); w.Code != http.StatusBadRequest {
			t.Fatalf("bad body = %d", w.Code)
		}
	}
	f.auth.limiter.mu.Lock()
	inflight := f.auth.limiter.globalInflight
	_, tracked := f.auth.limiter.remotes["192.0.2.9"]
	f.auth.limiter.mu.Unlock()
	if inflight != 0 || tracked {
		t.Fatalf("bad bodies reserved slots: inflight=%d tracked=%v", inflight, tracked)
	}
	if w := f.login(f.token, "192.0.2.9"); w.Code != http.StatusNoContent {
		t.Fatalf("login after bad bodies = %d", w.Code)
	}
}

func (f *authFixture) loginRaw(body, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
	r.RemoteAddr = remote + ":5555"
	w := httptest.NewRecorder()
	f.auth.handleLogin(w, r)
	return w
}

// Finding 4: while running, an admin credential cannot become the MCP
// Bearer (SaveMCPSettings), and the MCP Bearer or an agent token cannot
// become the admin password (handlePassword, setAdminPasswordFile).
func TestRuntimeMCPAndAdminCredentialsStayApart(t *testing.T) {
	isolateEnv(t)
	app := NewApp()
	defer app.shutdown(context.Background())
	f := newAuthFixture(t, false, adminSessionPolicy{})
	const adminPW = "the correct admin password"
	setFixturePassword(t, f, adminPW)
	// What prepareHeadlessWeb wires.
	f.auth.mcpSecret = app.isMCPSecret
	app.adminCredential = f.auth.isAdminCredential

	settings := app.GetMCPSettings()
	for _, v := range []string{f.token, adminPW, "  " + f.token + "\n"} {
		settings.TokenInput = v
		if r := app.SaveMCPSettings(settings); r.OK {
			t.Fatalf("SaveMCPSettings accepted an admin credential as the Bearer: %+v", r)
		}
	}
	app.mu.RLock()
	saved := app.mcpToken
	app.mu.RUnlock()
	if saved == f.token || saved == adminPW {
		t.Fatal("admin credential stored as the MCP Bearer")
	}

	// A legitimate Bearer still saves.
	const bearer = "mcp-bearer-token-for-review-0123456789"
	settings.TokenInput = bearer
	settings.BearerEnabled = true
	if r := app.SaveMCPSettings(settings); !r.OK {
		t.Fatalf("SaveMCPSettings refused a normal Bearer: %+v", r)
	}
	issued := app.IssueAgentToken("agent-r")
	if !issued.OK || issued.Token == "" {
		t.Fatalf("IssueAgentToken: %+v", issued)
	}

	login := f.login(f.token, "127.0.0.1")
	cookie := login.Result().Cookies()[0]
	for _, pw := range []string{bearer, issued.Token} {
		w := f.changePassword(cookie, f.token, pw)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("handlePassword accepted an MCP secret as the admin password: %d %s", w.Code, w.Body.String())
		}
	}
	if !f.auth.isAdminCredential(adminPW) {
		t.Fatal("refused change replaced the password")
	}
	pwPath := filepath.Join(filepath.Dir(f.path), adminPasswordFileName)
	if err := setAdminPasswordFile(pwPath, bearer, app.isMCPSecret); !errors.Is(err, errAdminPasswordIsMCPSecret) {
		t.Fatalf("setAdminPasswordFile accepted the Bearer: %v", err)
	}
	// Positive control: an unrelated password is accepted.
	if w := f.changePassword(cookie, f.token, "an unrelated new password"); w.Code != http.StatusNoContent {
		t.Fatalf("normal change = %d %s", w.Code, w.Body.String())
	}
}

// Finding 4, CLI path: --set-admin-password refuses the Bearer saved in the
// database, an agent token and SSH_GETE_TOKEN.
func TestSetAdminPasswordCLIRefusesMCPSecrets(t *testing.T) {
	h := newHeadlessHarness(t)
	dir := os.Getenv("SSH_GETE_CONFIG_DIR")
	app := NewApp()
	const bearer = "mcp-bearer-token-for-cli-0123456789ab"
	settings := app.GetMCPSettings()
	settings.TokenInput = bearer
	settings.BearerEnabled = true
	if r := app.SaveMCPSettings(settings); !r.OK {
		t.Fatalf("save bearer: %+v", r)
	}
	issued := app.IssueAgentToken("agent-cli")
	app.shutdown(context.Background())

	reserved := storedMCPSecrets(filepath.Join(dir, "ssh-gete.sqlite"))
	if !reserved(bearer) || !reserved(issued.Token) || reserved("an unrelated new password") {
		t.Fatal("storedMCPSecrets answers wrong")
	}
	t.Setenv("SSH_GETE_TOKEN", "env-bearer-token-0123456789abcdef")
	if !reserved("env-bearer-token-0123456789abcdef") || !reserved(bearer) {
		t.Fatal("SSH_GETE_TOKEN or the overridden saved Bearer not refused")
	}

	h.deps.stdin = strings.NewReader(bearer + "\n")
	h.deps.stdinIsTerminal = func() bool { return false }
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, SetAdminPassword: true}, fstest.MapFS{}, h.deps); code != exitUsage {
		t.Fatalf("CLI accepted the Bearer as the admin password: exit %d %s", code, h.errOut.String())
	}
	if _, _, err := readAdminPasswordFile(filepath.Join(os.Getenv("SSH_GETE_CONFIG_DIR"), adminPasswordFileName)); err == nil {
		t.Fatal("password file written")
	}
}

// Finding 5: the admin cookie alone (which browsers also send to other
// ports of the same host) does not open /api/*; the per-login session key,
// kept out of cookies, is needed too.
func TestAdminSessionNeedsKeyBesidesCookie(t *testing.T) {
	f := newAuthFixture(t, false, adminSessionPolicy{})
	w := f.login(f.token, "127.0.0.1")
	key := w.Header().Get(adminSessionKeyHeader)
	if len(key) < 40 {
		t.Fatalf("no session key in the login response: %q", key)
	}
	c := sessionCookie(t, w)
	if c.Value == key || strings.Contains(w.Header().Get("Set-Cookie"), key) {
		t.Fatal("session key put in a cookie")
	}
	h := f.auth.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	serve := func(path, k string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(c)
		if k != "" {
			r.Header.Set(adminSessionHeader, k)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := serve("/api/call/ApproveCommand", ""); code != http.StatusUnauthorized {
		t.Fatalf("replayed cookie without key = %d", code)
	}
	if code := serve("/api/call/ApproveCommand", key+"x"); code != http.StatusUnauthorized {
		t.Fatalf("replayed cookie with a wrong key = %d", code)
	}
	if code := serve("/api/session", key); code != 200 {
		t.Fatalf("cookie+key = %d (a failed replay must not end the session)", code)
	}
	if code := serve("/", ""); code != 200 {
		t.Fatalf("page with cookie only = %d", code)
	}
	// A second login hands out a different key; the old one does not fit the new cookie.
	w2 := f.login(f.token, "127.0.0.1")
	if k2 := w2.Header().Get(adminSessionKeyHeader); k2 == "" || k2 == key {
		t.Fatalf("second key %q", k2)
	}
}
