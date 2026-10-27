package main

// ssh_pool.go implements the optional stage-5 connection pool (design-v1
// §5.1 "第5段", §7 フェーズ5). It is self-contained: nothing in app.go,
// terminal.go, ssh_exec.go or mcp_server.go calls it yet.
//
// Ownership and locking (§5.0):
//   - The pool owns every *ssh.Client it holds. Only the pool closes them.
//     Callers get a PoolLease, which owns exactly one *ssh.Session and must be
//     released; Release closes the session, never the client.
//   - pool.mu guards entries and every pooledClient's counters. It is never
//     held while dialing, pinging, opening a session, closing a client or
//     calling OnChange.
//
// Retry rule: the pool retries only while obtaining a session (dial, liveness
// ping, NewSession). Once a lease is handed out the pool never touches the
// session again, so a command is never re-run. ExecOnce is the only helper
// that runs a command, and it calls Run exactly once.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// PoolOrigin says who opened a pooled client. Auto clients (opened for an
// agent) are reclaimed when idle; manual clients stay until disconnected.
type PoolOrigin string

const (
	PoolOriginManual PoolOrigin = "manual"
	PoolOriginAuto   PoolOrigin = "auto"
)

// PoolChannelKind is the use a session slot is reserved for.
type PoolChannelKind string

const (
	PoolChannelTerm PoolChannelKind = "term"
	PoolChannelExec PoolChannelKind = "exec"
)

// PoolDialFunc opens a new, authenticated client. It is called outside
// pool.mu with the pool's own context, never a caller's.
type PoolDialFunc func(ctx context.Context, conn Connection) (*ssh.Client, error)

var (
	ErrPoolClosed = errors.New("ssh pool: closed")
	// ErrPoolConnClosed is returned to callers waiting on a connection that a
	// human disconnected or deleted while they waited.
	ErrPoolConnClosed = errors.New("ssh pool: connection was disconnected")
)

const (
	defaultPoolKeepaliveInterval = 15 * time.Second
	defaultPoolKeepaliveTimeout  = 10 * time.Second
	defaultPoolKeepaliveMaxFail  = 2
	defaultPoolMaxChannels       = 8
	defaultPoolMaxExec           = 4
	defaultPoolMaxClients        = 2 // primary + one overflow
	defaultPoolAliveCheckAfter   = 45 * time.Second
	defaultPoolIdleTimeout       = 5 * time.Minute
	defaultPoolMaxEvictRetries   = 3
	defaultPoolMaxLonelyRefusal  = 5
	defaultPoolServerLimitTTL    = 30 * time.Second
)

// PoolOptions configures an SSHPool. Zero values take the defaults.
type PoolOptions struct {
	// Parent bounds the pool's own context (dials, keepalive). Default
	// context.Background(); pass appCtx.
	Parent context.Context
	// Dial opens a client. Default: openSSHClient(ctx, conn, StrictHostKey).
	Dial PoolDialFunc
	// StrictHostKey is passed to the default Dial and folded into the fp.
	StrictHostKey bool
	// Fingerprint computes a connection's fp. Default: poolFingerprint.
	Fingerprint func(conn Connection, strictHostKey bool) string

	KeepaliveInterval time.Duration // ping period; also the housekeeping tick
	KeepaliveTimeout  time.Duration // per-ping (and per-NewSession) timeout, 10s
	KeepaliveMaxFail  int           // consecutive failures before Close, 2
	MaxChannels       int           // session slots per client, 8
	MaxExec           int           // of which exec at most, 4
	MaxClients        int           // clients per connID (overflow), 2
	AliveCheckAfter   time.Duration // sync ping on lease if older, 45s
	AutoIdle          time.Duration // reclaim idle auto clients, 5m
	// ServerLimitTTL is how long a learned per-client channel cap (from a
	// server refusal) is trusted before the pool probes MaxChannels again.
	// A refusal can come from close lag, so the cap must not be permanent.
	// Default 30s.
	ServerLimitTTL time.Duration
	// SleepJump is how far the wall clock may run ahead of a housekeeping
	// tick before it is treated as a wake from sleep. Default
	// max(KeepaliveInterval, 5s).
	SleepJump time.Duration
	// Now is the clock. Default time.Now (monotonic reading stripped when
	// detecting sleep).
	Now func() time.Time
	// OnChange is called (outside pool.mu) after a connID's clients change:
	// dialed, evicted, drained, reclaimed or closed.
	OnChange func(connID string)
}

