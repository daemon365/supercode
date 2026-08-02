package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
)

type slashCommand struct {
	name, usage, description, group string
}

var slashCommands = []slashCommand{
	{name: "/help", description: "Show command help", group: "General"},
	{name: "/exit", description: "Exit SuperCode", group: "General"},
	{name: "/editor", description: "Edit the current draft in $VISUAL or $EDITOR", group: "General"},
	{name: "/new", description: "Start a new session", group: "Session"},
	{name: "/clear", description: "Clear the viewport, keep context", group: "Session"},
	{name: "/sessions", usage: "[all]", description: "Search saved sessions", group: "Session"},
	{name: "/resume", usage: "[id|latest]", description: "Open the session picker or resume by ID", group: "Session"},
	{name: "/backtrack", usage: "[turn]", description: "List user turns or fork from one", group: "Session"},
	{name: "/rename", usage: "<title>", description: "Rename the current session", group: "Session"},
	{name: "/fork", description: "Fork the current session", group: "Session"},
	{name: "/archive", description: "Archive the current session", group: "Session"},
	{name: "/delete", usage: "confirm", description: "Delete the current session", group: "Session"},
	{name: "/compact", description: "Compact conversation context", group: "Code & context"},
	{name: "/review", usage: "[focus]", description: "Review current Git changes", group: "Code & context"},
	{name: "/diff", usage: "[staged]", description: "Show a Git diff", group: "Code & context"},
	{name: "/mention", usage: "<path>", description: "Attach a workspace file", group: "Code & context"},
	{name: "/image", usage: "<path|clipboard>", description: "Attach an image to the next message", group: "Code & context"},
	{name: "/detach", usage: "[all|image-number|paste-number]", description: "Remove draft attachments", group: "Code & context"},
	{name: "/queue", description: "Show queued guidance while working", group: "Code & context"},
	{name: "/status", description: "Show runtime status", group: "Runtime"},
	{name: "/config", description: "Show config sources and trust", group: "Runtime"},
	{name: "/model", usage: "[id]", description: "Show or change the model", group: "Runtime"},
	{name: "/reasoning", usage: "[default|low|medium|high|xhigh]", description: "Show or change reasoning effort", group: "Runtime"},
	{name: "/service-tier", usage: "[provider|auto|default|flex|priority]", description: "Show or change service tier", group: "Runtime"},
	{name: "/theme", usage: "[violet|blue|green|mono]", description: "Show or change the TUI theme", group: "Runtime"},
	{name: "/keymap", usage: "[standard|vim]", description: "Show or change composer keymap", group: "Runtime"},
	{name: "/permissions", usage: "[mode]", description: "Show or change approvals", group: "Runtime"},
	{name: "/mode", usage: "[default|plan|execute|pair]", description: "Show or change collaboration mode", group: "Runtime"},
	{name: "/plan", usage: "[on|off|show|hide]", description: "Toggle Plan mode or the plan panel", group: "Runtime"},
	{name: "/goal", description: "Show the active goal", group: "Runtime"},
	{name: "/tools", description: "List available tools", group: "Runtime"},
	{name: "/mcp", description: "Show connected MCP servers", group: "Runtime"},
	{name: "/plugins", description: "Show enabled plugins", group: "Runtime"},
	{name: "/hooks", description: "Show enabled lifecycle hooks", group: "Runtime"},
	{name: "/agents", usage: "[name]", description: "Show the sub-agent tree or one transcript", group: "Runtime"},
	{name: "/skills", usage: "[reload]", description: "Search or reload discovered skills", group: "Runtime"},
	{name: "/ps", description: "List background processes", group: "Runtime"},
	{name: "/stop", usage: "<id|all>", description: "Stop background processes", group: "Runtime"},
	{name: "/copy", usage: "[assistant|tool|transcript|all]", description: "Copy conversation output", group: "Output & memory"},
	{name: "/raw", description: "Open a copy-friendly raw transcript", group: "Output & memory"},
	{name: "/markdown", description: "Toggle Markdown rendering", group: "Output & memory"},
	{name: "/memory", description: "Show long-term memory", group: "Output & memory"},
	{name: "/remember", usage: "<text>", description: "Save an explicit memory", group: "Output & memory"},
	{name: "/forget", usage: "[text]", description: "Queue a memory deletion or correction", group: "Output & memory"},
}

func slashCommandNames() []string {
	names := make([]string, 0, len(slashCommands))
	for _, command := range slashCommands {
		names = append(names, command.name)
	}
	return names
}

type rankedSlashCommand struct {
	command slashCommand
	score   int
	order   int
}

