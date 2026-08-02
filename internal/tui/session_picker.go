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

func (m *model) openSessionPicker(includeArchived bool) {
	if m.store == nil {
		m.addError("Session storage is unavailable.")
		return
	}
	values, err := m.store.ListAll(m.options.Workspace, 200, includeArchived)
	if err != nil {
		m.addError(err.Error())
		return
	}
	m.showSessionPicker = true
	m.sessionIncludeAll = includeArchived
	m.sessionQuery = ""
	m.sessionChoices = values
	m.sessionChoice = 0
	m.input.Blur()
	m.resize(m.width, m.height)
}

func (m model) updateSessionPicker(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc":
		m.showSessionPicker = false
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
		m.openSessionPicker(!m.sessionIncludeAll)
		return m, nil
	case "backspace":
		runes := []rune(m.sessionQuery)
		if len(runes) > 0 {
			m.sessionQuery = string(runes[:len(runes)-1])
			m.sessionChoice = 0
			m.resize(m.width, m.height)
		}
		return m, nil
	case "enter":
		matches := m.filteredSessions()
		if len(matches) == 0 {
			return m, nil
		}
		if m.sessionChoice >= len(matches) {
			m.sessionChoice = 0
		}
		m.resumeSession(matches[m.sessionChoice])
		m.showSessionPicker = false
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	}
	if message.Text != "" && !strings.Contains(message.String(), "ctrl+") {
		m.sessionQuery += message.Text
		m.sessionChoice = 0
		m.resize(m.width, m.height)
	}
	return m, nil
}

func (m model) filteredSessions() []session.Session {
	query := strings.ToLower(strings.TrimSpace(m.sessionQuery))
	if query == "" {
		return append([]session.Session(nil), m.sessionChoices...)
	}
	type ranked struct {
		value session.Session
		score int
	}
	var values []ranked
	for _, item := range m.sessionChoices {
		candidate := strings.ToLower(sessionSearchText(item))
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
	result := make([]session.Session, 0, len(values))
	for _, item := range values {
		result = append(result, item.value)
	}
	return result
}

func sessionSearchText(value session.Session) string {
	parts := []string{value.Title, value.ID, value.Model, value.Workspace}
	for _, message := range value.Messages {
		if strings.TrimSpace(message.Content) != "" {
			parts = append(parts, message.Content)
		}
	}
	return strings.Join(parts, " ")
}

func (m model) renderSessionPicker(width int) string {
	if !m.showSessionPicker {
		return ""
	}
	matches := m.filteredSessions()
	rows := []string{titleStyle.Render("Resume a session"), detailStyle.Render("Search: " + m.sessionQuery)}
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
			rows = append(rows, detailStyle.Render("    "+shortID(item.ID)+" · "+item.Model+" · "+sessionPreview(item)))
		}
	}
	if len(matches) == 0 {
		rows = append(rows, detailStyle.Render("No matching sessions."))
	}
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(strings.Join(rows, "\n"))
}

func sessionPreview(value session.Session) string {
	for _, item := range value.Messages {
		if string(item.Role) == "user" && strings.TrimSpace(item.Content) != "" {
			return preview(strings.Join(strings.Fields(item.Content), " "), 70)
		}
	}
	return "no messages"
}

func (m model) sessionPickerHeight() int {
	if value := m.renderSessionPicker(max(20, m.width)); value != "" {
		return lipgloss.Height(value)
	}
	return 0
}

func (m *model) resumeSession(loaded session.Session) {
	if loaded.Workspace != m.options.Workspace {
		m.addError("The session belongs to a different workspace.")
		return
	}
	m.session = loaded
	m.setCollaborationMode(prompts.NormalizeMode(loaded.Mode))
	if m.taskState != nil {
		m.taskState.Restore(loaded.Plan, loaded.Goal)
	}
	if m.collaboration != nil {
		_ = m.collaboration.Restore(loaded.Agents)
	}
	m.loadHistory(loaded.Messages)
	m.addStatus("Resumed session " + loaded.ID + ".")
}
