package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type sseEvent struct {
	ID   string
	Name string
	D    json.RawMessage
	O    *int64
}

type sseReader struct {
	t    *testing.T
	resp *http.Response
	br   *bufio.Reader
	ch   chan sseEvent
	errc chan error
}

// openSSE connects to the hub's test server and parses events in the
// background so reads can time out.
func openSSE(t *testing.T, url, lastID string) *sseReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	r := &sseReader{t: t, resp: resp, br: bufio.NewReader(resp.Body), ch: make(chan sseEvent, 1024), errc: make(chan error, 1)}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		return r
	}
	go func() {
		var id, data string
		for {
			line, err := r.br.ReadString('\n')
			if err != nil {
				r.errc <- err
				close(r.ch)
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if data != "" {
					var w wireEvent
					if err := json.Unmarshal([]byte(data), &w); err != nil {
						r.errc <- err
						close(r.ch)
						return
					}
					r.ch <- sseEvent{ID: id, Name: w.N, D: w.D, O: w.O}
				}
				id, data = "", ""
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "id: "):
				id = line[4:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			}
		}
	}()
	return r
}

func (r *sseReader) next() sseEvent {
	r.t.Helper()
	select {
	case e, ok := <-r.ch:
		if !ok {
			r.t.Fatalf("stream ended: %v", <-r.errc)
		}
		return e
	case <-time.After(3 * time.Second):
		r.t.Fatal("timed out waiting for an event")
	}
	return sseEvent{}
}

// waitClosed waits for the server to end the stream.
func (r *sseReader) waitClosed() []sseEvent {
	r.t.Helper()
	var got []sseEvent
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e, ok := <-r.ch:
			if !ok {
				return got
			}
			got = append(got, e)
		case <-deadline:
			r.t.Fatal("stream was not closed")
		}
	}
}

func (r *sseReader) noEvent(d time.Duration) {
	r.t.Helper()
	select {
	case e, ok := <-r.ch:
		if ok {
			r.t.Fatalf("unexpected event %+v", e)
		}
	case <-time.After(d):
	}
}

func newTestHub(t *testing.T, cfg hubConfig, snap func() sessionsPayload) (*eventHub, *httptest.Server) {
	t.Helper()
	h := newEventHub(cfg, snap)
	srv := httptest.NewServer(h)
	t.Cleanup(func() { h.shutdown(); srv.Close() })
	return h, srv
}

