package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	sshTestUser     = "tester"
	sshTestPassword = "secret"
)

// sshTestServer is a minimal in-process SSH server for tests. It accepts
// password auth and handles exec requests:
//
//	echo <text>   prints text
//	exit <n>      exits with status n
//	sleep <sec>   waits, then exits 0
//	yes           writes output until the channel closes
//
// Every exec is appended to execLog. RotateHostKey swaps the host key for
// later connections, to exercise known_hosts mismatch handling.
type sshTestServer struct {
	listener net.Listener

	mu      sync.Mutex
	signer  ssh.Signer
	execLog []string
	conns   []net.Conn
}

func newSSHTestServer(t *testing.T) *sshTestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sshTestServer{listener: listener, signer: newTestHostKey(t)}
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

func newTestHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// RotateHostKey replaces the host key (same algorithm) for new connections
// and returns the new public key.
func (s *sshTestServer) RotateHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	signer := newTestHostKey(t)
	s.mu.Lock()
	s.signer = signer
	s.mu.Unlock()
	return signer.PublicKey()
}

// HostKey is the public key currently presented to clients.
func (s *sshTestServer) HostKey() ssh.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signer.PublicKey()
}

func (s *sshTestServer) serverConfig() *ssh.ServerConfig {
	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == sshTestUser && string(password) == sshTestPassword {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	s.mu.Lock()
	config.AddHostKey(s.signer)
	s.mu.Unlock()
	return config
}

func (s *sshTestServer) Close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
}

func (s *sshTestServer) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *sshTestServer) Execs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.execLog...)
}

// Connection returns a password-auth Connection pointing at this server.
func (s *sshTestServer) Connection(name string) Connection {
	return Connection{
		Name:           name,
		Type:           "SSH",
		Host:           "127.0.0.1",
		Port:           s.Port(),
		User:           sshTestUser,
		AuthMethod:     "パスワード保管",
		Credential:     sshTestPassword,
		ConnectTimeout: 5,
		CommandTimeout: 10,
		Tags:           []string{},
	}
}

func (s *sshTestServer) serve() {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, nc)
		s.mu.Unlock()
		go s.handleConn(nc)
	}
}

func (s *sshTestServer) handleConn(nc net.Conn) {
	_, chans, reqs, err := ssh.NewServerConn(nc, s.serverConfig())
	if err != nil {
		_ = nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *sshTestServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		s.mu.Lock()
		s.execLog = append(s.execLog, payload.Command)
		s.mu.Unlock()
		status := s.run(ch, payload.Command)
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], uint32(status))
		_, _ = ch.SendRequest("exit-status", false, buf[:])
		return
	}
}

func (s *sshTestServer) run(ch ssh.Channel, command string) int {
	name, arg, _ := strings.Cut(command, " ")
	switch name {
	case "echo":
		_, _ = fmt.Fprintln(ch, arg)
		return 0
	case "exit":
		n, _ := strconv.Atoi(arg)
		return n
	case "sleep":
		sec, _ := strconv.ParseFloat(arg, 64)
		time.Sleep(time.Duration(sec * float64(time.Second)))
		return 0
	case "yes":
		line := []byte(strings.Repeat("y\n", 512))
		for {
			if _, err := ch.Write(line); err != nil {
				return 0
			}
		}
	default:
		_, _ = fmt.Fprintf(ch.Stderr(), "unknown command: %s\n", command)
		return 127
	}
}

// silentServer accepts TCP connections and never speaks SSH, to exercise the
// handshake deadline.
func newSilentServer(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	return listener.Addr().(*net.TCPAddr).Port
}
