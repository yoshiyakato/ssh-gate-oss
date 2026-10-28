package main

// App side of AI sessions (phase7-ai-session.md §2, §5, §7, §9.2): the
// mcpAISessionBackend the MCP tools call, the persistence hooks wired to the
// Store, startup recovery, the batched audit writer and the exec transport
// factory. The existing files only call into this file (the §9.2 wiring).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

var _ mcpAISessionBackend = (*App)(nil)

var (
	// aiAuditFlushBytes / aiAuditFlushEvery batch ai_session_output writes
	// (§7.2: 64 KiB or 2 s).
	aiAuditFlushBytes = 64 * 1024
	aiAuditFlushEvery = 2 * time.Second
	// aiInterruptHintWait is how long interrupt waits for the prompt before
	// it suggests close_session (§2.5).
	aiInterruptHintWait = 5 * time.Second
	// aiOutputRetention is how long ai_session_output is kept after close.
	aiOutputRetention = 30 * 24 * time.Hour
	aiPruneEvery      = time.Hour
)

// ---------------------------------------------------------------------------
// Host methods used by the manager

func (h appAIHost) settleLines(g *aiLineGate, reason string) {
	h.a.settleAISessionLines(g, reason, 0)
}

func (h appAIHost) reconcileLine(g *aiLineGate) { h.a.reconcileAISessionLine(g) }

// newAIExecTransport dials a dedicated client with the MCP host-key policy
// and credential timeout, then opens the exec mode transport on it.
func (a *App) newAIExecTransport(ctx context.Context, conn Connection, opt aiTransportOptions) (aiTransport, error) {
	client, err := a.dialAISessionClient(ctx, conn)
	if err != nil {
		return nil, err
	}
	return newExecTransport(ctx, client, opt.Shell, opt.Out, opt.NewFilter)
}

// ---------------------------------------------------------------------------
// Audit writer (§7.2)

// aiAuditWriter batches a session's AI text into ai_session_output. add is
// called under the output's write lock, so it only appends to memory; a
// goroutine writes a batch every aiAuditFlushEvery or when
// aiAuditFlushBytes are waiting. A failed write marks the session so no
// further line runs without an audit trail.
type aiAuditWriter struct {
	write  func([]aiSessionOutputChunk) error
	failed func()
	now    func() time.Time

	mu      sync.Mutex
	pending []aiSessionOutputChunk
	size    int
	kick    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	closed  bool
}

func newAIAuditWriter(write func([]aiSessionOutputChunk) error, failed func(), now func() time.Time) *aiAuditWriter {
	w := &aiAuditWriter{write: write, failed: failed, now: now,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{})}
	go w.loop()
	return w
}

func (w *aiAuditWriter) add(start uint64, p []byte) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if n := len(w.pending); n > 0 {
		last := &w.pending[n-1]
		if last.CursorStart+uint64(len(last.Text)) == start && len(last.Text) < aiAuditFlushBytes {
			last.Text += string(p)
			w.size += len(p)
			full := w.size >= aiAuditFlushBytes
			w.mu.Unlock()
			if full {
				w.signal()
			}
			return
		}
	}
	w.pending = append(w.pending, aiSessionOutputChunk{CursorStart: start, Text: string(p), CreatedAt: w.now()})
	w.size += len(p)
	full := w.size >= aiAuditFlushBytes
	w.mu.Unlock()
	if full {
		w.signal()
	}
}

func (w *aiAuditWriter) signal() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *aiAuditWriter) loop() {
	defer close(w.stopped)
	ticker := time.NewTicker(aiAuditFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			w.flush()
			return
		case <-ticker.C:
		case <-w.kick:
		}
		w.flush()
	}
}

func (w *aiAuditWriter) flush() {
	w.mu.Lock()
	batch := w.pending
	w.pending, w.size = nil, 0
	w.mu.Unlock()
	if len(batch) == 0 || w.write == nil {
		return
	}
	if err := w.write(batch); err != nil {
		log.Printf("ai session audit: %v", err)
		if w.failed != nil {
			w.failed()
		}
	}
}

// close writes what is left and stops the loop.
func (w *aiAuditWriter) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()
	close(w.stop)
	<-w.stopped
}

// ---------------------------------------------------------------------------
// Startup (§5 recovery, §7)