// SSHPool holds shared SSH clients keyed by connection ID.
type SSHPool struct {
	opts   PoolOptions
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	entries map[string]*poolEntry
}

type poolEntry struct {
	connID   string
	clients  []*pooledClient
	gen      uint64
	inflight *dialCall
	wake     chan struct{} // closed and replaced on every change
}

type dialCall struct {
	ctx    context.Context    // derived from pool.ctx; cancelled by Disconnect/Delete
	cancel context.CancelFunc // stops this dial only
	gen    uint64
	fp     string
	origin PoolOrigin
	done   chan struct{}
	err    error // read only after done; never cached past this call
}

type pooledClient struct {
	connID    string
	client    *ssh.Client
	fp        string
	hostKeyFP string // filled when the dial recorded one (RecordPoolHostKey)
	origin    PoolOrigin

	// Guarded by pool.mu.
	activeTerm    int
	activeExec    int
	serverLimit   int       // >0 once the server refused a channel: learned cap
	serverLimitAt time.Time // when serverLimit was learned; expires after ServerLimitTTL
	// suspect: a NewSession or ping timed out while other leases were open.
	// No new leases go here; keepalive clears it on success or closes the
	// client after KeepaliveMaxFail failures (§5.1: never close a busy
	// client on one failure).
	suspect     bool
	lastAliveAt time.Time
	lastUsedAt  time.Time
	stale       bool
	evicted     bool

	cancel    context.CancelFunc // stops keepalive
	closeOnce sync.Once
}

// PoolLease is one session borrowed from the pool.
type PoolLease struct {
	session *ssh.Session

	pool *SSHPool
	pc   *pooledClient
	kind PoolChannelKind
	once sync.Once
}

// PoolConnStat is a snapshot of one connID for the UI / MCP status.
type PoolConnStat struct {
	ConnID   string
	Clients  int
	Stale    int
	Term     int
	Exec     int
	Dialing  bool
	Origins  []PoolOrigin
	LastSeen time.Time
}

type poolHostKeyKey struct{}

type poolHostKeyRecorder struct {
	mu sync.Mutex
	fp string
}

// RecordPoolHostKey lets a Dial implementation report the server's host key
// from its HostKeyCallback. It is a no-op outside a pool dial.
func RecordPoolHostKey(ctx context.Context, key ssh.PublicKey) {
	r, _ := ctx.Value(poolHostKeyKey{}).(*poolHostKeyRecorder)
	if r == nil || key == nil {
		return
	}
	r.mu.Lock()
	r.fp = ssh.FingerprintSHA256(key)
	r.mu.Unlock()
}

