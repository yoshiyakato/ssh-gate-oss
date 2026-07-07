//go:build !windows

package main

import (
	"errors"
	"net"
	"os"
	"strings"
)

// dialSSHAgent connects to the running ssh-agent over the Unix domain socket
// advertised by SSH_AUTH_SOCK. Returns an error when no agent is configured.
func dialSSHAgent() (net.Conn, error) {
	socket := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if socket == "" {
		return nil, errors.New("SSH_AUTH_SOCK not set")
	}
	return net.Dial("unix", socket)
}
