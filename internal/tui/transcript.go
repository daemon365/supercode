package tui

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const streamingTranscriptPrefixBytes = 64 * 1024

const (
	streamingAssistantBytes        = 64 * 1024
	transcriptCellSourceBytes      = 128 * 1024
	transcriptCellViewBytes        = 512 * 1024
	transcriptViewBytes            = 2 * 1024 * 1024
	liveTranscriptTruncatedMessage = "… older content hidden in the live view; use /raw for the complete transcript …"
)

func (m *model) appendAssistantDelta(delta string) {
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].role != "assistant" {
		m.messages = append(m.messages, message{role: "assistant", streaming: true})
	}
	item := &m.messages[len(m.messages)-1]
	if len(item.streamChunks) == 0 && item.content != "" {
		item.streamChunks = append(item.streamChunks, item.content)
		item.streamBytes = len(item.content)
		item.streamTail = boundedTextSuffix(item.content, streamingAssistantBytes)
		item.content = ""
	}
	item.streamChunks = append(item.streamChunks, delta)
	item.streamBytes += len(delta)
	item.streamTail = boundedTextSuffix(item.streamTail+delta, streamingAssistantBytes)
	item.rendered = ""
}

func (m *model) finishStreamingAssistant() {
	for index := len(m.messages) - 1; index >= 0; index-- {
		if m.messages[index].role == "assistant" {
			item := &m.messages[index]
			if len(item.streamChunks) > 0 {
				item.content = strings.Join(item.streamChunks, "")
			}
			item.streaming = false
			item.streamChunks, item.streamTail, item.streamBytes = nil, "", 0
			item.rendered = ""
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
		m.resetTranscriptCache()
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
	if len(m.messages) == 0 {
		m.viewport.SetContent(statusStyle.Width(contentWidth).Padding(2, 2).Render("Welcome to SuperCode\n\nAsk a question or type /help to see commands."))
		if gotoBottom {
			m.viewport.GotoBottom()
		}
		return
	}

	// Everything before the first streaming assistant or running tool is
	// immutable. Cache that already-joined prefix so a token delta only renders
	// and joins the small mutable tail instead of rebuilding a long transcript.
	mutableStart := len(m.messages)
	for index := range m.messages {
		if m.messages[index].streaming || m.messages[index].toolRunning {
			mutableStart = index
			break
		}
	}
	if m.transcriptPrefixSize > mutableStart || m.transcriptPrefixSize > len(m.messages) {
		m.resetTranscriptCache()
	}
	for m.transcriptPrefixSize < mutableStart {
		index := m.transcriptPrefixSize
		cell := m.renderTranscriptMessage(index, contentWidth)
		if cell != "" {
			m.appendTranscriptPrefix(cell)
			m.appendTranscriptTail(cell)
		}
		if len(m.messages[index].content) > transcriptCellSourceBytes {
			m.transcriptPrefixCut = true
			m.transcriptTailCut = true
		}
		m.transcriptPrefixSize++
	}
	content := m.transcriptPrefix
	contentCut := false
	if mutableStart < len(m.messages) {
		content = m.transcriptPrefixTail
		if m.transcriptTailCut {
			content = appendTranscriptCell(statusStyle.Render("… older transcript hidden while output is streaming …"), content)
		}
	} else if m.transcriptPrefixCut {
		content = appendTranscriptCell(statusStyle.Render("… older transcript hidden in the live view; use /raw for the complete transcript …"), content)
	}
	for index := m.transcriptPrefixSize; index < len(m.messages); index++ {
		var cut bool
		content, cut = appendBoundedTranscriptCell(content, m.renderTranscriptMessage(index, contentWidth), transcriptViewBytes)
		contentCut = contentCut || cut
	}
	if contentCut {
		content = appendTranscriptCell(statusStyle.Render(liveTranscriptTruncatedMessage), content)
	}
	m.viewport.SetContent(content)
	if gotoBottom {
		m.viewport.GotoBottom()
	}
}

func (m *model) appendTranscriptPrefix(cell string) {
	var cut bool
	m.transcriptPrefix, cut = appendBoundedTranscriptCell(m.transcriptPrefix, cell, transcriptViewBytes)
	m.transcriptPrefixCut = m.transcriptPrefixCut || cut
}

func (m *model) resetTranscriptCache() {
	m.transcriptPrefix = ""
	m.transcriptPrefixTail = ""
	m.transcriptPrefixCut = false
	m.transcriptTailCut = false
	m.transcriptPrefixSize = 0
}

func (m *model) appendTranscriptTail(cell string) {
	var cut bool
	m.transcriptPrefixTail, cut = appendBoundedTranscriptCell(m.transcriptPrefixTail, cell, streamingTranscriptPrefixBytes)
	m.transcriptTailCut = m.transcriptTailCut || cut
}

func appendTranscriptCell(content, cell string) string {
	if cell == "" {
		return content
	}
	if content == "" {
		return cell
	}
	return content + "\n\n" + cell
}

// appendBoundedTranscriptCell retains a recent live-view window without ever
// constructing a string proportional to the complete history. Raw/copy data
// remains on message.content and is intentionally unaffected.
func appendBoundedTranscriptCell(content, cell string, maximum int) (string, bool) {
	if cell == "" {
		return content, false
	}
	if maximum <= 0 {
		return "", content != "" || cell != ""
	}
	separator := ""
	if content != "" {
		separator = "\n\n"
	}
	if len(content)+len(separator)+len(cell) <= maximum {
		return content + separator + cell, false
	}
	// A byte cut through terminal styling can leave an unterminated escape
	// sequence. Once a window rolls over, retain a plain-text suffix instead.
	plainCell := ansi.Strip(cell)
	if len(plainCell) >= maximum {
		return boundedTextSuffix(plainCell, maximum), true
	}
	plainContent := ansi.Strip(content)
	available := maximum - len(plainCell)
	if plainContent != "" {
		available -= 2
	}
	if available <= 0 {
		return boundedTextSuffix(plainCell, maximum), true
	}
	plainContent = boundedTextSuffix(plainContent, available)
	return appendTranscriptCell(plainContent, plainCell), true
}

func (m *model) renderTranscriptMessage(index, contentWidth int) string {
	item := &m.messages[index]
	switch item.role {
	case "user":
		if item.rendered == "" {
			item.rendered = renderBoundedTranscriptCell(item.content, func(value string) string {
				return userStyle.Width(contentWidth).Render("> " + value)
			})
		}
		return item.rendered
	case "assistant":
		if item.content == "" && item.streamTail == "" {
			return ""
		}
		if item.streaming {
			body := item.streamTail
			if body == "" {
				body = item.content
			}
			var rendered string
			if m.rawMode {
				rendered = lipgloss.NewStyle().Width(contentWidth).Render(body)
			} else {
				rendered = renderMarkdown(body, contentWidth)
			}
			if item.streamBytes > len(item.streamTail) && item.streamTail != "" {
				return appendTranscriptCell(statusStyle.Render("… older response hidden while streaming …"), rendered)
			}
			return rendered
		}
		if item.rendered == "" {
			item.rendered = renderBoundedTranscriptCell(item.content, func(value string) string {
				if m.rawMode {
					return lipgloss.NewStyle().Width(contentWidth).Render(value)
				}
				return renderMarkdown(value, contentWidth)
			})
		}
		return item.rendered
	case "error":
		if item.rendered == "" {
			item.rendered = renderBoundedTranscriptCell(item.content, func(value string) string {
				return errorStyle.Width(contentWidth).Render("Error: " + value)
			})
		}
		return item.rendered
	case "tool":
		if item.rendered == "" {
			item.rendered = renderBoundedTranscriptCell(item.content, func(value string) string {
				return lipgloss.NewStyle().Width(contentWidth).Render(value)
			})
		}
		return item.rendered
	default:
		if item.rendered == "" {
			item.rendered = renderBoundedTranscriptCell(item.content, func(value string) string {
				return statusStyle.Width(contentWidth).Render(value)
			})
		}
		return item.rendered
	}
}

func renderBoundedTranscriptCell(value string, render func(string) string) string {
	window := boundedTextSuffix(value, transcriptCellSourceBytes)
	cut := len(window) < len(value)
	rendered := render(window)
	if len(rendered) > transcriptCellViewBytes {
		rendered = boundedTextSuffix(ansi.Strip(rendered), transcriptCellViewBytes)
		cut = true
	}
	if cut {
		return appendTranscriptCell(statusStyle.Render(liveTranscriptTruncatedMessage), rendered)
	}
	return rendered
}

func boundedTextSuffix(value string, maximum int) string {
	if maximum <= 0 || len(value) <= maximum {
		return value
	}
	start := len(value) - maximum
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func (m *model) executeToolForStatus(name, arguments string) tea.Cmd {
	if m.options.Tools == nil {
		m.addError("Tools are unavailable.")
		return nil
	}
	item, ok := m.options.Tools.Lookup(name)
	if !ok {
		m.addError(name + " is unavailable.")
		return nil
	}
	m.runtimeJobs++
	ctx := m.ctx
	return func() tea.Msg {
		result, err := item.Execute(ctx, arguments)
		return toolStatusMsg{name: name, result: result, err: err}
	}
}
