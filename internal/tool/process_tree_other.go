//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package tool

import (
	"errors"
	"os"
	"os/exec"
)

func configureProcessTree(command *exec.Cmd, _ bool) {
	if command != nil {
		command.Cancel = func() error { return terminateProcessTree(command) }
	}
}

func terminateProcessTree(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func cleanupProcessTree(*exec.Cmd) {}
