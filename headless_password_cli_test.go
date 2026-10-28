package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseCLISetAdminPassword(t *testing.T) {
	opts, err := parseCLI([]string{"--set-admin-password"}, true)
	if err != nil || !opts.SetAdminPassword || !opts.wantsHeadless() {
		t.Fatalf("GUI build: %+v %v", opts, err)
	}
	for _, args := range [][]string{
		{"--set-admin-password", "--print-admin-token"},
		{"--set-admin-password", "--rotate-admin-token"},
		{"--set-admin-password", "--check-config"},
	} {
		if _, err := parseCLI(args, false); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRunHeadlessSetAdminPassword(t *testing.T) {
	h := newHeadlessHarness(t)
	const pw = "from standard input"
	h.deps.stdin = strings.NewReader(pw + "\n")
	h.deps.stdinIsTerminal = func() bool { return false }
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, SetAdminPassword: true}, fstest.MapFS{}, h.deps); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.errOut.String())
	}
	path := filepath.Join(os.Getenv("SSH_GETE_CONFIG_DIR"), "admin-password")
	h2, _, err := readAdminPasswordFile(path)
	if err != nil || !h2.verify(pw) {
		t.Fatalf("stored hash: %v", err)
	}
	if strings.Contains(h.out.String()+h.errOut.String(), pw) {
		t.Fatal("password printed")
	}

	// Interactive: prompts on stderr and needs a matching confirmation.
	h.errOut.Reset()
	h.deps.stdin = strings.NewReader("another long one!\nmismatch typed\n")
	h.deps.stdinIsTerminal = func() bool { return true }
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, SetAdminPassword: true}, fstest.MapFS{}, h.deps); code != exitUsage {
		t.Fatalf("mismatch: exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "もう一度") {
		t.Fatalf("no confirmation prompt: %q", h.errOut.String())
	}
	// Too short.
	h.deps.stdin = strings.NewReader("short\n")
	h.deps.stdinIsTerminal = func() bool { return false }
	if code := runHeadlessWith(context.Background(), CLIOptions{Headless: true, SetAdminPassword: true}, fstest.MapFS{}, h.deps); code != exitUsage {
		t.Fatalf("weak: exit %d", code)
	}
	if h3, _, _ := readAdminPasswordFile(path); h3 == nil || !h3.verify(pw) {
		t.Fatal("a refused password replaced the stored one")
	}
}