// initAISessions installs the manager's transports and persistence hooks,
// loads the settings, closes what was left open by a previous run
// (app_restart, pending lines withdrawn) and loads the request ↔ session
// links. NewApp calls it before the command requests are loaded.
func (a *App) initAISessions(store *Store) {
	m := a.aiMgr()
	if store == nil {
		return
	}
	settings := defaultAISessionSettings()
	if ok, err := store.loadAISessionSettings(&settings); err != nil {
		log.Printf("load ai session settings: %v", err)
		settings = defaultAISessionSettings()
	} else if !ok {
		settings = defaultAISessionSettings()
	}
	a.configureAISessions(a.aiSessionStoreHooks(store), settings)
	if closed, withdrawn, err := store.recoverOpenAISessionRows(a.clock()); err != nil {
		log.Printf("recover ai sessions: %v", err)
	} else if len(closed) > 0 || len(withdrawn) > 0 {
		log.Printf("recovered %d ai sessions (app_restart), withdrew %d pending lines", len(closed), len(withdrawn))
	}
	_ = m
}

// loadAISessionLinks marks the requests of earlier sessions (after
// app.requests is loaded): approve() refuses them and the frontend shows
// their session.
func (a *App) loadAISessionLinks(store *Store) {
	if store == nil {
		return
	}
	rows, err := store.loadAISessionLinks()
	if err != nil {
		log.Printf("load ai session lines: %v", err)
		return
	}
	links := make(map[string]aiLineLink, len(rows))
	for id, row := range rows {
		links[id] = aiLineLink{SessionID: row.SessionID, Seq: row.Seq, rec: aiLineRecord{Cwd: row.Cwd, Tainted: row.Tainted,
			Class: sessionLineClass{Effect: row.Effect, Reasons: row.RiskReasons}, CursorStart: row.CursorStart, CursorEnd: row.CursorEnd}}
	}
	a.markAISessionLinesKnown(links)
	a.mu.Lock()
	for i := range a.requests {
		if row, ok := rows[a.requests[i].ID]; ok {
			a.requests[i].SessionID, a.requests[i].SessionSeq = row.SessionID, row.Seq
		}
	}
	a.mu.Unlock()
}

