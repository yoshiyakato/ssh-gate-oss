package main

// Web event layer: GET /api/events as Server-Sent Events (phase6-headless §4).
//
// Every App event reaches publish through platformEmit. publish never
// blocks: it frames the event once, appends it to a replay ring and offers
// it to each client's bounded queue. A client whose queue overflows, or whose
// write stalls, is disconnected; it comes back with Last-Event-ID and is
// replayed from the ring, or gets a headless:resync summary when the ring no
// longer reaches back that far.
//
// Lock order: h.mu is a leaf lock. publish is called with a.sessionsMu held
// (notifySessionsChanged) and from terminal output goroutines, so nothing
// under h.mu takes another lock or calls into App. The sessions snapshot for
// a resync is taken in ServeHTTP after h.mu is released.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	headlessResyncEvent   = "headless:resync"
	headlessShutdownEvent = "headless:shutdown"
)

// hubConfig holds the §4.2 limits. Zero fields take defaultHubConfig values.
type hubConfig struct {
	ClientQueue  int // events queued per client before it is cut
	ClientBytes  int // bytes queued per client before it is cut
	RingEvents   int // replay ring length
	RingBytes    int // replay ring size
	PerSession   int // streams per admin session; the oldest is closed beyond it
	Total        int // streams overall; the oldest is closed beyond it
	WriteTimeout time.Duration
	Ping         time.Duration
	ExitKeep     time.Duration // how long terminal:exit payloads stay in resync
	ExitMax      int           // how many terminal:exit payloads stay in resync

	// SessionID returns the admin session that owns a request ("" if none).
	// The integrator wires it to web_auth's sessionFrom; the hub itself does
	// not authenticate (requireSession runs in front of it).
	SessionID func(r *http.Request) string
	// RequireSession makes a stream request without a session answer 401
	// (fail closed if the route is ever mounted without requireSession).
	RequireSession bool
}

func defaultHubConfig() hubConfig {
	return hubConfig{
		ClientQueue:  1024,
		ClientBytes:  4 << 20,
		RingEvents:   4096,
		RingBytes:    8 << 20,
		PerSession:   8,
		Total:        64,
		WriteTimeout: 10 * time.Second,
		Ping:         15 * time.Second,
		ExitKeep:     10 * time.Minute,
		ExitMax:      256,
	}
}

func (c hubConfig) withDefaults() hubConfig {
	d := defaultHubConfig()
	if c.ClientQueue <= 0 {
		c.ClientQueue = d.ClientQueue
	}
	if c.ClientBytes <= 0 {
		c.ClientBytes = d.ClientBytes
	}
	if c.RingEvents <= 0 {
		c.RingEvents = d.RingEvents
	}
	if c.RingBytes <= 0 {
		c.RingBytes = d.RingBytes
	}
	if c.PerSession <= 0 {
		c.PerSession = d.PerSession
	}
	if c.Total <= 0 {
		c.Total = d.Total
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = d.WriteTimeout
	}
	if c.Ping <= 0 {
		c.Ping = d.Ping
	}
	if c.ExitKeep <= 0 {
		c.ExitKeep = d.ExitKeep
	}
	if c.ExitMax <= 0 {
		c.ExitMax = d.ExitMax
	}
	return c
}

// resyncPayload is the body of headless:resync (§4.3 step 3). The shim turns
// it into synthetic terminal:data / terminal:exit / sessions:changed events.
type resyncPayload struct {
	Reason   string            `json:"reason"` // initial | epoch | gap
	Sessions *sessionsPayload  `json:"sessions,omitempty"`
	Offsets  map[string]int64  `json:"offsets"`
	Exits    []json.RawMessage `json:"exits"`
}

// hubEvent is one framed event kept in the replay ring.
type hubEvent struct {
	seq   uint64
	frame []byte
}

type hubExit struct {
	id   string
	at   time.Time
	data json.RawMessage
}

// sseClient is one open stream. Its queue is fed by publish under h.mu and
// drained by the ServeHTTP goroutine that owns the ResponseWriter.
type sseClient struct {
	session string
	ch      chan []byte
	bytes   int // queued bytes; guarded by h.mu
	done    chan struct{}
	reason  string // why done was closed; guarded by h.mu
	closed  bool   // guarded by h.mu
}

type eventHub struct {
	cfg      hubConfig
	snapshot func() sessionsPayload
	now      func() time.Time
	epoch    string

	mu        sync.Mutex
	seq       uint64
	ring      []hubEvent
	ringBytes int
	clients   []*sseClient // oldest first
	offsets   map[string]int64
	exits     []hubExit
	stopped   bool
	shutFrame []byte
}