// NewSSHPool starts a pool and its housekeeping goroutine. Dials run on a
// context derived from opts.Parent (pass appCtx), never on a caller's.
func NewSSHPool(opts PoolOptions) *SSHPool {
	parent := opts.Parent
	if parent == nil {
		parent = context.Background()
	}
	if opts.Dial == nil {
		strict := opts.StrictHostKey
		opts.Dial = func(ctx context.Context, conn Connection) (*ssh.Client, error) {
			return openSSHClient(ctx, conn, strict)
		}
	}
	if opts.Fingerprint == nil {
		opts.Fingerprint = poolFingerprint
	}
	if opts.KeepaliveInterval <= 0 {
		opts.KeepaliveInterval = defaultPoolKeepaliveInterval
	}
	if opts.KeepaliveTimeout <= 0 {
		opts.KeepaliveTimeout = defaultPoolKeepaliveTimeout
	}
	if opts.KeepaliveMaxFail <= 0 {
		opts.KeepaliveMaxFail = defaultPoolKeepaliveMaxFail
	}
	if opts.MaxChannels <= 0 {
		opts.MaxChannels = defaultPoolMaxChannels
	}
	if opts.MaxExec <= 0 {
		opts.MaxExec = defaultPoolMaxExec
	}
	if opts.MaxExec > opts.MaxChannels {
		opts.MaxExec = opts.MaxChannels
	}
	if opts.MaxClients <= 0 {
		opts.MaxClients = defaultPoolMaxClients
	}
	if opts.AliveCheckAfter <= 0 {
		opts.AliveCheckAfter = defaultPoolAliveCheckAfter
	}
	if opts.AutoIdle <= 0 {
		opts.AutoIdle = defaultPoolIdleTimeout
	}
	if opts.ServerLimitTTL <= 0 {
		opts.ServerLimitTTL = defaultPoolServerLimitTTL
	}
	if opts.SleepJump <= 0 {
		opts.SleepJump = opts.KeepaliveInterval
		if opts.SleepJump < 5*time.Second {
			opts.SleepJump = 5 * time.Second
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(parent)
	p := &SSHPool{opts: opts, ctx: ctx, cancel: cancel, entries: map[string]*poolEntry{}}
	p.wg.Add(1)
	go p.housekeeping()
	return p
}

// poolFingerprint hashes everything that, when changed, makes an existing
// client the wrong one: target, auth, credential, host key policy, the agent
// socket and the known_hosts / key file generation.
func poolFingerprint(conn Connection, strictHostKey bool) string {
	port := conn.Port
	if port == 0 {
		port = 22
	}
	h := sha256.New()
	field := func(s string) {
		_, _ = io.WriteString(h, strconv.Itoa(len(s)))
		_, _ = io.WriteString(h, ":")
		_, _ = io.WriteString(h, s)
	}
	field(conn.connectionType())
	field(conn.Host)
	field(strconv.Itoa(port))
	field(conn.User)
	field(conn.AuthMethod)
	field(conn.Credential)
	field(conn.KeyPassphrase)
	strict := strictHostKey || os.Getenv("SSH_GETE_STRICT_HOSTKEY") == "1"
	field(strconv.FormatBool(strict))
	field(os.Getenv("SSH_AUTH_SOCK"))
	if strict {
		// known_hosts only matters when host keys are checked; otherwise an
		// unrelated `ssh newhost` elsewhere would drain every pooled client.
		field(poolFileGeneration(expandPath("~/.ssh/known_hosts")))
	}
	if !conn.usesPassword() {
		keyPath := conn.Credential
		if keyPath == "" {
			keyPath = "~/.ssh/id_ed25519"
		}
		field(poolFileGeneration(expandPath(keyPath)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func poolFileGeneration(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return strconv.FormatInt(info.ModTime().UnixNano(), 10) + "/" + strconv.FormatInt(info.Size(), 10)
}

// ---- acquiring -------------------------------------------------------------

// Acquire returns a lease on a new session for conn. It reuses a live client
// with a free slot, waits for a slot, or dials (singleflight, at most
// MaxClients per connID). It retries only up to NewSession; the returned
// session has not been used. The caller must call Release.
func (p *SSHPool) Acquire(ctx context.Context, conn Connection, kind PoolChannelKind, origin PoolOrigin) (*PoolLease, error) {
	if kind != PoolChannelExec {
		kind = PoolChannelTerm
	}
	if origin != PoolOriginAuto {
		origin = PoolOriginManual
	}
	if conn.ID == "" {
		return nil, errors.New("ssh pool: connection has no id")
	}
	fp := p.opts.Fingerprint(conn, p.opts.StrictHostKey)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	e := p.entryLocked(conn.ID)
	startGen := e.gen
	p.mu.Unlock()

	evictRetries, lonelyRefusals := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}
		if p.entries[conn.ID] != e || e.gen != startGen {
			p.mu.Unlock()
			return nil, ErrPoolConnClosed
		}
		toClose := p.markStaleLocked(e, fp)
		p.expireLimitsLocked(e, p.opts.Now())
		pc := p.pickLocked(e, kind)
		if pc != nil {
			p.reserveLocked(pc, kind, origin)
			// Wall clock (Round(0) strips the monotonic reading): the
			// monotonic clock stops during sleep, so right after a wake it
			// would claim the client was alive moments ago.
			needPing := p.opts.Now().Round(0).Sub(pc.lastAliveAt.Round(0)) >= p.opts.AliveCheckAfter
			p.mu.Unlock()
			p.closeClients(toClose)

			if needPing {
				if err := p.ping(ctx, pc); err != nil {
					p.unreserve(pc, kind)
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					p.failClient(pc, err, "ping failed")
					if evictRetries++; evictRetries > defaultPoolMaxEvictRetries {
						return nil, fmt.Errorf("ssh pool: connection keeps failing: %w", err)
					}
					continue
				}
			}
			session, err := p.newSession(ctx, pc)
			if err == nil {
				p.touchAlive(pc)
				return &PoolLease{pool: p, pc: pc, kind: kind, session: session}, nil
			}
			others := p.unreserve(pc, kind)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var refused *ssh.OpenChannelError
			if errors.As(err, &refused) {
				// The connection is alive; the server is out of channels.
				if others == 0 {
					// Refused with nothing else open. Usually the server has not
					// yet processed a just-closed channel; back off briefly a few
					// times, then give up (the server forbids sessions).
					if lonelyRefusals++; lonelyRefusals > defaultPoolMaxLonelyRefusal {
						return nil, err
					}
					select {
					case <-time.After(p.refusalBackoff()):
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-p.ctx.Done():
						return nil, ErrPoolClosed
					}
					continue
				}
				p.mu.Lock()
				if pc.serverLimit == 0 || others < pc.serverLimit {
					pc.serverLimit = others
				}
				pc.serverLimitAt = p.opts.Now()
				p.mu.Unlock()
				continue // next round waits for a slot or dials overflow
			}
			// EOF / closed: the client is dead. Timeout: dead only if idle;
			// a busy one is marked suspect and left to keepalive.
			p.failClient(pc, err, "new session failed: "+err.Error())
			if evictRetries++; evictRetries > defaultPoolMaxEvictRetries {
				return nil, fmt.Errorf("ssh pool: connection keeps failing: %w", err)
			}
			continue
		}

		// No free slot. Dial (or join a dial) if allowed, else wait.
		if e.inflight != nil {
			call := e.inflight
			wake := e.wake // also fires on Disconnect/Delete while dialing
			p.mu.Unlock()
			p.closeClients(toClose)
			select {
			case <-call.done:
			case <-wake:
				// Re-check gen; rejoin the dial if it is still ours. call.done is
				// closed before wake fires, so a finished dial is seen here.
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-p.ctx.Done():
				return nil, ErrPoolClosed
			}
			select {
			case <-call.done:
				if call.err != nil && call.fp == fp {
					return nil, call.err
				}
			default:
			}
			continue
		}
		if p.liveCountLocked(e) < p.opts.MaxClients {
			dctx, dcancel := context.WithCancel(p.ctx)
			call := &dialCall{ctx: dctx, cancel: dcancel, gen: e.gen, fp: fp, origin: origin, done: make(chan struct{})}
			e.inflight = call
			p.mu.Unlock()
			p.closeClients(toClose)
			// Not tracked by wg: a Dial that ignores ctx must not block
			// CloseAll; its late client is discarded on gen mismatch.
			go p.runDial(e, call, conn)
			continue // joins call above
		}
		wake := e.wake
		p.mu.Unlock()
		p.closeClients(toClose)
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, ErrPoolClosed
		}
	}
}

// refusalBackoff is the pause after a channel refusal with no other channel
// open on the client.
func (p *SSHPool) refusalBackoff() time.Duration {
	d := p.opts.KeepaliveInterval
	if d > 200*time.Millisecond {
		d = 200 * time.Millisecond
	}
	return d
}

// ExecOnce runs command on a pooled session exactly once, writing to
// stdout/stderr (either may be nil). See Run for the retry rule.
func (p *SSHPool) ExecOnce(ctx context.Context, conn Connection, origin PoolOrigin, command string, stdout, stderr io.Writer) error {
	lease, err := p.Acquire(ctx, conn, PoolChannelExec, origin)
	if err != nil {
		return err
	}
	defer lease.Release()
	return p.runLeased(ctx, lease.Session(), command, stdout, stderr)
}

// runLeased calls Run once; on ctx cancel it kills the session.
func (p *SSHPool) runLeased(ctx context.Context, session *ssh.Session, command string, stdout, stderr io.Writer) error {
	session.Stdout = stdout
	session.Stderr = stderr
	session.Stdin = strings.NewReader("")
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}
}

// Run executes command once on a pooled exec session, like runSSHCommand
// but without owning the client. The pool may retry getting the session;
// it never re-runs the command, even if the connection dies mid-Run.
func (p *SSHPool) Run(ctx context.Context, conn Connection, origin PoolOrigin, command string, outputCap int) (commandResult, error) {
	start := time.Now()
	lease, err := p.Acquire(ctx, conn, PoolChannelExec, origin)
	if err != nil {
		return commandResult{Duration: time.Since(start)}, err
	}
	defer lease.Release()

	stdout := &cappedBuffer{max: outputCap}
	stderr := &cappedBuffer{max: outputCap}
	runErr := p.runLeased(ctx, lease.Session(), command, stdout, stderr)
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(start)}
	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		code := exitErr.ExitStatus()
		result.ExitCode = &code
	} else if runErr == nil {
		code := 0
		result.ExitCode = &code
	}
	return result, runErr
}

