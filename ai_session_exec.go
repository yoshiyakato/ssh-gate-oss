package main

// Exec mode transport for AI sessions (phase7-ai-session.md §1.1). This is
// the default mode: the session owns one ssh.Client and every line runs on a
// fresh exec channel as `/bin/sh -s`, with the agent's line on stdin (never
// in the exec command string). The gateway keeps cwd and the environment
// overlay and prefixes them to every line (buildExecScript), so a cd or an
// export carries over while aliases, functions, traps and shell options do
// not. stdin is closed after the script, so programs that read it get EOF
// instead of hanging.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	// aiExecInterruptGrace is how long a line may ignore SIGINT before its
	// channel is closed (§2.5: 3 seconds).
	aiExecInterruptGrace = 3 * time.Second
	// aiExecOpenTimeout bounds opening one exec channel.
	aiExecOpenTimeout = 15 * time.Second
	// aiExecLineCaptureMax caps the per-line stdout / stderr record before
	// MaxOutputKB truncation.
	aiExecLineCaptureMax = 1 << 20
)

// aiExecCwdGoneCode is the exit status the script uses when the gateway's cwd
// no longer exists on the remote side.
const aiExecCwdGoneCode = 201

// aiExecCommand is the fixed exec command string (no agent input).
func aiExecCommand(shell string) string {
	if shell == aiShellBash {
		return "exec env -u BASH_ENV -u ENV bash --noprofile --norc -s"
	}
	return "exec env -u BASH_ENV -u ENV /bin/sh -s"
}

// aiShQuote single-quotes s for a POSIX shell.
func aiShQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildExecScript is the stdin of one exec-mode line (§1.1): the gateway's
// cd and environment overlay, then exactly one agent line. With cdProbe the
// line is a standalone cd and the script reports the new directory in a
// marker the transport strips from the output.
func buildExecScript(cwd string, env map[string]string, unset []string, line string, nonce string, cdProbe bool) []byte {
	var b bytes.Buffer
	if cwd != "" {
		b.WriteString("cd -- " + aiShQuote(cwd) + " 2>/dev/null || { printf '\\036SGERR:%s:cwd\\036' " + aiShQuote(nonce) + " >&2; exit 201; }\n")
	}
	names := make([]string, 0, len(env))
	for name := range env {
		if validEnvName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString("export " + name + "=" + aiShQuote(env[name]) + "\n")
	}
	for _, name := range unset {
		if validEnvName(name) {
			if _, set := env[name]; !set {
				b.WriteString("unset " + name + "\n")
			}
		}
	}
	b.WriteString(line)
	if cdProbe {
		b.WriteString(" && printf '\\036SGCWD:%s:%s\\036' " + aiShQuote(nonce) + ` "$(pwd -P)"`)
	}
	b.WriteString("\n")
	return b.Bytes()
}

// aiExecTransport is the exec mode transport. It owns client.
type aiExecTransport struct {
	client    *ssh.Client
	shell     string
	out       io.Writer
	newFilter func() aiTextFilter
	cwd       string

	mu        sync.Mutex // leaf
	cur       *aiExecLine
	done      chan struct{}
	doneOnce  sync.Once
	endReason string
	closeOnce sync.Once
}

type aiExecLine struct {
	session *ssh.Session
	done    chan struct{}
	outcome aiTransportOutcome
	timer   *time.Timer
}

func (l *aiExecLine) Done() <-chan struct{}       { return l.done }
func (l *aiExecLine) Outcome() aiTransportOutcome { <-l.done; return l.outcome }

