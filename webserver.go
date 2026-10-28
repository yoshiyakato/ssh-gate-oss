package main

// Web 管理画面の HTTP サーバー（phase6-headless.md §5.1・§6.1・§6.4・§6.6・§6.8・§6.10・§8）。
//
// 要求は外側から次の順に処理する。
//
//	securityHeaders → (HSTS) → guardHTTP（Host・Origin・JSON の検査を MCP と共用）
//	→ webAPIGuard（/api/* だけ: Authorization 拒否・CSRF ヘッダ・Origin 必須・
//	  Sec-Fetch-Site・本文の上限）→ ルーティング（requireSession）
//
// 呼び出し層（/api/call/）・イベント層（/api/events）・/api/env・監査の閲覧は
// 他の担当の http.Handler を webServerOptions で受け取って載せるだけにする。

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	webAdminDefaultListen = "127.0.0.1:8788"
	webAPIBodyLimit       = 1 << 20
	webCSRFHeader         = "X-SSH-Gete-CSRF"
	webShimPath           = "/headless-shim.js"
	webShimFile           = "headless-shim.js"
	webLoginFile          = "login.html"
	webTLSDirName         = "web-tls"
	webSelfSignedDays     = 397
	webSelfSignedRenewAt  = 30 * 24 * time.Hour

	webTLSModeOff        = "off"
	webTLSModeFiles      = "files"
	webTLSModeSelfSigned = "self-signed"
)

// webEventStream はイベント層（web_events.go の *eventHub）が満たす形。
type webEventStream interface {
	http.Handler
	closeSession(sessionID string)
	shutdown()
}

// webServerOptions は newWebServer の入力。統合担当が WebConfig（設定ファイルと
// フラグを解決したもの）と他の担当のハンドラーから組み立てる。
type webServerOptions struct {
	Listen         string // host:port。空なら 127.0.0.1:8788
	PublicURL      string // 例 https://gate.example.lan:8788
	TLSMode        string // off | files | self-signed（空は off）
	TLSCert        string
	TLSKey         string
	DataDir        string // 空なら appConfigDir()
	AdminTokenFile string // 空なら <DataDir>/admin-token
	// 管理パスワード（argon2id のハッシュ）のファイル。空なら <DataDir>/admin-password。
	AdminPasswordFile string
	Session           adminSessionPolicy

	Dist  fs.FS // frontend/dist の中身（index.html が直下）
	Admin fs.FS // webadmin の中身（headless-shim.js・login.html・login.*）

	RPC       http.Handler   // /api/call/<Method>
	Events    webEventStream // GET /api/events
	Env       http.Handler   // GET /api/env
	AuditList http.Handler   // GET /api/admin/audit

	Audit func(webAuditEvent)
	Now   func() time.Time
	Logf  func(format string, args ...any)

	// テスト用。0 なら 5 秒。
	TokenCheckEvery time.Duration
}

type webServer struct {
	opts      webServerOptions
	host      string
	port      int
	tlsConfig *tls.Config
	tlsFP     string
	auth      *adminAuth
	static    http.Handler
	logf      func(string, ...any)

	srv      *http.Server
	listener net.Listener
	stop     chan struct{}
	stopOnce sync.Once

	callMu   sync.Mutex
	calls    int
	stopping bool
}

// ---------------------------------------------------------------------------
// 待受の安全条件（§6.1）

func webSplitListen(listen string) (string, int, error) {
	if listen == "" {
		listen = webAdminDefaultListen
	}
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return "", 0, fmt.Errorf("web.listen %q は host:port の形にしてください: %v", listen, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("web.listen %q のポートが正しくありません", listen)
	}
	if host == "" {
		return "", 0, fmt.Errorf("web.listen %q にはアドレスを書いてください（すべてのインターフェースなら 0.0.0.0）", listen)
	}
	if !strings.EqualFold(host, "localhost") && net.ParseIP(host) == nil {
		return "", 0, fmt.Errorf("web.listen %q のアドレスは IP か localhost にしてください", listen)
	}
	return host, port, nil
}

func webNormalizeTLSMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", webTLSModeOff:
		return webTLSModeOff, nil
	case webTLSModeFiles:
		return webTLSModeFiles, nil
	case webTLSModeSelfSigned:
		return webTLSModeSelfSigned, nil
	}
	return "", fmt.Errorf("web.tls.mode %q は off / files / self-signed のどれかです", mode)
}

func webParsePublicURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("web.publicURL %q は http(s)://host[:port] の形にしてください", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("web.publicURL %q にはパスやクエリを書けません（サブパスでの公開は未対応）", raw)
	}
	// ブラウザは既定のポート（https の 443、http の 80）を Host にも Origin にも書かない。
	// 書かれたままだと Host の検査とオリジンの一覧がどちらも一致しなくなるので外す。
	u.Host = hostWithoutDefaultPort(u.Scheme, strings.ToLower(u.Host))
	u.Path = ""
	return u, nil
}

// webCanonicalPublicURL は publicURL を scheme://host[:port] の形（既定のポートなし）にする。
// 解けなければそのまま返す（誤りは webCheckExposure が報告する）。
func webCanonicalPublicURL(raw string) string {
	if pub, err := webParsePublicURL(raw); err == nil && pub != nil {
		return pub.Scheme + "://" + pub.Host
	}
	return raw
}

// webCheckExposure は MCP の設定に依存しない §6.1 の条件を確かめる。
func webCheckExposure(opts webServerOptions) error {
	host, _, err := webSplitListen(opts.Listen)
	if err != nil {
		return err
	}
	mode, err := webNormalizeTLSMode(opts.TLSMode)
	if err != nil {
		return err
	}
	if mode == webTLSModeFiles && (opts.TLSCert == "" || opts.TLSKey == "") {
		return errors.New("web.tls.mode: files には証明書と鍵（--tls-cert / --tls-key）が必要です")
	}
	pub, err := webParsePublicURL(opts.PublicURL)
	if err != nil {
		return err
	}
	loopback := isLoopbackListen(host)
	if !loopback && mode == webTLSModeOff {
		return fmt.Errorf("web 管理画面の待受 %s はこの PC 以外からも接続できます。TLS なしでは起動できません。--tls-self-signed か --tls-cert / --tls-key を指定してください", opts.Listen)
	}
	if pub != nil && pub.Scheme == "http" {
		if !isLoopbackListen(pub.Hostname()) {
			return fmt.Errorf("web.publicURL %q は平文の http で、この PC 以外のホストです。https にしてください", opts.PublicURL)
		}
		if mode != webTLSModeOff {
			return fmt.Errorf("TLS を使うときは web.publicURL も https にしてください（%q）", opts.PublicURL)
		}
	}
	return nil
}

func webListenOverlaps(hostA string, portA int, hostB string, portB int) bool {
	if portA != portB || portA == 0 {
		return false
	}
	norm := func(h string) string {
		if strings.EqualFold(h, "localhost") {
			return "127.0.0.1"
		}
		if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
			if ip.IsUnspecified() {
				return "*"
			}
			return ip.String()
		}
		return strings.ToLower(h)
	}
	a, b := norm(hostA), norm(hostB)
	return a == b || a == "*" || b == "*"
}

