package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/session"
)

type sessionListResult struct {
	request         uint64
	includeArchived bool
	items           []session.Metadata
}

type sessionLoadContext struct {
	request uint64
}

func (m *model) openSessionPicker(includeArchived bool) tea.Cmd {
	if m.store == nil {
		m.addError("Session storage is unavailable.")
		return nil
	}
	m.showSessionPicker = true
	m.sessionQuery = ""
	m.sessionChoice = 0
	m.sessionWarnings = nil
	m.sessionPickerError = ""
	m.sessionPickerActivating = false
	m.input.Blur()
	command := m.loadSessionChoices(includeArchived)
	m.resize(m.width, m.height)
	return command
}

func (m *model) loadSessionChoices(includeArchived bool) tea.Cmd {
	m.sessionPickerRequest++
	request := m.sessionPickerRequest
	m.sessionIncludeAll = includeArchived
	m.sessionChoices = nil
	m.sessionChoice = 0
	m.sessionWarnings = nil
	m.sessionPickerError = ""
	m.sessionPickerLoading = true
	m.sessionPickerActivating = false
	store, workspace := m.store, m.options.Workspace
	return m.enqueueSessionJob(sessionJob{action: sessionActionList, run: func() sessionJobResult {
		items, warnings, err := store.ListMetadata(workspace, "", 200, includeArchived)
		return sessionJobResult{
			action: sessionActionList,
			payload: sessionListResult{
				request: request, includeArchived: includeArchived, items: items,
			},
			warnings: warnings,
			err:      err,
		}
	}})
}

func (m model) updateSessionPicker(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc":
		if m.sessionPickerActivating {
			return m, nil
		}
		m.showSessionPicker = false
		m.sessionPickerLoading = false
		m.sessionPickerActivating = false
		m.sessionPickerRequest++ // Ignore a list/load result that completes later.
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	case "up", "ctrl+p":
		matches := m.filteredSessions()
		if len(matches) > 0 {
			m.sessionChoice = (m.sessionChoice - 1 + len(matches)) % len(matches)
		}
		return m, nil
	case "down", "ctrl+n":
		matches := m.filteredSessions()
		if len(matches) > 0 {
			m.sessionChoice = (m.sessionChoice + 1) % len(matches)
		}
		return m, nil
	case "tab":
		if m.sessionPickerLoading {
			return m, nil
		}
		command := m.loadSessionChoices(!m.sessionIncludeAll)
		m.resize(m.width, m.height)
		return m, command
	case "backspace":
		runes := []rune(m.sessionQuery)
		if len(runes) > 0 {
			m.sessionQuery = string(runes[:len(runes)-1])
			m.sessionChoice = 0
			m.resize(m.width, m.height)
		}
		return m, nil
	case "enter":
		if m.sessionPickerLoading {
			return m, nil
		}
		matches := m.filteredSessions()
		if len(matches) == 0 {
			return m, nil
		}
		if m.sessionChoice >= len(matches) {
			m.sessionChoice = 0
		}
		selected := matches[m.sessionChoice]
		m.sessionPickerLoading = true
		m.sessionPickerError = ""
		request := m.sessionPickerRequest
		store := m.store
		command := m.enqueueSessionJob(sessionJob{action: sessionActionLoad, blocking: true, run: func() sessionJobResult {
			loaded, err := store.Load(selected.ID)
			return sessionJobResult{action: sessionActionLoad, value: loaded, payload: sessionLoadContext{request: request}, err: err}
		}})
		m.resize(m.width, m.height)
		return m, command
	}
	if message.Text != "" && !strings.Contains(message.String(), "ctrl+") {
		m.sessionQuery += message.Text
		m.sessionChoice = 0
		m.resize(m.width, m.height)
	}
	return m, nil
}

func (m model) filteredSessions() []session.Metadata {
	query := strings.ToLower(strings.TrimSpace(m.sessionQuery))
	if query == "" {
		return append([]session.Metadata(nil), m.sessionChoices...)
	}
	type ranked struct {
		value session.Metadata
		score int
	}
	var values []ranked
	for _, item := range m.sessionChoices {
		candidate := strings.ToLower(item.SearchText)
		if candidate == "" {
			candidate = strings.ToLower(strings.Join([]string{item.Title, item.ID, item.Model, item.Workspace}, " "))
		}
		score, ok := slashFuzzyScore(candidate, query)
		if !ok {
			continue
		}
		values = append(values, ranked{value: item, score: score})
	}
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].score == values[right].score {
			return values[left].value.UpdatedAt.After(values[right].value.UpdatedAt)
		}
		return values[left].score < values[right].score
	})
	result := make([]session.Metadata, 0, len(values))
	for _, item := range values {
		result = append(result, item.value)
	}
	return result
}

func (m model) renderSessionPicker(width int) string {
	if !m.showSessionPicker {
		return ""
	}
	matches := m.filteredSessions()
	archiveLabel := "active only"
	if m.sessionIncludeAll {
		archiveLabel = "including archived"
	}
	rows := []string{
		titleStyle.Render("Resume a session"),
		detailStyle.Render("Search: " + m.sessionQuery + "  ·  " + archiveLabel),
	}
	for _, warning := range m.sessionWarnings {
		rows = append(rows, errorStyle.Render("Warning: "+preview(warning, 120)))
		if len(rows) >= 5 {
			break
		}
	}
	if m.sessionPickerError != "" {
		rows = append(rows, errorStyle.Render("Error: "+preview(m.sessionPickerError, 120)))
	}
	if m.sessionPickerLoading {
		rows = append(rows, detailStyle.Render("Loading session data…"))
	}
	limit := min(7, len(matches))
	for index := 0; index < limit; index++ {
		item := matches[index]
		marker := "  "
		style := lipgloss.NewStyle().Foreground(white)
		if index == m.sessionChoice {
			marker = "› "
			style = style.Bold(true).Foreground(accentBright)
		}
		title := defaultString(strings.TrimSpace(item.Title), "Untitled session")
		archived := ""
		if item.ArchivedAt != nil {
			archived = " [archived]"
		}
		rows = append(rows, style.Render(fmt.Sprintf("%s%s  %s%s", marker, item.UpdatedAt.Local().Format("Jan 02 15:04"), preview(title, 54), archived)))
		if index == m.sessionChoice {
			rows = append(rows, detailStyle.Render(fmt.Sprintf("    %s · %s · %d messages", shortID(item.ID), defaultString(item.Model, "unknown model"), item.MessageCount)))
		}
	}
	if len(matches) == 0 && !m.sessionPickerLoading && m.sessionPickerError == "" {
		rows = append(rows, detailStyle.Render("No matching sessions."))
	}
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(strings.Join(rows, "\n"))
}

func (m model) sessionPickerHeight() int {
	if value := m.renderSessionPicker(max(20, m.width)); value != "" {
		return lipgloss.Height(value)
	}
	return 0
}

func (m *model) resumeSession(loaded session.Session, agentsRestored bool) error {
	if loaded.Workspace != m.options.Workspace {
		return fmt.Errorf("the session belongs to a different workspace")
	}
	if m.collaboration != nil && !agentsRestored {
		return fmt.Errorf("session agents were not restored before activation")
	}
	m.session = loaded
	m.setCollaborationMode(prompts.NormalizeMode(loaded.Mode))
	if m.taskState != nil {
		m.taskState.Restore(loaded.Plan, loaded.Goal)
	}
	m.loadHistory(loaded.Messages)
	return nil
}
