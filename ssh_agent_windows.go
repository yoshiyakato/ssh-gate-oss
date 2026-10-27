//go:build windows

package main

import (
	"net"
	"os"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

// windowsSSHAgentPipe is the named pipe exposed by the OpenSSH Authentication
// Agent service that ships with Windows. Unlike Unix, Windows ssh-agent does
// not use SSH_AUTH_SOCK / Unix sockets.
const windowsSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

// dialSSHAgent connects to the Windows ssh-agent over its named pipe. An
// SSH_AUTH_SOCK that names a pipe takes priority; a Unix-style value (set by
// Git Bash / MSYS tooling) is ignored in favour of the default pipe.
func dialSSHAgent() (net.Conn, error) {
	pipe := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if !strings.HasPrefix(pipe, `\\.\pipe\`) {
		pipe = windowsSSHAgentPipe
	}
	timeout := 2 * time.Second
	return winio.DialPipe(pipe, &timeout)
}