// Session is the leased session. It has not been started. Do not close the
// client behind it; call Release.
func (l *PoolLease) Session() *ssh.Session { return l.session }

// Kind is the slot kind the lease holds.
func (l *PoolLease) Kind() PoolChannelKind { return l.kind }

// Release closes the session and returns its slot. Safe to call twice. A
// stale client is closed here once its last lease is released (drain).
func (l *PoolLease) Release() {
	l.once.Do(func() {
		_ = l.session.Close()
		l.pool.unreserve(l.pc, l.kind)
	})
}

func (p *SSHPool) entryLocked(connID string) *poolEntry {
	e := p.entries[connID]
	if e == nil {
		e = &poolEntry{connID: connID, wake: make(chan struct{})}
		p.entries[connID] = e
	}
	return e
}

func (e *poolEntry) notifyLocked() {
	close(e.wake)
	e.wake = make(chan struct{})
}

func (pc *pooledClient) activeLocked() int { return pc.activeTerm + pc.activeExec }

func (p *SSHPool) capLocked(pc *pooledClient) int {
	limit := p.opts.MaxChannels
	if pc.serverLimit > 0 && pc.serverLimit < limit {
		limit = pc.serverLimit
	}
	return limit
}

// expireLimitsLocked forgets learned server caps older than ServerLimitTTL,
// so a refusal caused by close lag does not cap the client for its life. A
// real MaxSessions is simply learned again on the next refusal.
func (p *SSHPool) expireLimitsLocked(e *poolEntry, now time.Time) bool {
	changed := false
	for _, pc := range e.clients {
		if pc.serverLimit > 0 && now.Round(0).Sub(pc.serverLimitAt.Round(0)) >= p.opts.ServerLimitTTL {
			pc.serverLimit = 0
			changed = true
		}
	}
	return changed
}

