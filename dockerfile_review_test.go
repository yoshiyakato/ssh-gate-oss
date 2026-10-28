package main

import (
	"os"
	"strings"
	"testing"
)

// Review finding 6: the image runs as nonroot, so /data must be created
// owned by nonroot before VOLUME (otherwise a fresh named volume is
// root:root 0755 and nothing can be written). docker itself runs in CI
// (test.yml, job docker); this keeps the rule visible without it.
func TestDockerfileCreatesDataDirForNonroot(t *testing.T) {
	b, err := os.ReadFile("deploy/docker/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var copyLine, volumeLine, userLine = -1, -1, -1
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "COPY ") && strings.HasSuffix(line, " /data"):
			if !strings.Contains(line, "--chown=65532:65532") && !strings.Contains(line, "--chown=nonroot:nonroot") {
				t.Fatalf("/data is not chowned to nonroot: %s", line)
			}
			if !strings.Contains(line, "--chmod=0700") {
				t.Fatalf("/data is not 0700: %s", line)
			}
			copyLine = i
		case strings.HasPrefix(line, "VOLUME ") && strings.Contains(line, "/data"):
			volumeLine = i
		case strings.HasPrefix(line, "USER "):
			userLine = i
		}
	}
	if copyLine < 0 || volumeLine < 0 || copyLine > volumeLine {
		t.Fatalf("/data must be created (COPY --chown) before VOLUME: copy=%d volume=%d", copyLine, volumeLine)
	}
	if userLine < 0 {
		t.Fatal("no USER line")
	}
}
