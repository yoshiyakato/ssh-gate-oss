package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// SecretStore is where passwords (and, when the user opts in, key
// passphrases) live instead of the connections.credential column
// (phase5-keychain.md). Implementations: the OS keychain (osSecretStore) and
// memory (memorySecretStore: session-only secrets and tests).
//
// Every method returns by the time ctx is done, even if the OS call behind it
// hangs (Linux Secret Service prompts wait without a timeout). Callers must
// not hold a.mu, termMu or pool.mu while calling (design-v1 §5.0).
type SecretStore interface {
	Get(ctx context.Context, account string) (string, error)
	Set(ctx context.Context, account, secret string) error
	// Delete removes the item. A missing item is not an error.
	Delete(ctx context.Context, account string) error
}

var (
	// ErrSecretNotFound: the item does not exist (deleted in Keychain Access,
	// database copied to another machine).
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretUnavailable: unsupported OS, no D-Bus, unlock refused, timeout,
	// or a read-back that did not match what was written.
	ErrSecretUnavailable = errors.New("secret store unavailable")
	// ErrSecretTooBig: the OS refused the size (Windows: 2560 bytes).
	ErrSecretTooBig = errors.New("secret too big")
)

const (
	defaultKeyringService = "ssh-gete"
	// keyringServiceEnv overrides the service name (test/dev builds).
	keyringServiceEnv = "SSH_GETE_KEYRING_SERVICE"
	// keyringBackendEnv = "memory" keeps every secret in process memory only.
	// Tests set it so they never touch the real keychain; it never writes
	// plaintext anywhere, so it is safe to honour in release builds too.
	keyringBackendEnv = "SSH_GETE_KEYRING"
)

// keyringServiceName is the service under which every item is stored.
func keyringServiceName() string {
	if s := strings.TrimSpace(os.Getenv(keyringServiceEnv)); s != "" {
		return s
	}
	return defaultKeyringService
}

// defaultSecretStore is what NewApp should install: the OS keychain, or the
// memory store when SSH_GETE_KEYRING=memory.
func defaultSecretStore() SecretStore {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(keyringBackendEnv)), "memory") {
		return newMemorySecretStore()
	}
	return newOSSecretStore(keyringServiceName())
}

// passwordAccount is the account name of a connection's password. It is
// keyed by the immutable connection ID, so renames keep the item and no host
// or user name shows up in the keychain.
func passwordAccount(connID string) string {
	return "conn/" + connID + "/password"
}

// passphraseAccount is the account name of a saved key passphrase: the
// connection ID plus the first 16 hex digits of sha256(expanded key path),
// so changing the key path gives a different item (design-v1 §5.5).
func passphraseAccount(connID, keyPath string) string {
	sum := sha256.Sum256([]byte(expandPath(keyPath)))
	return "conn/" + connID + "/passphrase/" + hex.EncodeToString(sum[:])[:16]
}

// storeSecretVerified writes secret and reads it back, so a value mangled on
// the way (encoding prefixes, truncation) is never treated as saved. Only a
// nil return means the secret may be removed from anywhere else.
func storeSecretVerified(ctx context.Context, store SecretStore, account, secret string) error {
	if store == nil {
		return ErrSecretUnavailable
	}
	if err := store.Set(ctx, account, secret); err != nil {
		return err
	}
	got, err := store.Get(ctx, account)
	if err != nil {
		if errors.Is(err, ErrSecretNotFound) {
			return fmt.Errorf("%w: item missing right after it was written", ErrSecretUnavailable)
		}
		return err
	}
	if got != secret {
		return fmt.Errorf("%w: read-back did not match", ErrSecretUnavailable)
	}
	return nil
}

// lookupSecret resolves account from the session-only store first (values the
// user chose to use "for this session only", or entered while the keychain was
// unavailable), then from the persistent store. Either may be nil.
func lookupSecret(ctx context.Context, session, persistent SecretStore, account string) (string, error) {
	if session != nil {
		v, err := session.Get(ctx, account)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrSecretNotFound) {
			return "", err
		}
	}
	if persistent == nil {
		return "", ErrSecretNotFound
	}
	return persistent.Get(ctx, account)
}

// --- OS keychain ---------------------------------------------------------

// keyringBackend is the blocking OS call layer. The default calls
// github.com/zalando/go-keyring; tests inject a fake (keyring.MockInit swaps
// a package global and races under -race, so it is not used).
type keyringBackend interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type goKeyring struct{}

