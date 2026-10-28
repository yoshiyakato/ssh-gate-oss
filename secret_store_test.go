package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// faultSecretStore wraps another SecretStore and lets a test fail, delay or
// hang individual calls. hook is called before every operation with the
// operation name ("get", "set", "delete"), the account and the 1-based call
// number of that operation; a non-nil error is returned instead of calling
// inner. hang makes every call block until ctx is done.
type faultSecretStore struct {
	inner SecretStore
	hook  func(op, account string, n int) error
	hang  atomic.Bool

	mu     sync.Mutex
	counts map[string]int
}

func newFaultSecretStore(inner SecretStore) *faultSecretStore {
	return &faultSecretStore{inner: inner, counts: map[string]int{}}
}

func (f *faultSecretStore) before(ctx context.Context, op, account string) error {
	f.mu.Lock()
	f.counts[op]++
	n := f.counts[op]
	hook := f.hook
	f.mu.Unlock()
	if f.hang.Load() {
		<-ctx.Done()
		return unavailable(ctx)
	}
	if hook != nil {
		return hook(op, account, n)
	}
	return nil
}

func (f *faultSecretStore) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[op]
}

func (f *faultSecretStore) Get(ctx context.Context, account string) (string, error) {
	if err := f.before(ctx, "get", account); err != nil {
		return "", err
	}
	return f.inner.Get(ctx, account)
}

func (f *faultSecretStore) Set(ctx context.Context, account, secret string) error {
	if err := f.before(ctx, "set", account); err != nil {
		return err
	}
	return f.inner.Set(ctx, account, secret)
}

func (f *faultSecretStore) Delete(ctx context.Context, account string) error {
	if err := f.before(ctx, "delete", account); err != nil {
		return err
	}
	return f.inner.Delete(ctx, account)
}

// fakeKeyring is an in-memory keyringBackend whose calls can be blocked.
type fakeKeyring struct {
	mu    sync.Mutex
	items map[string]string
	gets  atomic.Int32
	sets  atomic.Int32
	// gate, when non-nil, blocks every call until it is closed.
	gate     chan struct{}
	setErr   error
	inflight atomic.Int32
	maxIn    atomic.Int32
}

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{items: map[string]string{}} }

