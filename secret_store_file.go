package main

// Secret stores for headless servers (phase6-headless.md §7):
//
//   - fileSecretStore: <dataDir>/secrets.json (0600) for Linux servers without
//     a keychain. Used only when explicitly chosen.
//   - envSecretStore: maps accounts to environment variables (containers,
//     systemd LoadCredential), layered over any backend.
//   - denySecretStore: the read-only lower layer of `secrets.backend: env`.
//   - selectSecretBackend / installSecretBackend: decide the backend before
//     NewApp, so the keychain migration in NewApp moves into the chosen store.
//
// passwordFile values are read by config.go (readSecretFile).
//
// All of them keep the SecretStore contract: they return by the time ctx is
// done, Delete of a missing item is not an error, and they are called without
// a.mu / termMu / pool.mu held. File operations are fast, so they are not
// wrapped in goroutines.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const (
	secretFileVersion = 1
	// secretFileItemMax / secretFileMax bound one item and the whole file
	// (§7.2). Larger values give ErrSecretTooBig.
	secretFileItemMax = 64 << 10
	secretFileMax     = 8 << 20
	// defaultSecretFileName is the file under the data directory.
	defaultSecretFileName = "secrets.json"

	secretBackendKeychain = "keychain"
	secretBackendFile     = "file"
	secretBackendEnv      = "env"
	secretBackendMemory   = "memory"

	// keychainUnavailableHint is the startup log line for a keychain backend
	// that does not work on this machine (§7.1).
	keychainUnavailableHint = "パスワードは保存できません。鍵認証か ssh-agent を使うか、secrets.backend: file / env を選んでください"
)

// --- file ----------------------------------------------------------------

type secretFileContent struct {
	Version int               `json:"version"`
	Items   map[string]string `json:"items"`
}

// fileSecretStore keeps every item in one JSON file, unencrypted (the key
// would have to live next to it). The file is read once (lazily, and again
// after a failed attempt, so a `chmod 600` takes effect without a restart)
// and then served from memory. Writes go to a 0600 temp file in the same
// directory, fsync, rename, fsync of the directory.
type fileSecretStore struct {
	path string

	mu     sync.Mutex
	loaded bool
	items  map[string]string

	// rename is os.Rename; tests replace it to simulate a failed swap.
	rename func(oldpath, newpath string) error
}

// newFileSecretStore returns the store for path (absolute or relative to
// the working directory). It creates the parent directory (0700) if needed.
// An existing file that is unreadable, has a mode wider than 0600 (Unix) or
// is malformed is reported in the log and through ErrSecretUnavailable on
// every call until it is fixed; it is never overwritten.
func newFileSecretStore(path string) (*fileSecretStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("secret file path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("secret file path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create secret file directory: %w", err)
	}
	s := &fileSecretStore{path: abs, rename: os.Rename}
	s.mu.Lock()
	err = s.loadLocked()
	s.mu.Unlock()
	if err != nil {
		log.Printf("secrets file: %v", err)
	}
	return s, nil
}

// loadLocked reads the file into memory once. A missing file is an empty
// store. Callers hold s.mu.
func (s *fileSecretStore) loadLocked() error {
	if s.loaded {
		return nil
	}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.items, s.loaded = map[string]string{}, true
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: open %s: %v", ErrSecretUnavailable, s.path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrSecretUnavailable, s.path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrSecretUnavailable, s.path)
	}
	if err := checkPrivateMode(s.path, info); err != nil {
		return err
	}
	if info.Size() > secretFileMax {
		return fmt.Errorf("%w: %s is larger than %d bytes", ErrSecretTooBig, s.path, secretFileMax)
	}
	data, err := io.ReadAll(io.LimitReader(f, secretFileMax+1))
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrSecretUnavailable, s.path, err)
	}
	if len(data) > secretFileMax {
		return fmt.Errorf("%w: %s is larger than %d bytes", ErrSecretTooBig, s.path, secretFileMax)
	}
	var content secretFileContent
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&content); err != nil {
		return fmt.Errorf("%w: %s is not a valid secrets file: %v", ErrSecretUnavailable, s.path, err)
	}
	if content.Version != secretFileVersion {
		return fmt.Errorf("%w: %s has version %d, want %d", ErrSecretUnavailable, s.path, content.Version, secretFileVersion)
	}
	if content.Items == nil {
		content.Items = map[string]string{}
	}
	s.items, s.loaded = content.Items, true
	return nil
}

// checkPrivateMode refuses a file that group or other may access (Unix; the
// mode bits mean nothing on Windows, where the data directory ACL applies).
func checkPrivateMode(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %04o; run `chmod 600 %s`", ErrSecretUnavailable, path, perm, path)
	}
	return nil
}

