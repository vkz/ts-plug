//go:build !unix

package main

import "os"

// dirLock is a no-op on platforms without flock (Windows). Serve mode still
// works; the state-dir concurrency guard is unavailable.
type dirLock struct{}

func lockDir(dir string) (*dirLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &dirLock{}, nil
}

func (l *dirLock) Close() error { return nil }
