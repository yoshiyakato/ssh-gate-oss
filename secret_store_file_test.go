package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func readSecretFileContent(t *testing.T, path string) secretFileContent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c secretFileContent
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return c
}

func TestFileSecretStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "secrets.json")
	s, err := newFileSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "a"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing item: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file created before the first write: %v", err)
	}
	if err := s.Set(ctx, "conn/1/password", "pw\n日本語\"\\"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(ctx, "conn/1/password"); err != nil || v != "pw\n日本語\"\\" {
		t.Fatalf("get = %q, %v", v, err)
	}
	if err := storeSecretVerified(ctx, s, "conn/2/password", "two"); err != nil {
		t.Fatalf("storeSecretVerified: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file mode = %04o, want 0600", perm)
		}
		dirInfo, _ := os.Stat(filepath.Dir(path))
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Fatalf("dir mode = %04o, want 0700", perm)
		}
	}
	c := readSecretFileContent(t, path)
	if c.Version != 1 || c.Items["conn/1/password"] != "pw\n日本語\"\\" || c.Items["conn/2/password"] != "two" {
		t.Fatalf("file content = %+v", c)
	}

	// A new store (next start) reads what was written.
	s2, err := newFileSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s2.Get(ctx, "conn/2/password"); err != nil || v != "two" {
		t.Fatalf("reload get = %q, %v", v, err)
	}
	if err := s2.Delete(ctx, "conn/2/password"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Delete(ctx, "conn/2/password"); err != nil {
		t.Fatalf("deleting a missing item: %v", err)
	}
	if err := s2.Delete(ctx, ""); err != nil {
		t.Fatalf("deleting the empty account: %v", err)
	}
	if _, err := s2.Get(ctx, ""); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("get empty account: %v", err)
	}
	if err := s2.Set(ctx, "", "x"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("set empty account: %v", err)
	}
	if _, ok := readSecretFileContent(t, path).Items["conn/2/password"]; ok {
		t.Fatal("deleted item is still in the file")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s2.Get(cancelled, "conn/1/password"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("cancelled get: %v", err)
	}
	if err := s2.Set(cancelled, "conn/1/password", "x"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("cancelled set: %v", err)
	}
	if err := s2.Delete(cancelled, "conn/1/password"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("cancelled delete: %v", err)
	}
}

func TestFileSecretStoreRefusesWideMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not used on Windows")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.json")
	original := []byte(`{"version":1,"items":{"conn/1/password":"pw"}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o644)
	s, err := newFileSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "conn/1/password"); !errors.Is(err, ErrSecretUnavailable) || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("get from 0644 file: %v", err)
	}
	if err := s.Set(ctx, "conn/2/password", "x"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("set into 0644 file: %v", err)
	}
	if err := s.Delete(ctx, "conn/1/password"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("delete from 0644 file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(original) {
		t.Fatalf("a refused file was rewritten: %s", got)
	}
	// After chmod 600 the same store works without a restart.
	os.Chmod(path, 0o600)
	if v, err := s.Get(ctx, "conn/1/password"); err != nil || v != "pw" {
		t.Fatalf("after chmod: %q, %v", v, err)
	}
}

func TestFileSecretStoreRefusesBadFiles(t *testing.T) {
	ctx := context.Background()
	for name, content := range map[string]string{
		"not json":      "not json",
		"wrong version": `{"version":2,"items":{}}`,
		"unknown field": `{"version":1,"items":{},"extra":1}`,
		"wrong type":    `{"version":1,"items":{"a":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := newFileSecretStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(ctx, "a"); !errors.Is(err, ErrSecretUnavailable) {
				t.Fatalf("get: %v", err)
			}
			if err := s.Set(ctx, "a", "x"); !errors.Is(err, ErrSecretUnavailable) {
				t.Fatalf("set: %v", err)
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Fatalf("a malformed file was rewritten: %s", got)
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secrets.json")
		os.Mkdir(path, 0o700)
		s, err := newFileSecretStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "a"); !errors.Is(err, ErrSecretUnavailable) {
			t.Fatalf("get: %v", err)
		}
	})
	if _, err := newFileSecretStore("  "); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestFileSecretStoreAtomicReplace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := newFileSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "conn/1/password", "old"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	s.rename = func(oldpath, newpath string) error { return errors.New("disk full") }
	if err := s.Set(ctx, "conn/1/password", "new"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("set with a failing rename: %v", err)
	}
	if err := s.Delete(ctx, "conn/1/password"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("delete with a failing rename: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("file changed after a failed replace:\n%s\n%s", before, after)
	}
	if v, _ := s.Get(ctx, "conn/1/password"); v != "old" {
		t.Fatalf("memory copy changed after a failed replace: %q", v)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	s.rename = os.Rename
	if err := s.Set(ctx, "conn/1/password", "new"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(ctx, "conn/1/password"); v != "new" {
		t.Fatalf("after recovery: %q", v)
	}
}

func TestFileSecretStoreLimits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	s, err := newFileSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "big", strings.Repeat("x", secretFileItemMax)); !errors.Is(err, ErrSecretTooBig) {
		t.Fatalf("64 KiB item: %v", err)
	}
	// Fill the memory copy just under 8 MiB (writing it item by item would
	// rewrite the file >100 times), then one more item must be refused.
	item := strings.Repeat("y", secretFileItemMax-100)
	s.mu.Lock()
	for i := 0; i < secretFileMax/(len(item)+40)-1; i++ {
		s.items[fmt.Sprintf("conn/%d/password", i)] = item
	}
	s.mu.Unlock()
	if err := s.Set(ctx, "conn/x/password", "small"); err != nil {
		t.Fatalf("set under the limit: %v", err)
	}
	var lastErr error
	for i := 0; i < 4 && lastErr == nil; i++ {
		lastErr = s.Set(ctx, fmt.Sprintf("conn/extra%d/password", i), item)
	}
	if !errors.Is(lastErr, ErrSecretTooBig) {
		t.Fatalf("filling past 8 MiB: %v", lastErr)
	}
	if info, _ := os.Stat(path); info.Size() > secretFileMax {
		t.Fatalf("file grew to %d bytes", info.Size())
	}

	// An oversized file is not loaded.
	bigPath := filepath.Join(dir, "big.json")
	f, _ := os.OpenFile(bigPath, os.O_CREATE|os.O_WRONLY, 0o600)
	f.Truncate(secretFileMax + 1)
	f.Close()
	big, err := newFileSecretStore(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := big.Get(ctx, "a"); !errors.Is(err, ErrSecretTooBig) {
		t.Fatalf("oversized file: %v", err)
	}
}

func TestFileSecretStoreConcurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := newFileSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			account := fmt.Sprintf("conn/%d/password", i)
			for j := 0; j < 10; j++ {
				if err := storeSecretVerified(ctx, s, account, fmt.Sprint(j)); err != nil {
					t.Errorf("set %s: %v", account, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	c := readSecretFileContent(t, path)
	if len(c.Items) != 8 {
		t.Fatalf("items = %d, want 8", len(c.Items))
	}
	for k, v := range c.Items {
		if v != "9" {
			t.Fatalf("%s = %q, want 9", k, v)
		}
	}
}

func TestEnvSecretStore(t *testing.T) {
	ctx := context.Background()
	env := map[string]string{"PW_WEB1": "from-env", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	lower := newMemorySecretStore()
	e := newEnvSecretStore(lower, lookup)

	e.bind("conn/1/password", "PW_WEB1")
	e.bind("conn/2/password", "UNSET")
	e.bind("conn/3/password", "EMPTY")
	e.bind("conn/4/password", " ")
	if e.bound() != 3 {
		t.Fatalf("bound = %d, want 3", e.bound())
	}

	if v, err := e.Get(ctx, "conn/1/password"); err != nil || v != "from-env" {
		t.Fatalf("bound get = %q, %v", v, err)
	}
	for _, a := range []string{"conn/2/password", "conn/3/password"} {
		if _, err := e.Get(ctx, a); !errors.Is(err, ErrSecretNotFound) {
			t.Fatalf("get %s (unset/empty variable): %v", a, err)
		}
	}
	// Same value: success without writing anywhere; storeSecretVerified passes.
	if err := storeSecretVerified(ctx, e, "conn/1/password", "from-env"); err != nil {
		t.Fatalf("storeSecretVerified with the env value: %v", err)
	}
	if _, err := lower.Get(ctx, "conn/1/password"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatal("bound Set wrote to the lower store")
	}
	if err := e.Set(ctx, "conn/1/password", "other"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("set a different value: %v", err)
	}
	if err := e.Set(ctx, "conn/2/password", ""); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("set an unset variable: %v", err)
	}
	if err := e.Delete(ctx, "conn/1/password"); err != nil {
		t.Fatalf("bound delete: %v", err)
	}
	if v, _ := e.Get(ctx, "conn/1/password"); v != "from-env" {
		t.Fatal("bound delete changed the value")
	}

	// Unbound accounts go to the lower store.
	if err := storeSecretVerified(ctx, e, "conn/9/password", "lower"); err != nil {
		t.Fatal(err)
	}
	if v, _ := lower.Get(ctx, "conn/9/password"); v != "lower" {
		t.Fatalf("lower store = %q", v)
	}
	if err := e.Delete(ctx, "conn/9/password"); err != nil {
		t.Fatal(err)
	}
	if _, err := lower.Get(ctx, "conn/9/password"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatal("unbound delete did not reach the lower store")
	}

	// Rebinding to "" removes the binding.
	e.bind("conn/1/password", "")
	if e.bound() != 2 {
		t.Fatalf("bound after unbind = %d", e.bound())
	}
	if _, err := e.Get(ctx, "conn/1/password"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("unbound account still read from env: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	e.bind("conn/1/password", "PW_WEB1")
	if _, err := e.Get(cancelled, "conn/1/password"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("cancelled get: %v", err)
	}
}

func TestEnvSecretStoreDefaultsAndConcurrency(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SSH_GETE_TEST_PW", "real-env")
	e := newEnvSecretStore(nil, nil)
	e.bind("conn/1/password", "SSH_GETE_TEST_PW")
	if v, err := e.Get(ctx, "conn/1/password"); err != nil || v != "real-env" {
		t.Fatalf("os.LookupEnv default: %q, %v", v, err)
	}
	// nil lower = denySecretStore.
	if err := e.Set(ctx, "conn/2/password", "x"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("set on deny store: %v", err)
	}
	if _, err := e.Get(ctx, "conn/2/password"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("get on deny store: %v", err)
	}
	if err := e.Delete(ctx, "conn/2/password"); err != nil {
		t.Fatalf("delete on deny store: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				a := fmt.Sprintf("conn/%d/password", j%5)
				e.bind(a, "SSH_GETE_TEST_PW")
				_, _ = e.Get(ctx, a)
				_ = e.bound()
			}
		}(i)
	}
	wg.Wait()
}

func TestSelectSecretBackend(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(keyringBackendEnv, "")
	t.Setenv(keyringServiceEnv, "ssh-gete-test")

	lowerOf := func(t *testing.T, s SecretStore, env *envSecretStore) SecretStore {
		t.Helper()
		if s == nil || env == nil || s != SecretStore(env) {
			t.Fatalf("store %T is not the returned env layer", s)
		}
		return env.lower
	}

	s, env, err := selectSecretBackend("", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if ks, ok := lowerOf(t, s, env).(*osSecretStore); !ok || ks.service != "ssh-gete-test" {
		t.Fatalf("default backend = %T", env.lower)
	}
	s, env, _ = selectSecretBackend("Keychain", "", dir)
	if _, ok := lowerOf(t, s, env).(*osSecretStore); !ok {
		t.Fatalf("keychain backend = %T", env.lower)
	}
	s, env, _ = selectSecretBackend("memory", "", dir)
	if _, ok := lowerOf(t, s, env).(*memorySecretStore); !ok {
		t.Fatalf("memory backend = %T", env.lower)
	}
	s, env, _ = selectSecretBackend("env", "", dir)
	if _, ok := lowerOf(t, s, env).(denySecretStore); !ok {
		t.Fatalf("env backend = %T", env.lower)
	}
	s, env, _ = selectSecretBackend("file", "", dir)
	fs, ok := lowerOf(t, s, env).(*fileSecretStore)
	if !ok || fs.path != filepath.Join(dir, "secrets.json") {
		t.Fatalf("file backend = %#v", env.lower)
	}
	_, env, _ = selectSecretBackend("file", "nested/s.json", dir)
	if fs := env.lower.(*fileSecretStore); fs.path != filepath.Join(dir, "nested", "s.json") {
		t.Fatalf("relative secrets.file = %s", fs.path)
	}
	abs := filepath.Join(t.TempDir(), "abs.json")
	_, env, _ = selectSecretBackend("file", abs, dir)
	if fs := env.lower.(*fileSecretStore); fs.path != abs {
		t.Fatalf("absolute secrets.file = %s", fs.path)
	}

	if _, _, err := selectSecretBackend("vault", "", dir); err == nil {
		t.Fatal("unknown backend accepted")
	}
	if _, _, err := selectSecretBackend("keychain", "x.json", dir); err == nil {
		t.Fatal("secrets.file accepted without backend: file")
	}

	// SSH_GETE_KEYRING wins over the configuration.
	t.Setenv(keyringBackendEnv, "memory")
	_, env, _ = selectSecretBackend("file", "", dir)
	if _, ok := env.lower.(*memorySecretStore); !ok {
		t.Fatalf("SSH_GETE_KEYRING=memory over file = %T", env.lower)
	}
	t.Setenv(keyringBackendEnv, "FILE")
	_, env, _ = selectSecretBackend("keychain", "", dir)
	if _, ok := env.lower.(*fileSecretStore); !ok {
		t.Fatalf("SSH_GETE_KEYRING=file over keychain = %T", env.lower)
	}
	t.Setenv(keyringBackendEnv, "env")
	_, env, _ = selectSecretBackend("", "", dir)
	if _, ok := env.lower.(denySecretStore); !ok {
		t.Fatalf("SSH_GETE_KEYRING=env = %T", env.lower)
	}
	// Other values of the variable are ignored (as before), never an error.
	for _, v := range []string{"keychain", "bogus"} {
		t.Setenv(keyringBackendEnv, v)
		_, env, err := selectSecretBackend("memory", "", dir)
		if err != nil {
			t.Fatalf("SSH_GETE_KEYRING=%s: %v", v, err)
		}
		if _, ok := env.lower.(*memorySecretStore); !ok {
			t.Fatalf("SSH_GETE_KEYRING=%s overrode the configuration: %T", v, env.lower)
		}
	}
}

func TestInstallSecretBackend(t *testing.T) {
	t.Cleanup(func() { installSecretBackend(nil) })
	t.Setenv(keyringBackendEnv, "memory")
	custom := newEnvSecretStore(newMemorySecretStore(), nil)
	installSecretBackend(custom)
	if got := defaultSecretStore(); got != SecretStore(custom) {
		t.Fatalf("defaultSecretStore = %T, want the installed store", got)
	}
	installSecretBackend(nil)
	if _, ok := defaultSecretStore().(*memorySecretStore); !ok {
		t.Fatal("installSecretBackend(nil) did not restore the default")
	}
}

func TestProbeSecretStore(t *testing.T) {
	ctx := context.Background()
	if err := probeSecretStore(ctx, newMemorySecretStore()); err != nil {
		t.Fatalf("memory: %v", err)
	}
	if err := probeSecretStore(ctx, denySecretStore{}); err != nil {
		t.Fatalf("deny: %v", err)
	}
	broken := newFaultSecretStore(newMemorySecretStore())
	broken.hook = func(op, account string, n int) error { return ErrSecretUnavailable }
	if err := probeSecretStore(ctx, broken); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("broken: %v", err)
	}
	if err := probeSecretStore(ctx, nil); err == nil {
		t.Fatal("nil store probed as usable")
	}
}

// NewApp's keychain migration must move legacy plaintext passwords into the
// backend chosen before NewApp (§7.1), here the secrets file.
func TestNewAppMigratesIntoSelectedFileBackend(t *testing.T) {
	dir := isolateEnv(t)
	t.Setenv("SSH_GETE_NO_SEED", "1")
	t.Setenv(keyringBackendEnv, "")
	store, err := openStore(defaultDatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	legacy := Connection{ID: newID(), Name: "legacy", Type: "SSH", Host: "10.0.0.9", Port: 22, User: "u",
		AuthMethod: "パスワード保管", Credential: "old-plaintext", Tags: []string{}}
	if err := store.insertConnection(legacy); err != nil {
		t.Fatal(err)
	}
	store.Close()

	selected, env, err := selectSecretBackend("file", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	installSecretBackend(selected)
	t.Cleanup(func() { installSecretBackend(nil) })

	app := NewApp()
	t.Cleanup(func() { app.shutdown(context.Background()) })
	if app.secrets != selected {
		t.Fatalf("app.secrets = %T, want the selected store", app.secrets)
	}
	if credential, _ := appCredentialRow(t, app, legacy.ID); credential != "" {
		t.Fatalf("plaintext still in the database: %q", credential)
	}
	c := readSecretFileContent(t, filepath.Join(dir, "secrets.json"))
	if c.Items[passwordAccount(legacy.ID)] != "old-plaintext" {
		t.Fatalf("secrets.json = %+v", c)
	}

	// An env binding on top: SaveConnection with the env value succeeds and
	// nothing new is written to the file.
	t.Setenv("SSH_GETE_TEST_PW2", "env-secret")
	saved := app.SaveConnection(Connection{Name: "env", Type: "SSH", Host: "10.0.0.10", Port: 22, User: "u",
		AuthMethod: "パスワード保管", Tags: []string{}})
	if !saved.OK {
		t.Fatalf("save: %s", saved.Message)
	}
	env.bind(passwordAccount(saved.ID), "SSH_GETE_TEST_PW2")
	conn, _ := app.connectionByID(saved.ID)
	conn.Credential = "env-secret"
	if r := app.SaveConnection(conn); !r.OK {
		t.Fatalf("save with env value: %s", r.Message)
	}
	if _, ok := readSecretFileContent(t, filepath.Join(dir, "secrets.json")).Items[passwordAccount(saved.ID)]; ok {
		t.Fatal("env-bound password was written to secrets.json")
	}
	if v, err := app.secrets.Get(context.Background(), passwordAccount(saved.ID)); err != nil || v != "env-secret" {
		t.Fatalf("env-bound get = %q, %v", v, err)
	}
}

func TestSelectSecretBackendMemoryOnlyInTests(t *testing.T) {
	t.Setenv(keyringBackendEnv, "")
	if _, _, err := selectSecretBackend("memory", "", t.TempDir()); err != nil {
		t.Fatalf("test binary: %v", err)
	}
	prev := memorySecretsAllowed
	memorySecretsAllowed = func() bool { return false }
	t.Cleanup(func() { memorySecretsAllowed = prev })
	if _, _, err := selectSecretBackend("memory", "", t.TempDir()); err == nil {
		t.Fatal("memory backend accepted outside tests")
	}
	// SSH_GETE_KEYRING=memory is ignored (with a warning), so the configured
	// backend is used instead of silently dropping secrets.
	t.Setenv(keyringBackendEnv, "memory")
	if got, err := effectiveSecretBackend("file"); err != nil || got != secretBackendFile {
		t.Fatalf("effective = %q %v", got, err)
	}
	if got, err := effectiveSecretBackend(""); err != nil || got != secretBackendKeychain {
		t.Fatalf("effective default = %q %v", got, err)
	}
	t.Setenv(keyringBackendEnv, "file")
	if got, _ := effectiveSecretBackend("keychain"); got != secretBackendFile {
		t.Fatalf("SSH_GETE_KEYRING=file = %q", got)
	}
}