func (k *fakeKeyring) wait() {
	n := k.inflight.Add(1)
	for {
		m := k.maxIn.Load()
		if n <= m || k.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	k.mu.Lock()
	gate := k.gate
	k.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (k *fakeKeyring) done() { k.inflight.Add(-1) }

func (k *fakeKeyring) Set(service, user, password string) error {
	k.sets.Add(1)
	k.wait()
	defer k.done()
	if k.setErr != nil {
		return k.setErr
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[service+":"+user] = password
	return nil
}

func (k *fakeKeyring) Get(service, user string) (string, error) {
	k.gets.Add(1)
	k.wait()
	defer k.done()
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[service+":"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *fakeKeyring) Delete(service, user string) error {
	k.wait()
	defer k.done()
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.items[service+":"+user]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.items, service+":"+user)
	return nil
}

func TestMemorySecretStore(t *testing.T) {
	ctx := context.Background()
	m := newMemorySecretStore()
	if _, err := m.Get(ctx, "a"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing item: %v", err)
	}
	if err := m.Set(ctx, "a", "pw\n日本語"); err != nil {
		t.Fatal(err)
	}
	if v, err := m.Get(ctx, "a"); err != nil || v != "pw\n日本語" {
		t.Fatalf("get = %q, %v", v, err)
	}
	if err := m.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "a"); err != nil {
		t.Fatalf("deleting a missing item: %v", err)
	}
	m.Set(ctx, "conn/x/password", "1")
	m.Set(ctx, "conn/x/passphrase/abc", "2")
	m.Set(ctx, "conn/y/password", "3")
	m.deletePrefix(connSecretPrefix("x"))
	if _, err := m.Get(ctx, "conn/x/passphrase/abc"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("deletePrefix kept an item")
	}
	if v, _ := m.Get(ctx, "conn/y/password"); v != "3" {
		t.Fatalf("deletePrefix removed another connection's item")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Get(cancelled, "conn/y/password"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("cancelled ctx: %v", err)
	}
}

func TestMemorySecretStoreConcurrent(t *testing.T) {
	ctx := context.Background()
	m := newMemorySecretStore()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			account := fmt.Sprintf("conn/%d/password", i%4)
			for j := 0; j < 50; j++ {
				_ = m.Set(ctx, account, fmt.Sprint(j))
				_, _ = m.Get(ctx, account)
				if j%10 == 0 {
					_ = m.Delete(ctx, account)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestOSSecretStoreRoundTripAndErrors(t *testing.T) {
	ctx := context.Background()
	k := newFakeKeyring()
	s := newOSSecretStoreWith("ssh-gete-test", k)
	if _, err := s.Get(ctx, "conn/a/password"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Set(ctx, "conn/a/password", "secret"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(ctx, "conn/a/password"); err != nil || v != "secret" {
		t.Fatalf("get = %q, %v", v, err)
	}
	if k.items["ssh-gete-test:conn/a/password"] != "secret" {
		t.Fatalf("service/account not passed through: %v", k.items)
	}
	if err := s.Delete(ctx, "conn/a/password"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "conn/a/password"); err != nil {
		t.Fatalf("delete of missing item: %v", err)
	}

	k.setErr = keyring.ErrSetDataTooBig
	if err := s.Set(ctx, "conn/a/password", "x"); !errors.Is(err, ErrSecretTooBig) {
		t.Fatalf("too big: %v", err)
	}
	k.setErr = keyring.ErrUnsupportedPlatform
	if err := s.Set(ctx, "conn/a/password", "x"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("unsupported platform: %v", err)
	}
	if err := s.Set(ctx, "", "x"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("empty account: %v", err)
	}
}

// A hung OS call (Linux prompt) must not hang the caller: it returns at the
// deadline, and later calls for the account neither pile up goroutines in the
// OS nor return before their own deadline.
func TestOSSecretStoreTimeoutAndSingleflight(t *testing.T) {
	k := newFakeKeyring()
	k.items["svc:conn/a/password"] = "pw"
	k.gate = make(chan struct{})
	s := newOSSecretStoreWith("svc", k)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.Get(ctx, "conn/a/password"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("hung get: %v", err)
	}
	if late := time.Since(start) - 100*time.Millisecond; late > 50*time.Millisecond {
		t.Fatalf("returned %v after the deadline", late)
	}

	// Ten concurrent Gets join the one still in flight.
	var wg sync.WaitGroup
	results := make([]string, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results[i], _ = s.Get(c, "conn/a/password")
		}(i)
	}
	// A Set while the Get is stuck waits for the account and gives up at its
	// own deadline without starting a second OS call.
	setCtx, setCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer setCancel()
	if err := s.Set(setCtx, "conn/a/password", "new"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("set behind a hung call: %v", err)
	}
	if n := k.sets.Load(); n != 0 {
		t.Fatalf("set reached the OS while the account was busy (%d calls)", n)
	}
	time.Sleep(20 * time.Millisecond)
	close(k.gate)
	wg.Wait()
	if n := k.gets.Load(); n != 1 {
		t.Fatalf("OS Get calls = %d, want 1 (singleflight)", n)
	}
	for i, v := range results {
		if v != "pw" {
			t.Fatalf("joined get %d = %q", i, v)
		}
	}
	if m := k.maxIn.Load(); m != 1 {
		t.Fatalf("max concurrent OS calls = %d, want 1", m)
	}
	// After the hung call finished the store works normally again.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := storeSecretVerified(ctx2, s, "conn/a/password", "new"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	leftover := len(s.locks) + len(s.gets)
	s.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("per-account state not released: %d entries", leftover)
	}
}

func TestOSSecretStoreConcurrentAccounts(t *testing.T) {
	s := newOSSecretStoreWith("svc", newFakeKeyring())
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			account := passwordAccount(fmt.Sprint(i % 4))
			for j := 0; j < 20; j++ {
				if err := s.Set(ctx, account, fmt.Sprint(j)); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Get(ctx, account); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestSecretAccountNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := passwordAccount("0123abcd"); got != "conn/0123abcd/password" {
		t.Fatalf("passwordAccount = %q", got)
	}
	abs := filepath.Join(home, ".ssh", "id_ed25519")
	cases := []struct {
		name       string
		a, b       [2]string // connID, keyPath
		wantEquals bool
	}{
		{"tilde expands to the same key", [2]string{"c1", "~/.ssh/id_ed25519"}, [2]string{"c1", abs}, true},
		{"quoted path is the same key", [2]string{"c1", `"` + abs + `"`}, [2]string{"c1", abs}, true},
		{"other key path", [2]string{"c1", "~/.ssh/id_ed25519"}, [2]string{"c1", "~/.ssh/id_rsa"}, false},
		{"other connection", [2]string{"c1", abs}, [2]string{"c2", abs}, false},
	}
	for _, tc := range cases {
		x := passphraseAccount(tc.a[0], tc.a[1])
		y := passphraseAccount(tc.b[0], tc.b[1])
		if (x == y) != tc.wantEquals {
			t.Errorf("%s: %q vs %q", tc.name, x, y)
		}
	}
	got := passphraseAccount("c1", abs)
	if want := "conn/c1/passphrase/"; len(got) != len(want)+16 || got[:len(want)] != want {
		t.Fatalf("passphraseAccount shape = %q", got)
	}
	if containsAny(got, "id_ed25519", home) {
		t.Fatalf("account leaks the key path: %q", got)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// mangleStore returns a different value than was written (an encoding bug).
type mangleStore struct{ *memorySecretStore }

func (m mangleStore) Get(ctx context.Context, account string) (string, error) {
	v, err := m.memorySecretStore.Get(ctx, account)
	return v + "~", err
}

func TestStoreSecretVerified(t *testing.T) {
	ctx := context.Background()
	if err := storeSecretVerified(ctx, newMemorySecretStore(), "a", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := storeSecretVerified(ctx, mangleStore{newMemorySecretStore()}, "a", "pw"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("mismatched read-back: %v", err)
	}
	if err := storeSecretVerified(ctx, nil, "a", "pw"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("nil store: %v", err)
	}
	f := newFaultSecretStore(newMemorySecretStore())
	f.hook = func(op, _ string, _ int) error {
		if op == "set" {
			return ErrSecretTooBig
		}
		return nil
	}
	if err := storeSecretVerified(ctx, f, "a", "pw"); !errors.Is(err, ErrSecretTooBig) {
		t.Fatalf("set error not passed through: %v", err)
	}
}

func TestLookupSecret(t *testing.T) {
	ctx := context.Background()
	session, persistent := newMemorySecretStore(), newMemorySecretStore()
	persistent.Set(ctx, "a", "saved")
	if v, err := lookupSecret(ctx, session, persistent, "a"); err != nil || v != "saved" {
		t.Fatalf("persistent = %q, %v", v, err)
	}
	session.Set(ctx, "a", "session")
	if v, _ := lookupSecret(ctx, session, persistent, "a"); v != "session" {
		t.Fatalf("session value must win, got %q", v)
	}
	if _, err := lookupSecret(ctx, session, nil, "b"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing: %v", err)
	}
	down := newFaultSecretStore(persistent)
	down.hook = func(string, string, int) error { return ErrSecretUnavailable }
	if _, err := lookupSecret(ctx, nil, down, "a"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("unavailable: %v", err)
	}
}

func TestDefaultSecretStoreFromEnv(t *testing.T) {
	t.Setenv(keyringBackendEnv, "memory")
	if _, ok := defaultSecretStore().(*memorySecretStore); !ok {
		t.Fatalf("SSH_GETE_KEYRING=memory did not give the memory store")
	}
	t.Setenv(keyringBackendEnv, "")
	t.Setenv(keyringServiceEnv, "ssh-gete-dev")
	s, ok := defaultSecretStore().(*osSecretStore)
	if !ok || s.service != "ssh-gete-dev" {
		t.Fatalf("service override not applied: %#v", s)
	}
	t.Setenv(keyringServiceEnv, "")
	if keyringServiceName() != "ssh-gete" {
		t.Fatalf("default service = %q", keyringServiceName())
	}
}

// E3: outside a test binary SSH_GETE_KEYRING=memory must not switch saves to
// a store that loses every password on restart.
func TestMemoryKeyringOnlyInTestBinaries(t *testing.T) {
	t.Setenv(keyringServiceEnv, "")
	if _, ok := selectSecretStore("memory", true).(*memorySecretStore); !ok {
		t.Fatalf("test binary: memory store expected")
	}
	for _, backend := range []string{"memory", " MEMORY ", ""} {
		if _, ok := selectSecretStore(backend, false).(*osSecretStore); !ok {
			t.Fatalf("release build with %q: OS keychain expected", backend)
		}
	}
}
