//go:build windows

package credential

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func configureCredentialCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	command.Cancel = func() error { return terminateCredentialCommand(command) }
}

func terminateCredentialCommand(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	if err := exec.Command("taskkill", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F").Run(); err == nil {
		return nil
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func cleanupCredentialCommand(*exec.Cmd) {}
