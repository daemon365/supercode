//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tool

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcessTree(command *exec.Cmd, tty bool) {
	if command == nil {
		return
	}
	if !tty {
		if command.SysProcAttr == nil {
			command.SysProcAttr = &syscall.SysProcAttr{}
		}
		command.SysProcAttr.Setpgid = true
	}
	command.Cancel = func() error { return terminateProcessTree(command) }
}

func terminateProcessTree(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	// Non-PTY commands use Setpgid; the PTY package uses Setsid. In both
	// cases the command PID is also the process-group ID.
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if killErr := command.Process.Kill(); killErr == nil || errors.Is(killErr, os.ErrProcessDone) {
		return nil
	}
	return err
}

func cleanupProcessTree(command *exec.Cmd) { _ = terminateProcessTree(command) }