// validateWebListen は §6.1 の起動拒否の条件を全部確かめる（終了コード2にする誤り）。
// adminTokenHash は管理トークンの sha256 の hex（adminTokenHashHex / adminAuth.tokenHashHex）。
func validateWebListen(opts webServerOptions, mcp MCPSettings, mcpToken, adminTokenHash string) error {
	if err := webCheckExposure(opts); err != nil {
		return err
	}
	host, port, _ := webSplitListen(opts.Listen)
	if mcp.Enabled && webListenOverlaps(host, port, mcp.ListenAddress, mcp.Port) {
		return fmt.Errorf("web 管理画面の待受 %s が MCP の待受 %s と重なっています。別のポートにしてください",
			net.JoinHostPort(host, strconv.Itoa(port)), net.JoinHostPort(mcp.ListenAddress, strconv.Itoa(mcp.Port)))
	}
	if mcpToken != "" && adminTokenHash != "" {
		got := adminTokenHashHex(mcpToken)
		if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(adminTokenHash))) == 1 {
			return errors.New("MCP の Bearer トークンと管理トークンが同じです。別の値にしてください（--rotate-admin-token）")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// オリジンと検査（§6.4）

// webOrigins は自分のオリジンの一覧。publicURL があればそれだけ。無ければ
// ループバックの3つと待受の IP（0.0.0.0 / :: ならこの PC のアドレス全部）。
func webOrigins(listen, publicURL string, port int, useTLS bool) []string {
	if pub, err := webParsePublicURL(publicURL); err == nil && pub != nil {
		return []string{pub.Scheme + "://" + pub.Host}
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	p := strconv.Itoa(port)
	seen := map[string]bool{}
	var out []string
	add := func(host string) {
		o := scheme + "://" + net.JoinHostPort(host, p)
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	add("127.0.0.1")
	add("localhost")
	add("::1")
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return out
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	switch {
	case ip != nil && ip.IsUnspecified():
		for _, a := range webInterfaceIPs(ip.To4() == nil) {
			add(a)
		}
	case ip != nil:
		add(ip.String())
	}
	return out
}

// webInterfaceAddrs is net.InterfaceAddrs; a variable so tests can fail it.
var webInterfaceAddrs = net.InterfaceAddrs

func webInterfaceIPs(includeV6 bool) []string {
	addrs, err := webInterfaceAddrs()
	if err != nil {
		// Under a sandbox without AF_NETLINK (systemd RestrictAddressFamilies)
		// this fails on Linux; only loopback origins remain, so say so.
		log.Printf("web: この PC のアドレスを読めません（%v）。オリジンと証明書はループバックだけになります。LAN から使うなら web.publicURL を設定してください", err)
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			continue
		}
		if ip.To4() == nil && !includeV6 {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// webGuard は guardHTTP をそのまま流用し、その内側に /api/* の追加検査を重ねる。
func webGuard(origins []string, publicURL string, port int, next http.Handler) http.Handler {
	settings := MCPSettings{Port: port, BaseURL: webCanonicalPublicURL(strings.TrimRight(publicURL, "/")), AllowedOrigins: origins}
	return guardHTTP(settings, webAPIGuard(origins, next))
}

func webAPIGuard(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "" {
			webauthError(w, http.StatusBadRequest, "authorization header is not accepted here")
			return
		}
		if r.Header.Get(webCSRFHeader) != "1" {
			webauthError(w, http.StatusForbidden, "missing csrf header")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin == "" || !originAllowed(origin, origins) {
				webauthError(w, http.StatusForbidden, "origin required")
				return
			}
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			webauthError(w, http.StatusForbidden, "cross-site request")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, webAPIBodyLimit)
		next.ServeHTTP(w, r)
	})
}

// webContentSecurityPolicy は §6.8 の CSP。
const webContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders は全応答にセキュリティ用のヘッダを付ける。Cache-Control の既定は
// no-store で、/assets/* だけ静的配信が上書きする。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", webContentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), serial=()")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// webHSTSHost は Strict-Transport-Security を付けてよいホスト名を返す（空なら付けない）。
// HSTS はポートを区別せずホスト名全体に効くので、https の publicURL があり、その
// ホストが localhost・ループバック・IP でないときだけにする。localhost に付けると、
// 同じ PC の http://localhost:<別のポート>（開発サーバーや MCP の http://localhost:8787）
// まで 1 年間 https に書き換えられる。IP にはブラウザが HSTS を適用しない。
func webHSTSHost(publicURL string) string {
	pub, err := webParsePublicURL(publicURL)
	if err != nil || pub == nil || pub.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(pub.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host) != nil {
		return ""
	}
	return host
}

// webHSTS は要求の Host が host（publicURL のホスト名）のときだけ HSTS を付ける。
func webHSTS(host string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqHost := r.Host
		if h, _, err := net.SplitHostPort(reqHost); err == nil {
			reqHost = h
		}
		if strings.EqualFold(reqHost, host) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// TLS（§6.6）

// webTLSConfig は mode に応じて *tls.Config と証明書の SHA-256 指紋を返す。
// off なら nil。hosts は自己署名の SAN に入れる名前（webTLSHosts）。
func webTLSConfig(mode, certFile, keyFile, dataDir string, hosts []string, now time.Time, logf func(string, ...any)) (*tls.Config, string, error) {
	mode, err := webNormalizeTLSMode(mode)
	if err != nil {
		return nil, "", err
	}
	var cert tls.Certificate
	var fp string
	switch mode {
	case webTLSModeOff:
		return nil, "", nil
	case webTLSModeFiles:
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, "", fmt.Errorf("TLS の証明書と鍵を読めません: %w", err)
		}
		if info, statErr := os.Stat(keyFile); statErr == nil && logf != nil {
			if permErr := webCheckPrivateMode(keyFile, info); permErr != nil {
				logf("web admin: 警告: %v", permErr)
			}
		}
		fp = webCertFingerprint(cert.Certificate[0])
	case webTLSModeSelfSigned:
		if dataDir == "" {
			dataDir = appConfigDir()
		}
		cert, fp, err = loadOrCreateSelfSigned(filepath.Join(dataDir, webTLSDirName), hosts, now)
		if err != nil {
			return nil, "", err
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, fp, nil
}

// webTLSHosts は自己署名の SAN: 待受の IP、publicURL のホスト、ホスト名、localhost、127.0.0.1、::1。
func webTLSHosts(listen, publicURL string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.Trim(strings.TrimSpace(h), "[]")
		if h == "" || seen[strings.ToLower(h)] {
			return
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsUnspecified() {
			return
		}
		seen[strings.ToLower(h)] = true
		out = append(out, h)
	}
	if host, _, err := net.SplitHostPort(listen); err == nil {
		add(host)
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsUnspecified() {
			for _, a := range webInterfaceIPs(ip.To4() == nil) {
				add(a)
			}
		}
	}
	if pub, err := webParsePublicURL(publicURL); err == nil && pub != nil {
		add(pub.Hostname())
	}
	if hn, err := os.Hostname(); err == nil {
		add(hn)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	return out
}

func webCertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	hexed := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(hexed); i += 2 {
		parts = append(parts, hexed[i:i+2])
	}
	return strings.Join(parts, ":")
}

func webCertCovers(c *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			found := false
			for _, got := range c.IPAddresses {
				if got.Equal(ip) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
			continue
		}
		found := false
		for _, got := range c.DNSNames {
			if strings.EqualFold(got, h) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// loadOrCreateSelfSigned は dir の cert.pem / key.pem を使う。無い、期限まで30日を切った、
// SAN が足りない、鍵の権限が広い、のどれかなら作り直す（ECDSA P-256、397日）。
func loadOrCreateSelfSigned(dir string, hosts []string, now time.Time) (tls.Certificate, string, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if cert, fp, ok := webLoadSelfSigned(certPath, keyPath, hosts, now); ok {
		return cert, fp, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ssh-gete web admin (self-signed)"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(webSelfSignedDays * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := webWritePrivateFile(keyPath, keyPEM); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := webWritePrivateFile(certPath, certPEM); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, webCertFingerprint(der), nil
}

func webLoadSelfSigned(certPath, keyPath string, hosts []string, now time.Time) (tls.Certificate, string, bool) {
	info, err := os.Stat(keyPath)
	if err != nil || webCheckPrivateMode(keyPath, info) != nil {
		return tls.Certificate{}, "", false
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(cert.Certificate) == 0 {
		return tls.Certificate{}, "", false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", false
	}
	if now.Add(webSelfSignedRenewAt).After(leaf.NotAfter) || now.Before(leaf.NotBefore) {
		return tls.Certificate{}, "", false
	}
	if !webCertCovers(leaf, hosts) {
		return tls.Certificate{}, "", false
	}
	cert.Leaf = leaf
	return cert, webCertFingerprint(cert.Certificate[0]), true
}

// ---------------------------------------------------------------------------
// 静的配信と shim の差し込み（§5.1）

var webHeadOpenRe = regexp.MustCompile(`(?i)<head(\s[^>]*)?>`)

// injectShim は index.html の <head> の直後に shim の classic スクリプトを1行差し込む。
// <head> が無ければ誤り（ビルドの取り違えを起動時に見つける）。
func injectShim(index []byte) ([]byte, error) {
	loc := webHeadOpenRe.FindIndex(index)
	if loc == nil {
		return nil, errors.New("frontend/dist/index.html に <head> がありません")
	}
	if bytes.Contains(index, []byte(`src="`+webShimPath+`"`)) {
		return nil, errors.New("frontend/dist/index.html にはすでに headless-shim.js が入っています")
	}
	tag := []byte("\n<script src=\"" + webShimPath + "\"></script>")
	out := make([]byte, 0, len(index)+len(tag))
	out = append(out, index[:loc[1]]...)
	out = append(out, tag...)
	out = append(out, index[loc[1]:]...)
	return out, nil
}

type webStaticHandler struct {
	dist  fs.FS
	admin fs.FS
	index []byte
	shim  []byte
	login []byte
	mod   time.Time
}

// newStaticHandler は frontend/dist と webadmin の配信を作る。
//   - / と /index.html: shim を差し込んだ index.html（セッション必須。外側で requireSession）
//   - /headless-shim.js: webadmin の shim（セッション必須）
//   - /login: webadmin/login.html（セッション不要）
//   - /login-assets/<login*>: webadmin のログイン画面の CSS・JS（セッション不要）
//   - それ以外: frontend/dist のファイル。/assets/* は private, max-age=86400
func newStaticHandler(dist fs.FS, admin fs.FS) (*webStaticHandler, error) {
	if dist == nil || admin == nil {
		return nil, errors.New("web admin: 配信するファイルがありません")
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil, fmt.Errorf("frontend/dist/index.html を読めません: %w", err)
	}
	injected, err := injectShim(index)
	if err != nil {
		return nil, err
	}
	shim, err := fs.ReadFile(admin, webShimFile)
	if err != nil {
		return nil, fmt.Errorf("webadmin/%s を読めません: %w", webShimFile, err)
	}
	login, err := fs.ReadFile(admin, webLoginFile)
	if err != nil {
		return nil, fmt.Errorf("webadmin/%s を読めません: %w", webLoginFile, err)
	}
	return &webStaticHandler{dist: dist, admin: admin, index: injected, shim: shim, login: login, mod: time.Now()}, nil
}

func webServeBytes(w http.ResponseWriter, r *http.Request, name string, data []byte, mod time.Time) {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, name, mod, bytes.NewReader(data))
}

func (h *webStaticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/" || p == "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		webServeBytes(w, r, "index.html", h.index, h.mod)
		return
	case p == webShimPath:
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		webServeBytes(w, r, webShimFile, h.shim, h.mod)
		return
	case p == "/login" || p == "/login/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		webServeBytes(w, r, webLoginFile, h.login, h.mod)
		return
	case strings.HasPrefix(p, "/login-assets/"):
		name := strings.TrimPrefix(p, "/login-assets/")
		if !strings.HasPrefix(name, "login") || strings.ContainsAny(name, `/\`) || name == webLoginFile {
			http.NotFound(w, r)
			return
		}
		h.serveFile(w, r, h.admin, name)
		return
	}
	name := strings.TrimPrefix(path.Clean(p), "/")
	if name == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	h.serveFile(w, r, h.dist, name)
}

func (h *webStaticHandler) serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	f, err := fsys.Open(name)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	if rs, ok := f.(io.ReadSeeker); ok {
		if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeContent(w, r, name, info.ModTime(), rs)
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	webServeBytes(w, r, name, data, info.ModTime())
}

// ---------------------------------------------------------------------------
// サーバー本体

// newWebServer は待受の安全条件（MCP に依存しない部分）を確かめ、管理トークン・TLS・
// 静的配信を用意する。Listen はまだしない（Start で行う）。
// MCP との重なりとトークンの使い回しは、MCP の設定が決まった後に validateWebListen で確かめる。
func newWebServer(opts webServerOptions) (*webServer, error) {
	if opts.Listen == "" {
		opts.Listen = webAdminDefaultListen
	}
	if err := webCheckExposure(opts); err != nil {
		return nil, err
	}
	opts.PublicURL = webCanonicalPublicURL(opts.PublicURL)
	if opts.Now == nil {
		opts.Now = time.Now
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	if opts.DataDir == "" {
		opts.DataDir = appConfigDir()
	}
	host, port, _ := webSplitListen(opts.Listen)
	mode, _ := webNormalizeTLSMode(opts.TLSMode)
	tlsConfig, fp, err := webTLSConfig(mode, opts.TLSCert, opts.TLSKey, opts.DataDir, webTLSHosts(opts.Listen, opts.PublicURL), opts.Now(), logf)
	if err != nil {
		return nil, err
	}
	pub, _ := webParsePublicURL(opts.PublicURL)
	secure := tlsConfig != nil || (pub != nil && pub.Scheme == "https")
	tokenPath := opts.AdminTokenFile
	if tokenPath == "" {
		tokenPath = defaultAdminTokenPath(opts.DataDir)
	}
	auth, err := newAdminAuth(tokenPath, opts.Session, secure, port, opts.Now)
	if err != nil {
		return nil, err
	}
	auth.audit = opts.Audit
	passwordPath := opts.AdminPasswordFile
	if passwordPath == "" {
		passwordPath = defaultAdminPasswordPath(opts.DataDir)
	}
	if err := auth.usePasswordFile(passwordPath); err != nil {
		return nil, err
	}
	static, err := newStaticHandler(opts.Dist, opts.Admin)
	if err != nil {
		return nil, err
	}
	if opts.Events != nil {
		auth.onInvalidate(opts.Events.closeSession)
	}
	return &webServer{
		opts:      opts,
		host:      host,
		port:      port,
		tlsConfig: tlsConfig,
		tlsFP:     fp,
		auth:      auth,
		static:    static,
		logf:      logf,
		stop:      make(chan struct{}),
	}, nil
}

// TLSFingerprint は証明書の SHA-256 指紋（TLS でなければ空）。起動時の表示用。
func (s *webServer) TLSFingerprint() string { return s.tlsFP }

// AdminTokenPath は管理トークンのファイルの場所（起動時の表示用。値は出さない）。
func (s *webServer) AdminTokenPath() string { return s.auth.tokenPath }

// AdminTokenHash は validateWebListen に渡す管理トークンの sha256 の hex。
func (s *webServer) AdminTokenHash() string { return s.auth.tokenHashHex() }

// Start は Listen して配信を始める。ポート 0 なら OS が選んだポートを使う。
func (s *webServer) Start() error {
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = tcp.Port
		s.auth.port = tcp.Port // Serve の前なので競合しない
	}
	handler := s.handler()
	s.srv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         s.tlsConfig,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	s.listener = ln
	srv := s.srv
	useTLS := s.tlsConfig != nil
	go func() {
		var err error
		if useTLS {
			// ServeTLS は NextProtos に h2 を足すので HTTP/2 が有効になる
			// （平文の HTTP/1.1 にある同時接続6本の制限を受けない）。
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("web admin: 停止しました: %v", err)
		}
	}()
	go s.auth.watch(s.stop, s.opts.TokenCheckEvery, s.logf)
	return nil
}

// Port は待受のポート（Start の後は実際の値）。
func (s *webServer) Port() int { return s.port }

// URL は利用者に見せる URL。publicURL があればそれ。
func (s *webServer) URL() string {
	if pub, err := webParsePublicURL(s.opts.PublicURL); err == nil && pub != nil {
		return pub.Scheme + "://" + pub.Host + "/"
	}
	scheme := "http"
	if s.tlsConfig != nil {
		scheme = "https"
	}
	host := s.host
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsUnspecified() {
		host = defaultRouteIP()
		if host == "" {
			host = "127.0.0.1"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(s.port)) + "/"
}

func (s *webServer) origins() []string {
	return webOrigins(net.JoinHostPort(s.host, strconv.Itoa(s.port)), s.opts.PublicURL, s.port, s.tlsConfig != nil)
}

func (s *webServer) handler() http.Handler {
	mux := http.NewServeMux()
	need := s.auth.requireSession
	notConfigured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webauthError(w, http.StatusNotFound, "not found")
	})
	orNotFound := func(h http.Handler) http.Handler {
		if h == nil {
			return notConfigured
		}
		return h
	}

	mux.Handle("/api/login", s.intake(http.HandlerFunc(s.auth.handleLogin)))
	mux.Handle("/api/logout", http.HandlerFunc(s.auth.handleLogout))
	mux.Handle("/api/session", need(http.HandlerFunc(s.auth.handleSession)))
	mux.Handle("/api/admin/rotate-token", need(http.HandlerFunc(s.auth.handleRotate)))
	mux.Handle("/api/admin/password", need(s.intake(http.HandlerFunc(s.auth.handlePassword))))
	mux.Handle("/api/call/", need(s.trackCall(orNotFound(s.opts.RPC))))
	var events http.Handler
	if s.opts.Events != nil {
		events = s.opts.Events
	}
	mux.Handle("/api/events", need(webLongLived(orNotFound(events))))
	mux.Handle("/api/env", need(orNotFound(s.opts.Env)))
	mux.Handle("/api/admin/audit", need(orNotFound(s.opts.AuditList)))
	mux.Handle("/api/", need(notConfigured))
	mux.Handle("/login", s.static)
	mux.Handle("/login/", s.static)
	mux.Handle("/login-assets/", s.static)
	mux.Handle("/", need(s.static))

	var h http.Handler = mux
	h = webGuard(s.origins(), s.opts.PublicURL, s.port, h)
	if host := webHSTSHost(s.opts.PublicURL); host != "" {
		h = webHSTS(host, h)
	}
	return securityHeaders(h)
}

// webLongLived は SSE のために、http.Server の ReadTimeout が接続の読み取り期限として
// 残らないよう外す（残ると 30 秒で要求の ctx が取り消される）。書き込みの期限は
// イベント層が stream ごとに付ける（§4.2）。
func webLongLived(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		next.ServeHTTP(w, r)
	})
}

// intake は StopIntake の後に 503 を返す。
func (s *webServer) intake(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.callMu.Lock()
		stopping := s.stopping
		s.callMu.Unlock()
		if stopping {
			webauthError(w, http.StatusServiceUnavailable, "shutting down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// trackCall は /api/call の実行中の数を数える（WaitCalls 用）。StopIntake の後は 503。
func (s *webServer) trackCall(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.callMu.Lock()
		if s.stopping {
			s.callMu.Unlock()
			webauthError(w, http.StatusServiceUnavailable, "shutting down")
			return
		}
		s.calls++
		s.callMu.Unlock()
		defer func() {
			s.callMu.Lock()
			s.calls--
			s.callMu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// StopIntake は新しい /api/call と /api/login を 503 にし、イベント層を閉じる
// （イベント層は全 SSE に headless:shutdown を送って閉じる）。何度呼んでもよい。
func (s *webServer) StopIntake() {
	s.callMu.Lock()
	already := s.stopping
	s.stopping = true
	s.callMu.Unlock()
	s.auth.setStopping()
	if !already && s.opts.Events != nil {
		s.opts.Events.shutdown()
	}
}

// WaitCalls は実行中の /api/call が無くなるまで最大 timeout 待つ。待ちきれたら true。
func (s *webServer) WaitCalls(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.callMu.Lock()
		n := s.calls
		s.callMu.Unlock()
		if n == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Close は http.Server を Shutdown（最大2秒）し、だめなら Close する。
func (s *webServer) Close(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.srv == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		_ = s.srv.Close()
		return err
	}
	return nil
}

// webServerOptionsFromConfig は設定ファイルとフラグを解決した WebConfig
// （config.go の resolveWebConfig の結果）から webServerOptions の設定部分を作る。
// ハンドラー（RPC・Events・Env・AuditList）、Dist・Admin、Audit は呼び出し側で入れる。
func webServerOptionsFromConfig(cfg WebConfig, dataDir string) webServerOptions {
	return webServerOptions{
		Listen:            cfg.Listen,
		PublicURL:         cfg.PublicURL,
		TLSMode:           cfg.TLS.Mode,
		TLSCert:           cfg.TLS.Cert,
		TLSKey:            cfg.TLS.Key,
		DataDir:           dataDir,
		AdminTokenFile:    cfg.AdminTokenFile,
		AdminPasswordFile: adminPasswordPath(cfg, dataDir),
		Session: adminSessionPolicy{
			IdleMinutes: cfg.Session.IdleMinutes,
			MaxHours:    cfg.Session.MaxHours,
			MaxSessions: cfg.MaxSessions,
		},
	}
}
