//go:build !unix

package main

import (
	"log/slog"
	"net"
)

func listenControlSocket(sockPath string) (net.Listener, error) {
	slog.Debug("control Unix socket is not supported on this platform", slog.String("path", sockPath))
	return nil, nil
}
