package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
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
		m.runtimeJobs++
		return m, func() tea.Msg {
			return clipboardWrittenMsg{target: target, err: writeClipboard(content)}
		}
	case "/queue":
		m.addStatus(m.queueSummary())
	case "/raw":
		m.showHelp = false
		m.showRawTranscript = true
		m.refreshMessages(false)
	case "/markdown":
		m.rawMode = !m.rawMode
		m.resetTranscriptCache()
		for index := range m.messages {
			m.messages[index].rendered = ""
		}
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
		store := m.memory
		m.runtimeJobs++
		return m, func() tea.Msg {
			summary, err := store.Summary()
			if summary == "" {
				summary = "Memory summary is empty."
			}
			return memoryCommandMsg{action: "summary", content: store.Status() + "\n\nSummary\n" + summary, err: err}
		}
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
		store := m.memory
		m.runtimeJobs++
		return m, func() tea.Msg {
			err := store.Remember(text)
			return memoryCommandMsg{action: "remember", content: "Queued an explicit note for the next memory consolidation.", err: err}
		}
	case "/forget":
		if m.memory == nil {
			m.addError("Memory storage is unavailable.")
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		store := m.memory
		m.runtimeJobs++
		return m, func() tea.Msg {
			err := store.Forget(text)
			return memoryCommandMsg{action: "forget", content: "Queued a forget/update note for the next memory consolidation.", err: err}
		}
	case "/sessions":
		includeArchived := len(fields) > 1 && strings.EqualFold(fields[1], "all")
		return m, m.openSessionPicker(includeArchived)
	case "/resume":
		if m.store == nil {
			m.addError("Session storage is unavailable.")
			break
		}
		if len(fields) < 2 {
			return m, m.openSessionPicker(false)
		}
		store := m.store
		if fields[1] == "latest" {
			workspace := m.options.Workspace
			return m, m.enqueueSessionJob(sessionJob{action: sessionActionLoadLatest, blocking: true, run: func() sessionJobResult {
				loaded, err := store.Latest(workspace)
				return sessionJobResult{action: sessionActionLoadLatest, value: loaded, err: err}
			}})
		}
		id := fields[1]
		return m, m.enqueueSessionJob(sessionJob{action: sessionActionLoad, blocking: true, run: func() sessionJobResult {
			loaded, err := store.Load(id)
			return sessionJobResult{action: sessionActionLoad, value: loaded, err: err}
		}})
	}
	return m, nil
}