// newExecTransport takes ownership of client, asks a POSIX shell for the
// initial directory (`pwd -P` on stdin) and starts the keepalive.
func newExecTransport(ctx context.Context, client *ssh.Client, shell string, out io.Writer, newFilter func() aiTextFilter) (aiTransport, error) {
	t := &aiExecTransport{client: client, shell: shell, out: out, newFilter: newFilter, done: make(chan struct{})}
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	session, err := newSessionContext(ctx, client, aiExecOpenTimeout)
	if err != nil {
		t.Close()
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	session.Stdin = strings.NewReader("pwd -P\n")
	session.Stdout = &aiCappedBuffer{buf: &stdout, max: 64 * 1024}
	session.Stderr = &aiCappedBuffer{buf: &stderr, max: 64 * 1024}
	err = session.Run(aiExecCommand(shell))
	_ = session.Close()
	if ctx.Err() != nil {
		t.Close()
		return nil, context.Cause(ctx)
	}
	cwd := strings.TrimSpace(stdout.String())
	if err != nil || !strings.HasPrefix(cwd, "/") || strings.Contains(cwd, "\n") {
		var exitErr *ssh.ExitError
		t.Close()
		if err == nil || errors.As(err, &exitErr) {
			return nil, errAINoPOSIXShell
		}
		return nil, err
	}
	t.cwd = cwd
	go t.keepalive()
	return t, nil
}

func newAIExecNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// StartLine runs one line on a new exec channel.
func (t *aiExecTransport) StartLine(ctx context.Context, plan aiLinePlan) (aiLineHandle, error) {
	if strings.ContainsAny(plan.Line, "\r\n") {
		// validateSessionLine already refuses these; never let one through.
		return nil, errors.New("line contains a newline")
	}
	t.mu.Lock()
	select {
	case <-t.done:
		t.mu.Unlock()
		return nil, errAITransportClosed
	default:
	}
	if t.cur != nil {
		t.mu.Unlock()
		return nil, errAITransportBusy
	}
	l := &aiExecLine{done: make(chan struct{})}
	t.cur = l
	t.mu.Unlock()

	fail := func(err error) (aiLineHandle, error) {
		t.mu.Lock()
		if t.cur == l {
			t.cur = nil
		}
		t.mu.Unlock()
		return nil, err
	}
	session, err := newSessionContext(context.Background(), t.client, aiExecOpenTimeout)
	if err != nil {
		t.end(aiCloseConnLost)
		return fail(err)
	}
	l.session = session
	nonce := newAIExecNonce()
	cwdMark := newAIValueMarker("\x1eSGCWD:" + nonce + ":")
	errMark := newAIValueMarker("\x1eSGERR:" + nonce + ":")
	newFilter := t.newFilter
	if newFilter == nil {
		newFilter = func() aiTextFilter { return newAIPlainText() }
	}
	stdout := &aiExecStream{t: t, marker: cwdMark, filter: newFilter()}
	stderr := &aiExecStream{t: t, marker: errMark, filter: newFilter()}
	session.Stdin = bytes.NewReader(buildExecScript(plan.Cwd, plan.Env, plan.Unset, plan.Line, nonce, plan.CdProbe))
	session.Stdout = stdout
	session.Stderr = stderr
	if err := session.Start(aiExecCommand(t.shell)); err != nil {
		_ = session.Close()
		return fail(err)
	}
	stopCtx := context.AfterFunc(ctx, func() { _ = t.Interrupt() })
	go func() {
		err := session.Wait()
		stopCtx()
		_ = session.Close()
		stdout.finish()
		stderr.finish()
		oc := aiTransportOutcome{Stdout: stdout.text(), Stderr: stderr.text()}
		var exitErr *ssh.ExitError
		switch {
		case err == nil:
			code := 0
			oc.ExitCode = &code
		case errors.As(err, &exitErr):
			code := exitErr.ExitStatus()
			oc.ExitCode = &code
		default:
			oc.Err = err
		}
		if len(errMark.values()) > 0 && oc.ExitCode != nil && *oc.ExitCode == aiExecCwdGoneCode {
			oc.Err = errAICwdGone
		}
		if v := cwdMark.values(); len(v) > 0 && oc.ExitCode != nil && *oc.ExitCode == 0 {
			oc.NewCwd = v[len(v)-1]
		}
		t.mu.Lock()
		if t.cur == l {
			t.cur = nil
		}
		if l.timer != nil {
			l.timer.Stop()
		}
		t.mu.Unlock()
		l.outcome = oc
		close(l.done)
	}()
	return l, nil
}

// Interrupt sends SIGINT to the running line; if the line is still running
// aiExecInterruptGrace later, its channel is closed.
func (t *aiExecTransport) Interrupt() error {
	t.mu.Lock()
	l := t.cur
	t.mu.Unlock()
	if l == nil || l.session == nil {
		return nil
	}
	err := l.session.Signal(ssh.SIGINT)
	t.mu.Lock()
	if t.cur == l && l.timer == nil {
		session := l.session
		l.timer = time.AfterFunc(aiExecInterruptGrace, func() { _ = session.Close() })
	}
	t.mu.Unlock()
	return err
}

// interruptGrace is how long after Interrupt the line may still hold the
// transport: the close timer plus a margin for the channel to wind down.
func (t *aiExecTransport) interruptGrace() time.Duration {
	return aiExecInterruptGrace + time.Second
}

func (t *aiExecTransport) Ready() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.done:
		return false
	default:
	}
	return t.cur == nil
}

func (t *aiExecTransport) Cwd() string { return t.cwd }

