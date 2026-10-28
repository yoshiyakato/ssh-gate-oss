package main

// --headless (phase6-headless.md §1.1, §1.5, §8): the command line, the
// startup order, the single-instance lock and the shutdown order. The web
// admin server, the event hub, the admin token, TLS, the audit writer and
// the secret backends live in their own files; runHeadless reaches them only
// through headlessDeps, so this file builds (and its tests run) without them
// and the GUI build can share it.
//
// Wiring: those files fill headlessHooks from an init function (see the
// headlessDeps fields). A nil hook means "not in this build": no web admin,
// only the keychain and memory secret backends, no admin token commands.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	ossignal "os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// appVersion is set at release time: -ldflags "-X main.appVersion=1.6.0".
var appVersion = "dev"

// Exit codes (§1.1).
const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
	exitLocked  = 3
	exitForced  = 130
)

// CLIOptions are the flags of §1.1.
type CLIOptions struct {
	Headless         bool
	ConfigPath       string
	Listen           string
	DataDir          string
	TLSCert          string
	TLSKey           string
	TLSSelfSigned    bool
	PrintAdminToken  bool
	RotateAdminToken bool
	SetAdminPassword bool
	CheckConfig      bool
	ShowVersion      bool
	ShowHelp         bool
}

// wantsHeadless reports whether the GUI build should hand over to
// runHeadless instead of opening a window.
func (o CLIOptions) wantsHeadless() bool {
	return o.Headless || o.PrintAdminToken || o.RotateAdminToken || o.SetAdminPassword || o.CheckConfig || o.ShowVersion || o.ShowHelp
}

// headlessTriggers are the flags that make the GUI build parse strictly.
var headlessTriggers = map[string]bool{
	"headless": true, "print-admin-token": true, "rotate-admin-token": true,
	"set-admin-password": true, "check-config": true, "version": true,
}

func newCLIFlagSet(o *CLIOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("ssh-gete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.Headless, "headless", false, "ウィンドウを出さず、MCP と Web 管理画面だけを動かす")
	fs.StringVar(&o.ConfigPath, "config", "", "YAML の設定ファイル（環境変数 SSH_GETE_CONFIG でも指定できる）")
	fs.StringVar(&o.Listen, "listen", "", "Web 管理画面の待受 host:port（既定 "+defaultWebListen+"）")
	fs.StringVar(&o.DataDir, "data-dir", "", "DB・管理トークン・TLS 鍵などを置くディレクトリ")
	fs.StringVar(&o.TLSCert, "tls-cert", "", "TLS の証明書（PEM）")
	fs.StringVar(&o.TLSKey, "tls-key", "", "TLS の秘密鍵（PEM）")
	fs.BoolVar(&o.TLSSelfSigned, "tls-self-signed", false, "自己署名証明書を作って使う")
	fs.BoolVar(&o.PrintAdminToken, "print-admin-token", false, "管理トークンを表示して終了する（無ければ作る）")
	fs.BoolVar(&o.RotateAdminToken, "rotate-admin-token", false, "管理トークンを作り直して終了する")
	fs.BoolVar(&o.SetAdminPassword, "set-admin-password", false, "管理パスワードを標準入力から読んで設定し（argon2id のハッシュだけを保存）、終了する")
	fs.BoolVar(&o.CheckConfig, "check-config", false, "設定ファイルを検証し、DB への反映内容を表示して終了する")
	fs.BoolVar(&o.ShowVersion, "version", false, "版を表示して終了する")
	return fs
}

// cliUsage is the -h text.
func cliUsage() string {
	var b strings.Builder
	var o CLIOptions
	fs := newCLIFlagSet(&o)
	fs.SetOutput(&b)
	fmt.Fprintf(&b, "使い方: ssh-gete --headless [--config FILE] [--listen HOST:PORT] [--data-dir DIR] ...\n\n")
	fs.PrintDefaults()
	return b.String()
}

// parseCLI parses args (without the program name). With gui set (the Wails
// build) unknown arguments are ignored unless a headless flag is present
// (macOS adds -psn_… to old-style launches); then, and always in the
// headless build, parsing is strict and any unknown or positional argument
// is an error (exit 2).
func parseCLI(args []string, gui bool) (CLIOptions, error) {
	if gui {
		triggered := false
		for _, arg := range args {
			name := strings.TrimLeft(arg, "-")
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if strings.HasPrefix(arg, "-") && headlessTriggers[name] {
				triggered = true
				break
			}
		}
		if !triggered {
			return CLIOptions{}, nil
		}
	}
	var o CLIOptions
	fs := newCLIFlagSet(&o)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return CLIOptions{Headless: !gui, ShowHelp: true}, nil
		}
		return CLIOptions{}, fmt.Errorf("引数の誤り: %w", err)
	}
	if fs.NArg() > 0 {
		return CLIOptions{}, fmt.Errorf("引数の誤り: 不明な引数 %q", fs.Arg(0))
	}
	if !gui {
		o.Headless = true
	}
	oneShots := 0
	for _, on := range []bool{o.PrintAdminToken, o.RotateAdminToken, o.SetAdminPassword} {
		if on {
			oneShots++
		}
	}
	if oneShots > 1 {
		return CLIOptions{}, errors.New("引数の誤り: --print-admin-token / --rotate-admin-token / --set-admin-password は同時に1つだけ指定できます")
	}
	if o.CheckConfig && oneShots > 0 {
		return CLIOptions{}, errors.New("引数の誤り: --check-config はトークンの操作と同時に指定できません")
	}
	if o.ConfigPath == "" {
		o.ConfigPath = strings.TrimSpace(os.Getenv("SSH_GETE_CONFIG"))
	}
	return o, nil
}

