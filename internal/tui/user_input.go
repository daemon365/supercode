package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/daemon365/supercode/internal/userinput"
)

func nextUserInput(requests <-chan *userinput.Request) tea.Cmd {
	return func() tea.Msg { return userInputMsg{requests: requests, request: <-requests} }
}

func (m model) updateUserInput(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pendingUserInput == nil || m.userInputQuestion >= len(m.pendingUserInput.Questions) {
		return m, nil
	}
	question := m.pendingUserInput.Questions[m.userInputQuestion]
	if m.userInputCustom {
		switch message.String() {
		case "esc":
			m.userInputCustom = false
			m.input.SetValue("")
			m.input.Blur()
			m.resize(m.width, m.height)
			return m, nil
		case "enter":
			answer := strings.TrimSpace(m.input.Value())
			if answer == "" {
				return m, nil
			}
			return m.acceptUserInput(question.ID, answer)
		}
		var command tea.Cmd
		m.input, command = m.input.Update(message)
		m.resize(m.width, m.height)
		return m, command
	}
	choices := len(question.Options) + 1
	switch message.String() {
	case "up", "k", "shift+tab":
		m.userInputChoice = (m.userInputChoice - 1 + choices) % choices
		return m, nil
	case "down", "j", "tab":
		m.userInputChoice = (m.userInputChoice + 1) % choices
		return m, nil
	case "o":
		m.userInputChoice = len(question.Options)
		m.userInputCustom = true
		m.input.SetValue("")
		m.input.Placeholder = "Type a custom answer…"
		m.resize(m.width, m.height)
		return m, m.input.Focus()
	case "esc":
		m.pendingUserInput.Decide(map[string]string{"cancelled": "true"})
		return m.finishUserInput()
	case "enter":
		if m.userInputChoice == len(question.Options) {
			m.userInputCustom = true
			m.input.SetValue("")
			m.input.Placeholder = "Type a custom answer…"
			m.resize(m.width, m.height)
			return m, m.input.Focus()
		}
		return m.acceptUserInput(question.ID, question.Options[m.userInputChoice].Label)
	}
	return m, nil
}

func (m model) acceptUserInput(id, answer string) (tea.Model, tea.Cmd) {
	m.userInputAnswers[id] = answer
	m.userInputQuestion++
	m.userInputChoice = 0
	m.userInputCustom = false
	m.input.Reset()
	if m.userInputQuestion < len(m.pendingUserInput.Questions) {
		m.input.Blur()
		m.resize(m.width, m.height)
		return m, nil
	}
	m.pendingUserInput.Decide(m.userInputAnswers)
	return m.finishUserInput()
}

func (m model) finishUserInput() (tea.Model, tea.Cmd) {
	m.pendingUserInput = nil
	m.userInputQuestion, m.userInputChoice = 0, 0
	m.userInputAnswers = nil
	m.userInputCustom = false
	m.input.SetValue(m.userInputDraft)
	m.input.MoveToEnd()
	m.input.Placeholder = "Send a message after the next tool call…"
	m.userInputDraft = ""
	m.resize(m.width, m.height)
	commands := []tea.Cmd{m.input.Focus()}
	if m.options.UserInput != nil {
		commands = append(commands, nextUserInput(m.options.UserInput.Requests()))
	}
	return m, tea.Batch(commands...)
}

func (m model) renderUserInput(width int) string {
	if m.pendingUserInput == nil || m.userInputQuestion >= len(m.pendingUserInput.Questions) {
		return ""
	}
	question := m.pendingUserInput.Questions[m.userInputQuestion]
	rows := []string{
		titleStyle.Render(fmt.Sprintf("%s  (%d/%d)", question.Header, m.userInputQuestion+1, len(m.pendingUserInput.Questions))),
		question.Question,
		"",
	}
	for index, option := range question.Options {
		marker := "  "
		style := lipgloss.NewStyle().Foreground(white)
		if index == m.userInputChoice && !m.userInputCustom {
			marker = "› "
			style = style.Bold(true).Foreground(accentBright)
		}
		rows = append(rows, style.Render(marker+option.Label), detailStyle.Render("    "+option.Description))
	}
	marker := "  "
	style := lipgloss.NewStyle().Foreground(white)
	if m.userInputChoice == len(question.Options) || m.userInputCustom {
		marker = "› "
		style = style.Bold(true).Foreground(accentBright)
	}
	rows = append(rows, style.Render(marker+"Custom answer"))
	if m.userInputCustom {
		rows = append(rows, detailStyle.Render("    Type below and press Enter; Esc returns to choices."))
	}
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(strings.Join(rows, "\n"))
}

func (m model) userInputHeight() int {
	if value := m.renderUserInput(max(20, m.width)); value != "" {
		return lipgloss.Height(value)
	}
	return 0
}
