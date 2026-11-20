//go:build unix

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

type controlListener struct {
	net.Listener
	sockPath  string
	closeOnce sync.Once
	closeErr  error
}

func (l *controlListener) Close() error {
	l.closeOnce.Do(func() {
		l.closeErr = l.Listener.Close()
		if err := os.Remove(l.sockPath); err != nil && !errors.Is(err, os.ErrNotExist) && l.closeErr == nil {
			l.closeErr = err
		}
	})
	return l.closeErr
}

// listenControlSocket creates a 0600 Unix domain socket at sockPath. Callers
// must already hold the state-dir lock so stale socket removal is safe. Any
// existing non-socket path (regular file, directory, symlink) is refused.
func listenControlSocket(sockPath string) (net.Listener, error) {
	if sockPath == "" {
		return nil, errors.New("control socket path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create control socket directory: %w", err)
	}
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to remove non-socket path %q for control socket", sockPath)
		}
		if err := os.Remove(sockPath); err != nil {
			return nil, fmt.Errorf("remove stale control socket %q: %w", sockPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat control socket %q: %w", sockPath, err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket %q (pass -control-sock for a shorter path if over OS limit): %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("chmod control socket %q: %w", sockPath, err)
	}
	return &controlListener{Listener: ln, sockPath: sockPath}, nil
}
