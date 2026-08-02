package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m *model) openModelPicker() {
	seen := make(map[string]bool)
	values := []string{m.options.Model}
	values = append(values, m.options.Models...)
	values = append(values, m.options.FallbackModels...)
	if m.options.ModelCatalog != nil {
		values = append(values, m.options.ModelCatalog.Names()...)
	}
	m.modelChoices = nil
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		m.modelChoices = append(m.modelChoices, value)
	}
	m.showModelPicker = true
	m.modelQuery = ""
	m.modelChoice = 0
	m.input.Blur()
	m.resize(m.width, m.height)
}

func (m model) updateModelPicker(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc":
		m.showModelPicker = false
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	case "up", "ctrl+p":
		matches := m.filteredModels()
		if len(matches) > 0 {
			m.modelChoice = (m.modelChoice - 1 + len(matches)) % len(matches)
		}
		return m, nil
	case "down", "ctrl+n":
		matches := m.filteredModels()
		if len(matches) > 0 {
			m.modelChoice = (m.modelChoice + 1) % len(matches)
		}
		return m, nil
	case "backspace":
		runes := []rune(m.modelQuery)
		if len(runes) > 0 {
			m.modelQuery = string(runes[:len(runes)-1])
			m.modelChoice = 0
			m.resize(m.width, m.height)
		}
		return m, nil
	case "enter":
		matches := m.filteredModels()
		if len(matches) == 0 {
			return m, nil
		}
		if m.modelChoice >= len(matches) {
			m.modelChoice = 0
		}
		if err := m.runner.SetModel(matches[m.modelChoice]); err != nil {
			m.addError(err.Error())
			return m, nil
		}
		m.options.Model = matches[m.modelChoice]
		m.syncModelLimits()
		m.session.Model = m.options.Model
		m.showModelPicker = false
		m.saveSession()
		m.addStatus("Model changed to " + m.options.Model + ".")
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	}
	if message.Text != "" && !strings.Contains(message.String(), "ctrl+") {
		m.modelQuery += message.Text
		m.modelChoice = 0
		m.resize(m.width, m.height)
	}
	return m, nil
}

func (m model) filteredModels() []string {
	query := strings.ToLower(strings.TrimSpace(m.modelQuery))
	if query == "" {
		return append([]string(nil), m.modelChoices...)
	}
	type ranked struct {
		value string
		score int
	}
	var values []ranked
	for _, item := range m.modelChoices {
		score, ok := slashFuzzyScore(strings.ToLower(item), query)
		if ok {
			values = append(values, ranked{item, score})
		}
	}
	sort.SliceStable(values, func(left, right int) bool { return values[left].score < values[right].score })
	result := make([]string, 0, len(values))
	for _, item := range values {
		result = append(result, item.value)
	}
	return result
}

func (m model) renderModelPicker(width int) string {
	if !m.showModelPicker {
		return ""
	}
	matches := m.filteredModels()
	rows := []string{titleStyle.Render("Choose a model"), detailStyle.Render("Search: " + m.modelQuery)}
	for index := 0; index < min(8, len(matches)); index++ {
		marker := "  "
		style := lipgloss.NewStyle().Foreground(white)
		if index == m.modelChoice {
			marker = "› "
			style = style.Bold(true).Foreground(accentBright)
		}
		current := ""
		if matches[index] == m.options.Model {
			current = " (current)"
		}
		rows = append(rows, style.Render(marker+matches[index]+current))
	}
	if len(matches) == 0 {
		rows = append(rows, detailStyle.Render("No matching configured models."))
	}
	selected := m.options.Model
	if len(matches) > 0 {
		choice := min(m.modelChoice, len(matches)-1)
		selected = matches[choice]
	}
	rows = append(rows, detailStyle.Render(m.modelCapabilityLine(selected)))
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(strings.Join(rows, "\n"))
}

func (m *model) syncModelLimits() {
	if m.runner == nil {
		return
	}
	m.options.ContextWindowTokens, m.options.AutoCompactTokens, m.options.UsableContextTokens = m.runner.ContextLimits()
}

func (m model) modelCapabilityLine(name string) string {
	contextWindow, compact, usable := m.options.ContextWindowTokens, m.options.AutoCompactTokens, m.options.UsableContextTokens
	capability := "catalog entry not configured"
	if m.options.ModelCatalog != nil {
		if value, ok := m.options.ModelCatalog.Resolve(name); ok {
			contextWindow, compact, usable = m.options.ModelCatalog.Limits(name, contextWindow)
			parts := []string{"text"}
			if value.Supports("image") {
				parts = append(parts, "image")
			}
			if value.ToolCalling == nil || *value.ToolCalling {
				parts = append(parts, "tools")
			}
			if value.ParallelToolCalls != nil && *value.ParallelToolCalls {
				parts = append(parts, "parallel tools")
			}
			capability = strings.Join(parts, ", ")
		}
	}
	return fmt.Sprintf("%d nominal · %d compact · %d usable · %s · reasoning: %s · tier: %s", contextWindow, compact, usable, capability, defaultString(m.options.ReasoningEffort, "default"), defaultString(m.options.ServiceTier, "default"))
}

func (m model) modelPickerHeight() int {
	if value := m.renderModelPicker(max(20, m.width)); value != "" {
		return lipgloss.Height(value)
	}
	return 0
}