// startAISessionLoops starts the sweeper and the output retention loop.
func (a *App) startAISessionLoops() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runAISessionSweeper(a.appCtx)
	}()
	if a.store == nil {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		prune := func() {
			if _, err := a.store.pruneAISessionOutput(a.clock().Add(-aiOutputRetention)); err != nil {
				log.Printf("prune ai session output: %v", err)
			}
		}
		prune()
		ticker := time.NewTicker(aiPruneEvery)
		defer ticker.Stop()
		for {
			select {
			case <-a.appCtx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
}

// aiSessionStoreHooks connects the manager to the Store. Every hook runs
// with no AI lock held except onText (the audit writer only buffers).
func (a *App) aiSessionStoreHooks(store *Store) aiSessionHooks {
	m := a.aiMgr()
	return aiSessionHooks{
		onOpen: func(info AISessionInfo, crid string) error {
			opened, _ := time.Parse(time.RFC3339, info.OpenedAt)
			s, _ := m.lookup(info.ID, "", true)
			fp := ""
			if s != nil {
				fp = s.targetFP
			}
			return store.insertAISession(aiSessionRow{ID: info.ID, AgentName: info.Agent, ConnectionID: info.ConnID,
				HostName: info.ConnName, TargetFP: fp, Mode: info.Mode, Shell: info.Shell, Reason: info.Reason,
				ClientRequestID: crid, OpenedAt: opened})
		},
		onClose: func(info AISessionInfo) {
			closed, err := time.Parse(time.RFC3339, info.ClosedAt)
			if err != nil {
				closed = a.clock()
			}
			by := info.ClosedBy
			if !validAIClosedBy(by) {
				by = aiClosedBySystem
			}
			if _, err := store.closeAISessionRow(info.ID, info.CloseReason, by, closed, nil); err != nil {
				log.Printf("ai session %s: record close: %v", info.ID, err)
			}
		},
		onEvent: func(id, kind string, detail map[string]any) {
			if kind == aiEventOpen || kind == aiEventClose || !aiSessionEventKinds[kind] {
				return
			}
			if _, err := store.insertAISessionEvent(aiSessionEventRow{SessionID: id, At: a.clock(), Kind: kind, Detail: detail}); err != nil {
				log.Printf("ai session %s: record %s: %v", id, kind, err)
			}
		},
		newTextFilter: func() aiTextFilter { return newAIRedactFilter() },
		saveSettings:  func(s AISessionSettings) error { return store.saveAISessionSettings(s, a.clock()) },
		newAudit: func(id string, limit int64) *aiAuditWriter {
			return newAIAuditWriter(func(chunks []aiSessionOutputChunk) error {
				_, err := store.appendAISessionOutput(id, chunks, limit)
				if errors.Is(err, errAISessionNotFound) {
					return nil // opening failed before the row was written
				}
				return err
			}, func() {
				if s, _ := m.lookup(id, "", true); s != nil {
					s.markStorageFailed()
				}
			}, a.clock)
		},
		lineQueued: func(rec aiLineRecord) error {
			return store.insertAISessionLine(aiLineRow(rec))
		},
		lineFinished: func(rec aiLineRecord) {
			end := rec.CursorEnd
			if end < rec.CursorStart {
				end = rec.CursorStart
			}
			if _, err := store.finishAISessionLine(rec.RequestID, end); err != nil {
				log.Printf("ai session line %s: record end: %v", rec.RequestID, err)
			}
		},
	}
}

// aiLineRow is the ai_session_lines row of a queued line.
func aiLineRow(rec aiLineRecord) aiSessionLineRow {
	env := make([]aiSessionEnvEntry, 0, len(rec.Env)+len(rec.Unset))
	for _, v := range envViews(rec.Env, nil) {
		env = append(env, aiSessionEnvEntry{Name: v.Name, Value: rec.Env[v.Name]})
	}
	for _, name := range rec.Unset {
		env = append(env, aiSessionEnvEntry{Name: name, Unset: true})
	}
	effect := rec.Class.Effect
	if effect == "" {
		effect = lineEffectNone
	}
	return aiSessionLineRow{RequestID: rec.RequestID, SessionID: rec.SessionID, Seq: rec.Seq, Cwd: rec.Cwd, Env: env,
		Effect: effect, RiskReasons: rec.Class.Reasons, Tainted: rec.Tainted, CursorStart: rec.CursorStart}
}

// ---------------------------------------------------------------------------
// mcpAISessionBackend

// aiMCPErr converts a core error to the MCP error.
func aiMCPErr(err error) error {
	if err == nil {
		return nil
	}
	var se *aiSessError
	if errors.As(err, &se) {
		return newMCPAISessionErr(se.Code, se.Message, se.Extra)
	}
	var le *aiLineError
	if errors.As(err, &le) {
		code := le.Code
		if code == "invalid_params" {
			code = aiErrInvalidArgument
		}
		return newMCPAISessionErr(code, le.Message, le.Extra)
	}
	return err
}

func (a *App) aiSessionMCPEnabled() bool { return a.aiMgr().Settings().Enabled }

func (a *App) aiSessionMCPLookup(id string) (mcpAISessionRef, bool) {
	s, _ := a.aiMgr().lookup(id, "", true)
	if s == nil {
		return mcpAISessionRef{}, false
	}
	sn := s.snap()
	return mcpAISessionRef{ID: s.id, Owner: s.owner, Closed: sn.Base == aiStateClosing || sn.Base == aiStateClosed,
		CloseReason: sn.CloseReason}, true
}

// ownedSession is the backend's own owner / state re-check.
func (a *App) ownedSession(owner, id string) (*aiSession, error) {
	s, err := a.lookupAISession(id, owner)
	if s == nil {
		return nil, aiMCPErr(err)
	}
	return s, aiMCPErr(err)
}

func (a *App) aiSessionMCPOpen(ctx context.Context, owner string, caller mcpCaller, args openSessionArgs) (mcpAISessionOpenView, error) {
	if caller.Agent != owner {
		return mcpAISessionOpenView{}, newMCPAISessionErr(aiErrAgentTokenRequired, "an agent token is required", nil)
	}
	s, dedup, err := a.openAISession(ctx, caller, args.Host, aiOpenRequest{Reason: args.Reason, Mode: args.Mode,
		Shell: args.Shell, ClientRequestID: args.ClientRequestID})
	if err != nil {
		return mcpAISessionOpenView{}, aiMCPErr(err)
	}
	settings := a.aiMgr().Settings()
	info := s.info(settings)
	return mcpAISessionOpenView{
		SessionID: s.id, Connection: s.connName, Mode: s.mode, Shell: s.shell, State: info.State, Cwd: info.Cwd,
		OpenedAt: info.OpenedAt, IdleTimeoutS: settings.IdleMinutes * 60, ExpiresAt: info.ExpiresAt,
		OutputCursor: s.out.TextEnd(),
		Limits:       mcpAISessionLimits{PerAgent: settings.PerAgent, Total: settings.Total, RingBytes: settings.RingKB * 1024},
		Deduplicated: dedup,
	}, nil
}

func (a *App) aiSessionMCPSendLine(ctx context.Context, owner string, caller mcpCaller, args sendLineArgs) (mcpAISessionSendView, error) {
	s, err := a.ownedSession(owner, args.SessionID)
	if err != nil {
		return mcpAISessionSendView{}, err
	}
	if s.StorageFailed() {
		return mcpAISessionSendView{}, newMCPAISessionErr(aiErrStorage, "監査を書けないため送信を止めています / the audit log cannot be written; lines are refused", nil)
	}
	g := s.Gate()
	if g == nil {
		return mcpAISessionSendView{}, newMCPAISessionErr(aiErrSessionBusy, "the session is still opening", nil)
	}
	s.touch()
	reason := args.Reason
	if reason == "" {
		reason = s.reason
	}
	settings := a.aiMgr().Settings()
	res, lerr := a.sendSessionLine(ctx, caller, g, aiSendLineInput{
		Line: args.Line, Reason: reason, ClientRequestID: args.ClientRequestID, ForceQueue: args.ForceQueue,
		TimeoutSeconds: args.TimeoutSeconds, Wait: args.waitDuration(), BypassMaxRisk: settings.BypassMaxRisk,
	})
	if lerr != nil {
		return mcpAISessionSendView{}, aiMCPErr(lerr)
	}
	req := res.Request
	view := mcpAISessionSendView{
		RequestID: req.ID, SessionID: s.id, Seq: res.Seq, State: req.Status, Risk: res.Class.Risk,
		RiskReasons: res.Class.Reasons, RequiresApproval: res.RequiresApproval, Effect: res.Class.Effect,
		Cwd: res.Cwd, CwdPersisted: res.Class.CwdPersisted, SessionState: s.State(), Deduplicated: res.Deduplicated,
		Notice: res.Class.Notice,
	}
	if view.Effect == "" {
		view.Effect = lineEffectNone
	}
	rec, found := g.record(req.ID)
	if found {
		view.CursorBefore = rec.CursorStart
	}
	if req.Status == statePending {
		view.ExpiresAt = req.ExpiresAt
	}
	if requestIsFinal(req.Status) {
		end := s.out.TextEnd()
		if found && rec.CursorEnd >= rec.CursorStart && rec.CursorEnd > 0 {
			end = rec.CursorEnd
		}
		view.CursorAfter = &end
		view.ExitCode = req.ExitCode
		if req.ErrorKind != "" && req.ErrorKind != "exit_nonzero" {
			view.LineErrorKind = req.ErrorKind
		}
		if read, err := s.out.ReadText(view.CursorBefore, aiLineOutputMax); err == nil {
			if read.StartCursor+uint64(len(read.Text)) > end {
				read.Text = string(trimIncompleteRune([]byte(read.Text[:end-read.StartCursor])))
			}
			view.Output = read.Text
			view.OutputTruncated = read.MissedBytes > 0 || read.StartCursor+uint64(len(read.Text)) < end
		}
	}
	return view, nil
}

func (a *App) aiSessionMCPReadOutput(ctx context.Context, owner string, args readOutputArgs) (mcpAISessionReadView, error) {
	s, err := a.ownedSession(owner, args.SessionID)
	if s == nil {
		return mcpAISessionReadView{}, err
	}
	open := err == nil
	if open {
		s.touch()
	}
	since := uint64(0)
	fromStart := args.SinceCursor == nil
	if !fromStart {
		since = *args.SinceCursor
	}
	if args.WaitSeconds > 0 && open && since >= s.out.TextEnd() {
		waitCtx, cancel := context.WithTimeout(ctx, args.waitDuration())
		s.out.WaitText(waitCtx, since)
		cancel()
	}
	read, rerr := s.out.ReadText(since, args.MaxBytes)
	if rerr != nil {
		return mcpAISessionReadView{}, aiMCPErr(rerr)
	}
	if fromStart {
		read.MissedBytes = 0
	}
	sn := s.snap()
	view := mcpAISessionReadView{
		Text: read.Text, StartCursor: read.StartCursor, Cursor: read.Cursor, EndCursor: read.EndCursor,
		MissedBytes: read.MissedBytes, More: read.More, SessionState: sn.State, Redactions: s.out.Redactions(),
	}
	if sn.Base == aiStateClosing || sn.Base == aiStateClosed {
		view.CloseReason = sn.CloseReason
	}
	if sn.HasGate {
		view.Cwd = sn.Gate.Cwd
		var last *aiLineRecord
		if sn.Gate.Inflight != nil {
			last = sn.Gate.Inflight
		} else if n := len(sn.Gate.History); n > 0 {
			last = &sn.Gate.History[n-1]
		}
		if last != nil {
			ref := &mcpAISessionLineRef{RequestID: last.RequestID, Seq: last.Seq, State: last.State, ExitCode: last.ExitCode}
			if req, ok := a.findRequest(last.RequestID); ok {
				ref.State, ref.ExitCode = req.Status, req.ExitCode
			}
			view.Line = ref
		}
	}
	return view, nil
}

func (a *App) aiSessionMCPInterrupt(ctx context.Context, owner string, sessionID string) (mcpAISessionInterruptView, error) {
	s, err := a.ownedSession(owner, sessionID)
	if err != nil {
		return mcpAISessionInterruptView{}, err
	}
	interrupted, reqID, ierr := s.interrupt()
	if ierr != nil && aiSessErrorCode(ierr) != "" {
		return mcpAISessionInterruptView{}, aiMCPErr(ierr)
	}
	view := mcpAISessionInterruptView{SessionID: s.id, Interrupted: interrupted}
	if interrupted {
		deadline := time.NewTimer(aiInterruptHintWait)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
	wait:
		for s.State() == aiStateBusy {
			select {
			case <-deadline.C:
				view.Hint = "the prompt did not come back; call close_session"
				break wait
			case <-ctx.Done():
				break wait
			case <-s.Done():
				break wait
			case <-tick.C:
			}
		}
	}
	if reqID != "" {
		ref := &mcpAISessionLineRef{RequestID: reqID, State: stateRunning}
		if req, ok := a.findRequest(reqID); ok {
			ref.State, ref.ExitCode = req.Status, req.ExitCode
		}
		view.Line = ref
	}
	return view, nil
}

func (a *App) aiSessionMCPClose(ctx context.Context, owner string, sessionID string) (mcpAISessionCloseView, error) {
	s, err := a.ownedSession(owner, sessionID)
	if s == nil {
		return mcpAISessionCloseView{}, err
	}
	already := err != nil
	if !already {
		if !s.close(aiCloseAgent, aiClosedByAgent(owner)) {
			already = true
		}
	}
	<-s.Done()
	sn := s.snap()
	view := mcpAISessionCloseView{SessionID: s.id, Closed: true, CloseReason: sn.CloseReason, OutputBytes: s.out.TextEnd(), AlreadyClosed: already}
	if sn.HasGate {
		view.Lines = sn.Gate.Seq
	}
	return view, nil
}

func (a *App) aiSessionMCPList(owner string, includeClosed bool) ([]mcpAISessionListItem, error) {
	infos := a.listAISessionsFor(owner, includeClosed)
	out := make([]mcpAISessionListItem, 0, len(infos))
	for _, info := range infos {
		out = append(out, mcpAISessionListItem{
			Owner: info.Agent, SessionID: info.ID, Connection: info.ConnName, Mode: info.Mode, State: info.State,
			Cwd: info.Cwd, OpenedAt: info.OpenedAt, LastActivityAt: info.LastActivityAt, IdleExpiresAt: info.IdleExpiresAt,
			ExpiresAt: info.ExpiresAt, Lines: info.Lines, PendingRequestID: info.PendingRequestID,
			CloseReason: info.CloseReason, Tainted: info.Tainted,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Wiring helpers for existing files (§9.2)

// aiSessionDisconnectNote is the message suffix DisconnectConnection adds.
func aiSessionDisconnectNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("（AI セッション %d 件も終了しました）", n)
}

// aiLineDeleteRefusal is DeleteCommandRequest's check: a line of a session
// that is still open cannot be deleted.
func (a *App) aiLineDeleteRefusal(id string) (ActionResult, bool) {
	if a.aiLineOpen(strings.TrimSpace(id)) {
		return ActionResult{OK: false, Code: "ai_session_open", Message: "AI セッションが開いている間は、その行を削除できません"}, true
	}
	return ActionResult{}, false
}
