package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Host key verification is trust-on-first-use against a known_hosts file owned
// by ssh-gete (appConfigDir()/known_hosts, mode 0600). Only a connection the
// human started from the GUI may record an unknown key, and never in strict
// mode; an agent (MCP) dialing an unknown host gets hostkey_unknown. A
// mismatch is always refused.
//
// hostKeyMu is a leaf lock: it is never held together with a.mu, and nothing
// is called back while holding it (§5.0).

const knownHostsFileName = "known_hosts"

// hostKeyOrigin says who started the dial, which decides whether an unknown
// host key may be recorded.
type hostKeyOrigin int

const (
	hostKeyFromGUI hostKeyOrigin = iota
	hostKeyFromMCP
)

// hostKeyPolicy is passed to openSSHClientWithPolicy. Strict
// (SSH_GETE_STRICT_HOSTKEY / StrictHostKey) additionally trusts the user's
// ~/.ssh/known_hosts and never records: an unknown host fails with
// hostkey_unknown for every origin, as before TOFU existed. Mismatch handling
// is the same either way.
type hostKeyPolicy struct {
	Origin hostKeyOrigin
	Strict bool
}

var (
	hostKeyMu sync.Mutex
	// hostKeyGen changes whenever the known_hosts file is written, so a
	// future connection pool can fold it into its fingerprint (§5.1).
	hostKeyGen atomic.Uint64
)

// hostKeyUnknownError: the host is not in any known_hosts file and this dial
// may not record it. The message deliberately avoids "host key" so the
// string-based fallback in classifyExecError does not report "hostkey".
type hostKeyUnknownError struct {
	Address     string
	Fingerprint string
}

func (e *hostKeyUnknownError) Error() string {
	return fmt.Sprintf("hostkey_unknown: %s is not trusted yet (presented %s); connect once from the ssh-gete GUI to trust it", e.Address, e.Fingerprint)
}

// hostKeyMismatchError: known_hosts has keys for the host and none matches,
// or the presented key is revoked.
type hostKeyMismatchError struct {
	Address     string
	Fingerprint string
	Known       []string
	Revoked     bool
}

func (e *hostKeyMismatchError) Error() string {
	if e.Revoked {
		return fmt.Sprintf("host key mismatch: %s presented a revoked key %s", e.Address, e.Fingerprint)
	}
	return fmt.Sprintf("host key mismatch: %s presented %s, known_hosts has %v", e.Address, e.Fingerprint, e.Known)
}

// hostKeyErrorKind maps host key failures to the MCP error_kind vocabulary.
// It returns "" for any other error; classifyExecError should consult it
// before its string matching.
func hostKeyErrorKind(err error) string {
	var unknown *hostKeyUnknownError
	var mismatch *hostKeyMismatchError
	switch {
	case errors.As(err, &unknown):
		return "hostkey_unknown"
	case errors.As(err, &mismatch):
		return "hostkey"
	}
	return ""
}

// HostKeyEntry is one recorded key for an address.
type HostKeyEntry struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

// HostKeyInfo is what the GUI shows for a connection: the normalized address
// and the keys ssh-gete has recorded for it (empty when never trusted).
type HostKeyInfo struct {
	ConnID  string         `json:"connId"`
	Address string         `json:"address"`
	Known   bool           `json:"known"`
	Keys    []HostKeyEntry `json:"keys"`
}

func knownHostsPath() string {
	return filepath.Join(appConfigDir(), knownHostsFileName)
}

// hostKeyGeneration returns a counter that changes on every known_hosts write.
func hostKeyGeneration() uint64 {
	return hostKeyGen.Load()
}

// hostKeyAddress is the address dialing, recording, lookup and reset all use.
// The host is lowercased (DNS names are case-insensitive and OpenSSH writes
// them lowercased), so "Web01" and "web01" share one TOFU identity.
func hostKeyAddress(conn Connection) string {
	port := conn.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimSpace(conn.Host)), strconv.Itoa(port))
}

// ensureKnownHostsFile creates the file (0600, directory 0700) if missing and
// tightens the mode of an existing one. Caller holds hostKeyMu.
func ensureKnownHostsFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	return os.Chmod(path, 0o600)
}

