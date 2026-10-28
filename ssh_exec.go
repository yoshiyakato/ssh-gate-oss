package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
)

// cappedBuffer keeps at most max bytes. It is written by the SSH session's
// copy goroutines and read by runSSHCommand, so it is guarded by a mutex.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.max - b.buf.Len()
	if remaining <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
	} else {
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buf.String() + "\n[ssh-gete: output truncated]"
	}
	return b.buf.String()
}

// passphraseMissingError reports an encrypted private key with no passphrase
// available (and no ssh-agent identity to fall back on).
type passphraseMissingError struct {
	keyPath string
}

func (e *passphraseMissingError) Error() string {
	return fmt.Sprintf("ssh key %s is passphrase protected", e.keyPath)
}

// testSSHConnection is the GUI "接続テスト": an unknown host key is recorded
// (TOFU) unless strictHostKey is set.
func testSSHConnection(ctx context.Context, conn Connection, strictHostKey bool) error {
	client, err := openSSHClientWithPolicy(ctx, conn, hostKeyPolicy{Origin: hostKeyFromGUI, Strict: strictHostKey})
	if err != nil {
		return err
	}
	return client.Close()
}

// runSSHCommand serves MCP requests (approved or bypassed), so an unknown host
// key fails with hostKeyUnknownError instead of being recorded.
func runSSHCommand(ctx context.Context, conn Connection, command string, outputCap int, strictHostKey bool) (commandResult, error) {
	start := time.Now()
	client, err := openSSHClientWithPolicy(ctx, conn, hostKeyPolicy{Origin: hostKeyFromMCP, Strict: strictHostKey})
	if err != nil {
		return commandResult{Duration: time.Since(start)}, err
	}
	defer client.Close()

	session, err := newSessionContext(ctx, client, connectTimeout(conn))
	if err != nil {
		return commandResult{Duration: time.Since(start)}, err
	}
	defer session.Close()

	stdout := &cappedBuffer{max: outputCap}
	stderr := &cappedBuffer{max: outputCap}
	session.Stdout = stdout
	session.Stderr = stderr
	session.Stdin = strings.NewReader("")

	// A cancel that lands between the handshake and here must not start the
	// command on the remote side.
	if err := ctx.Err(); err != nil {
		return commandResult{Duration: time.Since(start)}, err
	}
	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		_ = client.Close()
		// Let the copy goroutines drain before reading the buffers.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		runErr = ctx.Err()
	case runErr = <-done:
	}
	result := commandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
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

// openSSHClient dials for a GUI terminal tab.
func openSSHClient(ctx context.Context, conn Connection, strictHostKey bool) (*ssh.Client, error) {
	return openSSHClientWithPolicy(ctx, conn, hostKeyPolicy{Origin: hostKeyFromGUI, Strict: strictHostKey})
}

// connectTimeout is conn.ConnectTimeout with the 10 second default applied.
func connectTimeout(conn Connection) time.Duration {
	if conn.ConnectTimeout <= 0 {
		return 10 * time.Second
	}
	return time.Duration(conn.ConnectTimeout) * time.Second
}

// sessionOpenGrace bounds how long newSessionContext waits for the abandoned
// NewSession call to return after it closed the client.
const sessionOpenGrace = 5 * time.Second

// newSessionContext is client.NewSession bounded by ctx and timeout.
// x/crypto waits for the server's CHANNEL_OPEN reply with no deadline, so a
// server that authenticates and then goes silent would block forever. On
// cancel or timeout the client is closed (which makes the pending open
// return) and must not be reused.
func newSessionContext(ctx context.Context, client *ssh.Client, timeout time.Duration) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	type opened struct {
		session *ssh.Session
		err     error
	}
	result := make(chan opened, 1)
	go func() {
		session, err := client.NewSession()
		result <- opened{session, err}
	}()
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	var err error
	select {
	case r := <-result:
		if r.err == nil && ctx.Err() != nil {
			_ = r.session.Close()
			_ = client.Close()
			return nil, ctx.Err()
		}
		return r.session, r.err
	case <-ctx.Done():
		err = ctx.Err()
	case <-expired:
		err = fmt.Errorf("ssh: server did not open a session within %v", timeout)
	}
	_ = client.Close()
	select {
	case r := <-result:
		if r.session != nil {
			_ = r.session.Close()
		}
	case <-time.After(sessionOpenGrace):
	}
	return nil, err
}

