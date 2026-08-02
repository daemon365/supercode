package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
)

func (m model) commandSession(invocation slashInvocation) (tea.Model, tea.Cmd) {
	value, fields := invocation.Raw, invocation.Fields
	switch invocation.Name {
	case "/exit", "/quit":
		return m, tea.Quit
	case "/help":
		m.showHelp = true
		m.refreshMessages(false)
		m.viewport.GotoTop()
	case "/editor":
		command, err := editDraftCommand(strings.TrimSpace(strings.TrimPrefix(value, fields[0])))
		if err != nil {
			m.addError(err.Error())
			break
		}
		m.input.Blur()
		return m, command
	case "/clear":
		m.messages = nil
		m.refreshMessages(false)
	case "/new":
		m.history, m.messages = nil, nil
		if m.store != nil {
			m.session, _ = m.store.New(m.options.Workspace, m.options.Model)
		} else {
			m.session = session.Session{}
		}
		if m.taskState != nil {
			m.taskState.Reset()
		}
		m.setCollaborationMode(prompts.ModeDefault)
		if m.collaboration != nil {
			_ = m.collaboration.Restore(json.RawMessage(`[]`))
		}
		m.resize(m.width, m.height)
		m.addStatus("Started a new session.")
	case "/rename":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		title := strings.TrimSpace(strings.TrimPrefix(value, fields[0]))
		if title == "" {
			m.addError("Usage: /rename <title>")
			break
		}
		updated, err := m.store.Rename(m.session.ID, title)
		if err != nil {
			m.addError(err.Error())
			break
		}
		m.session = updated
		m.addStatus("Renamed session to " + updated.Title + ".")
	case "/fork":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		m.saveSession()
		forked, err := m.store.Fork(m.session.ID)
		if err != nil {
			m.addError(err.Error())
			break
		}
		m.session = forked
		m.setCollaborationMode(prompts.NormalizeMode(forked.Mode))
		if m.collaboration != nil {
			_ = m.collaboration.Restore(forked.Agents)
		}
		m.loadHistory(forked.Messages)
		m.addStatus("Forked session " + forked.ID + ".")
	case "/backtrack":
		var userIndices []int
		for index, item := range m.history {
			if item.Role == provider.MessageRoleUser {
				userIndices = append(userIndices, index)
			}
		}
		if len(fields) == 1 {
			lines := []string{"User turns (use /backtrack <turn> to fork before one):"}
			start := max(0, len(userIndices)-20)
			for turn := start; turn < len(userIndices); turn++ {
				lines = append(lines, fmt.Sprintf("%d. %s", turn+1, preview(strings.Join(strings.Fields(m.history[userIndices[turn]].Content), " "), 100)))
			}
			if len(userIndices) == 0 {
				lines = append(lines, "No user turns are available.")
			}
			m.addStatus(strings.Join(lines, "\n"))
			break
		}
		turn, err := strconv.Atoi(fields[1])
		if err != nil || turn < 1 || turn > len(userIndices) {
			m.addError(fmt.Sprintf("Turn must be between 1 and %d.", len(userIndices)))
			break
		}
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		m.saveSession()
		forked, err := m.store.Fork(m.session.ID)
		if err != nil {
			m.addError(err.Error())
			break
		}
		selectedIndex := userIndices[turn-1]
		selectedPrompt := m.history[selectedIndex].Content
		m.session = forked
		m.history = append([]provider.Message(nil), m.history[:selectedIndex]...)
		m.loadHistory(m.history)
		m.input.SetValue(selectedPrompt)
		m.input.MoveToEnd()
		m.saveSession()
		m.addStatus(fmt.Sprintf("Forked from user turn %d. Edit the restored prompt and press Enter.", turn))
	case "/archive":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		m.saveSession()
		archivedID := m.session.ID
		if _, err := m.store.Archive(archivedID); err != nil {
			m.addError(err.Error())
			break
		}
		m.session, _ = m.store.New(m.options.Workspace, m.options.Model)
		m.history, m.messages = nil, nil
		m.setCollaborationMode(prompts.ModeDefault)
		if m.taskState != nil {
			m.taskState.Reset()
		}
		if m.collaboration != nil {
			_ = m.collaboration.Restore(json.RawMessage(`[]`))
		}
		m.addStatus("Archived session " + archivedID + " and started a new session.")
	case "/delete":
		if m.store == nil || m.session.ID == "" {
			m.addError("Session storage is unavailable.")
			break
		}
		if len(fields) != 2 || strings.ToLower(fields[1]) != "confirm" {
			m.addError("This permanently removes the session. Type /delete confirm to continue.")
			break
		}
		deletedID := m.session.ID
		if err := m.store.Delete(deletedID); err != nil {
			m.addError(err.Error())
			break
		}
		m.session, _ = m.store.New(m.options.Workspace, m.options.Model)
		m.history, m.messages = nil, nil
		m.setCollaborationMode(prompts.ModeDefault)
		if m.taskState != nil {
			m.taskState.Reset()
		}
		if m.collaboration != nil {
			_ = m.collaboration.Restore(json.RawMessage(`[]`))
		}
		m.addStatus("Deleted session " + deletedID + " and started a new session.")
	}
	return m, nil
}
