package main

// Web 管理画面の認証（phase6-headless.md §6.2・§6.3・§6.7）。
//
// - 管理トークン: "sgadm_" + 32 バイト乱数の base64url。0600 のファイルに保存し、
//   メモリには sha256 だけを持つ。比較は定数時間。
// - セッション: Cookie の値（32 バイト乱数）の sha256 をキーにメモリだけで持つ。
//   idle と最大の期限、トークンの世代、件数の上限を持つ。
// - 総当たり対策: loginLimiter（接続元ごとの遅延とロック、全体のロック）。
//
// ここで持つロック（adminAuth.mu、loginLimiter.mu）はどちらも葉のロックで、
// 保持したまま他のロックを取らず、コールバック（監査・invalidate）も呼ばない
// （design-v1 §5.0）。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	adminTokenPrefix      = "sgadm_"
	adminTokenRandomBytes = 32
	adminTokenFileName    = "admin-token"
	adminSessionIDBytes   = 32
	adminCookieSecureName = "__Host-ssh-gete-admin"
	adminCookiePlainName  = "ssh-gete-admin-" // + port
	adminLoginBodyLimit   = 4 << 10
	// adminSessionKeyHeader はログインの応答で一度だけ返すセッション鍵。
	// adminSessionHeader はその鍵を /api/* の要求ごとに送るヘッダー。鍵は Cookie に
	// 入れない（ブラウザは Cookie をポートを問わず同じホストの全サービスへ送るが、
	// この鍵は localStorage にあり、オリジン（ポートを含む）の外からは読めない）。
	adminSessionKeyHeader = "X-SSH-Gete-Session-Key"
	adminSessionHeader    = "X-SSH-Gete-Session"
	adminTokenCheckEvery  = 5 * time.Second

	defaultAdminIdleMinutes = 30
	defaultAdminMaxHours    = 12
	defaultAdminMaxSessions = 32
)

var (
	errAdminTokenFormat = errors.New("管理トークンの形式が正しくありません")
)

// ---------------------------------------------------------------------------
// 管理トークンのファイル

// newAdminTokenValue は新しい管理トークンを作る。
func newAdminTokenValue() (string, error) {
	buf := make([]byte, adminTokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return adminTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func validAdminTokenValue(token string) bool {
	if !strings.HasPrefix(token, adminTokenPrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, adminTokenPrefix))
	return err == nil && len(raw) == adminTokenRandomBytes
}

func adminTokenHash(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// adminTokenHashHex は validateWebListen に渡す形（sha256 の hex）。
func adminTokenHashHex(token string) string {
	sum := adminTokenHash(token)
	return hex.EncodeToString(sum[:])
}

// defaultAdminTokenPath は web.adminTokenFile が空のときの場所。
func defaultAdminTokenPath(dataDir string) string {
	if dataDir == "" {
		dataDir = appConfigDir()
	}
	return filepath.Join(dataDir, adminTokenFileName)
}

// webCheckPrivateMode は Unix で group / other に権限があれば誤りにする。
// Windows では mode が意味を持たないので検査しない（ACL はプロファイル配下を継承）。
func webCheckPrivateMode(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s の権限が広すぎます（%#o）。chmod 600 %s を実行してください", path, perm, path)
	}
	return nil
}

func readAdminTokenFile(path string) (string, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s は通常のファイルではありません", path)
	}
	if err := webCheckPrivateMode(path, info); err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	token := strings.TrimSpace(string(data))
	if !validAdminTokenValue(token) {
		return "", nil, fmt.Errorf("%s: %w", path, errAdminTokenFormat)
	}
	return token, info, nil
}

// webWritePrivateFile は同じディレクトリの一時ファイル（0600）に書いて fsync し、
// rename してからディレクトリも fsync する。ディレクトリが無ければ 0700 で作る。
func webWritePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // Windows ではディレクトリの fsync ができないので誤りを無視する
		_ = d.Close()
	}
	return nil
}

