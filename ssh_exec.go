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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
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

func testSSHConnection(ctx context.Context, conn Connection, strictHostKey bool) error {
	client, err := openSSHClient(ctx, conn, strictHostKey)
	if err != nil {
		return err
	}
	return client.Close()
}

func runSSHCommand(ctx context.Context, conn Connection, command string, outputCap int, strictHostKey bool) (commandResult, error) {
	start := time.Now()
	client, err := openSSHClient(ctx, conn, strictHostKey)
	if err != nil {
		return commandResult{Duration: time.Since(start)}, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return commandResult{Duration: time.Since(start)}, err
	}
	defer session.Close()

	stdout := &cappedBuffer{max: outputCap}
	stderr := &cappedBuffer{max: outputCap}
	session.Stdout = stdout
	session.Stderr = stderr
	session.Stdin = strings.NewReader("")

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

func openSSHClient(ctx context.Context, conn Connection, strictHostKey bool) (*ssh.Client, error) {
	if conn.Port == 0 {
		conn.Port = 22
	}
	if conn.ConnectTimeout <= 0 {
		conn.ConnectTimeout = 10
	}
	auth, closeAgent, err := sshAuthMethods(conn)
	if err != nil {
		return nil, err
	}
	// The agent is only needed while authenticating.
	defer closeAgent()
	hostKeyCallback, err := hostKeyCallback(strictHostKey)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(conn.ConnectTimeout) * time.Second
	config := &ssh.ClientConfig{
		User:            conn.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}
	addr := net.JoinHostPort(conn.Host, strconv.Itoa(conn.Port))
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 15 * time.Second}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// ClientConfig.Timeout only bounds ssh.Dial's TCP connect; a server that
	// accepts but never sends a banner would otherwise hang the handshake.
	_ = netConn.SetDeadline(time.Now().Add(timeout))
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
func sshAuthMethods(conn Connection) ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	if conn.usesPassword() {
		if conn.Credential == "" {
			return nil, noop, errors.New("password credential is empty")
		}
		return []ssh.AuthMethod{ssh.Password(conn.Credential)}, noop, nil
	}

	agentClient, closeAgent := dialAgentClient()
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
	}
	closeAgent()
	return nil, noop, &passphraseMissingError{keyPath: keyPath}
}

// dialAgentClient connects to the platform ssh-agent (Unix socket or Windows
// named pipe, see dialSSHAgent). It returns a nil client when none is
// reachable; the close func is always safe to call.
func dialAgentClient() (sshagent.ExtendedAgent, func()) {
	conn, err := dialSSHAgent()
	if err != nil || conn == nil {
		return nil, func() {}
	}
	var once sync.Once
	return sshagent.NewClient(conn), func() { once.Do(func() { _ = conn.Close() }) }
}

func hostKeyCallback(strict bool) (ssh.HostKeyCallback, error) {
	if !strict && os.Getenv("SSH_GETE_STRICT_HOSTKEY") != "1" {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	knownHostsPath := expandPath("~/.ssh/known_hosts")
	if _, err := os.Stat(knownHostsPath); err == nil {
		return knownhosts.New(knownHostsPath)
	}
	return nil, fmt.Errorf("known_hosts not found: %s", knownHostsPath)
}

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
			return filepath.Join(home, path[2:])
		}
	}
	return os.ExpandEnv(expandWindowsEnv(path))
}

// expandWindowsEnv turns %VAR% into $VAR for os.ExpandEnv.
func expandWindowsEnv(path string) string {
	for {
		start := strings.IndexByte(path, '%')
		if start < 0 {
			return path
		}
		end := strings.IndexByte(path[start+1:], '%')
		if end < 0 {
			return path
		}
		name := path[start+1 : start+1+end]
		path = path[:start] + "${" + name + "}" + path[start+2+end:]
	}
}

// splitHostPortLoose accepts "host:port", "[v6]:port" and bare IPv6 "::1:8787"
// is rejected as ambiguous by net.SplitHostPort, which is the desired result.
func splitHostPortLoose(addr string) (string, string, error) {
	return net.SplitHostPort(addr)
}

var _ io.Writer = (*cappedBuffer)(nil)
