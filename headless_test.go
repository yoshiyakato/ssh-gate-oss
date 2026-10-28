package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

func TestParseCLIGUIIgnoresUnknownWithoutHeadless(t *testing.T) {
	opts, err := parseCLI([]string{"-psn_0_12345", "whatever"}, true)
	if err != nil || opts.wantsHeadless() {
		t.Fatalf("GUI launch args: %+v, %v", opts, err)
	}
	opts, err = parseCLI(nil, true)
	if err != nil || opts.wantsHeadless() {
		t.Fatalf("no args: %+v, %v", opts, err)
	}
}

func TestParseCLIStrictWithHeadless(t *testing.T) {
	t.Setenv("SSH_GETE_CONFIG", "")
	opts, err := parseCLI([]string{"--headless", "--config", "/etc/c.yaml", "--listen=0.0.0.0:9443",
		"--data-dir", "/var/lib/x", "--tls-self-signed"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := CLIOptions{Headless: true, ConfigPath: "/etc/c.yaml", Listen: "0.0.0.0:9443", DataDir: "/var/lib/x", TLSSelfSigned: true}
	if !reflect.DeepEqual(opts, want) {
		t.Fatalf("opts = %+v", opts)
	}
	for _, args := range [][]string{
		{"--headless", "--bogus"},
		{"--headless", "positional"},
		{"--headless", "--print-admin-token", "--rotate-admin-token"},
		{"--headless", "--check-config", "--print-admin-token"},
		{"-psn_0_1", "--headless"},
	} {
		if _, err := parseCLI(args, true); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestParseCLIHeadlessBuild(t *testing.T) {
	t.Setenv("SSH_GETE_CONFIG", "/from/env.yaml")
	opts, err := parseCLI(nil, false)
	if err != nil || !opts.Headless || opts.ConfigPath != "/from/env.yaml" {
		t.Fatalf("headless build: %+v, %v", opts, err)
	}
	opts, err = parseCLI([]string{"--config", "/flag.yaml"}, false)
	if err != nil || opts.ConfigPath != "/flag.yaml" {
		t.Fatalf("flag must win over SSH_GETE_CONFIG: %+v, %v", opts, err)
	}
	if _, err := parseCLI([]string{"-psn_0_1"}, false); err == nil {
		t.Fatalf("headless build accepted an unknown flag")
	}
	opts, err = parseCLI([]string{"-h"}, false)
	if err != nil || !opts.ShowHelp {
		t.Fatalf("-h: %+v, %v", opts, err)
	}
	for _, flag := range []string{"--check-config", "--version", "--print-admin-token", "--rotate-admin-token"} {
		opts, err := parseCLI([]string{flag}, true)
		if err != nil || !opts.wantsHeadless() {
			t.Fatalf("%s in the GUI build: %+v, %v", flag, opts, err)
		}
	}
}

func TestResolveDataDirPrecedence(t *testing.T) {
	isolateEnv(t)
	flagDir, envDir, fileDir := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := &FileConfig{DataDir: fileDir}
	t.Setenv("SSH_GETE_CONFIG_DIR", envDir)
	if got, _ := resolveDataDir(CLIOptions{DataDir: flagDir}, cfg); got != flagDir {
		t.Fatalf("flag: %q", got)
	}
	if os.Getenv("SSH_GETE_CONFIG_DIR") != flagDir {
		t.Fatalf("SSH_GETE_CONFIG_DIR not exported")
	}
	t.Setenv("SSH_GETE_CONFIG_DIR", envDir)
	if got, _ := resolveDataDir(CLIOptions{}, cfg); got != envDir {
		t.Fatalf("env: %q", got)
	}
	t.Setenv("SSH_GETE_CONFIG_DIR", "")
	if got, _ := resolveDataDir(CLIOptions{}, cfg); got != fileDir {
		t.Fatalf("file: %q", got)
	}
}

func TestDataDirLock(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireDataDirLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireDataDirLock(dir); !errors.Is(err, errHeadlessLocked) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	release() // idempotent
	release2, err := acquireDataDirLock(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()

	// A lock left by a process that is gone is taken over.
	dead := 1 << 22 // above the default pid_max on Linux and macOS
	if processAlive(dead) {
		t.Skip("pid in use")
	}
	path := filepath.Join(dir, "ssh-gete.lock")
	if err := os.WriteFile(path, []byte(strconv.Itoa(dead)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release3, err := acquireDataDirLock(dir)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	release3()
	if fileExists(path) {
		t.Fatalf("lock file left behind")
	}
}

// --- runHeadlessWith ---------------------------------------------------------

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	l.events = append(l.events, s)
	l.mu.Unlock()
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.events...)
}

type fakeWeb struct {
	log       *eventLog
	app       *App
	waitBlock chan struct{} // WaitCalls blocks until closed (nil: no wait)
}

func (f *fakeWeb) Start() error { f.log.add("web.Start"); return nil }
func (f *fakeWeb) StopIntake()  { f.log.add("web.StopIntake") }
func (f *fakeWeb) WaitCalls(time.Duration) bool {
	f.log.add("web.WaitCalls")
	if f.waitBlock != nil {
		<-f.waitBlock
	}
	return true
}
func (f *fakeWeb) Close(context.Context) error {
	// The store must still be open while the web layer closes.
	if f.app.store.db.Ping() != nil {
		f.log.add("web.Close(store closed!)")
	} else {
		f.log.add("web.Close")
	}
	return nil
}
func (f *fakeWeb) URL() string { return "http://127.0.0.1:8788/" }

type ctxKeyTest struct{}

type headlessHarness struct {
	t       *testing.T
	log     *eventLog
	sigs    chan os.Signal
	exitMu  sync.Mutex
	exits   []int
	out     bytes.Buffer
	errOut  bytes.Buffer
	app     *App
	appMu   sync.Mutex
	web     *fakeWeb
	started chan struct{}
	deps    headlessDeps
}

func newHeadlessHarness(t *testing.T) *headlessHarness {
	t.Helper()
	isolateEnv(t)
	// Never bind the default MCP port 8787 (another app may own it).
	t.Setenv("SSH_GETE_MCP_ADDR", freeLoopbackAddr(t))
	h := &headlessHarness{t: t, log: &eventLog{}, sigs: make(chan os.Signal, 4), started: make(chan struct{})}
	h.deps = headlessDeps{
		stdout: &syncWriter{w: &h.out},
		stderr: &syncWriter{w: &h.errOut},
		newAudit: func(app *App) (configAudit, func(time.Duration)) {
			return func(action, target, detail string) { h.log.add("audit." + action) },
				func(time.Duration) {
					if app.store.db.Ping() != nil {
						h.log.add("audit.Close(store closed!)")
					} else {
						h.log.add("audit.Close")
					}
				}
		},
		prepareWeb: func(p headlessWebParams) (headlessWebPlan, error) {
			h.appMu.Lock()
			h.app = p.App
			h.appMu.Unlock()
			h.web = &fakeWeb{log: h.log, app: p.App}
			return headlessWebPlan{
				Server: h.web,
				WithEvents: func(ctx context.Context) context.Context {
					return context.WithValue(ctx, ctxKeyTest{}, "hub")
				},
				Banner: []string{"TLS           : off"},
			}, nil
		},
		signals: func() (<-chan os.Signal, func()) {
			close(h.started)
			return h.sigs, func() {}
		},
		exit: func(code int) {
			h.exitMu.Lock()
			h.exits = append(h.exits, code)
			h.exitMu.Unlock()
		},
	}
	return h
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// run starts runHeadlessWith in the background and returns its exit code
// channel once the signal wait has begun.
func (h *headlessHarness) run(ctx context.Context, opts CLIOptions) <-chan int {
	done := make(chan int, 1)
	go func() { done <- runHeadlessWith(ctx, opts, fstest.MapFS{}, h.deps) }()
	select {
	case <-h.started:
	case code := <-done:
		h.t.Fatalf("runHeadless returned %d before waiting; stderr:\n%s", code, h.errOut.String())
	case <-time.After(20 * time.Second):
		h.t.Fatalf("runHeadless did not reach the signal wait")
	}
	return done
}

func waitCode(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(30 * time.Second):
		t.Fatalf("runHeadless did not return")
		return -1
	}
}

const headlessTestConfig = `version: 1
mcp:
  enabled: false
connections:
  - name: web-1
    host: 10.0.0.5
    user: deploy
`

func TestRunHeadlessStartupAndShutdownOrder(t *testing.T) {
	h := newHeadlessHarness(t)
	cfgPath := writeConfig(t, headlessTestConfig)
	done := h.run(context.Background(), CLIOptions{Headless: true, ConfigPath: cfgPath})

	h.appMu.Lock()
	app := h.app
	h.appMu.Unlock()
	if app.ctx == nil || app.ctx.Value(ctxKeyTest{}) != "hub" {
		t.Fatalf("startup did not get the event hub ctx")
	}
	if _, ok := connByName(t, app, "web-1"); !ok {
		t.Fatalf("config not applied before startup")
	}
	if os.Getenv("SSH_GETE_NO_SEED") != "1" {
		t.Fatalf("SSH_GETE_NO_SEED not set")
	}
	if fileExists(filepath.Join(os.Getenv("SSH_GETE_CONFIG_DIR"), "ssh-gete.lock")) == false {
		t.Fatalf("instance lock not held while running")
	}

	h.sigs <- syscall.SIGHUP // ignored
	h.sigs <- syscall.SIGTERM
	if code := waitCode(t, done); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.errOut.String())
	}
	want := []string{"audit.config_create", "audit.config_override", "web.Start", "web.StopIntake", "web.WaitCalls", "web.Close", "audit.Close"}
	if got := h.log.list(); !reflect.DeepEqual(got, want) {
		t.Fatalf("order\n got %v\nwant %v", got, want)
	}
	if err := app.store.db.Ping(); err == nil {
		t.Fatalf("store still open after shutdown")
	}
	if rel, err := acquireInstanceLock(os.Getenv("SSH_GETE_CONFIG_DIR")); err != nil {
		t.Fatalf("lock not released: %v", err)
	} else {
		rel()
	}
	out := h.out.String()
	for _, want := range []string{"headless", "data dir", cfgPath, "1 created", "web admin     : http://127.0.0.1:8788/", "TLS           : off", "admin-token", "MCP           : disabled"} {
		if !strings.Contains(out, want) {
			t.Fatalf("banner lacks %q:\n%s", want, out)
		}
	}
	h.exitMu.Lock()
	defer h.exitMu.Unlock()
	if len(h.exits) != 0 {
		t.Fatalf("forced exit on a single signal: %v", h.exits)
	}
}

func TestRunHeadlessSecondSignalExitsAtOnce(t *testing.T) {
	h := newHeadlessHarness(t)
	block := make(chan struct{})
	prepare := h.deps.prepareWeb
	h.deps.prepareWeb = func(p headlessWebParams) (headlessWebPlan, error) {
		plan, err := prepare(p)
		h.web.waitBlock = block
		return plan, err
	}
	done := h.run(context.Background(), CLIOptions{Headless: true, ConfigPath: writeConfig(t, headlessTestConfig)})
	h.sigs <- os.Interrupt
	deadline := time.Now().Add(10 * time.Second)
	for !contains(h.log.list(), "web.WaitCalls") {
		if time.Now().After(deadline) {
			t.Fatalf("shutdown did not reach WaitCalls")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.sigs <- os.Interrupt
	for {
		h.exitMu.Lock()
		n := len(h.exits)
		h.exitMu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second signal did not force the exit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.exitMu.Lock()
	if h.exits[0] != exitForced {
		t.Fatalf("exit code %d", h.exits[0])
	}
	h.exitMu.Unlock()
	close(block) // the fake exit returned; let the shutdown finish
	waitCode(t, done)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestRunHeadlessContextCancelStops(t *testing.T) {
	h := newHeadlessHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := h.run(ctx, CLIOptions{Headless: true})
	cancel()
	if code := waitCode(t, done); code != exitOK {
		t.Fatalf("exit %d", code)
	}
}

func TestRunHeadlessWithoutWebKeepsCtxNil(t *testing.T) {
	h := newHeadlessHarness(t)
	var app *App
	h.deps.prepareWeb = nil
	h.deps.newAudit = func(a *App) (configAudit, func(time.Duration)) {
		app = a
		return nil, nil
	}
	done := h.run(context.Background(), CLIOptions{Headless: true, ConfigPath: writeConfig(t, headlessTestConfig)})
	if app.ctx != nil {
		t.Fatalf("a.ctx must stay nil without an event hub (no Wails runtime)")
	}
	h.sigs <- syscall.SIGTERM
	if code := waitCode(t, done); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "not in this build") {
		t.Fatalf("banner:\n%s", h.out.String())
	}
}

func TestRunHeadlessSecondInstanceExit3(t *testing.T) {
	h := newHeadlessHarness(t)
	release, err := acquireInstanceLock(os.Getenv("SSH_GETE_CONFIG_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	code := runHeadlessWith(context.Background(), CLIOptions{Headless: true}, fstest.MapFS{}, h.deps)
	if code != exitLocked {
		t.Fatalf("exit %d, want %d; stderr:\n%s", code, exitLocked, h.errOut.String())
	}
}

func TestRunHeadlessConfigErrorsExit2(t *testing.T) {
	h := newHeadlessHarness(t)
	for name, opts := range map[string]CLIOptions{
		"missing file":         {Headless: true, ConfigPath: filepath.Join(t.TempDir(), "nope.yaml")},
		"unknown key":          {Headless: true, ConfigPath: writeConfig(t, "version: 1\npassword: x\n")},
		"bad listen":           {Headless: true, Listen: "nope"},
		"check w/o config":     {Headless: true, CheckConfig: true},
		"file backend unbuilt": {Headless: true, ConfigPath: writeConfig(t, "version: 1\nsecrets: {backend: env}\n")},
	} {
		h.errOut.Reset()
		t.Setenv("SSH_GETE_KEYRING", "") // let the file pick the backend
		if code := runHeadlessWith(context.Background(), opts, fstest.MapFS{}, h.deps); code != exitUsage {
			t.Fatalf("%s: exit %d, want 2; stderr:\n%s", name, code, h.errOut.String())
		}
	}
}

func TestRunHeadlessApplyErrorStopsCleanly(t *testing.T) {
	h := newHeadlessHarness(t)
	// The file must reach the listener check (it is refused before any Listen).
	t.Setenv("SSH_GETE_MCP_ADDR", "")
	cfg := writeConfig(t, "version: 1\nmcp:\n  enabled: true\n  listenAddress: 0.0.0.0\n  bearerEnabled: false\n")
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, ConfigPath: cfg}, fstest.MapFS{}, h.deps); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	if rel, err := acquireInstanceLock(os.Getenv("SSH_GETE_CONFIG_DIR")); err != nil {
		t.Fatalf("lock left after a refused start: %v", err)
	} else {
		rel()
	}
	if contains(h.log.list(), "web.Start") {
		t.Fatalf("web started after a refused config")
	}
}

func TestRunHeadlessCheckConfig(t *testing.T) {
	h := newHeadlessHarness(t)
	code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, CheckConfig: true,
		ConfigPath: writeConfig(t, headlessTestConfig)}, fstest.MapFS{}, h.deps)
	if code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, h.errOut.String())
	}
	if !strings.Contains(h.out.String(), "create     connection:web-1") {
		t.Fatalf("output:\n%s", h.out.String())
	}
	if fileExists(filepath.Join(os.Getenv("SSH_GETE_CONFIG_DIR"), "ssh-gete.sqlite")) {
		t.Fatalf("--check-config created the database")
	}
}

func TestRunHeadlessAdminTokenCommands(t *testing.T) {
	h := newHeadlessHarness(t)
	var calls []string
	h.deps.adminToken = func(path string, rotate bool) (string, error) {
		calls = append(calls, path+" "+strconv.FormatBool(rotate))
		return "sgadm_test-token-value", nil
	}
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, PrintAdminToken: true}, fstest.MapFS{}, h.deps); code != exitOK {
		t.Fatalf("print: exit %d", code)
	}
	if strings.TrimSpace(h.out.String()) != "sgadm_test-token-value" {
		t.Fatalf("print output %q", h.out.String())
	}
	h.out.Reset()
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, RotateAdminToken: true}, fstest.MapFS{}, h.deps); code != exitOK {
		t.Fatalf("rotate: exit %d", code)
	}
	if strings.Contains(h.out.String(), "sgadm_") {
		t.Fatalf("rotate printed the token: %q", h.out.String())
	}
	wantPath := filepath.Join(os.Getenv("SSH_GETE_CONFIG_DIR"), "admin-token")
	if !reflect.DeepEqual(calls, []string{wantPath + " false", wantPath + " true"}) {
		t.Fatalf("calls = %v", calls)
	}
	h.deps.adminToken = nil
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, PrintAdminToken: true}, fstest.MapFS{}, h.deps); code != exitRuntime {
		t.Fatalf("without the web layer: exit %d", code)
	}
}

