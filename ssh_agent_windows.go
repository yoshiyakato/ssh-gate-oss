//go:build windows

package main

import (
	"net"
	"os"
	"strings"

	"github.com/Microsoft/go-winio"
)

// windowsSSHAgentPipe is the named pipe exposed by the OpenSSH Authentication
// Agent service that ships with Windows. Unlike Unix, Windows ssh-agent does
// not use SSH_AUTH_SOCK / Unix sockets.
const windowsSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

// dialSSHAgent connects to the Windows ssh-agent over its named pipe. An
// explicit SSH_AUTH_SOCK (some tooling sets it to a pipe path) takes priority,
// otherwise the default OpenSSH agent pipe is used.
func dialSSHAgent() (net.Conn, error) {
	pipe := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if pipe == "" {
		pipe = windowsSSHAgentPipe
	}
	return winio.DialPipe(pipe, nil)
}
