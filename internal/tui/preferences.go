package tui

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func applyTheme(name string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "blue":
		accent, accentBright, panel = lipgloss.Color("#2563EB"), lipgloss.Color("#93C5FD"), lipgloss.Color("#334155")
	case "green":
		accent, accentBright, panel = lipgloss.Color("#059669"), lipgloss.Color("#6EE7B7"), lipgloss.Color("#334155")
	case "mono", "monochrome":
		accent, accentBright, panel = lipgloss.Color("#525252"), lipgloss.Color("#E5E5E5"), lipgloss.Color("#737373")
	default:
		accent, accentBright, panel = lipgloss.Color("#8B5CF6"), lipgloss.Color("#C4B5FD"), lipgloss.Color("#334155")
	}
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(white).Background(accent).Padding(0, 1)
	inputStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(panel).Padding(0, 1)
	userStyle = lipgloss.NewStyle().Foreground(white).Background(userGray).Padding(0, 1)
}

func (m model) updateVimNormal(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "i":
		m.vimNormal = false
		return m, m.input.Focus()
	case "a":
		m.vimNormal = false
		m.input, _ = m.input.Update(tea.KeyPressMsg{Code: tea.KeyRight})
		return m, m.input.Focus()
	case "h":
		m.input, _ = m.input.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	case "l":
		m.input, _ = m.input.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	case "j":
		m.input.CursorDown()
	case "k":
		m.input.CursorUp()
	case "0":
		m.input.CursorStart()
	case "$":
		m.input.CursorEnd()
	case "x":
		m.input, _ = m.input.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
	}
	m.resize(m.width, m.height)
	return m, nil
}

func terminalNotification(mode, message string) tea.Cmd {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "bell"
	}
	if mode == "off" || mode == "none" {
		return nil
	}
	return func() tea.Msg {
		terminal, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
		if err != nil {
			return nil
		}
		defer terminal.Close()
		if mode == "osc9" {
			_, _ = terminal.WriteString("\x1b]9;" + strings.ReplaceAll(message, "\a", "") + "\a")
		} else {
			_, _ = terminal.WriteString("\a")
		}
		return nil
	}
}
