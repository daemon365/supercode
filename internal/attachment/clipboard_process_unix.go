//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package attachment

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureClipboardProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return terminateClipboardProcess(command) }
}

func terminateClipboardProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if killErr := command.Process.Kill(); killErr == nil || errors.Is(killErr, os.ErrProcessDone) {
		return nil
	}
	return err
}