func (goKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (goKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (goKeyring) Delete(service, user string) error        { return keyring.Delete(service, user) }

// osSecretStore adapts a blocking keyringBackend to SecretStore:
//   - each OS call runs in its own goroutine; the caller returns
//     ErrSecretUnavailable when ctx ends first, and the goroutine is left to
//     finish on its own;
//   - calls for one account are serialized (a Get after a Set sees it), and at
//     most one goroutine per account is ever stuck in the OS: Set/Delete wait
//     for the account in the caller (bounded by ctx), and concurrent Gets of
//     one account share a single call (singleflight).
type osSecretStore struct {
	service string
	backend keyringBackend

	mu    sync.Mutex
	locks map[string]*accountLock
	gets  map[string]*secretCall
}

type accountLock struct {
	ch   chan struct{} // capacity 1: holding the token = owning the account
	refs int
}

type secretCall struct {
	done  chan struct{}
	value string
	err   error
}

func newOSSecretStore(service string) SecretStore {
	return newOSSecretStoreWith(service, goKeyring{})
}

func newOSSecretStoreWith(service string, backend keyringBackend) *osSecretStore {
	return &osSecretStore{
		service: service,
		backend: backend,
		locks:   map[string]*accountLock{},
		gets:    map[string]*secretCall{},
	}
}

func (s *osSecretStore) ref(account string) *accountLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.locks[account]
	if l == nil {
		l = &accountLock{ch: make(chan struct{}, 1)}
		s.locks[account] = l
	}
	l.refs++
	return l
}

func (s *osSecretStore) unref(account string, l *accountLock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l.refs--
	if l.refs == 0 {
		delete(s.locks, account)
	}
}

func unavailable(ctx context.Context) error {
	return fmt.Errorf("%w: %v", ErrSecretUnavailable, context.Cause(ctx))
}

func (s *osSecretStore) Get(ctx context.Context, account string) (string, error) {
	if account == "" {
		return "", ErrSecretNotFound
	}
	if err := ctx.Err(); err != nil {
		return "", unavailable(ctx)
	}
	s.mu.Lock()
	call := s.gets[account]
	if call == nil {
		call = &secretCall{done: make(chan struct{})}
		s.gets[account] = call
		s.mu.Unlock()
		l := s.ref(account)
		go func() {
			l.ch <- struct{}{} // detached: waits for an in-flight Set/Delete
			v, err := s.backend.Get(s.service, account)
			<-l.ch
			s.unref(account, l)
			call.value, call.err = v, mapKeyringError(err)
			s.mu.Lock()
			delete(s.gets, account)
			s.mu.Unlock()
			close(call.done)
		}()
	} else {
		s.mu.Unlock()
	}
	select {
	case <-call.done:
		return call.value, call.err
	case <-ctx.Done():
		return "", unavailable(ctx)
	}
}

func (s *osSecretStore) Set(ctx context.Context, account, secret string) error {
	if account == "" {
		return fmt.Errorf("%w: empty account", ErrSecretUnavailable)
	}
	return s.write(ctx, account, func() error { return s.backend.Set(s.service, account, secret) })
}

func (s *osSecretStore) Delete(ctx context.Context, account string) error {
	if account == "" {
		return nil
	}
	err := s.write(ctx, account, func() error { return s.backend.Delete(s.service, account) })
	if errors.Is(err, ErrSecretNotFound) {
		return nil
	}
	return err
}

func (s *osSecretStore) write(ctx context.Context, account string, op func() error) error {
	if err := ctx.Err(); err != nil {
		return unavailable(ctx)
	}
	l := s.ref(account)
	select {
	case l.ch <- struct{}{}:
	case <-ctx.Done():
		s.unref(account, l)
		return unavailable(ctx)
	}
	result := make(chan error, 1)
	go func() {
		err := op()
		<-l.ch
		s.unref(account, l)
		result <- mapKeyringError(err)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return unavailable(ctx)
	}
}

func mapKeyringError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return ErrSecretNotFound
	case errors.Is(err, keyring.ErrSetDataTooBig):
		return ErrSecretTooBig
	default:
		return fmt.Errorf("%w: %v", ErrSecretUnavailable, err)
	}
}

// --- memory --------------------------------------------------------------

// memorySecretStore keeps secrets in process memory only: the "use for this
// session only" choice, and tests. Safe for concurrent use.
type memorySecretStore struct {
	mu    sync.RWMutex
	items map[string]string
}

func newMemorySecretStore() *memorySecretStore {
	return &memorySecretStore{items: map[string]string{}}
}

func (m *memorySecretStore) Get(ctx context.Context, account string) (string, error) {
	if ctx.Err() != nil {
		return "", unavailable(ctx)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.items[account]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (m *memorySecretStore) Set(ctx context.Context, account, secret string) error {
	if ctx.Err() != nil {
		return unavailable(ctx)
	}
	if account == "" {
		return fmt.Errorf("%w: empty account", ErrSecretUnavailable)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[account] = secret
	return nil
}

func (m *memorySecretStore) Delete(ctx context.Context, account string) error {
	if ctx.Err() != nil {
		return unavailable(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, account)
	return nil
}

// deletePrefix forgets every item whose account starts with prefix, e.g.
// "conn/<id>/" when a connection is deleted or its target changes.
func (m *memorySecretStore) deletePrefix(prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.items {
		if strings.HasPrefix(k, prefix) {
			delete(m.items, k)
		}
	}
}

// connSecretPrefix is the account prefix shared by all of a connection's
// items (for memorySecretStore.deletePrefix).
func connSecretPrefix(connID string) string {
	return "conn/" + connID + "/"
}