// headlessWeb is the web admin server as runHeadless drives it (§8).
type headlessWeb interface {
	Start() error
	// StopIntake answers new /api/call and /api/login with 503 and closes
	// every SSE stream with headless:shutdown.
	StopIntake()
	// WaitCalls waits for running /api/call handlers; false on timeout.
	WaitCalls(timeout time.Duration) bool
	Close(ctx context.Context) error
	URL() string
}

// headlessWebParams is what the web layer gets to build itself (§1.5 step 7).
type headlessWebParams struct {
	App            *App
	Web            WebConfig
	DataDir        string
	Dist           fs.FS
	AdminTokenPath string
	Audit          configAudit
	AuditSink      auditSink // nil without newAuditSink
}

// headlessWebPlan is the prepared (not yet listening) web admin.
type headlessWebPlan struct {
	Server headlessWeb
	// WithEvents puts the event hub into the startup ctx (withWebHub), so
	// emitEvent reaches the browser through platformEmit. nil: no events.
	WithEvents func(ctx context.Context) context.Context
	// Banner lines for the startup output ("TLS : self-signed, SHA-256 …").
	Banner []string
}

// headlessDeps are the parts runHeadless calls but does not implement. A
// nil field falls back to the built-in behaviour noted on it.
type headlessDeps struct {
	stdout, stderr io.Writer

	// lock takes the data directory's single-instance lock (§1.1). nil:
	// acquireInstanceLock (flock / an unshared handle, released by the OS
	// on a crash; the PID file acquireDataDirLock elsewhere). Must return an error wrapping errHeadlessLocked
	// (or errInstanceLocked via lockedErr) when another process holds it.
	lock      func(dir string) (release func(), err error)
	lockedErr error

	// selectSecrets returns the secret store NewApp must use for backend
	// ("keychain" | "file" | "env" | "memory") and the env binder (§7).
	// nil: keychain is the default store, memory sets SSH_GETE_KEYRING,
	// file and env are refused. A nil store keeps defaultSecretStore().
	selectSecrets func(cfg SecretsConfig, backend, dataDir string) (SecretStore, configSecretBinder, error)
	// installSecrets makes defaultSecretStore return s (secretBackendFactory).
	installSecrets func(s SecretStore)

	// newAudit starts the admin_audit writer. nil: no audit.
	newAudit func(app *App) (audit configAudit, closeFn func(timeout time.Duration))
	// newAuditSink is newAudit for the real writer (web_audit.go): the sink
	// also reaches the web layer (headlessWebParams.AuditSink). It wins over
	// newAudit when both are set.
	newAuditSink func(app *App) (sink auditSink, closeFn func(timeout time.Duration), err error)
	// prepareWeb checks the listener rules of §6.1, loads the admin token
	// and TLS and builds the server and event hub. nil: no web admin.
	prepareWeb func(p headlessWebParams) (headlessWebPlan, error)
	// adminToken returns the admin token at path, created if missing, or a
	// new one when rotate (--print-admin-token / --rotate-admin-token).
	adminToken func(path string, rotate bool) (string, error)

	// stdin is where --set-admin-password reads from. nil: os.Stdin.
	stdin io.Reader
	// stdinIsTerminal reports whether stdin is a terminal (prompt and
	// confirmation). nil: a character-device check on os.Stdin.
	stdinIsTerminal func() bool

	// signals delivers SIGINT / SIGTERM / SIGHUP. nil: signal.Notify.
	signals func() (<-chan os.Signal, func())
	// exit ends the process on the second signal. nil: os.Exit.
	exit func(code int)
}