// matchingSlashCommands returns fuzzy matches while preserving the curated
// presentation order for equally good results. Prefix matches always rank
// ahead of subsequence matches.
func matchingSlashCommands(value string) []slashCommand {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, " \t\r\n") {
		return nil
	}
	query := strings.ToLower(strings.TrimPrefix(value, "/"))
	ranked := make([]rankedSlashCommand, 0, len(slashCommands))
	for order, command := range slashCommands {
		candidate := strings.TrimPrefix(strings.ToLower(command.name), "/")
		score, ok := slashFuzzyScore(candidate, query)
		if !ok {
			continue
		}
		ranked = append(ranked, rankedSlashCommand{command: command, score: score, order: order})
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].score == ranked[right].score {
			return ranked[left].order < ranked[right].order
		}
		return ranked[left].score < ranked[right].score
	})
	matches := make([]slashCommand, 0, len(ranked))
	for _, match := range ranked {
		matches = append(matches, match.command)
	}
	return matches
}

func slashFuzzyScore(candidate, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	if strings.HasPrefix(candidate, query) {
		return len(candidate) - len(query), true
	}
	position, gaps := 0, 0
	for _, character := range query {
		found := strings.IndexRune(candidate[position:], character)
		if found < 0 {
			return 0, false
		}
		gaps += found
		position += found + 1
	}
	return 100 + gaps + len(candidate) - len(query), true
}

func commandByName(name string) (slashCommand, bool) {
	for _, command := range slashCommands {
		if command.name == name {
			return command, true
		}
	}
	return slashCommand{}, false
}

func similarSlashCommand(name string) string {
	best, bestPrefix := "", 1
	for _, command := range slashCommands {
		prefix := commonPrefix(strings.ToLower(name), command.name)
		if prefix > bestPrefix {
			best, bestPrefix = command.name, prefix
		}
	}
	return best
}

func commonPrefix(left, right string) int {
	maximum := min(len(left), len(right))
	for index := 0; index < maximum; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return maximum
}

func helpMarkdown() string {
	var output strings.Builder
	output.WriteString("# SuperCode commands\n\n")
	output.WriteString("Type `/` to search commands. Use `↑`/`↓` to choose, `Tab` to complete, and `Enter` to run.\n")
	group := ""
	for _, command := range slashCommands {
		if command.group != group {
			group = command.group
			output.WriteString("\n**" + group + "**\n\n")
		}
		usage := command.name
		if command.usage != "" {
			usage += " " + command.usage
		}
		fmt.Fprintf(&output, "- `%s` — %s\n", usage, command.description)
	}
	return output.String()
}

func (m model) commandMenuVisible() bool {
	value := m.input.Value()
	return !m.commandMenuDismissed && m.pendingApproval == nil && len(matchingSlashCommands(value)) > 0
}

func (m model) renderCommandMenu(width int) string {
	if !m.commandMenuVisible() {
		return ""
	}
	matches := matchingSlashCommands(m.input.Value())
	selected := min(max(0, m.commandChoice), len(matches)-1)
	const visible = 6
	start := 0
	if selected >= visible {
		start = selected - visible + 1
	}
	end := min(len(matches), start+visible)
	lines := []string{titleStyle.Render("Commands")}
	for index := start; index < end; index++ {
		command := matches[index]
		marker := "  "
		nameStyle := lipgloss.NewStyle().Foreground(white)
		if index == selected {
			marker = "› "
			nameStyle = nameStyle.Bold(true).Foreground(accentBright)
		}
		label := command.name
		if command.usage != "" {
			label += " " + command.usage
		}
		available := max(8, width-8)
		description := command.description
		if lipgloss.Width(label)+lipgloss.Width(description)+4 > available {
			limit := max(8, available-lipgloss.Width(label)-5)
			description = truncateRunes(description, limit)
		}
		gap := strings.Repeat(" ", max(2, available-lipgloss.Width(label)-lipgloss.Width(description)))
		lines = append(lines, marker+nameStyle.Render(label)+gap+detailStyle.Render(description))
	}
	if len(matches) > visible {
		lines = append(lines, detailStyle.Render(fmt.Sprintf("  %d of %d matches", selected+1, len(matches))))
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(panel).Padding(0, 1).Width(max(10, width-2)).Render(strings.Join(lines, "\n"))
}

func (m model) selectedSlashCommand() (slashCommand, bool) {
	matches := matchingSlashCommands(m.input.Value())
	if len(matches) == 0 {
		return slashCommand{}, false
	}
	return matches[min(max(0, m.commandChoice), len(matches)-1)], true
}

func (m model) commandMenuHeight() int {
	return lipgloss.Height(m.renderCommandMenu(max(20, m.width)))
}

func truncateRunes(value string, limit int) string {
	characters := []rune(value)
	if len(characters) <= limit {
		return value
	}
	if limit <= 1 {
		return "…"
	}
	return string(characters[:limit-1]) + "…"
}