// loadOrCreateAdminToken は管理トークンを読む。無ければ作って 0600 で保存する。
// 値はログにも標準出力にも出さない（呼び出し側は --print-admin-token のときだけ出す）。
func loadOrCreateAdminToken(path string) (token string, created bool, err error) {
	token, _, err = readAdminTokenFile(path)
	if err == nil {
		return token, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	token, err = newAdminTokenValue()
	if err != nil {
		return "", false, err
	}
	if err := webWritePrivateFile(path, []byte(token+"\n")); err != nil {
		return "", false, err
	}
	return token, true, nil
}

// rotateAdminToken は管理トークンを作り直して保存する。動作中のサーバーは
// adminAuth.checkTokenFile（最大5秒ごと）で変更を見つけ、全セッションを無効にする。
func rotateAdminToken(path string) (string, error) {
	token, err := newAdminTokenValue()
	if err != nil {
		return "", err
	}
	if err := webWritePrivateFile(path, []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}

// ---------------------------------------------------------------------------
// セッション

// adminSessionPolicy は WebConfig.Session（idleMinutes / maxHours）と
// WebConfig.MaxSessions に対応する。0 は既定値。
type adminSessionPolicy struct {
	IdleMinutes int
	MaxHours    int
	MaxSessions int
}

func (p adminSessionPolicy) normalized() adminSessionPolicy {
	if p.IdleMinutes <= 0 {
		p.IdleMinutes = defaultAdminIdleMinutes
	}
	if p.MaxHours <= 0 {
		p.MaxHours = defaultAdminMaxHours
	}
	if p.MaxSessions <= 0 {
		p.MaxSessions = defaultAdminMaxSessions
	}
	return p
}

func (p adminSessionPolicy) validate() error {
	if p.IdleMinutes != 0 && (p.IdleMinutes < 5 || p.IdleMinutes > 720) {
		return fmt.Errorf("web.session.idleMinutes は 5〜720 です（%d）", p.IdleMinutes)
	}
	if p.MaxHours != 0 && (p.MaxHours < 1 || p.MaxHours > 168) {
		return fmt.Errorf("web.session.maxHours は 1〜168 です（%d）", p.MaxHours)
	}
	if p.MaxSessions < 0 || p.MaxSessions > 1024 {
		return fmt.Errorf("web.maxSessions は 1〜1024 です（%d）", p.MaxSessions)
	}
	return nil
}

// adminSession は認証済みの要求の ctx に入る（sessionFrom で取り出す）。
// ID は Cookie の値そのものではなく、その sha256 の hex。イベント層の
// closeSession や監査の actor にはこの値（またはその先頭）を使う。
type adminSession struct {
	ID            string
	Actor         string // "web:<ID の先頭8桁>"
	Remote        string
	CreatedAt     time.Time
	LastUsed      time.Time
	IdleExpiresAt time.Time
	ExpiresAt     time.Time
}

type adminSessionState struct {
	created    time.Time
	lastUsed   time.Time
	remote     string
	generation uint64
	// bindHash はセッション鍵（adminSessionKeyHeader）の sha256。/api/* では
	// Cookie に加えてこの鍵が要る。
	bindHash [32]byte
}

// webAuditEvent は監査ログ（web_audit.go の adminAuditEntry）へ渡す1件。
// 統合時に auditSink.record(adminAuditEntry{...}) へ変換する。秘密は入れない。
type webAuditEvent struct {
	Actor  string
	Remote string
	Action string // login_ok | login_fail | login_locked | logout | token_rotate | session_expired
	Target string
	Detail string
	OK     bool
	Code   string
}

type adminSessionCtxKey struct{}

// sessionFrom は requireSession を通った要求の ctx からセッションを取り出す。
func sessionFrom(ctx context.Context) (adminSession, bool) {
	s, ok := ctx.Value(adminSessionCtxKey{}).(adminSession)
	return s, ok
}

func withAdminSession(ctx context.Context, s adminSession) context.Context {
	return context.WithValue(ctx, adminSessionCtxKey{}, s)
}

// adminAuth は管理トークンのハッシュとセッション表を持つ。
type adminAuth struct {
	tokenPath string
	policy    adminSessionPolicy
	secure    bool
	now       func() time.Time
	limiter   *loginLimiter

	// 起動前（Start の Listen の後）に一度だけ書く。以後は読むだけ。
	port int

	audit func(webAuditEvent)

	mu          sync.Mutex
	tokenHash   [32]byte
	tokenMod    time.Time
	tokenSize   int64
	generation  uint64
	sessions    map[string]*adminSessionState // key: sha256(cookie) の hex
	invalidated []func(sessionID string)
	stopping    bool
	password    adminPasswordState

	// argon2id の同時計算の枠（adminPasswordHashSlots）。
	hashSlots chan struct{}

	// mcpSecret は値が MCP の Bearer か生きているエージェントのトークンかを答える
	// （headless_wire.go が App.isMCPSecret を入れる）。新しいパスワードの検査に使う。
	// Start の前に一度だけ書く。
	mcpSecret func(string) bool
}

// newAdminAuth は管理トークンのファイルを読み（無ければ作り）、セッション表を用意する。
// secure は TLS か https の publicURL のとき true（__Host- と Secure の Cookie）。
// port は平文のときの Cookie 名に使う（0 なら Start で決まった値を後から入れる）。
func newAdminAuth(tokenPath string, policy adminSessionPolicy, secure bool, port int, now func() time.Time) (*adminAuth, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	token, _, err := loadOrCreateAdminToken(tokenPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		return nil, err
	}
	a := &adminAuth{
		tokenPath: tokenPath,
		policy:    policy.normalized(),
		secure:    secure,
		now:       now,
		limiter:   newLoginLimiter(now),
		port:      port,
		tokenHash: adminTokenHash(token),
		tokenMod:  info.ModTime(),
		tokenSize: info.Size(),
		sessions:  map[string]*adminSessionState{},
		hashSlots: make(chan struct{}, adminPasswordHashSlots),
	}
	// 既定はトークンと同じディレクトリ。newWebServer が web.adminPasswordFile で上書きする。
	if err := a.usePasswordFile(filepath.Join(filepath.Dir(tokenPath), adminPasswordFileName)); err != nil {
		return nil, err
	}
	return a, nil
}

// tokenHashHex は validateWebListen での使い回し検査に使う。
func (a *adminAuth) tokenHashHex() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return hex.EncodeToString(a.tokenHash[:])
}

// onInvalidate はセッションが無効になったとき（ログアウト・期限切れ・上限での追い出し・
// トークンの再発行）に呼ばれる関数を登録する。イベント層の closeSession を渡す。
// 呼び出しはロックの外で行う。
func (a *adminAuth) onInvalidate(f func(sessionID string)) {
	if f == nil {
		return
	}
	a.mu.Lock()
	a.invalidated = append(a.invalidated, f)
	a.mu.Unlock()
}

func (a *adminAuth) cookieName() string {
	if a.secure {
		return adminCookieSecureName
	}
	return adminCookiePlainName + strconv.Itoa(a.port)
}

func (a *adminAuth) record(e webAuditEvent) {
	if a.audit != nil {
		a.audit(e)
	}
}

func (a *adminAuth) fireInvalidated(ids []string) {
	if len(ids) == 0 {
		return
	}
	a.mu.Lock()
	callbacks := append([]func(string){}, a.invalidated...)
	a.mu.Unlock()
	for _, id := range ids {
		for _, f := range callbacks {
			f(id)
		}
	}
}

func newAdminSessionID() (string, error) {
	buf := make([]byte, adminSessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func adminSessionKey(cookieValue string) string {
	sum := sha256.Sum256([]byte(cookieValue))
	return hex.EncodeToString(sum[:])
}

func sessionActor(key string) string {
	if len(key) > 8 {
		key = key[:8]
	}
	return "web:" + key
}

func webRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// expiredLocked はセッションが期限切れか世代違いかを返す。
func (a *adminAuth) expiredLocked(s *adminSessionState, now time.Time) bool {
	if s.generation != a.generation {
		return true
	}
	if now.Sub(s.lastUsed) >= time.Duration(a.policy.IdleMinutes)*time.Minute {
		return true
	}
	return now.Sub(s.created) >= time.Duration(a.policy.MaxHours)*time.Hour
}

func (a *adminAuth) viewLocked(key string, s *adminSessionState) adminSession {
	return adminSession{
		ID:            key,
		Actor:         sessionActor(key),
		Remote:        s.remote,
		CreatedAt:     s.created,
		LastUsed:      s.lastUsed,
		IdleExpiresAt: s.lastUsed.Add(time.Duration(a.policy.IdleMinutes) * time.Minute),
		ExpiresAt:     s.created.Add(time.Duration(a.policy.MaxHours) * time.Hour),
	}
}

// lookup は Cookie からセッションを探し、有効なら最終利用時刻を進める。
// needKey なら adminSessionHeader のセッション鍵も一致しなければならない
// （一致しなくてもセッションは消さない。漏れた Cookie だけで管理者をログアウトさせないため）。
func (a *adminAuth) lookup(r *http.Request, touch, needKey bool) (adminSession, bool) {
	c, err := r.Cookie(a.cookieName())
	if err != nil || c.Value == "" {
		return adminSession{}, false
	}
	keyHash := sha256.Sum256([]byte(r.Header.Get(adminSessionHeader)))
	key := adminSessionKey(c.Value)
	now := a.now()
	var expired []string
	a.mu.Lock()
	s, ok := a.sessions[key]
	if ok && a.expiredLocked(s, now) {
		delete(a.sessions, key)
		expired = append(expired, key)
		ok = false
	}
	if ok && needKey && subtle.ConstantTimeCompare(keyHash[:], s.bindHash[:]) != 1 {
		ok = false
	}
	var view adminSession
	if ok {
		if touch {
			s.lastUsed = now
		}
		view = a.viewLocked(key, s)
	}
	a.mu.Unlock()
	a.fireInvalidated(expired)
	return view, ok
}

// sweep は期限切れのセッションを消して invalidate を呼ぶ（ウォッチャーから定期的に）。
func (a *adminAuth) sweep() {
	now := a.now()
	var expired []string
	a.mu.Lock()
	for key, s := range a.sessions {
		if a.expiredLocked(s, now) {
			delete(a.sessions, key)
			expired = append(expired, key)
		}
	}
	a.mu.Unlock()
	a.fireInvalidated(expired)
}

// invalidateAll はトークンの世代を進め、全セッションを消す。
func (a *adminAuth) invalidateAllLocked() []string {
	a.generation++
	ids := make([]string, 0, len(a.sessions))
	for key := range a.sessions {
		ids = append(ids, key)
	}
	a.sessions = map[string]*adminSessionState{}
	return ids
}

// checkTokenFile は管理トークンのファイルが外から（--rotate-admin-token など）
// 書き換えられていないかを確かめる。変わっていれば全セッションを無効にして true。
// ファイルが読めない・形式が誤っているときは、既存のハッシュのまま続ける
// （書き換えの途中を誤って受け入れないため）。誤りは返す。
func (a *adminAuth) checkTokenFile() (bool, error) {
	info, err := os.Stat(a.tokenPath)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	same := info.ModTime().Equal(a.tokenMod) && info.Size() == a.tokenSize
	a.mu.Unlock()
	if same {
		return false, nil
	}
	token, info, err := readAdminTokenFile(a.tokenPath)
	if err != nil {
		return false, err
	}
	hash := adminTokenHash(token)
	a.mu.Lock()
	a.tokenMod = info.ModTime()
	a.tokenSize = info.Size()
	if subtle.ConstantTimeCompare(hash[:], a.tokenHash[:]) == 1 {
		a.mu.Unlock()
		return false, nil
	}
	a.tokenHash = hash
	ids := a.invalidateAllLocked()
	a.mu.Unlock()
	a.fireInvalidated(ids)
	a.record(webAuditEvent{Actor: "cli", Action: "token_rotate", Detail: "token file changed", OK: true})
	return true, nil
}

// watch は stop が閉じるまで、トークンのファイルの確認と期限切れの掃除を繰り返す。
func (a *adminAuth) watch(stop <-chan struct{}, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		every = adminTokenCheckEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if changed, err := a.checkTokenFile(); err != nil {
				if logf != nil {
					logf("web admin: 管理トークンのファイルを確認できません: %v", err)
				}
			} else if changed && logf != nil {
				logf("web admin: 管理トークンが変わったので全セッションを無効にしました")
			}
			if changed, err := a.checkPasswordFile(); err != nil {
				if logf != nil {
					logf("web admin: 管理パスワードのファイルを読めないため、パスワードでのログインを止めました: %v", err)
				}
			} else if changed && logf != nil {
				logf("web admin: 管理パスワードが変わったので全セッションを無効にしました")
			}
			a.sweep()
		}
	}
}

func (a *adminAuth) setStopping() {
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
}

func (a *adminAuth) isStopping() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopping
}