// headlessHooks is filled by the files that implement the web admin, the
// audit log and the secret backends (from init), and read by runHeadless.
var headlessHooks headlessDeps

// Shutdown budgets (§8).
var (
	headlessWaitCalls    = 5 * time.Second
	headlessWebClose     = 2 * time.Second
	headlessAuditFlush   = 1 * time.Second
	headlessAppShutdown  = 15 * time.Second
	headlessSecretsProbe = 3 * time.Second
)

// embeddedDist is frontend/dist from the embedded assets (both builds
// define assets).
func embeddedDist() fs.FS {
	sub, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// runHeadless runs until SIGINT / SIGTERM and returns the exit code.
func runHeadless(opts CLIOptions, dist fs.FS) int {
	return runHeadlessWith(context.Background(), opts, dist, headlessHooks)
}

func (d headlessDeps) withDefaults() headlessDeps {
	if d.stdout == nil {
		d.stdout = os.Stdout
	}
	if d.stderr == nil {
		d.stderr = os.Stderr
	}
	if d.lock == nil {
		d.lock = acquireInstanceLock
	}
	if d.selectSecrets == nil {
		d.selectSecrets = builtinSecretBackend
	}
	if d.signals == nil {
		d.signals = func() (<-chan os.Signal, func()) {
			ch := make(chan os.Signal, 2)
			ossignal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			return ch, func() { ossignal.Stop(ch) }
		}
	}
	if d.exit == nil {
		d.exit = os.Exit
	}
	return d
}

// runHeadlessWith is runHeadless with its collaborators given. Cancelling
// ctx acts like SIGTERM (tests).
func runHeadlessWith(ctx context.Context, opts CLIOptions, dist fs.FS, deps headlessDeps) int {
	deps = deps.withDefaults()
	out, errOut := deps.stdout, deps.stderr
	fail := func(code int, format string, args ...any) int {
		fmt.Fprintf(errOut, "ssh-gete: "+format+"\n", args...)
		return code
	}

	if opts.ShowHelp {
		fmt.Fprint(out, cliUsage())
		return exitOK
	}
	if opts.ShowVersion {
		fmt.Fprintf(out, "ssh-gete %s (%s/%s)\n", appVersion, runtime.GOOS, runtime.GOARCH)
		return exitOK
	}

	// 1. the file and the flags
	var cfg *FileConfig
	if opts.ConfigPath != "" {
		c, err := loadConfigFile(opts.ConfigPath)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		cfg = c
	} else if opts.CheckConfig {
		return fail(exitUsage, "--check-config には --config（または SSH_GETE_CONFIG）が必要です")
	}
	webCfg, err := resolveWebConfig(cfg, opts)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	dataDir, err := resolveDataDir(opts, cfg)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	tokenPath := adminTokenPath(webCfg, dataDir)

	// One-shot commands.
	if opts.SetAdminPassword {
		return setAdminPasswordCommand(deps, adminPasswordPath(webCfg, dataDir), storedMCPSecrets(filepath.Join(dataDir, "ssh-gete.sqlite")))
	}
	if opts.PrintAdminToken || opts.RotateAdminToken {
		if deps.adminToken == nil {
			return fail(exitRuntime, "このビルドには Web 管理画面が入っていないため、管理トークンを扱えません")
		}
		token, err := deps.adminToken(tokenPath, opts.RotateAdminToken)
		if err != nil {
			return fail(exitRuntime, "管理トークン: %v", err)
		}
		if opts.PrintAdminToken {
			fmt.Fprintln(out, token)
		} else {
			fmt.Fprintf(out, "管理トークンを作り直しました: %s（表示するには --print-admin-token）\n", tokenPath)
		}
		return exitOK
	}
	if opts.CheckConfig {
		return checkConfigCommand(out, errOut, cfg, dataDir)
	}

	// 2. environment for NewApp
	if err := applyConfigEnv(cfg); err != nil {
		return fail(exitUsage, "%v", err)
	}
	os.Setenv("SSH_GETE_NO_SEED", "1")

	// 3. single instance
	release, err := deps.lock(dataDir)
	if err != nil {
		if errors.Is(err, errHeadlessLocked) || (deps.lockedErr != nil && errors.Is(err, deps.lockedErr)) {
			return fail(exitLocked, "%v", err)
		}
		return fail(exitRuntime, "ロックを取れません: %v", err)
	}
	defer release()

	// 4. secrets backend, before NewApp's keychain migration (§7.1)
	backend := secretsBackend(cfg)
	var secretsCfg SecretsConfig
	if cfg != nil {
		secretsCfg = cfg.Secrets
	}
	store, binder, err := deps.selectSecrets(secretsCfg, backend, dataDir)
	if err != nil {
		return fail(exitUsage, "秘密情報の保存先 %s: %v", backend, err)
	}
	if store != nil {
		if deps.installSecrets == nil {
			return fail(exitRuntime, "秘密情報の保存先 %s を組み込めません（installSecrets がありません）", backend)
		}
		deps.installSecrets(store)
		defer deps.installSecrets(nil)
	}

	// 5. the app
	app := NewApp()
	if app.store == nil {
		app.shutdown(context.Background())
		if errors.Is(app.storeErr, errAnotherInstance) {
			// A GUI on the same data holds only the database lock, not
			// ssh-gate.lock; it is still "another process" (§1.1: exit 3).
			return fail(exitLocked, "データベースを開けません: %s", app.storageError)
		}
		return fail(exitRuntime, "データベースを開けません: %s", app.storageError)
	}
	stopApp := func() {
		sctx, cancel := context.WithTimeout(context.Background(), headlessAppShutdown)
		defer cancel()
		app.shutdown(sctx)
	}
	if backend == "keychain" {
		warnKeychainUnavailable(app)
	}

	var audit configAudit
	var sink auditSink
	closeAudit := func(time.Duration) {}
	if deps.newAuditSink != nil {
		s, c, err := deps.newAuditSink(app)
		if err != nil {
			stopApp()
			return fail(exitRuntime, "監査ログを開けません: %v", err)
		}
		sink = s
		audit = configAuditTo(s)
		if c != nil {
			closeAudit = c
		}
	} else if deps.newAudit != nil {
		a, c := deps.newAudit(app)
		audit = a
		if c != nil {
			closeAudit = c
		}
	}

	// 6. the file into the database
	var report ConfigApplyReport
	if cfg != nil {
		report, err = applyConfig(app, cfg, binder, audit)
		if err != nil {
			closeAudit(headlessAuditFlush)
			stopApp()
			return fail(exitUsage, "設定ファイルを反映できません: %v", err)
		}
		for _, note := range report.Notes {
			log.Printf("config: %s", note)
		}
	}

	// 7. web admin (token, TLS, listener rules)
	var plan headlessWebPlan
	if deps.prepareWeb != nil {
		plan, err = deps.prepareWeb(headlessWebParams{
			App: app, Web: webCfg, DataDir: dataDir, Dist: dist, AdminTokenPath: tokenPath, Audit: audit, AuditSink: sink,
		})
		if err != nil {
			closeAudit(headlessAuditFlush)
			stopApp()
			return fail(exitUsage, "Web 管理画面: %v", err)
		}
	} else {
		log.Printf("headless: this build has no web admin; only MCP is served")
	}

	// 8. startup: serial watch and MCP. Without an event hub a.ctx stays
	// nil so emitEvent drops events (it must never reach the Wails runtime
	// with a non-Wails ctx).
	var startCtx context.Context
	if plan.WithEvents != nil {
		startCtx = plan.WithEvents(context.Background())
	}
	app.startup(startCtx)

	// 9. listen
	if plan.Server != nil {
		if err := plan.Server.Start(); err != nil {
			closeAudit(headlessAuditFlush)
			stopApp()
			return fail(exitRuntime, "Web 管理画面を開始できません: %v", err)
		}
	}
	printBanner(out, app, cfg, report, dataDir, backend, binder, plan, tokenPath, adminPasswordPath(webCfg, dataDir))

	// 10. wait
	sigs, stopSignals := deps.signals()
	defer stopSignals()
	waitForStop(ctx, sigs)
	log.Printf("headless: stopping (a second signal exits at once)")
	var finished sync.WaitGroup
	done := make(chan struct{})
	finished.Add(1)
	go func() {
		defer finished.Done()
		select {
		case <-sigs:
			log.Printf("headless: second signal, exiting now")
			deps.exit(exitForced)
		case <-done:
		}
	}()
	headlessShutdown(app, plan.Server, closeAudit, stopApp)
	close(done)
	finished.Wait()
	return exitOK
}

// setAdminPasswordCommand is --set-admin-password: read the new password
// from stdin, store only its argon2id hash (0600) and exit. A running server
// notices the file within 5 s and logs every session out.
// reserved reports MCP-side secrets (storedMCPSecrets) the password must not equal.
func setAdminPasswordCommand(deps headlessDeps, path string, reserved func(string) bool) int {
	in := deps.stdin
	interactive := false
	if in == nil {
		in = os.Stdin
		if deps.stdinIsTerminal == nil {
			if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
				interactive = true
			}
		}
	}
	if deps.stdinIsTerminal != nil {
		interactive = deps.stdinIsTerminal()
	}
	pw, err := readNewAdminPassword(in, interactive, func(s string) { fmt.Fprint(deps.stderr, s) })
	if err != nil {
		fmt.Fprintf(deps.stderr, "ssh-gete: 管理パスワード: %v\n", err)
		return exitUsage
	}
	if err := setAdminPasswordFile(path, pw, reserved); err != nil {
		fmt.Fprintf(deps.stderr, "ssh-gete: 管理パスワード: %v\n", err)
		if validateNewAdminPassword(pw) != nil || errors.Is(err, errAdminPasswordIsMCPSecret) {
			return exitUsage
		}
		return exitRuntime
	}
	fmt.Fprintf(deps.stdout, "管理パスワードを設定しました: %s（argon2id のハッシュだけを保存。管理トークンでもログインできます）\n", path)
	return exitOK
}

