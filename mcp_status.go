package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

const (
	// maxMCPExec bounds approval-bypassed executions running at once (the
	// synchronous ones and those detached after RequestTimeout).
	maxMCPExec = 4
	// maxMCPWaiters bounds concurrent get_request_status long-polls.
	maxMCPWaiters = 64
	// busyRetryAfter is the retry_after_ms hint returned with server_busy.
	busyRetryAfter = time.Second
)

// mcpRuntime is the MCP server's in-process admission state: execution and
// long-poll slots and the per-key locks for client_request_id. It does not
// track executions: every run (bypassed or GUI-approved) is registered with
// App.trackExec, which owns cancellation and the shutdown drain. Its zero
// value is ready to use. mu is a leaf lock; nothing else is acquired while
// holding it.
type mcpRuntime struct {
	mu      sync.Mutex
	closed  bool
	execs   int
	waiters int
	keys    map[string]*idempotencyLock
}

type idempotencyLock struct {
	mu   sync.Mutex
	refs int
}

// close refuses further execution and long-poll slots (shutdown, §5.0
// step 1). Slots already taken are released as usual.
func (rt *mcpRuntime) close() {
	rt.mu.Lock()
	rt.closed = true
	rt.mu.Unlock()
}

func (rt *mcpRuntime) acquireWaiter() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed || rt.waiters >= maxMCPWaiters {
		return false
	}
	rt.waiters++
	return true
}

func (rt *mcpRuntime) releaseWaiter() {
	rt.mu.Lock()
	rt.waiters--
	rt.mu.Unlock()
}

// acquireExec takes an approval-bypassed execution slot without blocking.
// ok=false means answer server_busy.
func (rt *mcpRuntime) acquireExec() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed || rt.execs >= maxMCPExec {
		return false
	}
	rt.execs++
	return true
}

func (rt *mcpRuntime) releaseExec() {
	rt.mu.Lock()
	rt.execs--
	rt.mu.Unlock()
}

// lockKey serializes requests that share an idempotency key, so concurrent
// retries cannot both miss the lookup and both create a request. The returned
// unlock may be called more than once.
func (rt *mcpRuntime) lockKey(key string) func() {
	rt.mu.Lock()
	if rt.keys == nil {
		rt.keys = map[string]*idempotencyLock{}
	}
	lock := rt.keys[key]
	if lock == nil {
		lock = &idempotencyLock{}
		rt.keys[key] = lock
	}
	lock.refs++
	rt.mu.Unlock()

	lock.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			lock.mu.Unlock()
			rt.mu.Lock()
			lock.refs--
			if lock.refs == 0 {
				delete(rt.keys, key)
			}
			rt.mu.Unlock()
		})
	}
}

// requestStatus returns the views of ids. With wait > 0 it long-polls (via
// waitForRequests, woken by reqCond) while all of them are still open: it
// returns once any reaches a final state or is unknown, when wait elapses, or
// when the HTTP request ends. The pending → running step alone does not end
// the wait, so an approved command's output arrives in the same call.
func (a *App) requestStatus(ctx context.Context, caller mcpCaller, ids []string, wait time.Duration, includeOutput bool) []map[string]any {
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		entries := a.waitForRequests(ctx, ids, remaining)
		if remaining <= 0 || ctx.Err() != nil || lookupsSettled(entries) {
			return a.lookupViews(caller, entries, includeOutput)
		}
	}
}

// isAISessionLine reports whether req is a line of an AI session: by the
// field filled in memory, the line registry, or ai_session_lines.
func (a *App) isAISessionLine(req CommandRequest) bool {
	if a.isAISessionLineLocked(req) {
		return true
	}
	if a.store != nil {
		if _, ok, err := a.store.loadAISessionLine(req.ID); err != nil || ok {
			return true // fail closed on a storage error
		}
	}
	return false
}

// isAISessionLineLocked is isAISessionLine without SQLite; it takes no App
// lock, so callers may hold a.mu.
func (a *App) isAISessionLineLocked(req CommandRequest) bool {
	if req.SessionID != "" {
		return true
	}
	_, ok := a.aiLineLink(req.ID)
	return ok
}