func waitClients(t *testing.T, h *eventHub, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		got := len(h.clients)
		h.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("want %d clients", n)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func decodeResync(t *testing.T, e sseEvent) resyncPayload {
	t.Helper()
	if e.Name != headlessResyncEvent {
		t.Fatalf("want %s, got %s", headlessResyncEvent, e.Name)
	}
	var p resyncPayload
	if err := json.Unmarshal(e.D, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlatformEmitRoutesToHub(t *testing.T) {
	h := newEventHub(hubConfig{}, nil)
	a := &App{ctx: withWebHub(context.Background(), h)}
	a.emitEvent(sessionsChangedEvent, sessionsPayload{SessionsSeq: 7})
	if h.seq != 1 || len(h.ring) != 1 || !strings.Contains(string(h.ring[0].frame), `"sessionsSeq":7`) {
		t.Fatalf("event did not reach the hub: seq=%d ring=%d", h.seq, len(h.ring))
	}
	if webHubFrom(context.Background()) != nil || webHubFrom(nil) != nil { //nolint:staticcheck
		t.Fatal("plain ctx must not carry a hub")
	}
	// The test hook still wins over the hub.
	var hooked string
	a.emit = func(name string, _ any) { hooked = name }
	a.emitEvent(terminalExitEvent, terminalExitPayload{ID: "T"})
	if hooked != terminalExitEvent || h.seq != 1 {
		t.Fatal("a.emit must take precedence")
	}
}

func TestBase64Len(t *testing.T) {
	for _, s := range []string{"", "a", "ab", "abc", "abcd", strings.Repeat("x", 1000)} {
		if got := base64Len(b64(s)); got != int64(len(s)) {
			t.Fatalf("%q: got %d", s, got)
		}
	}
}

func TestParseEventID(t *testing.T) {
	if e, s, ok := parseEventID("abc123-42"); !ok || e != "abc123" || s != 42 {
		t.Fatal(e, s, ok)
	}
	for _, bad := range []string{"", "-1", "abc", "abc-x", "abc-"} {
		if _, _, ok := parseEventID(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestEventsInitialResyncAndLive(t *testing.T) {
	h, srv := newTestHub(t, hubConfig{}, func() sessionsPayload { return sessionsPayload{SessionsSeq: 99} })
	h.publish(terminalDataEvent, terminalDataPayload{ID: "T1", Data: b64("hello"), Dropped: 3})
	h.publish(terminalDataEvent, terminalDataPayload{ID: "T2", Data: b64("x")})
	h.publish(terminalExitEvent, terminalExitPayload{ID: "T2", Reason: "user"})

	r := openSSE(t, srv.URL, "")
	if ct := r.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	if r.resp.Header.Get("Cache-Control") != "no-store" || r.resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("missing SSE headers")
	}
	first := r.next()
	p := decodeResync(t, first)
	if p.Reason != "initial" || p.Sessions == nil || p.Sessions.SessionsSeq != 99 {
		t.Fatalf("resync %+v", p)
	}
	if len(p.Offsets) != 1 || p.Offsets["T1"] != 8 {
		t.Fatalf("offsets %v (exited terminals must be removed)", p.Offsets)
	}
	if len(p.Exits) != 1 || !strings.Contains(string(p.Exits[0]), `"T2"`) {
		t.Fatalf("exits %s", p.Exits)
	}
	if first.ID != h.eventID(3) {
		t.Fatalf("resync id %q", first.ID)
	}

	h.publish(terminalDataEvent, terminalDataPayload{ID: "T1", Data: b64("abc")})
	e := r.next()
	if e.Name != terminalDataEvent || e.O == nil || *e.O != 11 || e.ID != h.eventID(4) {
		t.Fatalf("live event %+v", e)
	}
	h.publish(sessionsChangedEvent, sessionsPayload{SessionsSeq: 100})
	if e := r.next(); e.Name != sessionsChangedEvent || e.O != nil {
		t.Fatalf("sessions event %+v", e)
	}
	// Unknown event names pass through unchanged.
	h.publish("future:event", map[string]int{"x": 1})
	if e := r.next(); e.Name != "future:event" || string(e.D) != `{"x":1}` {
		t.Fatalf("future event %+v", e)
	}
}

func TestEventsReplayFromLastEventID(t *testing.T) {
	snaps := 0
	h, srv := newTestHub(t, hubConfig{}, func() sessionsPayload { snaps++; return sessionsPayload{} })
	for i := 0; i < 5; i++ {
		h.publish(terminalDataEvent, terminalDataPayload{ID: "T", Data: b64("ab")})
	}
	r := openSSE(t, srv.URL, h.eventID(2))
	for want := uint64(3); want <= 5; want++ {
		e := r.next()
		if e.ID != h.eventID(want) || e.Name != terminalDataEvent || *e.O != int64(want*2) {
			t.Fatalf("replay %d: %+v", want, e)
		}
	}
	h.publish(sessionsChangedEvent, sessionsPayload{})
	if e := r.next(); e.ID != h.eventID(6) {
		t.Fatalf("live after replay %+v", e)
	}
	if snaps != 0 {
		t.Fatal("replay must not take a snapshot")
	}

	// Up to date: nothing to replay, no resync.
	r2 := openSSE(t, srv.URL, h.eventID(6))
	h.publish(sessionsChangedEvent, sessionsPayload{})
	if e := r2.next(); e.ID != h.eventID(7) {
		t.Fatalf("up-to-date reconnect %+v", e)
	}
}

func TestEventsResyncWhenReplayImpossible(t *testing.T) {
	h, srv := newTestHub(t, hubConfig{RingEvents: 3}, nil)
	for i := 0; i < 10; i++ {
		h.publish(terminalDataEvent, terminalDataPayload{ID: "T", Data: b64("a")})
	}
	if len(h.ring) != 3 || h.ring[0].seq != 8 {
		t.Fatalf("ring len=%d first=%d", len(h.ring), h.ring[0].seq)
	}
	cases := []struct{ last, reason string }{
		{h.eventID(6), "gap"},     // older than the ring
		{h.eventID(7), "replay"},  // ring starts at 8: exactly reachable
		{h.eventID(11), "gap"},    // from the future
		{"otherepoch-7", "epoch"}, // server restarted
		{"garbage", "epoch"},      // unparsable
	}
	for _, c := range cases {
		r := openSSE(t, srv.URL, c.last)
		e := r.next()
		if c.reason == "replay" {
			if e.ID != h.eventID(8) {
				t.Fatalf("%s: want replay from 8, got %+v", c.last, e)
			}
			continue
		}
		p := decodeResync(t, e)
		if p.Reason != c.reason || p.Offsets["T"] != 10 || p.Sessions != nil {
			t.Fatalf("%s: %+v", c.last, p)
		}
	}
}

func TestEventsRingByteBound(t *testing.T) {
	h := newEventHub(hubConfig{RingBytes: 1000}, nil)
	for i := 0; i < 50; i++ {
		h.publish(terminalDataEvent, terminalDataPayload{ID: "T", Data: b64(strings.Repeat("z", 100))})
	}
	if h.ringBytes > 1000 || len(h.ring) == 0 {
		t.Fatalf("ringBytes=%d len=%d", h.ringBytes, len(h.ring))
	}
	sum := 0
	for _, e := range h.ring {
		sum += len(e.frame)
	}
	if sum != h.ringBytes {
		t.Fatalf("accounting %d != %d", sum, h.ringBytes)
	}
	// A single event larger than the ring is still kept (the newest one).
	h.publish(terminalDataEvent, terminalDataPayload{ID: "T", Data: b64(strings.Repeat("z", 5000))})
	if len(h.ring) != 1 || h.ring[0].seq != h.seq {
		t.Fatalf("oversized event: len=%d", len(h.ring))
	}
}

func TestEventsSlowClientIsCut(t *testing.T) {
	h := newEventHub(hubConfig{ClientQueue: 4}, nil)
	slow, _, _, _, err := h.attach("", "")
	if err != nil {
		t.Fatal(err)
	}
	fast, _, _, _, _ := h.attach("", "")
	for i := 0; i < 4; i++ {
		h.publish(sessionsChangedEvent, sessionsPayload{})
		<-fast.ch // fast drains
		h.mu.Lock()
		fast.bytes = 0
		h.mu.Unlock()
	}
	select {
	case <-slow.done:
		t.Fatal("cut too early")
	default:
	}
	h.publish(sessionsChangedEvent, sessionsPayload{})
	select {
	case <-slow.done:
	default:
		t.Fatal("slow client was not cut on queue overflow")
	}
	if slow.reason != "slow" {
		t.Fatalf("reason %q", slow.reason)
	}
	select {
	case <-fast.done:
		t.Fatal("fast client must not be affected")
	default:
	}
	if len(h.clients) != 1 || h.clients[0] != fast {
		t.Fatal("slow client not removed")
	}
}

func TestEventsSlowClientCutByBytes(t *testing.T) {
	h := newEventHub(hubConfig{ClientBytes: 500}, nil)
	c, _, _, _, _ := h.attach("", "")
	h.publish(terminalDataEvent, terminalDataPayload{ID: "T", Data: b64(strings.Repeat("q", 200))})
	select {
	case <-c.done:
		t.Fatal("cut too early")
	default:
	}
	h.publish(terminalDataEvent, terminalDataPayload{ID: "T", Data: b64(strings.Repeat("q", 200))})
	select {
	case <-c.done:
	default:
		t.Fatal("client over the byte budget was not cut")
	}
}

func TestEventsSlowClientStreamEndsAndResumes(t *testing.T) {
	h, srv := newTestHub(t, hubConfig{ClientQueue: 2}, nil)
	r := openSSE(t, srv.URL, "")
	decodeResync(t, r.next())
	waitClients(t, h, 1)
	// Fill the queue under the lock so the writer cannot drain in between.
	h.mu.Lock()
	c := h.clients[0]
	h.mu.Unlock()
	h.mu.Lock()
	for len(c.ch) < cap(c.ch) {
		c.ch <- []byte(": filler\n\n")
		c.bytes += 10
	}
	h.kickLocked(c, "slow") // what publish does on overflow
	h.compactLocked()
	h.mu.Unlock()
	r.waitClosed()

	h.publish(sessionsChangedEvent, sessionsPayload{SessionsSeq: 5})
	r2 := openSSE(t, srv.URL, h.eventID(0))
	if e := r2.next(); e.ID != h.eventID(1) || e.Name != sessionsChangedEvent {
		t.Fatalf("resume after cut %+v", e)
	}
}

func TestEventsStreamLimits(t *testing.T) {
	h := newEventHub(hubConfig{PerSession: 2, Total: 3}, nil)
	a1, _, _, _, _ := h.attach("A", "")
	a2, _, _, _, _ := h.attach("A", "")
	a3, _, _, _, _ := h.attach("A", "")
	if !a1.closed || a2.closed || a3.closed {
		t.Fatal("per-session limit must close the oldest stream of that session")
	}
	b1, _, _, _, _ := h.attach("B", "")
	b2, _, _, _, _ := h.attach("B", "")
	if !a2.closed || a3.closed || b1.closed || b2.closed {
		t.Fatalf("total limit must close the oldest stream overall: a2=%v a3=%v", a2.closed, a3.closed)
	}
	if len(h.clients) != 3 {
		t.Fatalf("clients %d", len(h.clients))
	}
	h.closeSession("B")
	if !b1.closed || !b2.closed || a3.closed || b1.reason != "session" {
		t.Fatal("closeSession must close only that session's streams")
	}
	h.closeSession("") // no-op
	if a3.closed {
		t.Fatal("empty session id must not close anything")
	}
}

func TestEventsSessionIDHookAndCloseSession(t *testing.T) {
	h, srv := newTestHub(t, hubConfig{SessionID: func(r *http.Request) string { return r.URL.Query().Get("s") }}, nil)
	ra := openSSE(t, srv.URL+"?s=A", "")
	rb := openSSE(t, srv.URL+"?s=B", "")
	ra.next()
	rb.next()
	waitClients(t, h, 2)
	h.closeSession("A")
	ra.waitClosed()
	h.publish(sessionsChangedEvent, sessionsPayload{})
	if e := rb.next(); e.Name != sessionsChangedEvent {
		t.Fatal(e)
	}
}

func TestEventsShutdown(t *testing.T) {
	h, srv := newTestHub(t, hubConfig{}, nil)
	r := openSSE(t, srv.URL, "")
	r.next()
	waitClients(t, h, 1)
	h.shutdown()
	h.shutdown() // idempotent
	got := r.waitClosed()
	if len(got) != 1 || got[0].Name != headlessShutdownEvent {
		t.Fatalf("want a final %s, got %+v", headlessShutdownEvent, got)
	}
	h.publish(sessionsChangedEvent, sessionsPayload{}) // no-op, no panic
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after shutdown: %d", resp.StatusCode)
	}
}

func TestEventsMethodNotAllowed(t *testing.T) {
	_, srv := newTestHub(t, hubConfig{}, nil)
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestEventsPing(t *testing.T) {
	_, srv := newTestHub(t, hubConfig{Ping: 20 * time.Millisecond}, nil)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == ": ping\n" {
			return
		}
	}
	t.Fatal("no ping")
}

func TestEventsExitRetention(t *testing.T) {
	now := time.Unix(1000, 0)
	h := newEventHub(hubConfig{ExitMax: 3, ExitKeep: time.Minute}, nil)
	h.now = func() time.Time { return now }
	for _, id := range []string{"a", "b", "c", "d"} {
		h.publish(terminalExitEvent, &terminalExitPayload{ID: id})
	}
	if len(h.exits) != 3 || h.exits[0].id != "b" {
		t.Fatalf("max: %+v", h.exits)
	}
	now = now.Add(2 * time.Minute)
	_, _, rs, _, _ := h.attach("", "")
	if len(rs.Exits) != 0 {
		t.Fatalf("expired exits kept: %s", rs.Exits)
	}
}

// publish runs under a.sessionsMu (notifySessionsChanged) while ServeHTTP's
// resync snapshot takes the same lock: the hub must never hold h.mu while
// snapshotting, or this deadlocks. Run with -race.
func TestEventsNoDeadlockWithSessionsLock(t *testing.T) {
	var sessionsMu sync.Mutex
	// The publishers never pause, so with the default queue a reader can be
	// cut as "slow" (by design) before it reads its events; this test is
	// about deadlocks, so give the readers room.
	h, srv := newTestHub(t, hubConfig{ClientQueue: 1 << 20, ClientBytes: 1 << 30}, func() sessionsPayload {
		sessionsMu.Lock()
		defer sessionsMu.Unlock()
		return sessionsPayload{}
	})
	done := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				sessionsMu.Lock()
				h.publish(sessionsChangedEvent, sessionsPayload{})
				sessionsMu.Unlock()
				h.publish(terminalDataEvent, terminalDataPayload{ID: "T", Data: b64("x")})
			}
		}()
	}
	for i := 0; i < 20; i++ {
		r := openSSE(t, srv.URL, "")
		decodeResync(t, r.next())
		r.next()
	}
	close(done)
	wg.Wait()
}

// TestEventsRequireSessionFailClosed: with RequireSession (the headless
// wiring), a stream request without an admin session gets 401 and never
// attaches; with a session it streams.
func TestEventsRequireSessionFailClosed(t *testing.T) {
	h := newEventHub(hubConfig{SessionID: webSessionID, RequireSession: true}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Authorization", "Bearer sga_agent-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without session: %d", rec.Code)
	}
	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d clients attached without a session", n)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withAdminSession(r.Context(), adminSession{ID: "s1", Actor: "web:s1"})))
	}))
	defer srv.Close()
	defer h.shutdown() // ends the stream before srv.Close waits for it
	r := openSSE(t, srv.URL, "")
	if ev := r.next(); ev.Name != headlessResyncEvent {
		t.Fatalf("first event %q", ev.Name)
	}
}
