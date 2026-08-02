package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/permission"
)

var (
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#34D399"))
	deleteStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FB7185"))
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(white)
	detailStyle  = lipgloss.NewStyle().Foreground(muted)
)

const maxLiveToolOutput = 64 * 1024

func boundedLiveToolOutput(value string) string {
	if len(value) <= maxLiveToolOutput {
		return value
	}
	return "[earlier live output truncated]\n" + value[len(value)-maxLiveToolOutput:]
}

func renderLiveToolOutput(value string) string {
	value = strings.TrimRight(value, "\r\n")
	if value == "" {
		return ""
	}
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		line = strings.ReplaceAll(line, "\r", "")
		if index == 0 {
			lines[index] = detailStyle.Render("  └ " + preview(line, 220))
		} else {
			lines[index] = detailStyle.Render("    " + preview(line, 220))
		}
	}
	return "\n" + strings.Join(lines, "\n")
}

func formatToolEvent(event agent.Event, finished bool) string {
	if event.Call == nil {
		return toolLine(event, finished, "Called tool", "")
	}
	arguments := argumentMap(event.Call.Arguments)
	if strings.HasPrefix(event.Call.Name, "mcp__") {
		parts := strings.Split(event.Call.Name, "__")
		server, remote := "server", event.Call.Name
		if len(parts) >= 3 {
			server, remote = parts[1], strings.Join(parts[2:], "/")
		}
		detail := compactArguments(arguments)
		if finished && toolSucceeded(event) && detail == "" {
			detail = selectedOutput(event, 5)
		}
		return toolLine(event, finished, choose(finished, "Called ", "Calling ")+server+" · "+remote, detail)
	}
	switch event.Call.Name {
	case "list_files":
		path := defaultString(stringValue(arguments, "path"), ".")
		return toolLine(event, finished, choose(finished, "Listed ", "Listing ")+path, "")
	case "search_text":
		query := strconv.Quote(stringValue(arguments, "query"))
		scope := searchScope(arguments)
		detail := ""
		if finished && toolSucceeded(event) {
			detail = selectedOutput(event, 5)
		}
		return toolLine(event, finished, choose(finished, "Searched for ", "Searching for ")+query+" in "+scope, detail)
	case "read_file":
		path := stringValue(arguments, "path")
		rangeText := lineRange(arguments)
		return toolLine(event, finished, choose(finished, "Read ", "Reading ")+path+rangeText, "")
	case "apply_patch":
		return formatPatch(event, arguments, finished)
	case "run_command", "exec_command":
		return formatCommand(event, arguments, finished)
	case "write_stdin":
		session := numberValue(arguments, "session_id")
		chars := stringValue(arguments, "chars")
		title := choose(finished, "Continued", "Continuing") + " process #" + session
		if chars != "" {
			title = choose(finished, "Wrote to", "Writing to") + " process #" + session
		}
		return toolLine(event, finished, title, commandResultOutput(event))
	case "wait":
		session := numberValue(arguments, "session_id")
		title := choose(finished, "Waited for", "Waiting for") + " process #" + session
		if boolValue(arguments, "terminate") {
			title = choose(finished, "Stopped", "Stopping") + " process #" + session
		}
		return toolLine(event, finished, title, commandResultOutput(event))
	case "list_processes":
		return toolLine(event, finished, choose(finished, "Listed background processes", "Listing background processes"), selectedOutput(event, 8))
	case "stop_process":
		target := numberValue(arguments, "session_id")
		if boolValue(arguments, "all") {
			target = "all"
		}
		return toolLine(event, finished, choose(finished, "Stopped ", "Stopping ")+"process "+target, selectedOutput(event, 3))
	case "tool_search":
		return toolLine(event, finished, choose(finished, "Searched tools", "Searching tools"), stringValue(arguments, "query"))
	case "git_status":
		return toolLine(event, finished, choose(finished, "Checked", "Checking")+" Git status", selectedOutput(event, 8))
	case "git_diff":
		kind := "working tree"
		if boolValue(arguments, "staged") {
			kind = "staged changes"
		}
		return toolLine(event, finished, choose(finished, "Inspected ", "Inspecting ")+kind, selectedOutput(event, 10))
	case "view_image":
		return toolLine(event, finished, choose(finished, "Viewed ", "Viewing ")+stringValue(arguments, "path"), "")
	case "web__run":
		return formatWeb(event, arguments, finished)
	case "update_plan":
		steps := arrayLength(arguments, "plan")
		detail := fmt.Sprintf("%d step", steps)
		if steps != 1 {
			detail += "s"
		}
		return toolLine(event, finished, choose(finished, "Updated plan", "Updating plan"), detail)
	case "create_goal":
		return toolLine(event, finished, choose(finished, "Created goal", "Creating goal"), stringValue(arguments, "objective"))
	case "get_goal":
		return toolLine(event, finished, choose(finished, "Checked goal", "Checking goal"), "")
	case "update_goal":
		status := stringValue(arguments, "status")
		return toolLine(event, finished, choose(finished, "Marked goal ", "Marking goal ")+status, "")
	case "spawn_agent":
		return toolLine(event, finished, choose(finished, "Spawned ", "Spawning ")+stringValue(arguments, "task_name"), "")
	case "send_message":
		return toolLine(event, finished, choose(finished, "Messaged ", "Messaging ")+stringValue(arguments, "target"), preview(stringValue(arguments, "message"), 180))
	case "followup_task":
		return toolLine(event, finished, choose(finished, "Followed up with ", "Following up with ")+stringValue(arguments, "target"), preview(stringValue(arguments, "message"), 180))
	case "interrupt_agent":
		return toolLine(event, finished, choose(finished, "Interrupted ", "Interrupting ")+stringValue(arguments, "target"), "")
	case "list_agents":
		return toolLine(event, finished, choose(finished, "Listed sub-agents", "Listing sub-agents"), agentListOutput(event))
	case "wait_agent":
		target := stringValue(arguments, "target")
		return toolLine(event, finished, choose(finished, "Waited for ", "Waiting for ")+target, agentWaitOutput(event))
	case "request_user_input":
		return toolLine(event, finished, choose(finished, "Received user input", "Waiting for user input"), firstNestedString(arguments, "questions", "question"))
	case "request_permissions":
		reason := stringValue(arguments, "reason")
		return toolLine(event, finished, choose(finished, "Granted permissions", "Requesting permissions"), reason)
	default:
		detail := compactArguments(arguments)
		if finished && toolSucceeded(event) && detail == "" {
			detail = selectedOutput(event, 4)
		}
		return toolLine(event, finished, choose(finished, "Called ", "Calling ")+event.Call.Name, detail)
	}
}

