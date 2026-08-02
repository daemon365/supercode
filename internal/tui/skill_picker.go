package tui

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/daemon365/supercode/internal/skill"
)

func (m *model) openSkillPicker() {
	if m.skills == nil || m.skills.Len() == 0 {
		m.addStatus("No skills found.")
		return
	}
	m.skillChoices = m.skills.Skills()
	m.skillQuery = ""
	m.skillChoice = 0
	m.showSkillPicker = true
	m.input.Blur()
	m.resize(m.width, m.height)
}

func (m model) updateSkillPicker(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc":
		m.showSkillPicker = false
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	case "up", "ctrl+p":
		matches := m.filteredSkills()
		if len(matches) > 0 {
			m.skillChoice = (m.skillChoice - 1 + len(matches)) % len(matches)
		}
		return m, nil
	case "down", "ctrl+n":
		matches := m.filteredSkills()
		if len(matches) > 0 {
			m.skillChoice = (m.skillChoice + 1) % len(matches)
		}
		return m, nil
	case "backspace":
		runes := []rune(m.skillQuery)
		if len(runes) > 0 {
			m.skillQuery = string(runes[:len(runes)-1])
			m.skillChoice = 0
			m.resize(m.width, m.height)
		}
		return m, nil
	case "enter":
		matches := m.filteredSkills()
		if len(matches) == 0 {
			return m, nil
		}
		if m.skillChoice >= len(matches) {
			m.skillChoice = 0
		}
		value := strings.TrimSpace(m.input.Value())
		if value != "" {
			value += " "
		}
		m.input.SetValue(value + "$" + matches[m.skillChoice].Name + " ")
		m.input.MoveToEnd()
		m.showSkillPicker = false
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	}
	if message.Text != "" && !strings.Contains(message.String(), "ctrl+") {
		m.skillQuery += message.Text
		m.skillChoice = 0
		m.resize(m.width, m.height)
	}
	return m, nil
}

func (m model) filteredSkills() []skill.Skill {
	query := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(m.skillQuery, "$")))
	if query == "" {
		return append([]skill.Skill(nil), m.skillChoices...)
	}
	type ranked struct {
		value skill.Skill
		score int
	}
	var values []ranked
	for _, item := range m.skillChoices {
		candidate := strings.ToLower(item.Name + " " + item.Description)
		if score, ok := slashFuzzyScore(candidate, query); ok {
			values = append(values, ranked{item, score})
		}
	}
	sort.SliceStable(values, func(left, right int) bool { return values[left].score < values[right].score })
	result := make([]skill.Skill, 0, len(values))
	for _, item := range values {
		result = append(result, item.value)
	}
	return result
}

func (m model) renderSkillPicker(width int) string {
	if !m.showSkillPicker {
		return ""
	}
	matches := m.filteredSkills()
	rows := []string{titleStyle.Render("Insert a skill"), detailStyle.Render("Search: " + m.skillQuery)}
	for index := 0; index < min(8, len(matches)); index++ {
		item := matches[index]
		marker := "  "
		style := lipgloss.NewStyle().Foreground(white)
		if index == m.skillChoice {
			marker = "› "
			style = style.Bold(true).Foreground(accentBright)
		}
		status := ""
		if missing := item.MissingDependencies(); len(missing) > 0 {
			status = " [missing: " + strings.Join(missing, ", ") + "]"
		}
		rows = append(rows, style.Render(marker+"$"+item.Name+status), detailStyle.Render("    "+item.Description+" · "+item.Path))
	}
	if len(matches) == 0 {
		rows = append(rows, detailStyle.Render("No matching skills."))
	}
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(strings.Join(rows, "\n"))
}

func (m model) skillPickerHeight() int {
	if value := m.renderSkillPicker(max(20, m.width)); value != "" {
		return lipgloss.Height(value)
	}
	return 0
}