// hostKeyFiles lists the files to verify against. The app file always comes
// first and always exists afterwards; ~/.ssh/known_hosts is read in strict
// mode only, because a stale line there could not be reset from the GUI.
func hostKeyFiles(strict bool) ([]string, error) {
	path := knownHostsPath()
	hostKeyMu.Lock()
	err := ensureKnownHostsFile(path)
	hostKeyMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("prepare %s: %w", path, err)
	}
	files := []string{path}
	if strict {
		user := expandPath("~/.ssh/known_hosts")
		if _, err := os.Stat(user); err == nil {
			files = append(files, user)
		}
	}
	return files, nil
}

// probeKey is never in any known_hosts file; checking it reveals the keys that
// are known for an address through KeyError.Want.
var probeKey = sync.OnceValue(func() ssh.PublicKey {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		panic(err)
	}
	return key
})

// knownKeysFor returns the recorded keys for addr according to check.
func knownKeysFor(check ssh.HostKeyCallback, addr string) []knownhosts.KnownKey {
	err := check(addr, &net.TCPAddr{}, probeKey())
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		return keyErr.Want
	}
	return nil
}

// hostKeyAlgorithmsFor limits negotiation to the key types already recorded,
// so a server that also has another key type is not misread as a mismatch.
// It returns nil (library default) for an unknown host.
func hostKeyAlgorithmsFor(known []knownhosts.KnownKey) []string {
	var algos []string
	seen := map[string]bool{}
	add := func(names ...string) {
		for _, name := range names {
			if !seen[name] {
				seen[name] = true
				algos = append(algos, name)
			}
		}
	}
	for _, k := range known {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			add(t)
		}
	}
	return algos
}

// newHostKeyVerifier builds the HostKeyCallback for one dial plus the host key
// algorithms to offer for addr.
func newHostKeyVerifier(addr string, policy hostKeyPolicy) (ssh.HostKeyCallback, []string, error) {
	files, err := hostKeyFiles(policy.Strict)
	if err != nil {
		return nil, nil, err
	}
	check, err := knownhosts.New(files...)
	if err != nil {
		return nil, nil, fmt.Errorf("load known_hosts: %w", err)
	}
	known := knownKeysFor(check, addr)
	algos := hostKeyAlgorithmsFor(known)
	if anyCertAuthority(known, certAuthorityLines(files)) {
		// A @cert-authority entry's key is the CA's, not the host's: limiting
		// negotiation to its plain type would keep the server from presenting
		// its certificate. Use the library defaults, which offer cert algorithms.
		algos = nil
	}
	callback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := check(hostname, remote, key)
		if err == nil {
			return nil
		}
		fingerprint := ssh.FingerprintSHA256(key)
		var revoked *knownhosts.RevokedError
		if errors.As(err, &revoked) {
			return &hostKeyMismatchError{Address: knownhosts.Normalize(hostname), Fingerprint: fingerprint, Revoked: true}
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) > 0 {
			known := make([]string, 0, len(keyErr.Want))
			for _, k := range keyErr.Want {
				known = append(known, k.Key.Type()+" "+ssh.FingerprintSHA256(k.Key))
			}
			return &hostKeyMismatchError{Address: knownhosts.Normalize(hostname), Fingerprint: fingerprint, Known: known}
		}
		if policy.Origin != hostKeyFromGUI || policy.Strict {
			return &hostKeyUnknownError{Address: knownhosts.Normalize(hostname), Fingerprint: fingerprint}
		}
		return recordHostKey(hostname, key)
	}
	return callback, algos, nil
}

// certAuthorityLines returns "file:line" for every @cert-authority line of
// files, numbered the way knownhosts.KnownKey reports them.
func certAuthorityLines(files []string) map[string]bool {
	lines := map[string]bool{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for n := 1; scanner.Scan(); n++ {
			if bytes.HasPrefix(bytes.TrimSpace(scanner.Bytes()), []byte("@cert-authority")) {
				lines[file+":"+strconv.Itoa(n)] = true
			}
		}
	}
	return lines
}

func anyCertAuthority(known []knownhosts.KnownKey, caLines map[string]bool) bool {
	for _, k := range known {
		if caLines[k.Filename+":"+strconv.Itoa(k.Line)] {
			return true
		}
	}
	return false
}