func searchScope(arguments map[string]any) string {
	path := stringValue(arguments, "path")
	glob := stringValue(arguments, "glob")
	if path == "" || path == "." {
		if glob == "" {
			return "workspace files"
		}
		return "files matching " + glob
	}
	if glob == "" {
		return path
	}
	return path + " files matching " + glob
}

func formatCommand(event agent.Event, arguments map[string]any, finished bool) string {
	command := stringValue(arguments, "cmd")
	if command == "" {
		command = stringValue(arguments, "command")
	}
	title := "Running " + command
	if finished {
		title = "Ran " + command
		if state, ok := processToolResult(event); ok && state.Running {
			title = fmt.Sprintf("Started %s  [process #%d]", command, state.SessionID)
		}
	}
	detail := ""
	if stringValue(arguments, "sandbox_permissions") == "require-escalated" {
		detail = "Outside sandbox"
		if justification := stringValue(arguments, "justification"); justification != "" {
			detail += " — " + justification
		}
	}
	if output := commandResultOutput(event); output != "" {
		if detail != "" {
			detail += "\n"
		}
		detail += output
	}
	return toolLine(event, finished, title, detail)
}

type processResult struct {
	Output          string  `json:"output"`
	SessionID       int64   `json:"session_id"`
	Running         bool    `json:"running"`
	ExitCode        *int    `json:"exit_code"`
	WallTimeSeconds float64 `json:"wall_time_seconds"`
}

