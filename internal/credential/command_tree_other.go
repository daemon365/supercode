//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package credential

import (
	"errors"
	"os"
	"os/exec"
)

func configureCredentialCommand(command *exec.Cmd) {
	command.Cancel = func() error { return terminateCredentialCommand(command) }
}

func terminateCredentialCommand(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func cleanupCredentialCommand(*exec.Cmd) {}
