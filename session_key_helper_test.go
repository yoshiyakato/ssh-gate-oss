package main

import (
	"net/http"
	"sync"
)

// Since review finding 5, /api/* needs the per-login session key
// (adminSessionHeader) besides the cookie. These helpers do what
// login.js / headless-shim.js do in the browser.

var testSessionKeys sync.Map // cookie value -> session key

// rememberSessionKey stores the key a login response handed out.
func rememberSessionKey(h http.Header, cookies []*http.Cookie) {
	key := h.Get(adminSessionKeyHeader)
	if key == "" {
		return
	}
	for _, c := range cookies {
		if c.Value != "" {
			testSessionKeys.Store(c.Value, key)
		}
	}
}

// addAdminCookie adds the cookie and, if known, its session key header.
func addAdminCookie(r *http.Request, c *http.Cookie) {
	r.AddCookie(c)
	if k, ok := testSessionKeys.Load(c.Value); ok {
		r.Header.Set(adminSessionHeader, k.(string))
	}
}

// sessionKeyTransport keeps the latest session key from a login response
// and sends it on every later request, like the shim.
type sessionKeyTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	key  string
}

func (t *sessionKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	key := t.key
	t.mu.Unlock()
	if key != "" && r.Header.Get(adminSessionHeader) == "" {
		r = r.Clone(r.Context())
		r.Header.Set(adminSessionHeader, key)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	res, err := base.RoundTrip(r)
	if err == nil {
		if k := res.Header.Get(adminSessionKeyHeader); k != "" {
			t.mu.Lock()
			t.key = k
			t.mu.Unlock()
		}
	}
	return res, err
}

func (t *sessionKeyTransport) current() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.key
}

// withSessionKey wraps the client's transport with sessionKeyTransport.
func withSessionKey(c *http.Client) *http.Client {
	if _, ok := c.Transport.(*sessionKeyTransport); !ok {
		c.Transport = &sessionKeyTransport{base: c.Transport}
	}
	return c
}