func processToolResult(event agent.Event) (processResult, bool) {
	if event.Result == nil {
		return processResult{}, false
	}
	var result processResult
	if json.Unmarshal([]byte(event.Result.Content), &result) != nil || result.SessionID == 0 && result.Output == "" && result.ExitCode == nil {
		return processResult{}, false
	}
	return result, true
}

func formatPatch(event agent.Event, arguments map[string]any, finished bool) string {
	if diff := rawStringValue(arguments, "unified_diff"); diff != "" {
		added, removed := 0, 0
		for _, line := range strings.Split(diff, "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added++
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				removed++
			}
		}
		return toolLineFull(event, finished, choose(finished, "Applied", "Applying")+fmt.Sprintf(" unified diff (+%d -%d)", added, removed), renderUnifiedDiff(diff))
	}
	if operations, ok := arguments["operations"].([]any); ok && len(operations) > 0 {
		verb := choose(finished, "Edited", "Editing")
		if finished && !toolSucceeded(event) {
			return toolLine(event, true, "Could not apply multi-file edit", selectedOutput(event, 4))
		}
		lines := make([]string, 0, len(operations))
		for _, raw := range operations {
			operation, _ := raw.(map[string]any)
			path := stringValue(operation, "path")
			action := "edit"
			if boolValue(operation, "delete") {
				action = "delete"
			} else if stringValue(operation, "move_to") != "" {
				action = "move to " + stringValue(operation, "move_to")
			} else if rawStringValue(operation, "old_text") == "" {
				action = "add"
			}
			oldText, newText := rawStringValue(operation, "old_text"), rawStringValue(operation, "new_text")
			header := fmt.Sprintf("%s  %s (+%d -%d)", action, path, lineCount(newText), lineCount(oldText))
			if detail := renderPatchLines(oldText, newText, boolValue(operation, "delete")); detail != "" {
				header += "\n" + detail
			}
			lines = append(lines, header)
		}
		return toolLineFull(event, finished, fmt.Sprintf("%s %d files", verb, len(operations)), strings.Join(lines, "\n"))
	}
	path := stringValue(arguments, "path")
	if moveTo := stringValue(arguments, "move_to"); moveTo != "" {
		return toolLine(event, finished, choose(finished, "Moved ", "Moving ")+path+" → "+moveTo, "")
	}
	oldText, newText := rawStringValue(arguments, "old_text"), rawStringValue(arguments, "new_text")
	deleting := boolValue(arguments, "delete")
	verb := "Editing"
	if deleting {
		verb = choose(finished, "Deleted", "Deleting")
	} else if oldText == "" {
		verb = choose(finished, "Added", "Adding")
	} else if finished {
		verb = "Edited"
	}
	added, removed := lineCount(newText), lineCount(oldText)
	header := fmt.Sprintf("%s %s (+%d -%d)", verb, path, added, removed)
	if finished && !toolSucceeded(event) {
		if event.Result != nil && strings.Contains(strings.ToLower(event.Result.Content), "denied") {
			return toolLine(event, true, "Skipped edit "+path, "Denied by approval policy.")
		}
		return toolLine(event, true, "Could not edit "+path, selectedOutput(event, 3))
	}
	return toolLineFull(event, finished, header, renderPatchLines(oldText, newText, deleting))
}

func renderPatchLines(oldText, newText string, deleting bool) string {
	var lines []string
	appendLines := func(value, sign string, style lipgloss.Style) {
		for index, line := range textLines(value) {
			lines = append(lines, detailStyle.Render(fmt.Sprintf("%4d ", index+1))+style.Render(sign+line))
		}
	}
	if oldText != "" {
		appendLines(oldText, "-", deleteStyle)
	}
	if !deleting && newText != "" {
		appendLines(newText, "+", successStyle)
	}
	return strings.Join(lines, "\n")
}

