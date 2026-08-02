//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package mcp

import (
	"errors"
	"os"
	"os/exec"
)

func configureCommandTree(command *exec.Cmd) {
	command.Cancel = func() error { return terminateCommandTree(command) }
}

func terminateCommandTree(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func cleanupCommandTree(*exec.Cmd) {}
