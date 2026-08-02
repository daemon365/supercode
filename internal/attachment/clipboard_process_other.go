//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package attachment

import (
	"errors"
	"os"
	"os/exec"
)

func configureClipboardProcess(command *exec.Cmd) {
	command.Cancel = func() error { return terminateClipboardProcess(command) }
}

func terminateClipboardProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