func (p *SSHPool) hasRoomLocked(pc *pooledClient, kind PoolChannelKind) bool {
	if pc.stale || pc.evicted || pc.suspect {
		return false
	}
	if pc.activeLocked() >= p.capLocked(pc) {
		return false
	}
	return kind != PoolChannelExec || pc.activeExec < p.opts.MaxExec
}

// pickLocked returns the least loaded client with room for kind.
func (p *SSHPool) pickLocked(e *poolEntry, kind PoolChannelKind) *pooledClient {
	var best *pooledClient
	for _, pc := range e.clients {
		if !p.hasRoomLocked(pc, kind) {
			continue
		}
		if best == nil || pc.activeLocked() < best.activeLocked() {
			best = pc
		}
	}
	return best
}

func (p *SSHPool) liveCountLocked(e *poolEntry) int {
	n := 0
	for _, pc := range e.clients {
		if !pc.stale {
			n++
		}
	}
	return n
}

// markStaleLocked flags clients whose fp differs and removes the idle ones;
// busy ones drain. Returns clients the caller must close after unlocking.
func (p *SSHPool) markStaleLocked(e *poolEntry, fp string) []*pooledClient {
	var toClose []*pooledClient
	changed := false
	for _, pc := range e.clients {
		if pc.fp != fp && !pc.stale {
			pc.stale = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	kept := e.clients[:0]
	for _, pc := range e.clients {
		if pc.stale && pc.activeLocked() == 0 {
			pc.evicted = true
			toClose = append(toClose, pc)
			continue
		}
		kept = append(kept, pc)
	}
	clearTail(e.clients, len(kept))
	e.clients = kept
	e.notifyLocked()
	return toClose
}

func clearTail(s []*pooledClient, from int) {
	for i := from; i < len(s); i++ {
		s[i] = nil
	}
}

func (p *SSHPool) reserveLocked(pc *pooledClient, kind PoolChannelKind, origin PoolOrigin) {
	if kind == PoolChannelExec {
		pc.activeExec++
	} else {
		pc.activeTerm++
	}
	if origin == PoolOriginManual {
		pc.origin = PoolOriginManual // a human using it keeps it from being reclaimed
	}
	pc.lastUsedAt = p.opts.Now()
}

// unreserve returns a slot and reports how many slots the client still has
// in use. A drained stale client is closed when it reaches zero.
func (p *SSHPool) unreserve(pc *pooledClient, kind PoolChannelKind) int {
	p.mu.Lock()
	if kind == PoolChannelExec {
		pc.activeExec--
	} else {
		pc.activeTerm--
	}
	pc.lastUsedAt = p.opts.Now()
	left := pc.activeLocked()
	var drained bool
	if pc.stale && left == 0 && !pc.evicted {
		drained = p.removeLocked(pc)
	}
	if e := p.entries[pc.connID]; e != nil {
		e.notifyLocked()
	}
	p.mu.Unlock()
	if drained {
		p.closeClients([]*pooledClient{pc})
	}
	return left
}

func (p *SSHPool) touchAlive(pc *pooledClient) {
	p.mu.Lock()
	pc.lastAliveAt = p.opts.Now()
	if pc.suspect {
		pc.suspect = false
		if e := p.entries[pc.connID]; e != nil {
			e.notifyLocked() // waiters may use it again
		}
	}
	p.mu.Unlock()
}

func isPoolTimeout(err error) bool {
	var t errPoolTimeout
	return errors.As(err, &t)
}

// failClient handles a failed ping or NewSession during Acquire (the
// caller's slot is already returned). A non-timeout error means the
// connection is gone, so it is evicted. A timeout evicts only an idle
// client; a busy one may just be congested, so it is marked suspect and
// keepalive decides (§5.1: Close after KeepaliveMaxFail failures in a row).
func (p *SSHPool) failClient(pc *pooledClient, err error, reason string) {
	if !isPoolTimeout(err) {
		p.evict(pc, reason)
		return
	}
	p.mu.Lock()
	removed := false
	if pc.activeLocked() == 0 {
		removed = p.removeLocked(pc)
	} else if !pc.evicted {
		pc.suspect = true
		if e := p.entries[pc.connID]; e != nil {
			e.notifyLocked()
		}
	}
	p.mu.Unlock()
	if removed {
		p.closeClients([]*pooledClient{pc})
	}
}

// removeLocked removes pc from its entry by pointer (CAS). Only the caller
// that gets true may close it.
func (p *SSHPool) removeLocked(pc *pooledClient) bool {
	if pc.evicted {
		return false
	}
	pc.evicted = true
	e := p.entries[pc.connID]
	if e == nil {
		return true
	}
	for i, c := range e.clients {
		if c == pc {
			e.clients = append(e.clients[:i], e.clients[i+1:]...)
			clearTail(e.clients[:cap(e.clients)], len(e.clients))
			break
		}
	}
	e.notifyLocked()
	return true
}

// evict removes pc if it is still in the pool and closes it.
func (p *SSHPool) evict(pc *pooledClient, _ string) {
	p.mu.Lock()
	removed := p.removeLocked(pc)
	p.mu.Unlock()
	if removed {
		p.closeClients([]*pooledClient{pc})
	}
}

func (p *SSHPool) closeClients(list []*pooledClient) {
	if len(list) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, pc := range list {
		pc.closeOnce.Do(func() {
			if pc.cancel != nil {
				pc.cancel()
			}
			_ = pc.client.Close()
		})
		seen[pc.connID] = true
	}
	if p.opts.OnChange != nil {
		for id := range seen {
			p.opts.OnChange(id)
		}
	}
}

// ---- dialing ---------------------------------------------------------------

func (p *SSHPool) runDial(e *poolEntry, call *dialCall, conn Connection) {
	defer call.cancel()
	rec := &poolHostKeyRecorder{}
	ctx := context.WithValue(call.ctx, poolHostKeyKey{}, rec)
	client, err := p.opts.Dial(ctx, conn)
	if err == nil && client == nil {
		err = errors.New("ssh pool: dial returned no client")
	}
	if err != nil && call.ctx.Err() != nil {
		// Cancelled by Disconnect/Delete or CloseAll, not a dial failure.
		if p.ctx.Err() != nil {
			err = ErrPoolClosed
		} else {
			err = ErrPoolConnClosed
		}
	}

	p.mu.Lock()
	var pc *pooledClient
	discard := false
	if err == nil {
		if p.closed || p.entries[e.connID] != e || e.gen != call.gen {
			discard = true
		} else {
			now := p.opts.Now()
			rec.mu.Lock()
			hk := rec.fp
			rec.mu.Unlock()
			kctx, cancel := context.WithCancel(p.ctx)
			pc = &pooledClient{
				connID: e.connID, client: client, fp: call.fp, hostKeyFP: hk,
				origin: call.origin, lastAliveAt: now, lastUsedAt: now, cancel: cancel,
			}
			e.clients = append(e.clients, pc)
			p.wg.Add(2)
			go p.keepalive(kctx, pc)
			go p.watch(pc)
		}
	}
	call.err = err
	if e.inflight == call {
		e.inflight = nil
	}
	close(call.done) // before notify, so woken waiters see the result
	e.notifyLocked()
	p.mu.Unlock()

	if discard {
		_ = client.Close()
	}
	if pc != nil && p.opts.OnChange != nil {
		p.opts.OnChange(e.connID)
	}
}

// ---- liveness --------------------------------------------------------------

// newSession wraps NewSession so a hung channel open respects ctx and the
// keepalive timeout. A late session is closed.
func (p *SSHPool) newSession(ctx context.Context, pc *pooledClient) (*ssh.Session, error) {
	type result struct {
		s   *ssh.Session
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := pc.client.NewSession()
		ch <- result{s, err}
	}()
	timer := time.NewTimer(p.opts.KeepaliveTimeout)
	defer timer.Stop()
	abandon := func() {
		go func() {
			if r := <-ch; r.s != nil {
				_ = r.s.Close()
			}
		}()
	}
	select {
	case r := <-ch:
		return r.s, r.err
	case <-ctx.Done():
		abandon()
		return nil, ctx.Err()
	case <-timer.C:
		abandon()
		return nil, errPoolTimeout{"new session"}
	}
}

type errPoolTimeout struct{ op string }

func (e errPoolTimeout) Error() string   { return "ssh pool: " + e.op + " timed out" }
func (e errPoolTimeout) Timeout() bool   { return true }
func (e errPoolTimeout) Temporary() bool { return false }

var _ net.Error = errPoolTimeout{}

// ping sends one keepalive request bounded by ctx and KeepaliveTimeout. A
// reply of "false" (unsupported request) still proves the link is alive.
func (p *SSHPool) ping(ctx context.Context, pc *pooledClient) error {
	ch := make(chan error, 1)
	go func() {
		_, _, err := pc.client.SendRequest("keepalive@openssh.com", true, nil)
		ch <- err
	}()
	timer := time.NewTimer(p.opts.KeepaliveTimeout)
	defer timer.Stop()
	select {
	case err := <-ch:
		if err == nil {
			p.touchAlive(pc)
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errPoolTimeout{"keepalive"}
	}
}

func (p *SSHPool) keepalive(ctx context.Context, pc *pooledClient) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.opts.KeepaliveInterval)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := p.ping(ctx, pc); err != nil {
			if ctx.Err() != nil {
				return
			}
			fails++
			if fails >= p.opts.KeepaliveMaxFail {
				p.evict(pc, "keepalive failed")
				return
			}
			continue
		}
		fails = 0
	}
}