// newEventHub builds the hub. snapshot (normally app.sessionsSnapshot) is
// used for headless:resync and may be nil.
func newEventHub(cfg hubConfig, snapshot func() sessionsPayload) *eventHub {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return &eventHub{
		cfg:      cfg.withDefaults(),
		snapshot: snapshot,
		now:      time.Now,
		epoch:    hex.EncodeToString(b[:]),
		offsets:  map[string]int64{},
	}
}

func (h *eventHub) eventID(seq uint64) string {
	return h.epoch + "-" + strconv.FormatUint(seq, 10)
}

type wireEvent struct {
	N string          `json:"n"`
	D json.RawMessage `json:"d"`
	O *int64          `json:"o,omitempty"`
}

func frameEvent(id, name string, data json.RawMessage, offset *int64) []byte {
	body, err := json.Marshal(wireEvent{N: name, D: data, O: offset})
	if err != nil { // RawMessage was produced by json.Marshal; cannot happen
		body = []byte(`{"n":` + strconv.Quote(name) + `,"d":null}`)
	}
	// json.Marshal escapes newlines, so the event is a single data line.
	frame := make([]byte, 0, len(id)+len(body)+16)
	frame = append(frame, "id: "...)
	frame = append(frame, id...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, body...)
	frame = append(frame, "\n\n"...)
	return frame
}

// base64Len is the decoded length of standard base64 without decoding it.
func base64Len(s string) int64 {
	n := int64(len(s)) / 4 * 3
	if strings.HasSuffix(s, "==") {
		n -= 2
	} else if strings.HasSuffix(s, "=") {
		n--
	}
	if n < 0 {
		return 0
	}
	return n
}

// publish implements eventSink. It never blocks and never calls into App.
func (h *eventHub) publish(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("web events: drop %s: %v", name, err)
		return
	}
	var termID string
	var add int64
	var exitID string
	switch p := payload.(type) {
	case terminalDataPayload:
		termID, add = p.ID, base64Len(p.Data)+int64(p.Dropped)
	case *terminalDataPayload:
		termID, add = p.ID, base64Len(p.Data)+int64(p.Dropped)
	case terminalExitPayload:
		exitID = p.ID
	case *terminalExitPayload:
		exitID = p.ID
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.seq++
	var offset *int64
	if name == terminalDataEvent && termID != "" {
		o := h.offsets[termID] + add
		h.offsets[termID] = o
		offset = &o
	}
	if name == terminalExitEvent && exitID != "" {
		delete(h.offsets, exitID)
		h.recordExitLocked(exitID, data)
	}
	frame := frameEvent(h.eventID(h.seq), name, data, offset)

	h.ring = append(h.ring, hubEvent{seq: h.seq, frame: frame})
	h.ringBytes += len(frame)
	drop := 0
	for len(h.ring)-drop > h.cfg.RingEvents || (h.ringBytes > h.cfg.RingBytes && len(h.ring)-drop > 1) {
		h.ringBytes -= len(h.ring[drop].frame)
		h.ring[drop] = hubEvent{}
		drop++
	}
	if drop > 0 {
		h.ring = h.ring[drop:]
	}

	for _, c := range h.clients {
		if c.closed {
			continue
		}
		if c.bytes+len(frame) > h.cfg.ClientBytes {
			h.kickLocked(c, "slow")
			continue
		}
		select {
		case c.ch <- frame:
			c.bytes += len(frame)
		default:
			h.kickLocked(c, "slow")
		}
	}
	h.compactLocked()
}

func (h *eventHub) recordExitLocked(id string, data json.RawMessage) {
	now := h.now()
	h.exits = append(h.exits, hubExit{id: id, at: now, data: data})
	h.pruneExitsLocked(now)
}

func (h *eventHub) pruneExitsLocked(now time.Time) {
	start := 0
	for start < len(h.exits) && (now.Sub(h.exits[start].at) > h.cfg.ExitKeep || len(h.exits)-start > h.cfg.ExitMax) {
		start++
	}
	if start > 0 {
		h.exits = append([]hubExit(nil), h.exits[start:]...)
	}
}

// kickLocked closes a client's stream. The owning ServeHTTP goroutine sees
// done and returns; queued frames are discarded.
func (h *eventHub) kickLocked(c *sseClient, reason string) {
	if c.closed {
		return
	}
	c.closed = true
	c.reason = reason
	close(c.done)
}

// compactLocked drops closed clients from the list.
func (h *eventHub) compactLocked() {
	n := 0
	for _, c := range h.clients {
		if !c.closed {
			h.clients[n] = c
			n++
		}
	}
	for i := n; i < len(h.clients); i++ {
		h.clients[i] = nil
	}
	h.clients = h.clients[:n]
}

