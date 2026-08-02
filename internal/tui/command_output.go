package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daemon365/supercode/internal/session"
)

func (m model) commandOutput(invocation slashInvocation) (tea.Model, tea.Cmd) {
	fields := invocation.Fields
	switch invocation.Name {
	case "/copy":
		target := "assistant"
		if len(fields) > 1 {
			target = strings.ToLower(fields[1])
		}
		if target != "assistant" && target != "tool" && target != "transcript" && target != "all" {
			m.addError("Usage: /copy [assistant|tool|transcript|all]")
			break
		}
		content := m.copyOutput(target)
		if content == "" {
			m.addError("There is no " + target + " output to copy. Usage: /copy [assistant|tool|transcript|all]")
			break
		}
		if err := writeClipboard(content); err != nil {
			m.addError("Clipboard is unavailable: " + err.Error())
			break
		}
		m.addStatus("Copied " + target + " output.")
	case "/queue":
		m.addStatus(m.queueSummary())
	case "/raw":
		m.showHelp = false
		m.showRawTranscript = true
		m.refreshMessages(false)
	case "/markdown":
		m.rawMode = !m.rawMode
		m.refreshMessages(false)
		m.addStatus(fmt.Sprintf("Markdown rendering: %t.", !m.rawMode))
	}
	return m, nil
}

func (m model) commandMemoryAndResume(invocation slashInvocation) (tea.Model, tea.Cmd) {
	value, fields := invocation.Raw, invocation.Fields
	switch invocation.Name {
	case "/memory":
		if m.memory == nil {
			m.addError("Memory storage is unavailable.")
			break
		}
		value, err := m.memory.Summary()
		if err != nil {
			m.addError(err.Error())
			break
		}
		if value == "" {
			value = "Memory summary is empty."
		}
		m.addStatus(m.memory.Status() + "\n\nSummary\n" + value)
	case "/remember":
		if m.memory == nil {
			m.addError("Memory storage is unavailable.")
			break
		}
		if len(fields) < 2 {
			m.addError("Usage: /remember <text>")
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		if err := m.memory.Remember(text); err != nil {
			m.addError(err.Error())
			break
		}
		m.addStatus("Queued an explicit note for the next memory consolidation.")
	case "/forget":
		if m.memory == nil {
			m.addError("Memory storage is unavailable.")
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		if err := m.memory.Forget(text); err != nil {
			m.addError(err.Error())
			break
		}
		m.addStatus("Queued a forget/update note for the next memory consolidation.")
	case "/sessions":
		includeArchived := len(fields) > 1 && strings.EqualFold(fields[1], "all")
		m.openSessionPicker(includeArchived)
	case "/resume":
		if m.store == nil {
			m.addError("Session storage is unavailable.")
			break
		}
		if len(fields) < 2 {
			m.openSessionPicker(false)
			break
		}
		var loaded session.Session
		var err error
		if fields[1] == "latest" {
			loaded, err = m.store.Latest(m.options.Workspace)
		} else {
			loaded, err = m.store.Load(fields[1])
		}
		if err != nil {
			m.addError(err.Error())
			break
		}
		m.resumeSession(loaded)
	}
	return m, nil
}