// watch notices a dropped connection immediately via client.Wait.
func (p *SSHPool) watch(pc *pooledClient) {
	defer p.wg.Done()
	_ = pc.client.Wait()
	p.evict(pc, "connection lost")
}

// CheckAll pings every pooled client concurrently and evicts the dead ones.
// It runs automatically after a detected wake from sleep.
func (p *SSHPool) CheckAll(ctx context.Context) {
	p.mu.Lock()
	var all []*pooledClient
	for _, e := range p.entries {
		all = append(all, e.clients...)
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, pc := range all {
		wg.Add(1)
		go func(pc *pooledClient) {
			defer wg.Done()
			if err := p.ping(ctx, pc); err != nil && ctx.Err() == nil {
				p.evict(pc, "check failed")
			}
		}(pc)
	}
	wg.Wait()
}

// housekeeping reclaims idle auto clients and detects wake from sleep (the
// wall clock jumping ahead of the monotonic ticker).
func (p *SSHPool) housekeeping() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.opts.KeepaliveInterval)
	defer ticker.Stop()
	last := p.opts.Now().Round(0)
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
		}
		now := p.opts.Now().Round(0)
		if now.Sub(last) > p.opts.KeepaliveInterval+p.opts.SleepJump {
			p.CheckAll(p.ctx)
		}
		last = now
		p.reapIdle(now)
		p.expireLimits(now)
	}
}