func openSSHClientWithPolicy(ctx context.Context, conn Connection, policy hostKeyPolicy) (*ssh.Client, error) {
	if conn.Port == 0 {
		conn.Port = 22
	}
	timeout := connectTimeout(conn)
	// One deadline covers the ssh-agent conversation and the handshake, so
	// neither a silent server nor a silent agent outlives ConnectTimeout.
	deadline := time.Now().Add(timeout)
	auth, closeAgent, err := sshAuthMethods(ctx, conn, deadline)
	if err != nil {
		return nil, err
	}
	// The agent is only needed while authenticating.
	defer closeAgent()
	if os.Getenv("SSH_GETE_STRICT_HOSTKEY") == "1" {
		policy.Strict = true
	}
	addr := hostKeyAddress(conn)
	hostKeyCallback, hostKeyAlgorithms, err := newHostKeyVerifier(addr, policy)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:              conn.User,
		Auth:              auth,
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: hostKeyAlgorithms,
		Timeout:           timeout,
	}
	dialer := &net.Dialer{Deadline: deadline, KeepAlive: 15 * time.Second}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// ClientConfig.Timeout only bounds ssh.Dial's TCP connect; a server that
	// accepts but never sends a banner would otherwise hang the handshake.
	_ = netConn.SetDeadline(deadline)
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = netConn.Close()
		case <-handshakeDone:
		}
	}()
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, config)
	close(handshakeDone)
	if err != nil {
		_ = netConn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	_ = netConn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// sshAuthMethods builds the auth chain. The returned func closes the
// ssh-agent connection, if one was opened; call it once the handshake ends.
// The agent connection carries deadline and is closed when ctx ends, so a
// stuck agent (an unanswered Touch ID prompt, a wedged bridge) cannot block
// the caller past either.
func sshAuthMethods(ctx context.Context, conn Connection, deadline time.Time) ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	if conn.usesPassword() {
		if conn.Credential == "" {
			return nil, noop, errors.New("password credential is empty")
		}
		return []ssh.AuthMethod{ssh.Password(conn.Credential)}, noop, nil
	}

	agentClient, closeAgent := dialAgentClient(ctx, deadline)
	keyPath := conn.Credential
	if keyPath == "" {
		keyPath = "~/.ssh/id_ed25519"
	}
	keyPath = expandPath(keyPath)
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if agentClient != nil {
			return []ssh.AuthMethod{ssh.PublicKeysCallback(agentClient.Signers)}, closeAgent, nil
		}
		return nil, noop, fmt.Errorf("read ssh key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err == nil {
		methods := []ssh.AuthMethod{ssh.PublicKeys(signer)}
		if agentClient != nil {
			methods = append(methods, ssh.PublicKeysCallback(agentClient.Signers))
		}
		return methods, closeAgent, nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		closeAgent()
		return nil, noop, fmt.Errorf("parse ssh key %s: %w", keyPath, err)
	}
	if conn.KeyPassphrase != "" {
		signer, passErr := ssh.ParsePrivateKeyWithPassphrase(key, []byte(conn.KeyPassphrase))
		if passErr != nil {
			closeAgent()
			return nil, noop, fmt.Errorf("parse ssh key %s with passphrase: %w", keyPath, passErr)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, closeAgent, nil
	}
	// Encrypted key without passphrase: use the agent, offering only the
	// matching identity so unrelated keys do not exhaust MaxAuthTries.
	if agentClient != nil && missing.PublicKey != nil {
		want := missing.PublicKey.Marshal()
		filtered := func() ([]ssh.Signer, error) {
			signers, err := agentClient.Signers()
			if err != nil {
				return nil, err
			}
			for _, s := range signers {
				if bytes.Equal(s.PublicKey().Marshal(), want) {
					return []ssh.Signer{s}, nil
				}
			}
			return nil, &passphraseMissingError{keyPath: keyPath}
		}
		signers, err := filtered()
		if err == nil && len(signers) > 0 {
			return []ssh.AuthMethod{ssh.PublicKeysCallback(filtered)}, closeAgent, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			closeAgent()
			return nil, noop, ctxErr
		}
	}
	closeAgent()
	return nil, noop, &passphraseMissingError{keyPath: keyPath}
}

// agentDialTimeout caps connecting to the ssh-agent socket or pipe; the
// conversation after that is bounded by the caller's deadline.
const agentDialTimeout = 2 * time.Second

// dialAgentClient connects to the platform ssh-agent (Unix socket or Windows
// named pipe, see dialSSHAgent). It returns a nil client when none is
// reachable; the close func is always safe to call, more than once.
// Every read and write on the agent connection fails after deadline, and the
// connection is closed as soon as ctx ends.
func dialAgentClient(ctx context.Context, deadline time.Time) (sshagent.ExtendedAgent, func()) {
	dialDeadline := time.Now().Add(agentDialTimeout)
	if deadline.Before(dialDeadline) {
		dialDeadline = deadline
	}
	dialCtx, cancel := context.WithDeadline(ctx, dialDeadline)
	conn, err := dialSSHAgent(dialCtx)
	cancel()
	if err != nil || conn == nil {
		return nil, func() {}
	}
	_ = conn.SetDeadline(deadline)
	stop := make(chan struct{})
	var once sync.Once
	closeAgent := func() {
		once.Do(func() {
			close(stop)
			_ = conn.Close()
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			closeAgent()
		case <-stop:
		}
	}()
	return sshagent.NewClient(conn), closeAgent
}

// windowsDefaultAgentPipe is the named pipe exposed by the OpenSSH
// Authentication Agent service that ships with Windows.
const windowsDefaultAgentPipe = `\\.\pipe\openssh-ssh-agent`

// normalizeWindowsAgentPipe maps an SSH_AUTH_SOCK value to the named pipe to
// dial. Win32 accepts the pipe namespace in any case, with forward slashes
// (Git Bash often exports //./pipe/name) and via \\?\; all of these become
// \\.\pipe\<name>. ok is false when value does not name a pipe at all (empty,
// or a Unix-style path set by MSYS tooling); the caller then uses
// windowsDefaultAgentPipe.
func normalizeWindowsAgentPipe(value string) (pipe string, ok bool) {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	value = strings.ReplaceAll(value, "/", `\`)
	for _, prefix := range []string{`\\.\pipe\`, `\\?\pipe\`} {
		if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			name := strings.TrimLeft(value[len(prefix):], `\`)
			if name == "" {
				return "", false
			}
			return `\\.\pipe\` + name, true
		}
	}
	return "", false
}

// expandPath turns a user-entered key or known_hosts path into a file path:
// surrounding quotes are dropped, a leading ~ (~/ or ~\) becomes the home
// directory, and $VAR, ${VAR} and %VAR% are replaced only when that variable
// is set and non-empty. A path that already names an existing file is used
// as written, so a literal $ or % in a file name is never rewritten, and an
// unset variable is left in place instead of silently vanishing.
func expandPath(path string) string {
	path = strings.TrimSpace(path)
	if len(path) >= 2 && (path[0] == '"' && path[len(path)-1] == '"' || path[0] == '\'' && path[len(path)-1] == '\'') {
		path = path[1 : len(path)-1]
	}
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		if path == "~" {
			return home
		}
		if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
			path = filepath.Join(home, path[2:])
		}
	}
	if !strings.ContainsAny(path, "$%") {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return expandSetEnv(path)
}

// expandSetEnv replaces $NAME, ${NAME} and %NAME% with the variable's value
// when it is set and non-empty, and leaves every other $ and % untouched.
func expandSetEnv(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); {
		c := path[i]
		if c == '$' {
			if i+1 < len(path) && path[i+1] == '{' {
				if end := strings.IndexByte(path[i+2:], '}'); end >= 0 {
					name := path[i+2 : i+2+end]
					if value, ok := lookupSetEnv(name); ok {
						b.WriteString(value)
						i += 3 + end
						continue
					}
				}
			} else {
				n := envNameLen(path[i+1:])
				if value, ok := lookupSetEnv(path[i+1 : i+1+n]); ok {
					b.WriteString(value)
					i += 1 + n
					continue
				}
			}
		} else if c == '%' {
			if end := strings.IndexByte(path[i+1:], '%'); end >= 0 {
				if value, ok := lookupSetWindowsEnv(path[i+1 : i+1+end]); ok {
					b.WriteString(value)
					i += 2 + end
					continue
				}
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// envNameLen is the length of the [A-Za-z_][A-Za-z0-9_]* prefix of s.
func envNameLen(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || i > 0 && '0' <= c && c <= '9' {
			continue
		}
		return i
	}
	return len(s)
}

func lookupSetEnv(name string) (string, bool) {
	if name == "" || envNameLen(name) != len(name) {
		return "", false
	}
	return lookupNonEmptyEnv(name)
}

// lookupSetWindowsEnv accepts the wider %NAME% syntax (e.g.
// %ProgramFiles(x86)%) but never a name that spans a path separator.
func lookupSetWindowsEnv(name string) (string, bool) {
	if name == "" || strings.ContainsAny(name, `/\:=`) {
		return "", false
	}
	return lookupNonEmptyEnv(name)
}

func lookupNonEmptyEnv(name string) (string, bool) {
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

// splitHostPortLoose accepts "host:port", "[v6]:port" and bare IPv6 "::1:8787"
// is rejected as ambiguous by net.SplitHostPort, which is the desired result.
func splitHostPortLoose(addr string) (string, string, error) {
	return net.SplitHostPort(addr)
}

var _ io.Writer = (*cappedBuffer)(nil)