func TestRunHeadlessVersion(t *testing.T) {
	h := newHeadlessHarness(t)
	if code := runHeadlessWith(context.Background(), CLIOptions{ShowVersion: true}, nil, h.deps); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(h.out.String(), "ssh-gete "+appVersion) {
		t.Fatalf("output %q", h.out.String())
	}
}

func TestRunHeadlessInstallsSelectedSecretStore(t *testing.T) {
	h := newHeadlessHarness(t)
	store := newMemorySecretStore()
	var installed []SecretStore
	var gotBackend string
	h.deps.selectSecrets = func(_ SecretsConfig, backend, _ string) (SecretStore, configSecretBinder, error) {
		gotBackend = backend
		return store, nil, nil
	}
	h.deps.installSecrets = func(s SecretStore) { installed = append(installed, s) }
	done := h.run(context.Background(), CLIOptions{Headless: true})
	h.sigs <- syscall.SIGTERM
	waitCode(t, done)
	if gotBackend != "memory" { // isolateEnv sets SSH_GETE_KEYRING=memory
		t.Fatalf("backend = %q", gotBackend)
	}
	if len(installed) != 2 || installed[0] != SecretStore(store) || installed[1] != nil {
		t.Fatalf("install calls = %v", installed)
	}
}

func TestEmbeddedDistHasIndex(t *testing.T) {
	if _, err := fs.Stat(embeddedDist(), ".keep"); err == nil {
		if _, err := fs.Stat(embeddedDist(), "index.html"); err != nil {
			t.Skip("frontend/dist is the CI stub (no npm run build)")
		}
	}
	if _, err := embeddedDist().Open("index.html"); err != nil {
		t.Fatalf("frontend/dist/index.html not embedded: %v", err)
	}
}