// expireLimits drops stale learned caps and wakes waiters that may now fit.
func (p *SSHPool) expireLimits(now time.Time) {
	p.mu.Lock()
	for _, e := range p.entries {
		if p.expireLimitsLocked(e, now) {
			e.notifyLocked()
		}
	}
	p.mu.Unlock()
}

// ReapIdle closes auto clients unused for AutoIdle. Housekeeping calls
// it every tick; tests call it after advancing Now.
func (p *SSHPool) ReapIdle() { p.reapIdle(p.opts.Now().Round(0)) }

func (p *SSHPool) reapIdle(now time.Time) {
	var toClose []*pooledClient
	p.mu.Lock()
	for _, e := range p.entries {
		for _, pc := range append([]*pooledClient(nil), e.clients...) {
			if pc.origin == PoolOriginAuto && pc.activeLocked() == 0 && now.Round(0).Sub(pc.lastUsedAt.Round(0)) >= p.opts.AutoIdle {
				if p.removeLocked(pc) {
					toClose = append(toClose, pc)
				}
			}
		}
	}
	p.mu.Unlock()
	p.closeClients(toClose)
}

// ---- explicit control ------------------------------------------------------

// Drain marks connID's clients stale: idle ones close now, busy ones close
// when their last lease is released. Use after the saved config changes.
func (p *SSHPool) Drain(connID string) {
	var toClose []*pooledClient
	p.mu.Lock()
	if e := p.entries[connID]; e != nil {
		for _, pc := range append([]*pooledClient(nil), e.clients...) {
			pc.stale = true
			if pc.activeLocked() == 0 && p.removeLocked(pc) {
				toClose = append(toClose, pc)
			}
		}
		e.notifyLocked()
	}
	p.mu.Unlock()
	p.closeClients(toClose)
}

