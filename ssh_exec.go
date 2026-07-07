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
	"time"

	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 {
		return len(p), nil
	}
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if b.buf.Len() >= b.max {
		return b.buf.String() + "\n[ssh-gete: output truncated]"
	}
	return b.buf.String()
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
	session.Stdin = io.Reader(strings.NewReader(""))

	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return commandResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			Duration: time.Since(start),
		}, ctx.Err()
	case err := <-done:
		return commandResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			Duration: time.Since(start),
		}, err
	}
}

func openSSHClient(ctx context.Context, conn Connection, strictHostKey bool) (*ssh.Client, error) {
	if conn.Port == 0 {
		conn.Port = 22
	}
	if conn.ConnectTimeout <= 0 {
		conn.ConnectTimeout = 10
	}
	auth, err := sshAuthMethods(conn)
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := hostKeyCallback(strictHostKey)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:            conn.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         time.Duration(conn.ConnectTimeout) * time.Second,
	}
	addr := net.JoinHostPort(conn.Host, strconv.Itoa(conn.Port))
	dialer := &net.Dialer{Timeout: time.Duration(conn.ConnectTimeout) * time.Second}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, config)
	if err != nil {
		_ = netConn.Close()
		return nil, err
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

func sshAuthMethods(conn Connection) ([]ssh.AuthMethod, error) {
	if strings.Contains(conn.AuthMethod, "パスワード") {
		if conn.Credential == "" {
			return nil, errors.New("password credential is empty")
		}
		return []ssh.AuthMethod{ssh.Password(conn.Credential)}, nil
	}

	auth := agentAuthMethods()
	keyPath := conn.Credential
	if keyPath == "" {
		keyPath = "~/.ssh/id_ed25519"
	}
	keyPath = expandPath(keyPath)
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if len(auth) > 0 {
			return auth, nil
		}
		return nil, fmt.Errorf("read ssh key %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		if conn.KeyPassphrase != "" {
			signer, passphraseErr := ssh.ParsePrivateKeyWithPassphrase(key, []byte(conn.KeyPassphrase))
			if passphraseErr == nil {
				return append([]ssh.AuthMethod{ssh.PublicKeys(signer)}, auth...), nil
			}
			return nil, fmt.Errorf("parse ssh key %s with passphrase: %w", keyPath, passphraseErr)
		}
		return nil, fmt.Errorf("parse ssh key %s: %w", keyPath, err)
	}
	return append([]ssh.AuthMethod{ssh.PublicKeys(signer)}, auth...), nil
}

// agentAuthMethods returns an ssh-agent backed auth method when an agent is
// reachable. The actual transport differs per OS (Unix socket vs Windows
// named pipe) and is provided by dialSSHAgent in the platform-specific files.
func agentAuthMethods() []ssh.AuthMethod {
	conn, err := dialSSHAgent()
	if err != nil || conn == nil {
		return nil
	}
	return []ssh.AuthMethod{ssh.PublicKeysCallback(sshagent.NewClient(conn).Signers)}
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
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
