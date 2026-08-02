package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

func (m *model) appendAssistantDelta(delta string) {
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].role != "assistant" {
		m.messages = append(m.messages, message{role: "assistant", streaming: true})
	}
	m.messages[len(m.messages)-1].content += delta
	m.messages[len(m.messages)-1].rendered = ""
}

func (m *model) finishStreamingAssistant() {
	for index := len(m.messages) - 1; index >= 0; index-- {
		if m.messages[index].role == "assistant" {
			m.messages[index].streaming = false
			m.messages[index].rendered = ""
			return
		}
	}
}

func (m *model) addStatus(content string) {
	m.messages = append(m.messages, message{role: "status", content: content})
	m.refreshMessages(true)
}
func (m *model) addError(content string) {
	m.messages = append(m.messages, message{role: "error", content: content})
	m.refreshMessages(true)
}

func (m *model) refreshMessages(gotoBottom bool) {
	contentWidth := max(12, m.viewport.Width()-4)
	if m.renderCacheWidth != contentWidth {
		m.renderCacheWidth = contentWidth
		for index := range m.messages {
			m.messages[index].rendered = ""
		}
	}
	if m.showHelp {
		m.viewport.SetContent(renderMarkdown(helpMarkdown(), contentWidth))
		m.viewport.GotoTop()
		return
	}
	if m.showRawTranscript {
		m.viewport.SetContent(m.rawTranscript())
		m.viewport.GotoTop()
		return
	}
	rendered := make([]string, 0, len(m.messages))
	if len(m.messages) == 0 {
		rendered = append(rendered, statusStyle.Width(contentWidth).Padding(2, 2).Render("Welcome to SuperCode\n\nAsk a question or type /help to see commands."))
	}
	for index := range m.messages {
		item := &m.messages[index]
		switch item.role {
		case "user":
			if item.rendered == "" {
				item.rendered = userStyle.Width(contentWidth).Render("> " + item.content)
			}
			rendered = append(rendered, item.rendered)
		case "assistant":
			if item.content != "" {
				if m.rawMode {
					rendered = append(rendered, lipgloss.NewStyle().Width(contentWidth).Render(item.content))
				} else if item.streaming {
					// Completed cells remain cached, while the active tail is rendered on
					// every streamed delta so headings, lists, emphasis, and code blocks
					// update in place instead of flashing raw Markdown.
					rendered = append(rendered, renderMarkdown(item.content, contentWidth))
				} else {
					if item.rendered == "" {
						item.rendered = renderMarkdown(item.content, contentWidth)
					}
					rendered = append(rendered, item.rendered)
				}
			}
		case "error":
			if item.rendered == "" {
				item.rendered = errorStyle.Width(contentWidth).Render("Error: " + item.content)
			}
			rendered = append(rendered, item.rendered)
		case "tool":
			if item.rendered == "" {
				item.rendered = lipgloss.NewStyle().Width(contentWidth).Render(item.content)
			}
			rendered = append(rendered, item.rendered)
		default:
			if item.rendered == "" {
				item.rendered = statusStyle.Width(contentWidth).Render(item.content)
			}
			rendered = append(rendered, item.rendered)
		}
	}
	m.viewport.SetContent(strings.Join(rendered, "\n\n"))
	if gotoBottom {
		m.viewport.GotoBottom()
	}
}

func (m *model) executeToolForStatus(name, arguments string) {
	if m.options.Tools == nil {
		m.addError("Tools are unavailable.")
		return
	}
	item, ok := m.options.Tools.Lookup(name)
	if !ok {
		m.addError(name + " is unavailable.")
		return
	}
	result, err := item.Execute(m.ctx, arguments)
	if err != nil {
		m.addError(err.Error())
		return
	}
	if result.IsError {
		m.addError(result.Content)
		return
	}
	m.addStatus(result.Content)
}