// recordHostKey appends key for hostname unless a concurrent dial already
// recorded a key for it, in which case the key must match that one.
func recordHostKey(hostname string, key ssh.PublicKey) error {
	path := knownHostsPath()
	hostKeyMu.Lock()
	defer hostKeyMu.Unlock()
	if err := ensureKnownHostsFile(path); err != nil {
		return err
	}
	// Re-check under the lock: another GUI dial may have won the race.
	check, err := knownhosts.New(path)
	if err != nil {
		return err
	}
	err = check(hostname, &net.TCPAddr{}, key)
	if err == nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return err
	}
	if len(keyErr.Want) > 0 {
		return &hostKeyMismatchError{Address: knownhosts.Normalize(hostname), Fingerprint: ssh.FingerprintSHA256(key)}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key) + "\n"
	_, writeErr := f.WriteString(line)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	hostKeyGen.Add(1)
	return nil
}

// recordedHostKeys reads the plain (unhashed) lines of the app known_hosts
// file that name addr.
func recordedHostKeys(addr string) ([]HostKeyEntry, error) {
	path := knownHostsPath()
	hostKeyMu.Lock()
	data, err := os.ReadFile(path)
	hostKeyMu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return []HostKeyEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	want := knownhosts.Normalize(addr)
	entries := []HostKeyEntry{}
	forEachKnownHostsLine(data, func(_ []byte, hosts []string, key ssh.PublicKey) {
		if key != nil && containsString(hosts, want) {
			entries = append(entries, HostKeyEntry{Type: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)})
		}
	})
	return entries, nil
}

// removeHostKeys rewrites the app known_hosts file without the lines that name
// addr and reports how many were dropped. Other lines are kept byte for byte.
func removeHostKeys(addr string) (int, error) {
	path := knownHostsPath()
	hostKeyMu.Lock()
	defer hostKeyMu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	want := knownhosts.Normalize(addr)
	var kept bytes.Buffer
	removed := 0
	forEachKnownHostsLine(data, func(raw []byte, hosts []string, key ssh.PublicKey) {
		if key != nil && containsString(hosts, want) {
			removed++
			return
		}
		kept.Write(raw)
		kept.WriteByte('\n')
	})
	if removed == 0 {
		return 0, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), knownHostsFileName+".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if _, err := tmp.Write(kept.Bytes()); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, err
	}
	hostKeyGen.Add(1)
	return removed, nil
}

// forEachKnownHostsLine calls fn for every line; key is nil for blank,
// comment, marker (@revoked/@cert-authority) and unparsable lines.
func forEachKnownHostsLine(data []byte, fn func(raw []byte, hosts []string, key ssh.PublicKey)) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Bytes()
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			fn(raw, nil, nil)
			continue
		}
		marker, hosts, key, _, _, err := ssh.ParseKnownHosts(trimmed)
		if err != nil || marker != "" {
			fn(raw, nil, nil)
			continue
		}
		fn(raw, hosts, key)
	}
}

// containsString matches host entries case-insensitively, so a reset also
// drops a line recorded before hosts were lowercased.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// hostKeyConnection resolves connID to an SSH connection.
func (a *App) hostKeyConnection(connID string) (Connection, error) {
	conn, ok := a.connectionByID(connID)
	if !ok {
		return Connection{}, fmt.Errorf("接続先 %s が見つかりません", connID)
	}
	if conn.connectionType() == "Serial" {
		return Connection{}, errors.New("シリアル接続にはホスト鍵がありません")
	}
	return conn, nil
}

// hostKeyFingerprint returns the keys recorded for connID's host:port. It does
// not touch the network.
func (a *App) hostKeyFingerprint(connID string) (HostKeyInfo, error) {
	conn, err := a.hostKeyConnection(connID)
	if err != nil {
		return HostKeyInfo{}, err
	}
	addr := hostKeyAddress(conn)
	keys, err := recordedHostKeys(addr)
	if err != nil {
		return HostKeyInfo{}, err
	}
	return HostKeyInfo{ConnID: conn.ID, Address: knownhosts.Normalize(addr), Known: len(keys) > 0, Keys: keys}, nil
}

// resetHostKey forgets the recorded keys for connID's host:port so the next
// GUI connection records the current key. Every connection sharing that
// host:port is affected. It returns the number of lines removed.
func (a *App) resetHostKey(connID string) (int, error) {
	conn, err := a.hostKeyConnection(connID)
	if err != nil {
		return 0, err
	}
	return removeHostKeys(hostKeyAddress(conn))
}