// Disconnect is the human "切断": close every client of connID now, even in
// use, and fail callers waiting on it. The entry stays for future use.
func (p *SSHPool) Disconnect(connID string) { p.closeConn(connID, false) }

// Delete closes connID's clients now and forgets the entry.
func (p *SSHPool) Delete(connID string) { p.closeConn(connID, true) }

func (p *SSHPool) closeConn(connID string, forget bool) {
	var toClose []*pooledClient
	p.mu.Lock()
	if e := p.entries[connID]; e != nil {
		e.gen++
		for _, pc := range e.clients {
			pc.evicted = true
			toClose = append(toClose, pc)
		}
		e.clients = nil
		if e.inflight != nil {
			// Stop the TCP/auth attempt now, so a reconnect right after
			// 切断 does not run a second dial beside it (§5.1). A late
			// client is still discarded on gen mismatch.
			e.inflight.cancel()
			e.inflight = nil
		}
		e.notifyLocked()
		if forget {
			delete(p.entries, connID)
		}
	}
	p.mu.Unlock()
	p.closeClients(toClose)
}

// CloseAll closes every client, stops all goroutines and rejects further
// use. Call it after CloseAllTerminals during shutdown (§5.0 step 4).
func (p *SSHPool) CloseAll() {
	var toClose []*pooledClient
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	for _, e := range p.entries {
		e.gen++
		for _, pc := range e.clients {
			pc.evicted = true
			toClose = append(toClose, pc)
		}
		e.clients = nil
		if e.inflight != nil {
			e.inflight.cancel()
			e.inflight = nil
		}
		e.notifyLocked()
	}
	p.mu.Unlock()
	p.cancel()
	p.closeClients(toClose)
	p.wg.Wait()
}

// Stats returns a snapshot of every connID the pool knows.
func (p *SSHPool) Stats() []PoolConnStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PoolConnStat, 0, len(p.entries))
	for id, e := range p.entries {
		st := PoolConnStat{ConnID: id, Dialing: e.inflight != nil}
		for _, pc := range e.clients {
			st.Clients++
			if pc.stale {
				st.Stale++
			}
			st.Term += pc.activeTerm
			st.Exec += pc.activeExec
			st.Origins = append(st.Origins, pc.origin)
			if pc.lastAliveAt.After(st.LastSeen) {
				st.LastSeen = pc.lastAliveAt
			}
		}
		out = append(out, st)
	}
	return out
}

// Connected reports whether connID has at least one live, non-stale client.
func (p *SSHPool) Connected(connID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[connID]; e != nil {
		return p.liveCountLocked(e) > 0
	}
	return false
}

// ClientCount is the number of open clients for connID, including stale ones
// still draining.
func (p *SSHPool) ClientCount(connID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entries[connID]; e != nil {
		return len(e.clients)
	}
	return 0
}
