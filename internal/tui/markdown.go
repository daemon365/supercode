package tui

import (
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

var markdownRenderers sync.Map

// renderMarkdown delegates terminal Markdown parsing, wrapping, tables, links,
// and syntax-highlighted code blocks to Glamour instead of maintaining a
// partial Markdown implementation in SuperCode.
func renderMarkdown(markdown string, width int) string {
	width = max(1, width)
	renderer, err := markdownRenderer(width)
	if err != nil {
		return lipgloss.NewStyle().Width(width).Render(markdown)
	}
	rendered, err := renderer.Render(markdown)
	if err != nil {
		return lipgloss.NewStyle().Width(width).Render(markdown)
	}
	return strings.TrimRight(rendered, "\n")
}

func markdownRenderer(width int) (*glamour.TermRenderer, error) {
	if cached, ok := markdownRenderers.Load(width); ok {
		return cached.(*glamour.TermRenderer), nil
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
		glamour.WithTableWrap(true),
		glamour.WithEmoji(),
	)
	if err != nil {
		return nil, err
	}
	actual, _ := markdownRenderers.LoadOrStore(width, renderer)
	return actual.(*glamour.TermRenderer), nil
}
