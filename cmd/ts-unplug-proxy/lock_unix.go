//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// dirLock is an advisory exclusive lock on a tsnet state directory. The state
// file has no cross-process lock, so two tsnet servers on the same node key
// would corrupt it. Serve mode, doctor's online probe and -clear-exit-node all
// take this lock.
type dirLock struct{ f *os.File }

func lockDir(dir string) (*dirLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "ts-unplug-proxy.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state dir %s is already in use (lock held)", dir)
	}
	return &dirLock{f: f}, nil
}

func (l *dirLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}
