package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// guardHTTP is applied to every MCP listener route. It blocks the browser
// attack paths against a localhost service:
//   - Host must be an IP literal or "localhost" with the listener's port,
//     which defeats DNS rebinding (the attacker's hostname is rejected).
//   - Any Origin header is refused unless explicitly allow-listed; MCP clients
//     are not browsers and do not send one.
//   - POST bodies must be application/json, so a cross-site "simple" request
//     (text/plain form post, no preflight) cannot reach the JSON-RPC handler.
func guardHTTP(settings MCPSettings, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host, settings) {
			writeJSON(w, http.StatusForbidden, rpcErrorResponse(nil, -32001, "forbidden host"))
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && !originAllowed(origin, settings.AllowedOrigins) {
			writeJSON(w, http.StatusForbidden, rpcErrorResponse(nil, -32001, "forbidden origin"))
			return
		}
		if r.Method == http.MethodOptions {
			if origin == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "content-type, authorization, mcp-protocol-version")
			w.Header().Set("Vary", "Origin")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodPost {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				writeJSON(w, http.StatusUnsupportedMediaType, rpcErrorResponse(nil, -32600, "Content-Type must be application/json"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func allowedHost(hostHeader string, settings MCPSettings) bool {
	// An explicit BaseURL (reverse proxy) names the one extra hostname allowed.
	if settings.BaseURL != "" {
		if u, err := url.Parse(settings.BaseURL); err == nil && strings.EqualFold(u.Host, hostHeader) {
			return true
		}
	}
	port := settings.Port
	host, portText, err := net.SplitHostPort(hostHeader)
	if err != nil {
		// No port in Host means the default port 80, which is never ours
		// unless configured so.
		host, portText = strings.Trim(hostHeader, "[]"), "80"
	}
	if portText != strconv.Itoa(port) {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host) != nil
}

func originAllowed(origin string, allowed []string) bool {
	if origin == "null" {
		return false
	}
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimRight(candidate, "/"), strings.TrimRight(origin, "/")) {
			return true
		}
	}
	return false
}

// checkBearer validates the Authorization header in constant time.
func checkBearer(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	got := r.Header.Get("Authorization")
	want := "Bearer " + token
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="ssh-gete"`)
	writeJSON(w, http.StatusUnauthorized, rpcErrorResponse(nil, -32001, "unauthorized"))
}

func isLoopbackListen(address string) bool {
	if strings.EqualFold(address, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(address, "[]"))
	return ip != nil && ip.IsLoopback()
}

// validateListenSettings refuses to expose the gateway beyond this machine
// without Bearer authentication.
func validateListenSettings(settings MCPSettings, token string) error {
	if !settings.Enabled || isLoopbackListen(settings.ListenAddress) {
		return nil
	}
	if !settings.BearerEnabled || token == "" {
		return fmt.Errorf("待受アドレス %s はこのPC以外からも接続できます。Bearer認証を有効にしてトークンを設定するまで待受を開始できません", settings.ListenAddress)
	}
	return nil
}

// defaultRouteIP returns the local address the OS would use to reach the
// internet, i.e. the IP of the interface that holds the default route. UDP
// "connect" only consults the routing table; no packet is sent.
func defaultRouteIP() string {
	for _, target := range []string{"192.0.2.1:9", "[2001:db8::1]:9"} {
		conn, err := net.DialTimeout("udp", target, 200*time.Millisecond)
		if err != nil {
			continue
		}
		addr, ok := conn.LocalAddr().(*net.UDPAddr)
		_ = conn.Close()
		if ok && addr.IP != nil && !addr.IP.IsLoopback() && !addr.IP.IsUnspecified() {
			return addr.IP.String()
		}
	}
	return ""
}

func fillConnectURLs(settings *MCPSettings) {
	path := settings.MCPPath
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	port := strconv.Itoa(settings.Port)
	listen := settings.ListenAddress
	localHost := "127.0.0.1"
	if ip := net.ParseIP(listen); ip != nil && ip.To4() == nil && ip.IsLoopback() {
		localHost = "::1"
	}
	if settings.BaseURL != "" {
		settings.LocalURL = settings.BaseURL + path
	} else {
		settings.LocalURL = "http://" + net.JoinHostPort(localHost, port) + path
	}
	settings.LANAddress = defaultRouteIP()
	settings.LANURL = ""
	settings.LANReachable = false
	if settings.LANAddress == "" {
		return
	}
	settings.LANURL = "http://" + net.JoinHostPort(settings.LANAddress, port) + path
	ip := net.ParseIP(listen)
	settings.LANReachable = settings.Enabled && (listen == "0.0.0.0" || listen == "::" || (ip != nil && ip.String() == settings.LANAddress))
}

func (a *App) startMCPServer() error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	return a.startMCPServerLocked()
}

func (a *App) startMCPServerLocked() error {
	a.mu.RLock()
	settings := a.mcp
	token := a.effectiveTokenLocked()
	a.mu.RUnlock()

	if !settings.Enabled {
		a.setMCPStatus("停止中", "")
		return nil
	}
	if err := validateListenSettings(settings, token); err != nil {
		return err
	}
	addr := net.JoinHostPort(settings.ListenAddress, strconv.Itoa(settings.Port))

	stop := newMCPListenerStop()
	srv := &http.Server{
		Addr:              addr,
		Handler:           stop.wrap(a.newMCPHandler(settings, token)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       time.Duration(settings.RequestTimeout) * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	a.mcpServer = srv
	a.mcpServerStop = stop
	a.setMCPStatus("起動中", "")
	go func() {
		log.Printf("%s MCP server listening on http://%s%s", appName, addr, settings.MCPPath)
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.lifecycleMu.Lock()
			current := a.mcpServer == srv
			a.lifecycleMu.Unlock()
			if current {
				a.setMCPStatus("エラー", err.Error())
			}
			log.Printf("mcp server stopped: %v", err)
		}
	}()
	return nil
}

// newMCPHandler builds the listener's routes behind guardHTTP.
func (a *App) newMCPHandler(settings MCPSettings, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(settings.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": appName})
	})
	mux.HandleFunc(settings.MCPPath, func(w http.ResponseWriter, r *http.Request) {
		a.handleMCP(w, r, settings, token)
	})
	mux.HandleFunc(settings.AuditPath, func(w http.ResponseWriter, r *http.Request) {
		a.handleAudit(w, r, settings, token)
	})
	return guardHTTP(settings, mux)
}

func (a *App) stopMCPServer(ctx context.Context) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.stopMCPServerLocked(ctx)
}

func (a *App) stopMCPServerLocked(ctx context.Context) {
	if a.mcpServer == nil {
		return
	}
	// Tell handlers this is a server-side stop before Shutdown/Close cancel
	// their contexts, so synchronous runs detach instead of being cancelled.
	if a.mcpServerStop != nil {
		a.mcpServerStop.signal()
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.mcpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown mcp server: %v", err)
		_ = a.mcpServer.Close()
	}
	a.mcpServer = nil
	a.mcpServerStop = nil
}

func (a *App) restartMCPServer() error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.stopMCPServerLocked(context.Background())
	return a.startMCPServerLocked()
}
