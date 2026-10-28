//go:build windows

package main

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/Microsoft/go-winio"
)

// Windows ssh-agent does not use SSH_AUTH_SOCK / Unix sockets; it listens on
// windowsDefaultAgentPipe (see ssh_exec.go).

var warnAgentSockOnce sync.Once

// dialSSHAgent connects to the Windows ssh-agent over its named pipe. An
// SSH_AUTH_SOCK that names a pipe, in any spelling Win32 accepts (see
// normalizeWindowsAgentPipe), takes priority; any other value (a Unix-style
// path set by Git Bash / MSYS tooling) is ignored in favour of the default
// pipe, and that is logged once. ctx bounds the connect.
func dialSSHAgent(ctx context.Context) (net.Conn, error) {
	raw := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	pipe, ok := normalizeWindowsAgentPipe(raw)
	if !ok {
		if raw != "" {
			warnAgentSockOnce.Do(func() {
				log.Printf("ssh-gete: SSH_AUTH_SOCK %q is not a named pipe; using %s", raw, windowsDefaultAgentPipe)
			})
		}
		pipe = windowsDefaultAgentPipe
	}
	return winio.DialPipeContext(ctx, pipe)
}
