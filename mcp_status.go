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

// mcpRuntime is the MCP server's in-process coordination state: execution
// and long-poll slots, the per-key locks for client_request_id and the
// context that owns detached executions. Its zero value is ready to use. mu is
// a leaf lock; nothing else is acquired while holding it.
type mcpRuntime struct {
	mu      sync.Mutex
	closed  bool
	execs   int
	waiters int
	keys    map[string]*idempotencyLock

	baseCtx    context.Context
	baseCancel context.CancelFunc
	// wg counts executions so drain can wait for their outcome to be stored.
	wg sync.WaitGroup
}

type idempotencyLock struct {
	mu   sync.Mutex
	refs int
}

// initLocked derives the execution context from parent (the app context) on
// first use, so app shutdown also cancels detached executions.
func (rt *mcpRuntime) initLocked(parent context.Context) {
	if rt.baseCtx == nil {
		if parent == nil {
			parent = context.Background()
		}
		rt.baseCtx, rt.baseCancel = context.WithCancel(parent)
	}
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

// acquireExec takes an execution slot without blocking and returns the
// context the execution runs under (not the HTTP request's, so it can outlive
// the request after RequestTimeout). ok=false means answer server_busy.
func (rt *mcpRuntime) acquireExec(parent context.Context) (context.Context, context.CancelFunc, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.initLocked(parent)
	if rt.closed || rt.execs >= maxMCPExec || rt.baseCtx.Err() != nil {
		return nil, nil, false
	}
	rt.execs++
	rt.wg.Add(1)
	ctx, cancel := context.WithCancel(rt.baseCtx)
	return ctx, cancel, true
}

// acquireApproved registers a human-approved execution of a queued request.
// It takes no slot (the human already decided), but drain waits for it and
// cancels it like any MCP execution, so shutdown persists its outcome before
// the store closes. ok=false once drain has begun.
func (rt *mcpRuntime) acquireApproved(parent context.Context) (context.Context, context.CancelFunc, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.initLocked(parent)
	if rt.closed || rt.baseCtx.Err() != nil {
		return nil, nil, false
	}
	rt.wg.Add(1)
	ctx, cancel := context.WithCancel(rt.baseCtx)
	return ctx, cancel, true
}

func (rt *mcpRuntime) releaseApproved() {
	rt.wg.Done()
}

func (rt *mcpRuntime) releaseExec() {
	rt.mu.Lock()
	rt.execs--
	rt.mu.Unlock()
	rt.wg.Done()
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

// drain stops accepting MCP executions and long-polls and waits up to grace
// for running executions. Those still running are then cancelled (recorded
// as interrupted) and waited for, so their outcome is stored before the store
// closes. It reports whether all finished within grace.
func (rt *mcpRuntime) drain(grace time.Duration) bool {
	rt.mu.Lock()
	rt.initLocked(nil)
	rt.closed = true
	cancel := rt.baseCancel
	rt.mu.Unlock()

	done := make(chan struct{})
	go func() {
		rt.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
	}
	cancel()
	<-done
	return false
}

// drainMCPCommands is shutdown step 2 (§5.0): after the MCP listener has
// stopped, wait up to grace for MCP executions, then cancel the rest.
func (a *App) drainMCPCommands(grace time.Duration) bool {
	return a.mcpRuntime.drain(grace)
}

// requestStatus returns the views of ids. With wait > 0 it long-polls (via
// waitForRequests, woken by reqCond) while all of them are still open: it
// returns once any reaches a final state or is unknown, when wait elapses, or
// when the HTTP request ends. The pending → running step alone does not end
// the wait, so an approved command's output arrives in the same call.
func (a *App) requestStatus(ctx context.Context, ids []string, wait time.Duration, includeOutput bool) []map[string]any {
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		entries := a.waitForRequests(ctx, ids, remaining)
		if remaining <= 0 || ctx.Err() != nil || lookupsSettled(entries) {
			return a.lookupViews(entries, includeOutput)
		}
	}
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
func (a *App) lookupViews(entries []requestLookup, includeOutput bool) []map[string]any {
	views := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if !entry.Found && a.store != nil {
			if req, ok, err := a.store.loadCommandRequest(entry.ID); err == nil && ok {
				entry.Found, entry.Request = true, req
			}
		}
		if !entry.Found {
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
		req.Stdout, req.Stderr = "", ""
		out = append(out, req)
	}
	return out, nil
}

// mcpListenerStop is closed when ssh-gete itself stops (or restarts) one MCP
// listener. http.Server.Close cancels every handler's r.Context() just as a
// client disconnect does, so handlers consult it to tell the two apart: a
// stop initiated by the server detaches synchronous executions (drain alone
// then governs them, §5.0 step 2) instead of cancelling them as a disconnect.
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
