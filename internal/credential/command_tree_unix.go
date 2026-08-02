//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package credential

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureCredentialCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return terminateCredentialCommand(command) }
}

func terminateCredentialCommand(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return command.Process.Kill()
}

func cleanupCredentialCommand(command *exec.Cmd) { _ = terminateCredentialCommand(command) }