// mayReadRequest applies the AI session owner check (REQ-016 §2.1) to the
// request read paths: a session line is visible only to the agent token that
// sent it. Other requests keep their earlier visibility.
func (a *App) mayReadRequest(caller mcpCaller, req CommandRequest) bool {
	if caller.Agent != "" && caller.Agent == req.RequestedBy {
		return true
	}
	return !a.isAISessionLine(req)
}

// lookupsSettled reports whether any entry is unknown or final.
func lookupsSettled(entries []requestLookup) bool {
	for _, entry := range entries {
		if !entry.Found || requestIsFinal(entry.Request.Status) {
			return true
		}
	}
	return false
}

// lookupViews renders status entries. IDs no longer held in memory (older
// than the loaded window) are read from SQLite.
func (a *App) lookupViews(caller mcpCaller, entries []requestLookup, includeOutput bool) []map[string]any {
	views := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if !entry.Found && a.store != nil {
			if req, ok, err := a.store.loadCommandRequest(entry.ID); err == nil && ok {
				entry.Found, entry.Request = true, req
			}
		}
		// Another agent's AI session line looks like an unknown ID: its
		// output and session ID belong to its owner alone.
		if !entry.Found || !a.mayReadRequest(caller, entry.Request) {
			views = append(views, map[string]any{"request_id": entry.ID, "error_kind": "not_found", "message": "request not found"})
			continue
		}
		views = append(views, requestView(entry.Request, includeOutput))
	}
	return views
}

// existingClientRequest finds the request an agent already made with this
// client_request_id within 24 hours: in memory first, then in SQLite for
// requests outside the loaded window.
func (a *App) existingClientRequest(agentName string, clientRequestID string) (CommandRequest, bool) {
	if clientRequestID == "" {
		return CommandRequest{}, false
	}
	if req, ok := a.findRequestByClientID(agentName, clientRequestID); ok {
		return req, true
	}
	if a.store == nil {
		return CommandRequest{}, false
	}
	req, ok, err := a.store.loadRequestByClientID(agentName, clientRequestID, time.Now())
	return req, err == nil && ok
}

// commandHistory returns get_command_history rows newest first, without
// output: from SQLite (the whole history), or from memory without a store.
func (a *App) commandHistory(f historyFilter) ([]CommandRequest, error) {
	if a.store != nil {
		return a.store.loadCommandHistory(f)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := []CommandRequest{}
	for _, req := range a.requests {
		if len(out) >= f.Limit {
			break
		}
		// Match the resolved connection, or the name snapshot for history of
		// deleted or renamed connections.
		if f.HostName != "" && (f.ConnectionID == "" || req.ConnectionID != f.ConnectionID) && req.Host != f.HostName {
			continue
		}
		if f.State != "" && req.Status != f.State {
			continue
		}
		if f.RequestedBy != "" && req.RequestedBy != f.RequestedBy {
			continue
		}
		if f.HideSessionLines && req.RequestedBy != f.SessionViewer && a.isAISessionLineLocked(req) {
			continue
		}
		req.Stdout, req.Stderr = "", ""
		out = append(out, req)
	}
	return out, nil
}

// mcpListenerStop is closed when ssh-gete itself stops (or restarts) one MCP
// listener. http.Server.Close cancels every handler's r.Context() just as a
// client disconnect does, so handlers consult it to tell the two apart: a
// stop initiated by the server detaches synchronous executions (shutdown and
// drainMCPCommands alone then govern them, §5.0) instead of cancelling them
// as a client disconnect.
type mcpListenerStop struct {
	ch   chan struct{}
	once sync.Once
}

type mcpListenerStopKey struct{}

func newMCPListenerStop() *mcpListenerStop {
	return &mcpListenerStop{ch: make(chan struct{})}
}

func (s *mcpListenerStop) signal() {
	s.once.Do(func() { close(s.ch) })
}

// wrap puts the stop signal into every request's context.
func (s *mcpListenerStop) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpListenerStopKey{}, s.ch)))
	})
}

// listenerStopping returns the stop channel of the listener serving ctx, or
// nil (blocks forever in a select) outside a listener, e.g. in tests that
// call the handler directly.
func listenerStopping(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(mcpListenerStopKey{}).(chan struct{})
	return ch
}

func isClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// withListenerStop derives a context that also ends when the listener stops,
// for work (long-polls) that should answer early rather than be cut off.
func withListenerStop(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if stop := listenerStopping(ctx); stop != nil {
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}