// storedMCPSecrets answers whether a value is an MCP secret a running (or the
// next) server would accept: SSH_GETE_TOKEN, the Bearer saved in the database
// or a per-agent token. The database is opened read-only per call; if it
// cannot be read only the environment is checked.
func storedMCPSecrets(dbPath string) func(string) bool {
	return func(v string) bool {
		v = strings.TrimSpace(v)
		if v == "" {
			return false
		}
		a := &App{agentTokens: map[string]AgentToken{}, mcpEnvToken: strings.TrimSpace(os.Getenv("SSH_GETE_TOKEN"))}
		if store, err := openStoreReadOnly(dbPath); err == nil && store != nil {
			defer store.Close()
			if _, token, loaded, err := store.loadMCPSettings(dbPath); err == nil && loaded {
				a.mcpToken = token
			}
			if tokens, err := store.loadAgentTokens(); err == nil {
				for _, t := range tokens {
					a.agentTokens[t.AgentName] = t
				}
			}
		}
		if a.isMCPSecret(v) {
			return true
		}
		// The saved Bearer still counts when SSH_GETE_TOKEN overrides it now:
		// unsetting the variable would bring it back.
		return a.mcpToken != "" && v == a.mcpToken
	}
}

// waitForStop returns on SIGINT / SIGTERM or when ctx is done. SIGHUP is
// logged and ignored: v1 does not reload the file (§8).
func waitForStop(ctx context.Context, sigs <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				log.Printf("headless: SIGHUP ignored; restart to apply a changed config file")
				continue
			}
			return
		}
	}
}

