//go:build !windows

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
)

// dialSSHAgent connects to the running ssh-agent over the Unix domain socket
// advertised by SSH_AUTH_SOCK. Returns an error when no agent is configured.
// ctx bounds the connect (see dialAgentClient for the deadline).
func dialSSHAgent(ctx context.Context) (net.Conn, error) {
	socket := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if socket == "" {
		return nil, errors.New("SSH_AUTH_SOCK not set")
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", socket)
}
