package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain points every location the app reads or writes (config dir with the
// SQLite DB and known_hosts, HOME, the keychain) at a throwaway directory for
// the whole test binary. isolateEnv still gives each test its own dirs; this
// is the backstop for helpers that forget to call it, so a test run can never
// touch the developer's real app data.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "ssh-gete-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test root:", err)
		os.Exit(1)
	}
	for key, sub := range map[string]string{
		"SSH_GETE_CONFIG_DIR": "config",
		"HOME":                "home",
		"USERPROFILE":         "home",
		"XDG_CONFIG_HOME":     "xdg",
		"APPDATA":             "appdata",
	} {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "create test dir:", err)
			os.Exit(1)
		}
		os.Setenv(key, dir)
	}
	os.Setenv("SSH_GETE_KEYRING", "memory")
	os.Setenv("SSH_AUTH_SOCK", "")
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}