func renderUnifiedDiff(diff string) string {
	lines := textLines(diff)
	for index, line := range lines {
		style := detailStyle
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			style = successStyle
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			style = deleteStyle
		}
		lines[index] = style.Render(line)
	}
	return strings.Join(lines, "\n")
}

func formatWeb(event agent.Event, arguments map[string]any, finished bool) string {
	title, detail := "Accessing web", ""
	switch {
	case arrayLength(arguments, "search_query") > 0:
		title, detail = choose(finished, "Searched web", "Searching web"), firstNestedString(arguments, "search_query", "q")
	case arrayLength(arguments, "image_query") > 0:
		title, detail = choose(finished, "Searched images", "Searching images"), firstNestedString(arguments, "image_query", "q")
	case arrayLength(arguments, "open") > 0:
		title, detail = choose(finished, "Opened page", "Opening page"), firstNestedString(arguments, "open", "ref_id")
	case arrayLength(arguments, "click") > 0:
		title, detail = choose(finished, "Opened link", "Opening link"), firstNestedString(arguments, "click", "ref_id")
	case arrayLength(arguments, "find") > 0:
		title, detail = choose(finished, "Searched page", "Searching page"), firstNestedString(arguments, "find", "pattern")
	case arrayLength(arguments, "screenshot") > 0:
		title = choose(finished, "Captured PDF page", "Capturing PDF page")
	case arrayLength(arguments, "finance") > 0:
		title, detail = choose(finished, "Checked market data", "Checking market data"), firstNestedString(arguments, "finance", "ticker")
	case arrayLength(arguments, "weather") > 0:
		title, detail = choose(finished, "Checked weather", "Checking weather"), firstNestedString(arguments, "weather", "location")
	case arrayLength(arguments, "sports") > 0:
		title, detail = choose(finished, "Checked sports", "Checking sports"), firstNestedString(arguments, "sports", "league")
	case arrayLength(arguments, "time") > 0:
		title, detail = choose(finished, "Checked time", "Checking time"), firstNestedString(arguments, "time", "utc_offset")
	}
	if finished && !toolSucceeded(event) {
		detail = selectedOutput(event, 3)
	}
	return toolLine(event, finished, title, detail)
}

func toolLine(event agent.Event, finished bool, title, detail string) string {
	return formatToolLine(event, finished, title, detail, false)
}

// toolLineFull preserves every apply_patch line and character. Patch content
// is already bounded by the tool request limits and must remain auditable in
// the TUI instead of being replaced by a misleading truncation marker.
func toolLineFull(event agent.Event, finished bool, title, detail string) string {
	return formatToolLine(event, finished, title, detail, true)
}

func formatToolLine(event agent.Event, finished bool, title, detail string, fullDetail bool) string {
	succeeded := toolSucceeded(event)
	marker := detailStyle.Render("•")
	if finished && succeeded {
		marker = successStyle.Bold(true).Render("•")
	} else if finished && !succeeded {
		marker = deleteStyle.Bold(true).Render("•")
		if event.Result != nil && strings.Contains(strings.ToLower(event.Result.Content), "denied") && !strings.HasPrefix(title, "Skipped ") {
			title = "Skipped " + strings.TrimPrefix(strings.TrimPrefix(title, "Ran "), "Running ")
		}
	}
	result := marker + " " + titleStyle.Render(title)
	if detail != "" {
		lines := strings.Split(strings.TrimSpace(detail), "\n")
		for index, line := range lines {
			if !fullDetail {
				line = preview(line, 220)
			}
			if index == 0 {
				result += "\n" + detailStyle.Render("  └ "+line)
			} else {
				result += "\n" + detailStyle.Render("    "+line)
			}
		}
	}
	return result
}

