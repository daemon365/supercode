package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/taskstate"
)

func (m model) renderPlan(width int) string {
	if !m.showPlan || m.taskState == nil {
		return ""
	}
	plan, goal := m.taskState.Snapshot()
	if len(plan.Steps) == 0 && goal == nil {
		return ""
	}
	lines := []string{}
	if goal != nil {
		goalLine := lipgloss.NewStyle().Bold(true).Foreground(accentBright).Render("GOAL") + "  " + goal.Objective + "  " + statusStyle.Render("["+goal.Status+"]")
		if goal.TokenBudget > 0 {
			goalLine += statusStyle.Render(fmt.Sprintf("  %d / %d tokens", goal.TotalTokens, goal.TokenBudget))
		} else if goal.TotalTokens > 0 {
			goalLine += statusStyle.Render(fmt.Sprintf("  %d tokens", goal.TotalTokens))
		}
		lines = append(lines, goalLine)
	}
	if plan.Explanation != "" {
		lines = append(lines, statusStyle.Render(preview(plan.Explanation, 180)))
	}
	for index, step := range plan.Steps {
		if index >= 7 {
			lines = append(lines, statusStyle.Render(fmt.Sprintf("  … %d more steps", len(plan.Steps)-index)))
			break
		}
		icon, style := "○", statusStyle
		switch step.Status {
		case taskstate.StatusCompleted:
			icon, style = "✓", lipgloss.NewStyle().Foreground(lipgloss.Color("#34D399"))
		case taskstate.StatusInProgress:
			icon, style = "●", lipgloss.NewStyle().Bold(true).Foreground(accentBright)
		}
		lines = append(lines, style.Render(icon+" "+step.Step))
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(accentBright).Render("PLAN")
	body := title + "\n" + strings.Join(lines, "\n")
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(panel).Padding(0, 1).Render(body)
}

func (m model) renderQueued(width int) string {
	if len(m.queuedMessages) == 0 {
		return ""
	}
	lines := []string{lipgloss.NewStyle().Bold(true).Foreground(accentBright).Render("Messages to be submitted after next tool call")}
	for index, value := range m.queuedMessages {
		if index >= 3 {
			lines = append(lines, statusStyle.Render(fmt.Sprintf("  … %d more", len(m.queuedMessages)-index)))
			break
		}
		lines = append(lines, statusStyle.Render("  ↳ "+preview(value, 140)))
	}
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(panel).Padding(0, 1).Render(strings.Join(lines, "\n"))
}

func (m model) planHeight() int {
	value := m.renderPlan(max(20, m.width))
	if value == "" {
		return 0
	}
	return lipgloss.Height(value)
}
func (m model) queueHeight() int {
	value := m.renderQueued(max(20, m.width))
	if value == "" {
		return 0
	}
	return lipgloss.Height(value)
}

func (m model) approvalHeight() int {
	value := m.renderApproval(max(20, m.width))
	if value == "" {
		return 0
	}
	return lipgloss.Height(value)
}
func (m model) queueSummary() string {
	if len(m.queuedMessages) == 0 {
		return "No messages are queued."
	}
	return "Queued messages:\n- " + strings.Join(m.queuedMessages, "\n- ")
}

func (m model) copyOutput(target string) string {
	switch target {
	case "assistant", "tool":
		for index := len(m.messages) - 1; index >= 0; index-- {
			item := m.messages[index]
			if item.role != target {
				continue
			}
			content := item.content
			if target == "tool" && item.copyContent != "" {
				content = item.copyContent
			}
			if strings.TrimSpace(content) != "" {
				return ansi.Strip(content)
			}
		}
	case "transcript":
		return m.rawTranscript()
	case "all":
		var output []string
		for _, item := range m.messages {
			if item.role != "assistant" && item.role != "tool" {
				continue
			}
			content := item.content
			if item.role == "tool" && item.copyContent != "" {
				content = item.copyContent
			}
			if strings.TrimSpace(content) != "" {
				output = append(output, ansi.Strip(content))
			}
		}
		return strings.Join(output, "\n\n")
	}
	return ""
}

func (m model) rawTranscript() string {
	var output []string
	for _, item := range m.messages {
		content := item.content
		if item.role == "tool" && item.copyContent != "" {
			content = item.copyContent
		}
		content = strings.TrimSpace(ansi.Strip(content))
		if content == "" {
			continue
		}
		switch item.role {
		case "user":
			lines := strings.Split(content, "\n")
			for index := range lines {
				lines[index] = "> " + lines[index]
			}
			output = append(output, strings.Join(lines, "\n"))
		case "assistant":
			output = append(output, content)
		case "tool":
			output = append(output, "[tool]\n"+content)
		case "error":
			output = append(output, "Error: "+content)
		default:
			output = append(output, content)
		}
	}
	return strings.Join(output, "\n\n")
}

func preview(value string, maximum int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maximum {
		return string(runes[:maximum]) + "…"
	}
	return value
}

func nextAgentEvent(events <-chan agent.Event) tea.Cmd {
	return func() tea.Msg { event, ok := <-events; return agentEventMsg{events: events, event: event, ok: ok} }
}

func shortID(id string) string {
	if len(id) <= 13 {
		return id
	}
	return id[len(id)-13:]
}
func truncateTitle(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 60 {
		return string(runes[:57]) + "..."
	}
	return value
}
