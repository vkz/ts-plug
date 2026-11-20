//go:build unix

package main

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// passConnFD sends conn's underlying socket file descriptor over dstFD
// (normally stdout, fd 1, which OpenSSH connects to an AF_UNIX socketpair
// when ProxyUseFdpass is enabled).
func passConnFD(dstFD int, conn net.Conn) error {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return fmt.Errorf("connection %T does not expose a raw socket descriptor", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return fmt.Errorf("access raw socket: %w", err)
	}

	var sendErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		rights := syscall.UnixRights(int(fd))
		// OpenSSH's mm_receive_fd expects a single dummy byte alongside
		// the SCM_RIGHTS control message.
		dummy := []byte{0}
		const maxRetries = 100
		for attempt := 0; attempt < maxRetries; attempt++ {
			_, err := syscall.SendmsgN(dstFD, dummy, rights, nil, 0)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(time.Millisecond)
				continue
			}
			sendErr = err
			return
		}
		sendErr = syscall.EAGAIN
	})
	if ctrlErr != nil {
		return fmt.Errorf("control raw socket: %w", ctrlErr)
	}
	if sendErr != nil {
		if errors.Is(sendErr, syscall.ENOTSOCK) {
			return fmt.Errorf("send socket fd to stdout (is OpenSSH ProxyUseFdpass yes set?): %w", sendErr)
		}
		return fmt.Errorf("send socket fd to ssh: %w", sendErr)
	}
	return nil
}
