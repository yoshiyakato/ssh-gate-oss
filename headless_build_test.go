package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHeadlessBuildHasNoWailsOrCgo checks the REQ-015 promise that the Linux
// headless build runs without GTK: `-tags headless` must not pull in any
// Wails package, and it must build for linux with CGO_ENABLED=0 (no libc,
// no GTK/WebKit). CI checks the same in shell; this keeps it in `go test`.
func TestHeadlessBuildHasNoWailsOrCgo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")

	list := exec.Command(goBin, "list", "-deps", "-tags", "headless", ".")
	list.Env = env
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "wailsapp/wails") {
			t.Fatalf("headless build depends on %s", pkg)
		}
	}

	bin := filepath.Join(t.TempDir(), "ssh-gete")
	build := exec.Command(goBin, "build", "-tags", "headless", "-o", bin, ".")
	build.Env = env
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 GOOS=linux go build -tags headless: %v\n%s", err, out)
	}
	if st, err := os.Stat(bin); err != nil || st.Size() == 0 {
		t.Fatalf("no binary: %v", err)
	}
}
