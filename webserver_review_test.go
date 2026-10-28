package main

// Regression tests for the REQ-015 adversarial review, findings 2 and 3.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newReviewWebServer(t *testing.T, mutate func(*webServerOptions)) *webServer {
	t.Helper()
	dist, admin := webTestFS()
	opts := webServerOptions{Listen: "127.0.0.1:8788", DataDir: t.TempDir(), Dist: dist, Admin: admin, Logf: t.Logf}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := newWebServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func reviewServe(h http.Handler, method, target, host, origin, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = host
	r.RemoteAddr = "127.0.0.1:5555"
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if strings.HasPrefix(target, "/api/") {
		r.Header.Set(webCSRFHeader, "1")
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Finding 2: a publicURL that names the scheme's default port works with
// what browsers send (Host and Origin without the port).
func TestWebPublicURLDefaultPortIsNormalized(t *testing.T) {
	for _, tc := range []struct{ publicURL, host, origin string }{
		{"https://gate.example.lan:443", "gate.example.lan", "https://gate.example.lan"},
		{"https://Gate.Example.LAN:443/", "gate.example.lan", "https://gate.example.lan"},
		{"http://127.0.0.1:80", "127.0.0.1", "http://127.0.0.1"},
	} {
		srv := newReviewWebServer(t, func(o *webServerOptions) { o.PublicURL = tc.publicURL })
		h := srv.handler()
		if w := reviewServe(h, "GET", "/login", tc.host, "", ""); w.Code != http.StatusOK {
			t.Fatalf("%s: GET /login with Host %s = %d %s", tc.publicURL, tc.host, w.Code, w.Body.String())
		}
		// A proxy that keeps the default port in Host is accepted too.
		if w := reviewServe(h, "GET", "/login", tc.host+":"+map[bool]string{true: "443", false: "80"}[strings.HasPrefix(tc.publicURL, "https")], "", ""); w.Code != http.StatusOK {
			t.Fatalf("%s: Host with the default port = %d", tc.publicURL, w.Code)
		}
		body := `{"token":"` + mustReadToken(t, srv) + `"}`
		if w := reviewServe(h, "POST", "/api/login", tc.host, tc.origin, body); w.Code != http.StatusNoContent {
			t.Fatalf("%s: login with Origin %s = %d %s", tc.publicURL, tc.origin, w.Code, w.Body.String())
		}
		if got := srv.URL(); got != tc.origin+"/" {
			t.Fatalf("%s: URL() = %q", tc.publicURL, got)
		}
		if origins := srv.origins(); len(origins) != 1 || origins[0] != tc.origin {
			t.Fatalf("%s: origins = %v", tc.publicURL, origins)
		}
	}
	// A non-default port is kept.
	srv := newReviewWebServer(t, func(o *webServerOptions) { o.PublicURL = "https://gate.example.lan:8443" })
	if origins := srv.origins(); len(origins) != 1 || origins[0] != "https://gate.example.lan:8443" {
		t.Fatalf("origins = %v", origins)
	}
	if w := reviewServe(srv.handler(), "GET", "/login", "gate.example.lan", "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("Host without the non-default port = %d", w.Code)
	}
}

func mustReadToken(t *testing.T, srv *webServer) string {
	t.Helper()
	tok, _, err := loadOrCreateAdminToken(srv.AdminTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Finding 3: HSTS is sent only for an https publicURL host that is a real
// name, never for localhost (it would cover every http://localhost:* site).
func TestWebHSTSOnlyForPublicHostName(t *testing.T) {
	selfSigned := func(o *webServerOptions) { o.TLSMode = webTLSModeSelfSigned }
	srv := newReviewWebServer(t, selfSigned)
	for _, host := range []string{"localhost:8788", "127.0.0.1:8788", "[::1]:8788"} {
		if w := reviewServe(srv.handler(), "GET", "/login", host, "", ""); w.Header().Get("Strict-Transport-Security") != "" {
			t.Fatalf("HSTS sent to %s on a loopback TLS listener", host)
		}
	}
	for _, pub := range []string{"https://localhost:8788", "https://127.0.0.1:8788", "https://app.localhost:8788"} {
		srv := newReviewWebServer(t, func(o *webServerOptions) { selfSigned(o); o.PublicURL = pub })
		host := strings.TrimPrefix(pub, "https://")
		if w := reviewServe(srv.handler(), "GET", "/login", host, "", ""); w.Header().Get("Strict-Transport-Security") != "" {
			t.Fatalf("HSTS sent for publicURL %s", pub)
		}
	}
	srv = newReviewWebServer(t, func(o *webServerOptions) { selfSigned(o); o.PublicURL = "https://gate.example.lan:8788" })
	h := srv.handler()
	if w := reviewServe(h, "GET", "/login", "gate.example.lan:8788", "", ""); w.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("HSTS missing for the public host: %d %q", w.Code, w.Header().Get("Strict-Transport-Security"))
	}
	if w := reviewServe(h, "GET", "/login", "127.0.0.1:8788", "", ""); w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS sent to a request that did not use the public host")
	}
	if webHSTSHost("http://127.0.0.1:8788") != "" || webHSTSHost("") != "" {
		t.Fatal("HSTS host for a plain or missing publicURL")
	}
}