// headlessShutdown is §8 steps 1-5: web intake, running calls, the web
// server, the audit writer, then app.shutdown (§5.0 order inside).
func headlessShutdown(app *App, web headlessWeb, closeAudit func(time.Duration), stopApp func()) {
	if web != nil {
		web.StopIntake()
		if !web.WaitCalls(headlessWaitCalls) {
			log.Printf("headless: web calls still running after %s; app shutdown will cancel them", headlessWaitCalls)
		}
		ctx, cancel := context.WithTimeout(context.Background(), headlessWebClose)
		if err := web.Close(ctx); err != nil {
			log.Printf("headless: close web admin: %v", err)
		}
		cancel()
	}
	if closeAudit != nil {
		closeAudit(headlessAuditFlush)
	}
	if stopApp != nil {
		stopApp()
	} else if app != nil {
		app.shutdown(context.Background())
	}
}

// resolveDataDir: --data-dir > SSH_GETE_CONFIG_DIR > dataDir in the file >
// appConfigDir(). The result is exported as SSH_GETE_CONFIG_DIR, which is
// all NewApp reads, and created 0700.
func resolveDataDir(opts CLIOptions, cfg *FileConfig) (string, error) {
	dir := strings.TrimSpace(opts.DataDir)
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("SSH_GETE_CONFIG_DIR"))
	}
	if dir == "" && cfg != nil {
		dir = strings.TrimSpace(cfg.DataDir)
	}
	if dir == "" {
		dir = appConfigDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("データディレクトリ %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("データディレクトリ %s を作れません: %w", abs, err)
	}
	if err := os.Setenv("SSH_GETE_CONFIG_DIR", abs); err != nil {
		return "", err
	}
	return abs, nil
}

