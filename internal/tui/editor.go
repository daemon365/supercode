package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type editorFinishedMsg struct {
	content string
	err     error
}

// editDraftCommand pauses Bubble Tea while the user's configured editor owns
// the terminal, then restores the edited Markdown draft.
func editDraftCommand(seed string) (tea.Cmd, error) {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return nil, errors.New("set $VISUAL or $EDITOR to use the external editor")
	}
	file, err := os.CreateTemp("", "supercode-draft-*.md")
	if err != nil {
		return nil, fmt.Errorf("create editor draft: %w", err)
	}
	path := file.Name()
	if _, err := file.WriteString(seed); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write editor draft: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close editor draft: %w", err)
	}

	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/D", "/S", "/C", editor+" \""+strings.ReplaceAll(path, "\"", "\"\"")+"\"")
	} else {
		// The editor command is explicitly user-controlled. The draft path is
		// passed as a positional argument so it is never interpolated by the shell.
		command = exec.Command("/bin/sh", "-c", editor+` "$1"`, "supercode-editor", path)
	}
	return tea.ExecProcess(command, func(runErr error) tea.Msg {
		defer os.Remove(path)
		if runErr != nil {
			return editorFinishedMsg{err: runErr}
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return editorFinishedMsg{err: readErr}
		}
		return editorFinishedMsg{content: string(contents)}
	}), nil
}