// parseEventID splits "<epoch>-<seq>".
func parseEventID(s string) (epoch string, seq uint64, ok bool) {
	s = strings.TrimSpace(s)
	i := strings.LastIndexByte(s, '-')
	if i <= 0 {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(s[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return s[:i], seq, true
}

// attach registers a client. It returns the frames to replay, or
// resync=true with the reason and the offsets/exits as of registration, so
// that every event published afterwards lands in the client's queue.
func (h *eventHub) attach(session, lastID string) (c *sseClient, replay [][]byte, resync *resyncPayload, startID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil, nil, nil, "", errHubStopped
	}
	c = &sseClient{session: session, ch: make(chan []byte, h.cfg.ClientQueue), done: make(chan struct{})}

	reason := "initial"
	if lastID != "" {
		reason = "epoch"
		if epoch, seq, ok := parseEventID(lastID); ok && epoch == h.epoch {
			reason = "gap"
			switch {
			case seq == h.seq:
				replay = [][]byte{}
			case seq < h.seq && len(h.ring) > 0 && h.ring[0].seq <= seq+1:
				i := int(seq + 1 - h.ring[0].seq)
				for _, e := range h.ring[i:] {
					replay = append(replay, e.frame)
				}
			}
		}
	}
	if replay == nil {
		h.pruneExitsLocked(h.now())
		resync = &resyncPayload{Reason: reason, Offsets: make(map[string]int64, len(h.offsets)), Exits: make([]json.RawMessage, 0, len(h.exits))}
		for id, o := range h.offsets {
			resync.Offsets[id] = o
		}
		for _, e := range h.exits {
			resync.Exits = append(resync.Exits, e.data)
		}
		startID = h.eventID(h.seq)
	}

	// Enforce the stream limits by closing the oldest streams.
	if session != "" {
		same := 0
		for _, o := range h.clients {
			if !o.closed && o.session == session {
				same++
			}
		}
		for _, o := range h.clients {
			if same < h.cfg.PerSession {
				break
			}
			if !o.closed && o.session == session {
				h.kickLocked(o, "limit")
				same--
			}
		}
	}
	h.compactLocked()
	for i := 0; len(h.clients)-i >= h.cfg.Total; i++ {
		h.kickLocked(h.clients[i], "limit")
	}
	h.compactLocked()
	h.clients = append(h.clients, c)
	return c, replay, resync, startID, nil
}

func (h *eventHub) detach(c *sseClient) {
	h.mu.Lock()
	h.kickLocked(c, "gone")
	h.compactLocked()
	h.mu.Unlock()
}

var errHubStopped = errors.New("event hub stopped")

// ServeHTTP serves GET /api/events. Authentication and the CSRF/Origin
// checks run in front of it (web_auth / web_guard).
func (h *eventHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session := ""
	if h.cfg.SessionID != nil {
		session = h.cfg.SessionID(r)
	}
	if h.cfg.RequireSession && session == "" {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	c, replay, resync, startID, err := h.attach(session, r.Header.Get("Last-Event-ID"))
	if err != nil {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer h.detach(c)

	// Taken outside h.mu: the snapshot locks a.sessionsMu, which publish may
	// already hold (notifySessionsChanged).
	if resync != nil && h.snapshot != nil {
		s := h.snapshot()
		resync.Sessions = &s
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	write := func(b []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		return true
	}

	if resync != nil {
		data, _ := json.Marshal(resync)
		if !write(frameEvent(startID, headlessResyncEvent, data, nil)) {
			return
		}
	} else if len(replay) == 0 {
		if !write([]byte(": resumed\n\n")) {
			return
		}
	}
	for _, f := range replay {
		if !write(f) {
			return
		}
	}

	ping := time.NewTicker(h.cfg.Ping)
	defer ping.Stop()
	for {
		select {
		case f := <-c.ch:
			h.mu.Lock()
			c.bytes -= len(f)
			h.mu.Unlock()
			if !write(f) {
				return
			}
		case <-ping.C:
			if !write([]byte(": ping\n\n")) {
				return
			}
		case <-c.done:
			h.mu.Lock()
			reason, final := c.reason, h.shutFrame
			h.mu.Unlock()
			if reason == "shutdown" && final != nil {
				write(final)
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}

// closeSession closes every stream owned by an admin session (logout,
// expiry, token rotation). Wire it with adminAuth.onInvalidate.
func (h *eventHub) closeSession(sessionID string) {
	if sessionID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if c.session == sessionID {
			h.kickLocked(c, "session")
		}
	}
	h.compactLocked()
}

// shutdown sends headless:shutdown to every open stream, closes them and
// refuses new ones. publish becomes a no-op. Safe to call more than once.
func (h *eventHub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.stopped = true
	h.shutFrame = frameEvent(h.eventID(h.seq), headlessShutdownEvent, json.RawMessage(`{}`), nil)
	for _, c := range h.clients {
		h.kickLocked(c, "shutdown")
	}
	h.compactLocked()
}
