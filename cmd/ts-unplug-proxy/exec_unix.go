//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// execCommand replaces the current process with cmdArgs and env so signals,
// TTY state, stdin/stdout/stderr, and exit status belong natively to the child.
func execCommand(cmdArgs []string, env []string) (int, error) {
	if len(cmdArgs) == 0 {
		return 2, errors.New("missing command")
	}
	bin, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return 127, fmt.Errorf("command %q not found: %w", cmdArgs[0], err)
		}
		if errors.Is(err, os.ErrPermission) {
			return 126, fmt.Errorf("command %q not executable: %w", cmdArgs[0], err)
		}
		return 1, fmt.Errorf("locate command %q: %w", cmdArgs[0], err)
	}
	if err := syscall.Exec(bin, cmdArgs, env); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			return 127, fmt.Errorf("exec %q: %w", bin, err)
		}
		return 126, fmt.Errorf("exec %q: %w", bin, err)
	}
	return 0, nil
}