func (t *aiExecTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		l := t.cur
		t.mu.Unlock()
		if l != nil && l.session != nil {
			_ = l.session.Close()
		}
		_ = t.client.Close()
		t.end(aiCloseConnLost)
	})
	return nil
}

func (t *aiExecTransport) Done() <-chan struct{} { return t.done }

func (t *aiExecTransport) EndReason() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.endReason
}

func (t *aiExecTransport) end(reason string) {
	t.doneOnce.Do(func() {
		t.mu.Lock()
		if t.endReason == "" {
			t.endReason = reason
		}
		t.mu.Unlock()
		close(t.done)
	})
}

// keepalive sends keepalive@openssh.com; two misses in a row end the
// transport with conn_lost (§5).
func (t *aiExecTransport) keepalive() {
	if aiKeepaliveLoop(t.client, t.done) {
		_ = t.client.Close()
		t.end(aiCloseConnLost)
	}
}

// aiKeepaliveLoop pings client until done; it reports true after two misses
// in a row.
func aiKeepaliveLoop(client *ssh.Client, done <-chan struct{}) bool {
	ticker := time.NewTicker(aiKeepaliveInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-done:
			return false
		case <-ticker.C:
		}
		res := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			res <- err
		}()
		timer := time.NewTimer(aiKeepaliveTimeout)
		var err error
		select {
		case err = <-res:
		case <-timer.C:
			err = errors.New("keepalive timeout")
		case <-done:
			timer.Stop()
			return false
		}
		timer.Stop()
		if err == nil {
			misses = 0
			continue
		}
		if misses++; misses >= 2 {
			return true
		}
	}
}

// aiExecStream is one of stdout / stderr of an exec line: markers are
// stripped, the rest goes to the session output (merged in arrival order)
// and to this line's own filtered record.
type aiExecStream struct {
	t      *aiExecTransport
	marker *aiValueMarker
	filter aiTextFilter
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (w *aiExecStream) Write(p []byte) (int, error) {
	data := w.marker.feed(p)
	if len(data) > 0 {
		_, _ = w.t.out.Write(data)
		w.mu.Lock()
		if w.buf.Len() < aiExecLineCaptureMax {
			w.buf.Write(w.filter.Feed(data))
		}
		w.mu.Unlock()
	}
	return len(p), nil
}

func (w *aiExecStream) finish() {
	if rest := w.marker.flush(); len(rest) > 0 {
		_, _ = w.t.out.Write(rest)
		w.mu.Lock()
		w.buf.Write(w.filter.Feed(rest))
		w.mu.Unlock()
	}
	w.mu.Lock()
	if f, ok := w.filter.(aiTextFlusher); ok {
		w.buf.Write(f.Flush())
	}
	w.mu.Unlock()
}

func (w *aiExecStream) text() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte{}, w.buf.Bytes()...)
}

// aiCappedBuffer keeps the first max bytes written.
type aiCappedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (w *aiCappedBuffer) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// aiValueMarker strips "<prefix><value>\x1e" markers (prefix starts with
// \x1e) from a stream, across chunk boundaries, and keeps the values.
// Anything that turns out not to be a marker passes through.
type aiValueMarker struct {
	prefix []byte
	mu     sync.Mutex
	pend   []byte
	vals   []string
}

const aiValueMarkerMax = 8192

func newAIValueMarker(prefix string) *aiValueMarker { return &aiValueMarker{prefix: []byte(prefix)} }

func (m *aiValueMarker) feed(p []byte) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []byte
	queue := p
	for len(queue) > 0 {
		b := queue[0]
		queue = queue[1:]
		if len(m.pend) == 0 {
			if b == m.prefix[0] {
				m.pend = append(m.pend, b)
			} else {
				out = append(out, b)
			}
			continue
		}
		m.pend = append(m.pend, b)
		n := len(m.pend)
		switch {
		case n <= len(m.prefix):
			if m.pend[n-1] != m.prefix[n-1] {
				out = append(out, m.pend[0])
				rest := append([]byte(nil), m.pend[1:]...)
				m.pend = m.pend[:0]
				queue = append(rest, queue...)
			}
		case b == 0x1e:
			m.vals = append(m.vals, string(m.pend[len(m.prefix):n-1]))
			m.pend = m.pend[:0]
		case n > aiValueMarkerMax:
			out = append(out, m.pend...)
			m.pend = m.pend[:0]
		}
	}
	return out
}

// flush returns bytes held back at the end of the stream.
func (m *aiValueMarker) flush() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.pend
	m.pend = nil
	return out
}

func (m *aiValueMarker) values() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.vals...)
}
