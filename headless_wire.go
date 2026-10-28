package main

// headless_wire.go connects runHeadless (headless.go) to the parts that
// implement it: the secret backends (secret_store_file.go), the admin audit
// writer (web_audit.go), the admin token and the Web server (webauth.go,
// webserver.go), the call layer (webapi.go) and the event hub
// (webevents.go). phase6-headless.md §1.5 is the order.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func init() {
	headlessHooks.selectSecrets = headlessSelectSecrets
	headlessHooks.installSecrets = installSecretBackend
	headlessHooks.newAuditSink = headlessNewAudit
	headlessHooks.prepareWeb = prepareHeadlessWeb
	headlessHooks.adminToken = headlessAdminToken
}

// headlessSelectSecrets is §1.5 step 4. backend is already the effective one
// (SSH_GETE_KEYRING over the file); secrets.file only counts with "file".
func headlessSelectSecrets(cfg SecretsConfig, backend, dataDir string) (SecretStore, configSecretBinder, error) {
	file := cfg.File
	if !strings.EqualFold(strings.TrimSpace(backend), secretBackendFile) {
		file = ""
	}
	store, env, err := selectSecretBackend(backend, file, dataDir)
	if err != nil {
		return nil, nil, err
	}
	// A nil *envSecretStore inside a non-nil interface would panic in bind.
	var binder configSecretBinder
	if env != nil {
		binder = env
	}
	return store, binder, nil
}

// headlessNewAudit starts the admin_audit writer on the app's database.
func headlessNewAudit(app *App) (auditSink, func(time.Duration), error) {
	w, err := newAdminAuditWriter(app.store)
	if err != nil {
		return nil, nil, err
	}
	return w, w.Close, nil
}

// headlessAdminToken is --print-admin-token / --rotate-admin-token.
func headlessAdminToken(path string, rotate bool) (string, error) {
	if rotate {
		return rotateAdminToken(path)
	}
	token, _, err := loadOrCreateAdminToken(path)
	return token, err
}

// prepareHeadlessWeb is §1.5 step 7: event hub, call layer, Web server
// (admin token, TLS, static files), then the listener rules that depend on
// MCP (§6.1). Nothing listens yet.
func prepareHeadlessWeb(p headlessWebParams) (headlessWebPlan, error) {
	var audit func(webAuditEvent)
	if p.AuditSink != nil {
		sink := p.AuditSink
		audit = func(e webAuditEvent) { sink.record(fromWebAudit(e)) }
	}

	hub := newEventHub(hubConfig{SessionID: webSessionID, RequireSession: true}, p.App.sessionsSnapshot)
	rpc, err := newRPCHandler(p.App, webCallHooksFor(audit))
	if err != nil {
		return headlessWebPlan{}, err
	}

	opts := webServerOptionsFromConfig(p.Web, p.DataDir)
	if p.AdminTokenPath != "" {
		opts.AdminTokenFile = p.AdminTokenPath
	}
	opts.Dist = p.Dist
	opts.Admin = webAdminFS()
	opts.RPC = rpc
	opts.Events = hub
	opts.Env = http.HandlerFunc(handleEnv)
	opts.AuditList = adminAuditListHandler(p.App.store)
	opts.Audit = audit

	srv, err := newWebServer(opts)
	if err != nil {
		return headlessWebPlan{}, err
	}

	p.App.mu.Lock()
	mcp := p.App.mcp
	mcpToken := p.App.effectiveTokenLocked()
	p.App.mu.Unlock()
	if err := validateWebListen(opts, mcp, mcpToken, srv.AdminTokenHash()); err != nil {
		return headlessWebPlan{}, err
	}
	// The startup check above only covers the admin token. While running,
	// SaveMCPSettings must not take an admin credential as the Bearer, and a
	// new admin password must not be the Bearer or an agent token.
	srv.auth.mcpSecret = p.App.isMCPSecret
	p.App.mu.Lock()
	p.App.adminCredential = srv.auth.isAdminCredential
	p.App.mu.Unlock()

	var banner []string
	if fp := srv.TLSFingerprint(); fp != "" {
		banner = append(banner, fmt.Sprintf("TLS           : %s, SHA-256 %s", strings.TrimSpace(p.Web.TLS.Mode), fp))
	}
	return headlessWebPlan{Server: srv, WithEvents: func(ctx context.Context) context.Context { return withWebHub(ctx, hub) }, Banner: banner}, nil
}
