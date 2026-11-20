//go:build !unix

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// execCommand runs cmdArgs as a child process with env and wired stdio,
// returning the child's exit code cleanly.
func execCommand(cmdArgs []string, env []string) (int, error) {
	if len(cmdArgs) == 0 {
		return 2, errors.New("missing command")
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return 127, fmt.Errorf("command %q not found: %w", cmdArgs[0], err)
	}
	return 1, fmt.Errorf("run %q: %w", cmdArgs[0], err)
}
