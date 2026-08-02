package tui

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/atotto/clipboard"
)

const maxOSC52Bytes = 100 * 1024

func writeClipboard(content string) error {
	var attempts []error
	if err := clipboard.WriteAll(content); err == nil {
		return nil
	} else {
		attempts = append(attempts, err)
	}
	for _, candidate := range clipboardCommands() {
		path, err := exec.LookPath(candidate[0])
		if err != nil {
			continue
		}
		command := exec.Command(path, candidate[1:]...)
		command.Stdin = strings.NewReader(content)
		if err := command.Run(); err == nil {
			return nil
		} else {
			attempts = append(attempts, fmt.Errorf("%s: %w", candidate[0], err))
		}
	}
	if err := writeOSC52(content); err == nil {
		return nil
	} else {
		attempts = append(attempts, err)
	}
	return errors.Join(attempts...)
}

func clipboardCommands() [][]string {
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"pbcopy"}}
	case "windows":
		return [][]string{{"clip.exe"}}
	default:
		return [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}, {"clip.exe"}}
	}
}

func writeOSC52(content string) error {
	if len(content) > maxOSC52Bytes {
		return fmt.Errorf("OSC 52 copy is limited to %d bytes", maxOSC52Bytes)
	}
	terminal, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open terminal for OSC 52: %w", err)
	}
	defer terminal.Close()
	sequence := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(content)) + "\a"
	if os.Getenv("TMUX") != "" {
		sequence = "\x1bPtmux;\x1b" + strings.ReplaceAll(sequence, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	if _, err := terminal.WriteString(sequence); err != nil {
		return fmt.Errorf("write OSC 52: %w", err)
	}
	return nil
}
