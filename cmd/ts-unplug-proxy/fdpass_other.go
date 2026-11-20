//go:build !unix

package main

import (
	"errors"
	"net"
)

func passConnFD(_ int, _ net.Conn) error {
	return errors.New("-fdpass is only supported on Unix systems (macOS/Linux)")
}