// builtinSecretBackend is selectSecrets when the secret_store_* files are
// not in the build: keychain and memory work with the existing code.
func builtinSecretBackend(_ SecretsConfig, backend, _ string) (SecretStore, configSecretBinder, error) {
	switch backend {
	case "keychain":
		return nil, nil, nil
	case "memory":
		if !memorySecretsAllowed() {
			return nil, nil, errors.New("memory はテスト専用です（keychain を選んでください）")
		}
		return nil, nil, os.Setenv(keyringBackendEnv, "memory")
	default:
		return nil, nil, errors.New("このビルドでは使えません（keychain か memory を選んでください）")
	}
}

// warnKeychainUnavailable logs §7.1's advice when the default keychain
// backend cannot be reached (headless Linux without a Secret Service).
func warnKeychainUnavailable(app *App) {
	if app.secrets == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), headlessSecretsProbe)
	defer cancel()
	_, err := app.secrets.Get(ctx, "probe/headless")
	if err != nil && !errors.Is(err, ErrSecretNotFound) {
		log.Printf("secrets: キーチェーンを使えないため、パスワードは保存できません。鍵認証か ssh-agent を使うか、secrets.backend: file / env を選んでください（%v）", err)
	}
}

// checkConfigCommand is --check-config: validate (already done), then show
// what applyConfig would do, without writing the database.
func checkConfigCommand(out, errOut io.Writer, cfg *FileConfig, dataDir string) int {
	app, closeStore, err := checkConfigApp(filepath.Join(dataDir, "ssh-gete.sqlite"))
	if err != nil {
		fmt.Fprintf(errOut, "ssh-gete: %v\n", err)
		return exitRuntime
	}
	defer closeStore()
	report, err := planConfig(app, cfg)
	if err != nil {
		fmt.Fprintf(errOut, "ssh-gete: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintf(out, "config  : %s (OK, apply: %s)\n", cfg.path, cfg.applyMode())
	fmt.Fprintf(out, "data dir: %s\n", dataDir)
	formatReport(out, report)
	return exitOK
}

func printBanner(out io.Writer, app *App, cfg *FileConfig, report ConfigApplyReport, dataDir, backend string,
	binder configSecretBinder, plan headlessWebPlan, tokenPath, passwordPath string) {
	fmt.Fprintf(out, "ssh-gete %s headless\n", appVersion)
	fmt.Fprintf(out, "data dir      : %s\n", dataDir)
	if cfg != nil {
		fmt.Fprintf(out, "config        : %s (%s)\n", cfg.path, report.Summary())
	} else {
		fmt.Fprintf(out, "config        : (none)\n")
	}
	secrets := backend
	if b, ok := binder.(interface{ bound() int }); ok {
		secrets += fmt.Sprintf(" (env bindings: %d)", b.bound())
	}
	fmt.Fprintf(out, "secrets       : %s\n", secrets)
	mcp := app.GetMCPSettings()
	if mcp.Enabled {
		bearer := "off"
		if mcp.BearerEnabled {
			bearer = "on"
		}
		url := mcp.LocalURL
		if url == "" {
			url = fmt.Sprintf("http://%s:%d%s", mcp.ListenAddress, mcp.Port, mcp.MCPPath)
		}
		fmt.Fprintf(out, "MCP           : %s (bearer: %s, %s)\n", url, bearer, mcp.Status)
	} else {
		fmt.Fprintf(out, "MCP           : disabled\n")
	}
	if plan.Server != nil {
		fmt.Fprintf(out, "web admin     : %s\n", plan.Server.URL())
		for _, line := range plan.Banner {
			fmt.Fprintln(out, line)
		}
		fmt.Fprintf(out, "admin token   : %s (mode 0600) — 表示するには --print-admin-token\n", tokenPath)
		if _, err := os.Stat(passwordPath); passwordPath != "" && err == nil {
			fmt.Fprintf(out, "admin password: set (%s, argon2id)\n", passwordPath)
		} else {
			fmt.Fprintf(out, "admin password: not set — 設定するには --set-admin-password\n")
		}
	} else {
		fmt.Fprintf(out, "web admin     : (not in this build)\n")
	}
}

// ---------------------------------------------------------------------------
// Single-instance lock (fallback for platforms without an OS lock; the
// default is acquireInstanceLock in instance_lock_*.go)

// errHeadlessLocked: another process runs with this data directory (exit 3).
var errHeadlessLocked = errors.New("同じデータディレクトリで別の ssh-gete が動作中です")

// acquireDataDirLock is the portable fallback lock: <dir>/ssh-gete.lock
// created exclusively, holding the owner's PID. A file whose PID is no
// longer running is stale and taken over. (An flock / LockFileEx lock, which
// the OS releases on a crash, can replace it through headlessDeps.lock.)
func acquireDataDirLock(dir string) (func(), error) {
	path := filepath.Join(dir, "ssh-gete.lock")
	self := strconv.Itoa(os.Getpid())
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.WriteString(self + "\n")
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
				return nil, errors.Join(werr, cerr)
			}
			var once sync.Once
			return func() {
				once.Do(func() {
					if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == self {
						_ = os.Remove(path)
					}
				})
			}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // released meanwhile
			}
			return nil, err
		}
		owner := strings.TrimSpace(string(data))
		if pid, perr := strconv.Atoi(owner); perr == nil && pid > 0 && (owner == self || processAlive(pid)) {
			return nil, fmt.Errorf("%w（pid %d、%s）", errHeadlessLocked, pid, path)
		}
		if owner == "" && attempt == 0 {
			// Being written by a process that just created it.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		log.Printf("headless: removing stale lock %s (pid %q)", path, owner)
		_ = os.Remove(path)
	}
	return nil, fmt.Errorf("%w（%s）", errHeadlessLocked, path)
}

// processAlive: on Unix signal 0 probes the PID; on Windows FindProcess
// opens a handle and fails for a PID that does not exist.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		_ = p.Release()
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM)
}