func (s *fileSecretStore) Get(ctx context.Context, account string) (string, error) {
	if account == "" {
		return "", ErrSecretNotFound
	}
	if ctx.Err() != nil {
		return "", unavailable(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", err
	}
	v, ok := s.items[account]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (s *fileSecretStore) Set(ctx context.Context, account, secret string) error {
	if account == "" {
		return fmt.Errorf("%w: empty account", ErrSecretUnavailable)
	}
	if len(account)+len(secret) > secretFileItemMax {
		return ErrSecretTooBig
	}
	if ctx.Err() != nil {
		return unavailable(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if v, ok := s.items[account]; ok && v == secret {
		return nil
	}
	next := make(map[string]string, len(s.items)+1)
	for k, v := range s.items {
		next[k] = v
	}
	next[account] = secret
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.items = next
	return nil
}

func (s *fileSecretStore) Delete(ctx context.Context, account string) error {
	if account == "" {
		return nil
	}
	if ctx.Err() != nil {
		return unavailable(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	if _, ok := s.items[account]; !ok {
		return nil
	}
	next := make(map[string]string, len(s.items))
	for k, v := range s.items {
		if k != account {
			next[k] = v
		}
	}
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.items = next
	return nil
}

// writeLocked replaces the file with items atomically. On any error the old
// file (and the in-memory copy, which the caller only swaps on success) is
// left as it was. Callers hold s.mu.
func (s *fileSecretStore) writeLocked(items map[string]string) error {
	data, err := json.MarshalIndent(secretFileContent{Version: secretFileVersion, Items: items}, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode secrets: %v", ErrSecretUnavailable, err)
	}
	data = append(data, '\n')
	if len(data) > secretFileMax {
		return ErrSecretTooBig
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("%w: create temp file: %v", ErrSecretUnavailable, err)
	}
	tmpName := tmp.Name()
	fail := func(step string, err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("%w: %s: %v", ErrSecretUnavailable, step, err)
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fail("chmod temp file", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write temp file", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("fsync temp file", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("%w: close temp file: %v", ErrSecretUnavailable, err)
	}
	if err := s.rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("%w: replace %s: %v", ErrSecretUnavailable, s.path, err)
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename durable. Windows cannot open a directory for
// fsync (and NTFS journals the rename), so it is skipped there.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	d.Close()
}

// --- env -----------------------------------------------------------------

// envSecretStore serves bound accounts from environment variables and
// everything else from lower. Environment variables cannot be written, so:
//   - Get of a bound account reads the variable (unset or empty: not found);
//   - Set of a bound account succeeds without doing anything when the value
//     equals the variable (so storeSecretVerified passes and the DB keeps
//     only credentialSet), and fails with ErrSecretUnavailable otherwise;
//   - Delete of a bound account is a no-op.
//
// Bindings are rebuilt in memory on every start (applyConfig) and never
// persisted. Safe for concurrent use.
type envSecretStore struct {
	lower  SecretStore
	lookup func(string) (string, bool)

	mu       sync.RWMutex
	bindings map[string]string // account -> environment variable name
}

// newEnvSecretStore wraps lower (nil: denySecretStore). lookup defaults to
// os.LookupEnv.
func newEnvSecretStore(lower SecretStore, lookup func(string) (string, bool)) *envSecretStore {
	if lower == nil {
		lower = denySecretStore{}
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &envSecretStore{lower: lower, lookup: lookup, bindings: map[string]string{}}
}

// bind maps account to the environment variable envName. An empty envName
// removes the binding.
func (e *envSecretStore) bind(account, envName string) {
	envName = strings.TrimSpace(envName)
	e.mu.Lock()
	defer e.mu.Unlock()
	if envName == "" {
		delete(e.bindings, account)
		return
	}
	e.bindings[account] = envName
}

// bound is the number of bound accounts (for the startup log).
func (e *envSecretStore) bound() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.bindings)
}

func (e *envSecretStore) binding(account string) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	name, ok := e.bindings[account]
	return name, ok
}

func (e *envSecretStore) Get(ctx context.Context, account string) (string, error) {
	name, ok := e.binding(account)
	if !ok {
		return e.lower.Get(ctx, account)
	}
	if ctx.Err() != nil {
		return "", unavailable(ctx)
	}
	if v, ok := e.lookup(name); ok && v != "" {
		return v, nil
	}
	return "", ErrSecretNotFound
}

func (e *envSecretStore) Set(ctx context.Context, account, secret string) error {
	name, ok := e.binding(account)
	if !ok {
		return e.lower.Set(ctx, account, secret)
	}
	if ctx.Err() != nil {
		return unavailable(ctx)
	}
	if v, ok := e.lookup(name); ok && v != "" && v == secret {
		return nil
	}
	return fmt.Errorf("%w: the value comes from environment variable %s and cannot be changed here", ErrSecretUnavailable, name)
}

func (e *envSecretStore) Delete(ctx context.Context, account string) error {
	if _, ok := e.binding(account); ok {
		return nil
	}
	return e.lower.Delete(ctx, account)
}

// denySecretStore holds nothing and refuses writes: the lower layer of
// `secrets.backend: env`, where only bound environment variables exist.
type denySecretStore struct{}

func (denySecretStore) Get(ctx context.Context, account string) (string, error) {
	if ctx.Err() != nil {
		return "", unavailable(ctx)
	}
	return "", ErrSecretNotFound
}

func (denySecretStore) Set(ctx context.Context, account, secret string) error {
	return fmt.Errorf("%w: secrets.backend is env (read-only)", ErrSecretUnavailable)
}

func (denySecretStore) Delete(ctx context.Context, account string) error { return nil }

// --- selection -----------------------------------------------------------

// memorySecretsAllowed reports whether the memory backend may be chosen.
// It loses every saved password on restart, so, like defaultSecretStore on
// main (REQ-014: no silent fallback; phase5-keychain.md), it is honoured only
// inside a test binary. Tests may replace it to check the release behaviour.
var memorySecretsAllowed = testing.Testing

// keyringEnvOverride is the backend forced by SSH_GETE_KEYRING (file | env,
// and memory in a test binary), or "" when the variable is unset, has another
// value, or is memory outside tests (then a warning is logged once per call).
func keyringEnvOverride() string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(keyringBackendEnv))); v {
	case secretBackendFile, secretBackendEnv:
		return v
	case secretBackendMemory:
		if memorySecretsAllowed() {
			return v
		}
		log.Printf("warning: %s=memory is ignored outside tests; choose secrets.backend: keychain, file or env", keyringBackendEnv)
	}
	return ""
}

// effectiveSecretBackend returns the backend that will be used: the
// SSH_GETE_KEYRING environment variable (file | env; memory only in tests)
// wins over the configured one (tests, emergencies; other values of the
// variable are ignored, and match config.go's secretsBackend). An empty
// configured value means keychain; an unknown one, or memory outside tests,
// is an error (exit code 2).
func effectiveSecretBackend(configured string) (string, error) {
	if v := keyringEnvOverride(); v != "" {
		return v, nil
	}
	v := strings.ToLower(strings.TrimSpace(configured))
	if v == "" {
		return secretBackendKeychain, nil
	}
	if !validSecretBackend(v) {
		return "", fmt.Errorf("secrets.backend %q: want keychain, file or env", configured)
	}
	return v, nil
}

// validSecretBackend: memory is valid only in a test binary.
func validSecretBackend(v string) bool {
	switch v {
	case secretBackendKeychain, secretBackendFile, secretBackendEnv:
		return true
	case secretBackendMemory:
		return memorySecretsAllowed()
	}
	return false
}

// secretFilePath resolves `secrets.file`: empty is <dataDir>/secrets.json, a
// relative path is relative to dataDir. dataDir empty is appConfigDir().
func secretFilePath(file, dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = appConfigDir()
	}
	file = strings.TrimSpace(file)
	if file == "" {
		return filepath.Join(dataDir, defaultSecretFileName)
	}
	file = expandPath(file)
	if !filepath.IsAbs(file) {
		file = filepath.Join(dataDir, file)
	}
	return file
}

// selectSecretBackend builds the store for `secrets.backend` / `secrets.file`
// (the fields of SecretsConfig, passed as strings so this file does not
// depend on config_file.go). The returned SecretStore is the envSecretStore
// itself, layered over the chosen backend, so environment bindings work with
// every backend (§7.1); install it with installSecretBackend before NewApp
// and give the *envSecretStore to applyConfig.
func selectSecretBackend(backend, file, dataDir string) (SecretStore, *envSecretStore, error) {
	if strings.TrimSpace(file) != "" && !strings.EqualFold(strings.TrimSpace(backend), secretBackendFile) {
		return nil, nil, errors.New("secrets.file is only used with secrets.backend: file")
	}
	name, err := effectiveSecretBackend(backend)
	if err != nil {
		return nil, nil, err
	}
	var lower SecretStore
	switch name {
	case secretBackendKeychain:
		lower = newOSSecretStore(keyringServiceName())
	case secretBackendMemory:
		lower = newMemorySecretStore()
	case secretBackendEnv:
		lower = denySecretStore{}
	case secretBackendFile:
		fs, err := newFileSecretStore(secretFilePath(file, dataDir))
		if err != nil {
			return nil, nil, err
		}
		lower = fs
	}
	env := newEnvSecretStore(lower, nil)
	return env, env, nil
}

// installSecretBackend makes defaultSecretStore (and so NewApp) return s.
// nil restores the default (OS keychain / SSH_GETE_KEYRING=memory). Call it
// before NewApp, from one goroutine.
func installSecretBackend(s SecretStore) {
	if s == nil {
		secretBackendFactory = nil
		return
	}
	secretBackendFactory = func() SecretStore { return s }
}

// probeSecretStore reports whether s answers at all: a Get of an account that
// never exists must give ErrSecretNotFound. Anything else (no D-Bus, locked
// keyring, bad file mode) means passwords cannot be saved; log
// keychainUnavailableHint and keep running (§7.1).
func probeSecretStore(ctx context.Context, s SecretStore) error {
	if s == nil {
		return ErrSecretUnavailable
	}
	_, err := s.Get(ctx, "probe/ssh-gete-availability")
	if err == nil || errors.Is(err, ErrSecretNotFound) {
		return nil
	}
	return err
}
