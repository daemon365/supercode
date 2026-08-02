package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

func (m model) renderAttachments(width int) string {
	if len(m.draftImageLabels) == 0 && len(m.draftContexts) == 0 {
		return ""
	}
	items := make([]string, 0, len(m.draftImageLabels)+1)
	for index, label := range m.draftImageLabels {
		items = append(items, fmt.Sprintf("[%d] image: %s", index+1, label))
	}
	if len(m.draftContexts) > 0 {
		items = append(items, fmt.Sprintf("%d context file(s)", len(m.draftContexts)))
	}
	return lipgloss.NewStyle().Width(max(10, width-2)).Foreground(accentBright).Render("Attachments · " + strings.Join(items, " · "))
}

func (m model) renderComposer(width int) string {
	lines := make([]string, 0, len(m.draftPastes)+1)
	for index, pasted := range m.draftPastes {
		label := fmt.Sprintf("▣ Pasted context %d · %s chars", index+1, formatCount(utf8.RuneCountInString(pasted)))
		line := lipgloss.NewStyle().Foreground(accentBright).Background(userGray).Padding(0, 1).Render(label)
		if index == len(m.draftPastes)-1 {
			line += statusStyle.Render("  Backspace/Delete remove · /detach all clear")
		}
		lines = append(lines, line)
	}
	lines = append(lines, m.input.View())
	return inputStyle.Width(max(10, width-2)).Render(strings.Join(lines, "\n"))
}

func (m model) composerPasteHeight() int { return len(m.draftPastes) }

func collapsePaste(value string) bool {
	return utf8.RuneCountInString(value) >= 1000 || strings.Count(value, "\n") >= 8
}

func formatCount(value int) string {
	digits := fmt.Sprintf("%d", value)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}

func (m model) attachmentHeight() int {
	if value := m.renderAttachments(max(20, m.width)); value != "" {
		return lipgloss.Height(value)
	}
	return 0
}
