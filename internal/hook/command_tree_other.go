//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package hook

import (
	"errors"
	"os"
	"os/exec"
)

func configureHookCommand(command *exec.Cmd) {
	command.Cancel = func() error { return terminateHookCommand(command) }
}

func terminateHookCommand(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func cleanupHookCommand(*exec.Cmd) {}