func (a *adminAuth) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge, // 0: セッション Cookie、-1: Max-Age=0（削除）
	})
}

func webauthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func webauthError(w http.ResponseWriter, status int, msg string) {
	webauthJSON(w, status, map[string]string{"error": msg})
}

// handleLogin は POST /api/login {"token":"…"} か {"password":"…"}（どちらか一方）。
// パスワードは --set-admin-password などで設定したときだけ通る。成功は 204 と Cookie。
func (a *adminAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		webauthError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.isStopping() {
		webauthError(w, http.StatusServiceUnavailable, "shutting down")
		return
	}
	remote := webRemoteIP(r)
	// 本文を読んでから枠を取る。ヘッダーだけ送って check を先に通しておき、
	// 後から推測をまとめて送る、という並べ方をさせないため。
	body, err := io.ReadAll(io.LimitReader(r.Body, adminLoginBodyLimit+1))
	if err != nil || len(body) > adminLoginBodyLimit {
		webauthError(w, http.StatusBadRequest, "invalid request")
		return
	}
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		webauthError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if (req.Token == "") == (req.Password == "") {
		webauthError(w, http.StatusBadRequest, "send either token or password")
		return
	}
	attempt, ok := a.admitAttempt(w, r, remote, "login_locked")
	if !ok {
		return
	}
	defer attempt.release()
	method := "token"
	var matched bool
	if req.Token != "" {
		got := adminTokenHash(strings.TrimSpace(req.Token))
		a.mu.Lock()
		want := a.tokenHash
		a.mu.Unlock()
		matched = subtle.ConstantTimeCompare(got[:], want[:]) == 1
	} else {
		method = "password"
		ok, err := a.verifyPassword(r.Context(), req.Password)
		if err != nil {
			return // 接続が切れた
		}
		matched = ok
	}
	if !matched {
		attempt.fail()
		a.record(webAuditEvent{Actor: "web:-", Remote: remote, Action: "login_fail", Detail: method, OK: false, Code: "invalid_" + method})
		webauthError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	attempt.succeed()

	value, err := newAdminSessionID()
	if err != nil {
		webauthError(w, http.StatusInternalServerError, "internal error")
		return
	}
	bindKey, err := newAdminSessionID()
	if err != nil {
		webauthError(w, http.StatusInternalServerError, "internal error")
		return
	}
	key := adminSessionKey(value)
	now := a.now()
	var dropped []string
	a.mu.Lock()
	// セッション固定の防止: 送られてきた古い Cookie のセッションは消す。
	if old, err := r.Cookie(a.cookieName()); err == nil && old.Value != "" {
		oldKey := adminSessionKey(old.Value)
		if _, ok := a.sessions[oldKey]; ok {
			delete(a.sessions, oldKey)
			dropped = append(dropped, oldKey)
		}
	}
	for len(a.sessions) >= a.policy.MaxSessions {
		oldest := ""
		var oldestAt time.Time
		for k, s := range a.sessions {
			if oldest == "" || s.created.Before(oldestAt) || (s.created.Equal(oldestAt) && k < oldest) {
				oldest, oldestAt = k, s.created
			}
		}
		delete(a.sessions, oldest)
		dropped = append(dropped, oldest)
	}
	a.sessions[key] = &adminSessionState{created: now, lastUsed: now, remote: remote, generation: a.generation, bindHash: sha256.Sum256([]byte(bindKey))}
	a.mu.Unlock()
	a.fireInvalidated(dropped)

	a.setCookie(w, value, 0)
	w.Header().Set(adminSessionKeyHeader, bindKey)
	a.record(webAuditEvent{Actor: sessionActor(key), Remote: remote, Action: "login_ok", Detail: method, OK: true})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// admitAttempt は総当たり対策の枠を取り、必要なら待つ。断ったときは 429 を書いて false。
// 呼び出し側は defer attempt.release() を置き、照合したら fail / succeed を呼ぶ。
func (a *adminAuth) admitAttempt(w http.ResponseWriter, r *http.Request, remote, lockedAction string) (*loginAttempt, bool) {
	attempt, delay, err := a.limiter.check(remote)
	if err != nil {
		var locked *loginLockedError
		if errors.As(err, &locked) {
			w.Header().Set("Retry-After", strconv.Itoa(int((locked.RetryAfter+time.Second-1)/time.Second)))
			a.record(webAuditEvent{Actor: "web:-", Remote: remote, Action: lockedAction, Detail: locked.Scope, OK: false, Code: "locked"})
		}
		webauthError(w, http.StatusTooManyRequests, "too many login attempts")
		return nil, false
	}
	if delay > 0 {
		if !a.limiter.enterDelay() {
			attempt.release()
			webauthError(w, http.StatusTooManyRequests, "too many login attempts")
			return nil, false
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			a.limiter.leaveDelay()
			attempt.release()
			return nil, false
		}
		a.limiter.leaveDelay()
	}
	return attempt, true
}

// handleLogout は POST /api/logout。セッションを消し、Cookie を消す。
// セッションが無くても Cookie を消して 204 を返す。
func (a *adminAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		webauthError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var dropped []string
	actor := "web:-"
	if c, err := r.Cookie(a.cookieName()); err == nil && c.Value != "" {
		key := adminSessionKey(c.Value)
		a.mu.Lock()
		if _, ok := a.sessions[key]; ok {
			delete(a.sessions, key)
			dropped = append(dropped, key)
			actor = sessionActor(key)
		}
		a.mu.Unlock()
	}
	a.fireInvalidated(dropped)
	a.setCookie(w, "", -1)
	if len(dropped) > 0 {
		a.record(webAuditEvent{Actor: actor, Remote: webRemoteIP(r), Action: "logout", OK: true})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleRotate は POST /api/admin/rotate-token（requireSession の内側）。
// 新しいトークンを応答で1回だけ返し、自分を含む全セッションを無効にする。
func (a *adminAuth) handleRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		webauthError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess, ok := sessionFrom(r.Context())
	if !ok {
		webauthError(w, http.StatusUnauthorized, "login required")
		return
	}
	token, err := rotateAdminToken(a.tokenPath)
	if err != nil {
		a.record(webAuditEvent{Actor: sess.Actor, Remote: webRemoteIP(r), Action: "token_rotate", OK: false, Code: "write_failed"})
		webauthError(w, http.StatusInternalServerError, "could not rotate token")
		return
	}
	info, statErr := os.Stat(a.tokenPath)
	a.mu.Lock()
	a.tokenHash = adminTokenHash(token)
	if statErr == nil {
		a.tokenMod, a.tokenSize = info.ModTime(), info.Size()
	}
	ids := a.invalidateAllLocked()
	a.mu.Unlock()
	a.fireInvalidated(ids)
	a.setCookie(w, "", -1)
	a.record(webAuditEvent{Actor: sess.Actor, Remote: webRemoteIP(r), Action: "token_rotate", OK: true})
	webauthJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleSession は GET /api/session（requireSession の内側）。shim のバッジ用。
func (a *adminAuth) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionFrom(r.Context())
	if !ok {
		webauthError(w, http.StatusUnauthorized, "login required")
		return
	}
	webauthJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"idleExpiresAt": sess.IdleExpiresAt.UTC().Format(time.RFC3339),
		"expiresAt":     sess.ExpiresAt.UTC().Format(time.RFC3339),
		"passwordSet":   a.passwordSet(),
	})
}

// requireSession はセッションの無い要求を止める。/api/* は 401、ページは
// GET / HEAD なら /login?next=… へ 303、それ以外は 401。
func (a *adminAuth) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ページ（静的ファイル）は Cookie だけで配る。/api/* はセッション鍵も要る。
		// 無操作の時計を進めるのは鍵つきの /api/* だけ。Cookie だけのページ要求は
		// 別ポートの同一サイトのページ（<img src=…>）からも飛んでくるので、
		// それで idleMinutes を越えて延命させない。
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		sess, ok := a.lookup(r, isAPI, isAPI)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
				webauthError(w, http.StatusUnauthorized, "login required")
				return
			}
			target := "/login"
			if p := r.URL.RequestURI(); p != "" && p != "/" && strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") {
				target += "?next=" + url.QueryEscape(p)
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(withAdminSession(r.Context(), sess)))
	})
}

// sessionCount はテストと起動ログ用。
func (a *adminAuth) sessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}

// ---------------------------------------------------------------------------
// ログインの総当たり対策（§6.7）

const (
	loginWindow          = 15 * time.Minute
	loginRemoteMaxFails  = 10
	loginGlobalMaxFails  = 100
	loginLockDuration    = 15 * time.Minute
	loginBaseDelay       = 250 * time.Millisecond
	loginMaxDelay        = 8 * time.Second
	loginMaxDelayedTotal = 16
	loginMaxRemotes      = 4096
)

type loginLockedError struct {
	Scope      string // "remote" | "global"
	RetryAfter time.Duration
}

func (e *loginLockedError) Error() string {
	return fmt.Sprintf("login locked (%s) for %s", e.Scope, e.RetryAfter.Round(time.Second))
}

type loginRemoteState struct {
	fails       []time.Time
	lockedUntil time.Time
	// inflight は check で枠を取ったまま、まだ fail / succeed / release していない試行の数。
	// 同時に送られた試行も「失敗するかもしれない1回」として数えるので、
	// 失敗を記録する前の並列の総当たりも上限で止まる。
	inflight int
}

type loginLimiter struct {
	now func() time.Time
	// delayFor はテストで遅延を短くするため。nil なら loginDelay。
	delayFor func(n int) time.Duration

	mu             sync.Mutex
	remotes        map[string]*loginRemoteState
	globalFails    []time.Time
	globalUntil    time.Time
	globalInflight int
	delayed        int
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	if now == nil {
		now = time.Now
	}
	return &loginLimiter{now: now, remotes: map[string]*loginRemoteState{}}
}

func loginPruneTimes(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

func loginIsLoopback(remote string) bool {
	ip := net.ParseIP(strings.Trim(remote, "[]"))
	return ip != nil && ip.IsLoopback()
}

// loginBusyRetry は、失敗の記録と同時に走っている試行を合わせて上限に達したときの
// Retry-After（ロックではなく、前の試行が終わるまで待ってもらう）。
const loginBusyRetry = time.Second

// loginAttempt は check で取った1回分の枠。fail / succeed / release のどれかで
// 一度だけ返す（2回目以降は何もしない）。返し忘れないよう、呼び出し側は
// defer attempt.release() を置く。
type loginAttempt struct {
	l        *loginLimiter
	remote   string
	reserved bool // check で枠を取った（false はテスト用の fail / succeed）
	done     bool
}

// check はログインを受け付けてよいかと、受け付ける前に待つ時間を返す。
// 受け付けるときは枠（*loginAttempt）を取る。数えるのは「直近の失敗＋結果待ちの試行」で、
// 接続元で 10、全体で 100 に達したら 429（ロック中なら *loginLockedError）。
// 待つ時間は、その数 n に対して min(250ms×2^(n-1), 8s)。
func (l *loginLimiter) check(remote string) (*loginAttempt, time.Duration, error) {
	now := l.now()
	cutoff := now.Add(-loginWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	loopback := loginIsLoopback(remote)
	if !loopback && now.Before(l.globalUntil) {
		return nil, 0, &loginLockedError{Scope: "global", RetryAfter: l.globalUntil.Sub(now)}
	}
	st := l.remotes[remote]
	if st != nil && now.Before(st.lockedUntil) {
		return nil, 0, &loginLockedError{Scope: "remote", RetryAfter: st.lockedUntil.Sub(now)}
	}
	l.globalFails = loginPruneTimes(l.globalFails, cutoff)
	if !loopback && len(l.globalFails)+l.globalInflight >= loginGlobalMaxFails {
		return nil, 0, &loginLockedError{Scope: "global", RetryAfter: loginBusyRetry}
	}
	n := 0
	if st != nil {
		st.fails = loginPruneTimes(st.fails, cutoff)
		n = len(st.fails) + st.inflight
		if n >= loginRemoteMaxFails {
			return nil, 0, &loginLockedError{Scope: "remote", RetryAfter: loginBusyRetry}
		}
	} else {
		if len(l.remotes) >= loginMaxRemotes {
			l.evictLocked(now)
		}
		st = &loginRemoteState{}
		l.remotes[remote] = st
	}
	st.inflight++
	l.globalInflight++
	delay := loginDelay(n)
	if l.delayFor != nil {
		delay = l.delayFor(n)
	}
	return &loginAttempt{l: l, remote: remote, reserved: true}, delay, nil
}

func loginDelay(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := loginBaseDelay
	for i := 1; i < n; i++ {
		d *= 2
		if d >= loginMaxDelay {
			return loginMaxDelay
		}
	}
	return d
}

// releaseLocked は枠を返す。l.mu を持って呼ぶ。返した後の状態（無ければ nil）を返す。
func (at *loginAttempt) releaseLocked() *loginRemoteState {
	l := at.l
	st := l.remotes[at.remote]
	if !at.reserved {
		return st
	}
	if l.globalInflight > 0 {
		l.globalInflight--
	}
	if st != nil && st.inflight > 0 {
		st.inflight--
	}
	return st
}

// dropIfIdleLocked は失敗もロックも結果待ちも無い接続元を表から消す。
func (l *loginLimiter) dropIfIdleLocked(remote string, st *loginRemoteState, now time.Time) {
	if st != nil && st.inflight == 0 && len(st.fails) == 0 && !now.Before(st.lockedUntil) {
		delete(l.remotes, remote)
	}
}

// fail は枠を返して失敗を1回数える。接続元で 15 分に 10 回、全体で 15 分に 100 回に
// 達したらロックする。
func (at *loginAttempt) fail() {
	if at == nil || at.done {
		return
	}
	at.done = true
	l := at.l
	now := l.now()
	cutoff := now.Add(-loginWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	st := at.releaseLocked()
	if st == nil {
		if len(l.remotes) >= loginMaxRemotes {
			l.evictLocked(now)
		}
		st = &loginRemoteState{}
		l.remotes[at.remote] = st
	}
	st.fails = append(loginPruneTimes(st.fails, cutoff), now)
	if len(st.fails) >= loginRemoteMaxFails {
		st.lockedUntil = now.Add(loginLockDuration)
		st.fails = nil
	}
	l.globalFails = append(loginPruneTimes(l.globalFails, cutoff), now)
	if len(l.globalFails) >= loginGlobalMaxFails {
		l.globalUntil = now.Add(loginLockDuration)
		l.globalFails = nil
	}
}

// succeed は枠を返し、その接続元の失敗回数を消す。同時に走っている別の試行の枠と、
// その間に付いたロックは残す。
func (at *loginAttempt) succeed() {
	if at == nil || at.done {
		return
	}
	at.done = true
	l := at.l
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := at.releaseLocked(); st != nil {
		st.fails = nil
		l.dropIfIdleLocked(at.remote, st, now)
	}
}

// release は結果を数えずに枠を返す（本文が読めない・接続が切れたなど、照合しなかったとき）。
func (at *loginAttempt) release() {
	if at == nil || at.done {
		return
	}
	at.done = true
	l := at.l
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropIfIdleLocked(at.remote, at.releaseLocked(), now)
}

// fail / succeed は枠を取らずに結果だけを記録する（テストで状態を作るため）。
func (l *loginLimiter) fail(remote string)    { (&loginAttempt{l: l, remote: remote}).fail() }
func (l *loginLimiter) succeed(remote string) { (&loginAttempt{l: l, remote: remote}).succeed() }

// evictLocked は表が大きくなりすぎたとき、ロックしておらず結果待ちも無い古い接続元から消す。
func (l *loginLimiter) evictLocked(now time.Time) {
	type kv struct {
		k string
		t time.Time
	}
	var list []kv
	for k, st := range l.remotes {
		if now.Before(st.lockedUntil) || st.inflight > 0 {
			continue
		}
		var last time.Time
		if n := len(st.fails); n > 0 {
			last = st.fails[n-1]
		}
		list = append(list, kv{k, last})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.Before(list[j].t) })
	for i := 0; i < len(list) && i < loginMaxRemotes/4; i++ {
		delete(l.remotes, list[i].k)
	}
}

// enterDelay は遅延中の要求の枠（全体で16）を取る。取れなければ false（即 429）。
func (l *loginLimiter) enterDelay() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.delayed >= loginMaxDelayedTotal {
		return false
	}
	l.delayed++
	return true
}

func (l *loginLimiter) leaveDelay() {
	l.mu.Lock()
	if l.delayed > 0 {
		l.delayed--
	}
	l.mu.Unlock()
}

// webSessionID は requireSession を通った要求のセッション ID（sha256 の hex）を返す。
// 呼び出し層（webCallHooks.Session）とイベント層（hubConfig.SessionID）に渡す。
func webSessionID(r *http.Request) string {
	if s, ok := sessionFrom(r.Context()); ok {
		return s.ID
	}
	return ""
}

// ---------------------------------------------------------------------------
// 管理パスワード（任意。管理トークンに加えて使える覚えやすい資格情報）
//
// - 保存するのは argon2id のハッシュ（PHC 形式の1行）だけ。平文は残さない。
//   場所は web.adminPasswordFile、無ければ <dataDir>/admin-password（0600）。
// - 初回の設定: `ssh-gete --set-admin-password`（標準入力から読む）か、管理トークンで
//   ログインしてから POST /api/admin/password。設定するまではパスワードでの
//   ログインはできない（管理トークンだけ）。
// - 管理トークンは常に使える（パスワードを忘れたときの復旧手段。--print-admin-token）。
// - パスワードの設定・変更・削除で全セッションを無効にする（再ログインさせる）。
// - 総当たり対策はトークンと同じ loginLimiter を通る。argon2id はメモリを使うので、
//   同時に計算するのは adminPasswordHashSlots 件まで。

const (
	adminPasswordFileName  = "admin-password"
	adminPasswordMinRunes  = 12
	adminPasswordMaxBytes  = 1024
	adminPasswordHashSlots = 2

	argon2idTime    = 2
	argon2idMemory  = 19 * 1024 // KiB（OWASP の最小推奨）
	argon2idThreads = 1
	argon2idKeyLen  = 32
	argon2idSaltLen = 16
)

var errAdminPasswordFormat = errors.New("管理パスワードのファイルの形式が正しくありません")

// adminPasswordHash は argon2id の PHC 文字列を解いたもの。
type adminPasswordHash struct {
	time    uint32
	memory  uint32
	threads uint8
	salt    []byte
	key     []byte
}

// defaultAdminPasswordPath は web.adminPasswordFile が空のときの場所。
func defaultAdminPasswordPath(dataDir string) string {
	if dataDir == "" {
		dataDir = appConfigDir()
	}
	return filepath.Join(dataDir, adminPasswordFileName)
}

// validateNewAdminPassword は新しいパスワードの規則（12 文字以上、1024 バイト以下、
// 制御文字なし、前後の空白なし）を確かめる。
func validateNewAdminPassword(pw string) error {
	if !utf8.ValidString(pw) {
		return errors.New("パスワードが UTF-8 ではありません")
	}
	if len(pw) > adminPasswordMaxBytes {
		return fmt.Errorf("パスワードは %d バイト以下にしてください", adminPasswordMaxBytes)
	}
	if utf8.RuneCountInString(pw) < adminPasswordMinRunes {
		return fmt.Errorf("パスワードは %d 文字以上にしてください", adminPasswordMinRunes)
	}
	if strings.TrimSpace(pw) != pw {
		return errors.New("パスワードの前後に空白を入れないでください")
	}
	for _, r := range pw {
		if unicode.IsControl(r) {
			return errors.New("パスワードに制御文字を入れないでください")
		}
	}
	if strings.HasPrefix(pw, adminTokenPrefix) {
		return errors.New("管理トークンをパスワードにしないでください")
	}
	return nil
}

// hashAdminPassword は argon2id でハッシュし、PHC 形式の文字列を返す。
func hashAdminPassword(pw string) (string, error) {
	salt := make([]byte, argon2idSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argon2idTime, argon2idMemory, argon2idThreads, argon2idKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argon2idMemory, argon2idTime, argon2idThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// parseAdminPasswordHash は PHC 形式を解く。ファイルを書き換えられても計算が
// 重くなりすぎないよう、パラメーターに上限を設ける。
func parseAdminPasswordHash(s string) (*adminPasswordHash, error) {
	parts := strings.Split(strings.TrimSpace(s), "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return nil, errAdminPasswordFormat
	}
	var m, t, p uint64
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || n != 3 {
		return nil, errAdminPasswordFormat
	}
	if m < 8*1024 || m > 256*1024 || t < 1 || t > 10 || p < 1 || p > 16 {
		return nil, errAdminPasswordFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return nil, errAdminPasswordFormat
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return nil, errAdminPasswordFormat
	}
	return &adminPasswordHash{time: uint32(t), memory: uint32(m), threads: uint8(p), salt: salt, key: key}, nil
}

func (h *adminPasswordHash) verify(pw string) bool {
	got := argon2.IDKey([]byte(pw), h.salt, h.time, h.memory, h.threads, uint32(len(h.key)))
	return subtle.ConstantTimeCompare(got, h.key) == 1
}

// readAdminPasswordFile はハッシュを読む。ファイルが無ければ os.ErrNotExist を包んだ誤り。
func readAdminPasswordFile(path string) (*adminPasswordHash, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s は通常のファイルではありません", path)
	}
	if err := webCheckPrivateMode(path, info); err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	h, err := parseAdminPasswordHash(string(data))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return h, info, nil
}

// errAdminPasswordIsMCPSecret は新しいパスワードが MCP の Bearer かエージェントの
// トークンと同じとき。MCP クライアントがそのままパスワードでログインできてしまう。
var errAdminPasswordIsMCPSecret = errors.New("MCP の Bearer トークンやエージェントのトークンを管理パスワードにしないでください")

// setAdminPasswordFile は pw を検査してハッシュを 0600 で保存する（--set-admin-password）。
// reserved が pw を MCP 側の秘密（Bearer・エージェントのトークン）だと答えたら断る。
// 動作中のサーバーはファイルの変更を見つけて全セッションを無効にする。
func setAdminPasswordFile(path, pw string, reserved func(string) bool) error {
	if err := validateNewAdminPassword(pw); err != nil {
		return err
	}
	if reserved != nil && reserved(pw) {
		return errAdminPasswordIsMCPSecret
	}
	phc, err := hashAdminPassword(pw)
	if err != nil {
		return err
	}
	return webWritePrivateFile(path, []byte(phc+"\n"))
}

// clearAdminPasswordFile はパスワードを消す（以後は管理トークンだけでログインする）。
func clearAdminPasswordFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// readNewAdminPassword は r の1行目を新しいパスワードとして読む。interactive（端末）
// なら prompt を出して確認の2行目を必ず読み、一致を確かめる。端末でなければ2行目は
// 任意（あれば一致を確かめる）。行末の \n / \r\n だけを取り除く。
func readNewAdminPassword(r io.Reader, interactive bool, prompt func(string)) (string, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, 2*adminPasswordMaxBytes+8), 512)
	readLine := func() (string, bool, error) {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		got := line != ""
		return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), got, nil
	}
	if interactive && prompt != nil {
		prompt("新しい管理パスワード（12 文字以上。入力は画面に表示されます）: ")
	}
	pw, _, err := readLine()
	if err != nil {
		return "", err
	}
	if pw == "" {
		return "", errors.New("標準入力からパスワードを読めませんでした")
	}
	if interactive && prompt != nil {
		prompt("もう一度: ")
	}
	confirm, got, err := readLine()
	if err != nil {
		return "", err
	}
	if (interactive && confirm != pw) || (!interactive && got && confirm != "" && confirm != pw) {
		return "", errors.New("確認のパスワードが一致しません")
	}
	return pw, nil
}

// passwordState はメモリ上のパスワードの状態（adminAuth.mu で守る）。
type adminPasswordState struct {
	path   string
	hash   *adminPasswordHash // nil: 未設定
	mod    time.Time
	size   int64
	exists bool
}

// usePasswordFile はパスワードのファイルの場所を決めて読む（newWebServer から、Start の前）。
// ファイルが無いのは「未設定」で誤りではない。
func (a *adminAuth) usePasswordFile(path string) error {
	st := adminPasswordState{path: path}
	if path != "" {
		h, info, err := readAdminPasswordFile(path)
		switch {
		case err == nil:
			st.hash, st.mod, st.size, st.exists = h, info.ModTime(), info.Size(), true
		case errors.Is(err, os.ErrNotExist):
		default:
			return err
		}
	}
	a.mu.Lock()
	a.password = st
	a.mu.Unlock()
	return nil
}

func (a *adminAuth) passwordSet() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.password.hash != nil
}

// checkPasswordFile は外から（--set-admin-password など）パスワードのファイルが
// 変わっていないかを確かめる。変わっていれば読み直して全セッションを無効にする。
// 読めない・形式が誤っているときは、パスワードでのログインを止める（安全側）。
func (a *adminAuth) checkPasswordFile() (bool, error) {
	a.mu.Lock()
	cur := a.password
	a.mu.Unlock()
	if cur.path == "" {
		return false, nil
	}
	info, statErr := os.Stat(cur.path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	exists := statErr == nil
	if exists == cur.exists && (!exists || (info.ModTime().Equal(cur.mod) && info.Size() == cur.size)) {
		return false, nil
	}
	next := adminPasswordState{path: cur.path}
	var readErr error
	if exists {
		h, info2, err := readAdminPasswordFile(cur.path)
		if err != nil {
			readErr = err
			next.exists, next.mod, next.size = true, info.ModTime(), info.Size()
		} else {
			next.hash, next.mod, next.size, next.exists = h, info2.ModTime(), info2.Size(), true
		}
	}
	a.mu.Lock()
	a.password = next
	ids := a.invalidateAllLocked()
	a.mu.Unlock()
	a.fireInvalidated(ids)
	a.record(webAuditEvent{Actor: "cli", Action: "password_change", Detail: "password file changed", OK: readErr == nil})
	return true, readErr
}

// verifyPassword はパスワードを照合する。未設定でも同じだけ計算して、
// 設定の有無を応答時間から分からないようにする。
func (a *adminAuth) verifyPassword(ctx context.Context, pw string) (bool, error) {
	select {
	case a.hashSlots <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-a.hashSlots }()
	a.mu.Lock()
	h := a.password.hash
	a.mu.Unlock()
	if h == nil {
		adminPasswordDummy.verify(pw)
		return false, nil
	}
	return h.verify(pw), nil
}

// adminPasswordDummy は未設定のときの照合に使う（結果は捨てる）。
var adminPasswordDummy = &adminPasswordHash{
	time: argon2idTime, memory: argon2idMemory, threads: argon2idThreads,
	salt: make([]byte, argon2idSaltLen), key: make([]byte, argon2idKeyLen),
}

// handlePassword は POST /api/admin/password（requireSession の内側）。
//
//	{"current":"…","password":"…"}
//
// current は、パスワードが設定済みなら今のパスワードか管理トークン。未設定なら
// 管理トークン（ログインに使った資格情報をもう一度確かめる）。password が空なら
// パスワードを消す。成功は 204 で、自分を含む全セッションを無効にする。
func (a *adminAuth) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		webauthError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess, ok := sessionFrom(r.Context())
	if !ok {
		webauthError(w, http.StatusUnauthorized, "login required")
		return
	}
	remote := webRemoteIP(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, adminLoginBodyLimit+1))
	if err != nil || len(body) > adminLoginBodyLimit {
		webauthError(w, http.StatusBadRequest, "invalid request")
		return
	}
	var req struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		webauthError(w, http.StatusBadRequest, "invalid request")
		return
	}
	attempt, ok := a.admitAttempt(w, r, remote, "login_locked")
	if !ok {
		return
	}
	defer attempt.release()
	if !a.checkCurrentCredential(r.Context(), req.Current) {
		attempt.fail()
		a.record(webAuditEvent{Actor: sess.Actor, Remote: remote, Action: "password_change", OK: false, Code: "invalid_current"})
		webauthError(w, http.StatusForbidden, "current credential does not match")
		return
	}
	attempt.succeed()
	a.mu.Lock()
	path := a.password.path
	a.mu.Unlock()
	if path == "" {
		webauthError(w, http.StatusNotFound, "password login is not configured")
		return
	}
	detail := "set"
	if req.Password == "" {
		detail = "cleared"
		err = clearAdminPasswordFile(path)
	} else if verr := validateNewAdminPassword(req.Password); verr != nil {
		webauthError(w, http.StatusBadRequest, verr.Error())
		return
	} else if a.mcpSecret != nil && a.mcpSecret(req.Password) {
		a.record(webAuditEvent{Actor: sess.Actor, Remote: remote, Action: "password_change", Detail: detail, OK: false, Code: "mcp_secret"})
		webauthError(w, http.StatusBadRequest, errAdminPasswordIsMCPSecret.Error())
		return
	} else {
		err = setAdminPasswordFile(path, req.Password, a.mcpSecret)
	}
	if err != nil {
		a.record(webAuditEvent{Actor: sess.Actor, Remote: remote, Action: "password_change", Detail: detail, OK: false, Code: "write_failed"})
		webauthError(w, http.StatusInternalServerError, "could not save password")
		return
	}
	st := adminPasswordState{path: path}
	if h, info, rerr := readAdminPasswordFile(path); rerr == nil {
		st.hash, st.mod, st.size, st.exists = h, info.ModTime(), info.Size(), true
	}
	a.mu.Lock()
	a.password = st
	ids := a.invalidateAllLocked()
	a.mu.Unlock()
	a.fireInvalidated(ids)
	a.setCookie(w, "", -1)
	a.record(webAuditEvent{Actor: sess.Actor, Remote: remote, Action: "password_change", Detail: detail, OK: true})
	w.WriteHeader(http.StatusNoContent)
}

// isAdminCredential は値が管理トークンか（設定済みなら）管理パスワードと同じかを返す。
// MCP の Bearer を保存する前の検査（App.adminCredential）に使う。argon2id を計算するので
// 呼び出し側はロックを持たずに呼ぶ。
func (a *adminAuth) isAdminCredential(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	return a.checkCurrentCredential(context.Background(), v)
}

// checkCurrentCredential は管理トークンか（設定済みなら）今のパスワードに一致するか。
func (a *adminAuth) checkCurrentCredential(ctx context.Context, cur string) bool {
	cur = strings.TrimSpace(cur)
	if cur == "" {
		return false
	}
	got := adminTokenHash(cur)
	a.mu.Lock()
	want := a.tokenHash
	a.mu.Unlock()
	if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
		return true
	}
	ok, err := a.verifyPassword(ctx, cur)
	return err == nil && ok
}
