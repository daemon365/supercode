//go:build windows

package tool

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func configureProcessTree(command *exec.Cmd, _ bool) {
	if command == nil {
		return
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	command.Cancel = func() error { return terminateProcessTree(command) }
}

func terminateProcessTree(command *exec.Cmd) error {
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

func cleanupProcessTree(*exec.Cmd) {}