func (m model) renderApproval(width int) string {
	if m.pendingApproval == nil {
		return ""
	}
	request := m.pendingApproval
	title := "Would you like to allow this tool call?"
	switch request.Call.Name {
	case "exec_command", "run_command":
		title = "Would you like to run this command?"
	case "apply_patch":
		title = "Would you like to make this edit?"
	case "web__run":
		title = "Would you like to allow this network request?"
	case "request_permissions":
		title = "Would you like to grant these permissions?"
	}
	invocation := formatToolEvent(agent.Event{Call: &request.Call, Risk: request.Risk}, false)
	choices := approvalChoices(request)
	rows := []string{titleStyle.Render(title), "", invocation}
	if request.Permissions != nil {
		if detail := permissionRequestDetail(request.Permissions); detail != "" {
			rows = append(rows, "", detailStyle.Render(detail))
		}
	}
	rows = append(rows, "")
	for index, choice := range choices {
		prefix := "  "
		style := lipgloss.NewStyle().Foreground(white)
		if index == m.approvalChoice {
			prefix = "› "
			style = lipgloss.NewStyle().Bold(true).Foreground(accentBright)
		}
		rows = append(rows, style.Render(fmt.Sprintf("%s%d. %s", prefix, index+1, choice.label)))
	}
	rows = append(rows, "", detailStyle.Render("↑/↓ move  ·  Enter confirm  ·  y once  ·  a session  ·  Esc deny"))
	return lipgloss.NewStyle().Width(max(10, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1).Render(strings.Join(rows, "\n"))
}

type approvalChoice struct {
	label    string
	decision agent.ApprovalDecision
}

func approvalChoices(request *agent.ApprovalRequest) []approvalChoice {
	if request != nil && request.Permissions != nil {
		return []approvalChoice{
			{label: "Allow for this turn (y)", decision: agent.ApprovalAllowOnce},
			{label: "Allow for this session (a)", decision: agent.ApprovalAllowSession},
			{label: "Deny and continue (n)", decision: agent.ApprovalDeny},
		}
	}
	choices := []approvalChoice{
		{label: "Yes, just this once (y)", decision: agent.ApprovalAllowOnce},
		{label: "Yes, allow this tool for the session (a)", decision: agent.ApprovalAllowSession},
	}
	if request != nil && request.Prefix != "" {
		choices = append(choices, approvalChoice{
			label:    fmt.Sprintf("Yes, allow %q for this session (p)", request.Prefix),
			decision: agent.ApprovalAllowPrefix,
		})
		if request.PolicyPath != "" {
			choices = append(choices, approvalChoice{
				label:    fmt.Sprintf("Always allow %q in %s (r)", request.Prefix, request.PolicyPath),
				decision: agent.ApprovalAllowPersistentPrefix,
			})
		}
	}
	return append(choices, approvalChoice{label: "No, continue without it (n)", decision: agent.ApprovalDeny})
}

func permissionRequestDetail(request *permission.Request) string {
	if request == nil {
		return ""
	}
	var rows []string
	if values := request.Permissions.FileSystem.Read; len(values) > 0 {
		rows = append(rows, "Read: "+strings.Join(values, ", "))
	}
	if values := request.Permissions.FileSystem.Write; len(values) > 0 {
		rows = append(rows, "Write: "+strings.Join(values, ", "))
	}
	if values := request.Permissions.Network.Domains; len(values) > 0 {
		rows = append(rows, "Domains: "+strings.Join(values, ", "))
	}
	if values := request.Permissions.Network.Protocols; len(values) > 0 {
		rows = append(rows, "Protocols: "+strings.Join(values, ", "))
	}
	return strings.Join(rows, "\n")
}

func argumentMap(arguments string) map[string]any {
	var value map[string]any
	if json.Unmarshal([]byte(arguments), &value) != nil {
		return map[string]any{}
	}
	return value
}

func stringValue(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return strings.TrimSpace(result)
}
func rawStringValue(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}
func boolValue(value map[string]any, key string) bool {
	result, _ := value[key].(bool)
	return result
}
func numberValue(value map[string]any, key string) string {
	switch result := value[key].(type) {
	case float64:
		return strconv.FormatInt(int64(result), 10)
	case json.Number:
		return result.String()
	default:
		return "?"
	}
}
func arrayLength(value map[string]any, key string) int {
	items, _ := value[key].([]any)
	return len(items)
}
func firstNestedString(value map[string]any, arrayKey, itemKey string) string {
	items, _ := value[arrayKey].([]any)
	if len(items) == 0 {
		return ""
	}
	item, _ := items[0].(map[string]any)
	return stringValue(item, itemKey)
}
func lineRange(value map[string]any) string {
	start, end := numberValue(value, "start_line"), numberValue(value, "end_line")
	if start == "?" && end == "?" {
		return ""
	}
	if start == "?" {
		start = "1"
	}
	if end == "?" {
		return ":" + start
	}
	return ":" + start + "-" + end
}
func lineCount(value string) int { return len(textLines(value)) }
func textLines(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(value, "\n"), "\n")
}
func toolSucceeded(event agent.Event) bool {
	return event.Result == nil || !event.Result.IsError
}
func choose(condition bool, yes, no string) string {
	if condition {
		return yes
	}
	return no
}
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func commandResultOutput(event agent.Event) string {
	if event.Result == nil {
		return ""
	}
	if result, ok := processToolResult(event); ok {
		return boundedLines(result.Output, 6)
	}
	return boundedLines(event.Result.Content, 6)
}
func selectedOutput(event agent.Event, lines int) string {
	if event.Result == nil {
		return ""
	}
	return boundedLines(event.Result.Content, lines)
}
func boundedLines(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if strings.Contains(strings.ToLower(value), "tool call denied") {
		return "Denied by approval policy."
	}
	if value == "" || value == "No changes." {
		return value
	}
	lines := strings.Split(value, "\n")
	if len(lines) > maximum {
		lines = append(lines[:maximum], "… output truncated")
	}
	for index := range lines {
		lines[index] = preview(lines[index], 220)
	}
	return strings.Join(lines, "\n")
}
func compactArguments(value map[string]any) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, min(4, len(keys)))
	for _, key := range keys {
		if len(parts) == 4 {
			parts = append(parts, "…")
			break
		}
		switch item := value[key].(type) {
		case string:
			parts = append(parts, key+"="+strconv.Quote(preview(item, 80)))
		case float64, bool:
			parts = append(parts, fmt.Sprintf("%s=%v", key, item))
		case []any:
			parts = append(parts, fmt.Sprintf("%s=[%d items]", key, len(item)))
		default:
			parts = append(parts, key+"={…}")
		}
	}
	return strings.Join(parts, "  ")
}

func agentListOutput(event agent.Event) string {
	if event.Result == nil || event.Result.IsError {
		return selectedOutput(event, 3)
	}
	var values []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(event.Result.Content), &values) != nil {
		return ""
	}
	lines := make([]string, 0, min(6, len(values)))
	for index, value := range values {
		if index == 6 {
			lines = append(lines, "… more sub-agents")
			break
		}
		lines = append(lines, value.Name+" ["+value.Status+"]")
	}
	return strings.Join(lines, "\n")
}

func agentWaitOutput(event agent.Event) string {
	if event.Result == nil || event.Result.IsError {
		return selectedOutput(event, 3)
	}
	var value struct {
		Status string `json:"status"`
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if json.Unmarshal([]byte(event.Result.Content), &value) != nil {
		return ""
	}
	detail := value.Status
	if value.Error != "" {
		detail += " — " + value.Error
	}
	if value.Output != "" {
		detail += "\n" + boundedLines(value.Output, 4)
	}
	return detail
}
