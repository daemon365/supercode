package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/taskstate"
	"github.com/daemon365/supercode/internal/tool"
)

type fakeProvider struct {
	requests []provider.Request
	streams  []provider.Stream
}

func (*fakeProvider) Generate(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, errors.New("unexpected Generate call")
}

func (fake *fakeProvider) Stream(_ context.Context, request provider.Request) (provider.Stream, error) {
	fake.requests = append(fake.requests, request)
	stream := fake.streams[0]
	fake.streams = fake.streams[1:]
	return stream, nil
}

type fakeStream struct {
	events []provider.StreamEvent
	index  int
}

func (stream *fakeStream) Next() bool {
	if stream.index == len(stream.events) {
		return false
	}
	stream.index++
	return true
}

func (stream *fakeStream) Current() provider.StreamEvent { return stream.events[stream.index-1] }
func (*fakeStream) Err() error                           { return nil }
func (*fakeStream) Close() error                         { return nil }

type slowReadTool struct{ delay time.Duration }

func (*slowReadTool) Definition() provider.ToolDefinition {
	return provider.ToolDefinition{Name: "slow_read", Parameters: []byte(`{"type":"object"}`)}
}
func (*slowReadTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*slowReadTool) Summary(string) string { return "slow workspace read" }
func (t *slowReadTool) Execute(context.Context, string) (tool.Result, error) {
	time.Sleep(t.delay)
	return tool.Result{Content: "done"}, nil
}

func TestInputIsFocusedAtStartupAndAcceptsText(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	if !m.input.Focused() {
		t.Fatal("input is not focused at startup")
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: '你', Text: "你"})
	m = updated.(model)
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = updated.(model)
	if got := m.input.Value(); got != "你a" {
		t.Fatalf("input value = %q, want %q", got, "你a")
	}
}

func TestComposerAcceptsMultilineCJKPasteWithoutSubmitting(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	updated, _ := m.Update(tea.PasteMsg{Content: "first line\n你好，世界\n```go\nfmt.Println(1)\n```"})
	m = updated.(model)
	if got := m.input.Value(); got != "first line\n你好，世界\n```go\nfmt.Println(1)\n```" {
		t.Fatalf("composer value = %q", got)
	}
	if m.busy || len(m.messages) != 0 {
		t.Fatalf("paste unexpectedly submitted: busy=%t messages=%#v", m.busy, m.messages)
	}
}

func TestLargePasteIsCollapsedInsideComposerAndExpandsAfterSubmit(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	pasted := strings.Repeat("你", 1234)
	updated, _ := m.Update(tea.PasteMsg{Content: pasted})
	m = updated.(model)
	if m.input.Value() != "" || len(m.draftPastes) != 1 || m.draftPastes[0] != pasted {
		t.Fatalf("large paste was not collapsed: input=%q pastes=%d", m.input.Value(), len(m.draftPastes))
	}
	if panel := m.renderAttachments(80); panel != "" {
		t.Fatalf("pasted context leaked outside composer: %q", panel)
	}
	composer := m.renderComposer(80)
	if !strings.Contains(composer, "Pasted context 1 · 1,234 chars") || !strings.Contains(composer, "Backspace/Delete remove") {
		t.Fatalf("composer = %q", composer)
	}
	effective, display := m.promptWithPastes("Please review this")
	if effective != display || !strings.Contains(display, pasted) || strings.Contains(display, "Pasted context") {
		t.Fatalf("composed prompt lost pasted content: effective=%d chars display=%q", len([]rune(effective)), display)
	}
}

func TestCollapsedPasteCanBeRemovedByKeyOrNumber(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	m.draftPastes = []string{"first", "second"}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
	m = updated.(model)
	if len(m.draftPastes) != 1 || m.draftPastes[0] != "first" {
		t.Fatalf("Delete removed the wrong paste: %#v", m.draftPastes)
	}

	m.draftPastes = append(m.draftPastes, "second", "third")
	updated, _ = m.command("/detach paste-2")
	m = updated.(model)
	if got := strings.Join(m.draftPastes, ","); got != "first,third" {
		t.Fatalf("numbered detach result = %q", got)
	}
}

func TestSlashFuzzySearchFindsHelp(t *testing.T) {
	matches := matchingSlashCommands("/hlp")
	if len(matches) == 0 || matches[0].name != "/help" {
		t.Fatalf("matches = %#v", matches)
	}
}

func TestSlashCommandMenuCompletesAndRunsHelp(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	m.resize(90, 30)
	for _, key := range []tea.KeyPressMsg{{Code: '/', Text: "/"}, {Code: 'h', Text: "h"}} {
		updated, _ := m.Update(key)
		m = updated.(model)
	}
	if !m.commandMenuVisible() {
		t.Fatal("slash command menu is not visible")
	}
	menu := m.renderCommandMenu(90)
	if !strings.Contains(menu, "/help") || !strings.Contains(menu, "Show command help") {
		t.Fatalf("command menu = %q", menu)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	if m.input.Value() != "/help" {
		t.Fatalf("completed input = %q, want /help", m.input.Value())
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	if !m.showHelp || !m.viewport.AtTop() {
		t.Fatalf("help mode = %t, at top = %t", m.showHelp, m.viewport.AtTop())
	}
	content := helpMarkdown()
	for _, expected := range []string{"SuperCode commands", "Session", "Code & context", "Runtime", "Output & memory"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("help does not contain %q: %q", expected, content)
		}
	}
}

func TestViewAlwaysLeavesMouseSelectionToTerminal(t *testing.T) {
	inline := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	if view := inline.View(); view.MouseMode != tea.MouseModeNone {
		t.Fatalf("inline mouse mode = %v, want terminal selection enabled", view.MouseMode)
	}
	fullScreen := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second, AlternateScreen: true})
	if view := fullScreen.View(); view.MouseMode != tea.MouseModeNone {
		t.Fatalf("full-screen mouse mode = %v, want terminal selection enabled", view.MouseMode)
	}
}

func TestAlternateScrollKeysMoveViewportWhenComposerIsEmpty(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second, AlternateScreen: true})
	m.resize(80, 12)
	m.viewport.SetContent(strings.Repeat("line\n", 100))
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset()
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(model)
	if m.viewport.YOffset() >= bottom {
		t.Fatalf("wheel-compatible up key did not scroll viewport: before=%d after=%d", bottom, m.viewport.YOffset())
	}
	m.input.SetValue("draft")
	before := m.viewport.YOffset()
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(model)
	if m.viewport.YOffset() != before {
		t.Fatalf("up key scrolled viewport while editing a draft: before=%d after=%d", before, m.viewport.YOffset())
	}
}

func TestAlternateScreenFollowsOption(t *testing.T) {
	inline := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	if inline.View().AltScreen {
		t.Fatal("alternate screen option false was ignored")
	}
	fullScreen := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second, AlternateScreen: true})
	if !fullScreen.View().AltScreen {
		t.Fatal("alternate screen option was ignored")
	}
}

func TestCollaborationModeCommandPersistsAllModes(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	for _, value := range []struct {
		command string
		mode    string
		plan    bool
	}{
		{command: "/mode plan", mode: "plan", plan: true},
		{command: "/mode execute", mode: "execute"},
		{command: "/mode pair", mode: "pair"},
		{command: "/mode default", mode: "default"},
	} {
		updated, _ := m.command(value.command)
		m = updated.(model)
		if string(m.collaborationMode) != value.mode || m.session.Mode != value.mode || m.planMode != value.plan {
			t.Fatalf("%s => collaboration=%q session=%q plan=%t", value.command, m.collaborationMode, m.session.Mode, m.planMode)
		}
	}
}

func TestCommandMenuEscapePreservesDraftAndTypingReopensIt(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	for _, key := range []tea.KeyPressMsg{{Code: '/', Text: "/"}, {Code: 'h', Text: "h"}} {
		updated, _ := m.Update(key)
		m = updated.(model)
	}
	if !m.commandMenuVisible() {
		t.Fatal("command menu should be visible")
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(model)
	if m.input.Value() != "/h" || m.commandMenuVisible() {
		t.Fatalf("escape changed input or left menu open: input=%q visible=%t", m.input.Value(), m.commandMenuVisible())
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	m = updated.(model)
	if m.input.Value() != "/he" || !m.commandMenuVisible() {
		t.Fatalf("typing did not reopen menu: input=%q visible=%t", m.input.Value(), m.commandMenuVisible())
	}
}

func TestEscapeStopsActiveTurnAndRefocusesComposer(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	requestContext, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.cancelCurrentRequest = cancel
	m.agentEvents = make(chan agent.Event)
	m.messages = append(m.messages, message{role: "assistant", content: "partial", streaming: true})

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(model)
	if requestContext.Err() != context.Canceled {
		t.Fatal("escape did not cancel the active request")
	}
	if m.busy || m.agentEvents != nil || m.cancelCurrentRequest != nil {
		t.Fatalf("active turn state was not cleared: busy=%t events=%v cancel=%v", m.busy, m.agentEvents, m.cancelCurrentRequest)
	}
	if !m.input.Focused() || m.input.Placeholder != "Ask anything or type /help…" {
		t.Fatalf("composer was not restored: focused=%t placeholder=%q", m.input.Focused(), m.input.Placeholder)
	}
	if len(m.messages) == 0 || m.messages[0].streaming {
		t.Fatal("partial assistant output was not finalized")
	}
}

func TestUnknownCommandSuggestsClosestPrefix(t *testing.T) {
	if got := similarSlashCommand("/stats"); got != "/status" {
		t.Fatalf("suggestion = %q, want /status", got)
	}
}

func TestCopyOutputSupportsToolAndAll(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	m.messages = []message{
		{role: "assistant", content: "answer"},
		{role: "tool", content: "styled", copyContent: "raw tool output"},
	}
	if got := m.copyOutput("tool"); got != "raw tool output" {
		t.Fatalf("tool copy = %q", got)
	}
	if got := m.copyOutput("all"); got != "answer\n\nraw tool output" {
		t.Fatalf("all copy = %q", got)
	}
}

func TestRawTranscriptIncludesUserAssistantToolAndError(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	m.messages = []message{
		{role: "user", content: "hello\nworld"},
		{role: "assistant", content: "answer"},
		{role: "tool", content: "styled", copyContent: "raw output"},
		{role: "error", content: "failed"},
	}
	got := m.rawTranscript()
	for _, expected := range []string{"> hello\n> world", "answer", "[tool]\nraw output", "Error: failed"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("transcript = %q, want %q", got, expected)
		}
	}
}

func TestConversationUsesUserBubbleAndMarkdownWithoutRoleLabels(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	m.resize(80, 24)
	m.messages = []message{
		{role: "user", content: "hello"},
		{role: "assistant", content: "# Result\n\n- one\n\n`code`"},
	}
	m.refreshMessages(true)
	content := m.viewport.GetContent()
	if !strings.Contains(content, "> hello") {
		t.Fatalf("content = %q, want user bubble", content)
	}
	if strings.Contains(content, "YOU") || strings.Contains(content, "SUPERCODE") {
		t.Fatalf("content contains role labels: %q", content)
	}
	if strings.Contains(content, "# Result") || !strings.Contains(content, "Result") || !strings.Contains(content, "•") {
		t.Fatalf("markdown was not rendered: %q", content)
	}
}

func TestStreamingAssistantRendersMarkdownImmediately(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	m.resize(80, 24)
	m.messages = []message{{role: "assistant", content: "# Live\n\n- first", streaming: true}}
	m.refreshMessages(true)
	content := m.viewport.GetContent()
	if strings.Contains(content, "# Live") || !strings.Contains(content, "Live") || !strings.Contains(content, "•") {
		t.Fatalf("streaming Markdown was not rendered: %q", content)
	}
}

func TestComposerKeysDoNotLeakIntoViewportAndRepeatedPageUpStaysInTUI(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	m.resize(80, 12)
	m.viewport.SetContent(strings.Repeat("line\n", 80))
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset()

	updated, command := m.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	m = updated.(model)
	if m.input.Value() != "b" || m.viewport.YOffset() != bottom {
		t.Fatalf("ordinary input leaked to viewport: input=%q offset=%d/%d", m.input.Value(), m.viewport.YOffset(), bottom)
	}

	updated, command = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = updated.(model)
	first := m.viewport.YOffset()
	if command != nil || first >= bottom {
		t.Fatalf("first PgUp did not remain in viewport: command=%v offset=%d/%d", command, first, bottom)
	}
	updated, command = m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = updated.(model)
	if command != nil || m.viewport.YOffset() > first || m.viewport.YOffset() < 0 {
		t.Fatalf("second PgUp escaped viewport: command=%v offset=%d/%d", command, m.viewport.YOffset(), first)
	}
}

func TestPlanPanelAndQueuedMessagePreview(t *testing.T) {
	state := taskstate.New(taskstate.Plan{Explanation: "Implementing", Steps: []taskstate.Step{{Step: "Inspect", Status: taskstate.StatusCompleted}, {Step: "Build", Status: taskstate.StatusInProgress}, {Step: "Test", Status: taskstate.StatusPending}}}, nil)
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second, TaskState: state})
	m.resize(80, 30)
	m.queuedMessages = []string{"also check docs"}
	plan := m.renderPlan(80)
	if !strings.Contains(plan, "PLAN") || !strings.Contains(plan, "✓ Inspect") || !strings.Contains(plan, "● Build") {
		t.Fatalf("plan = %q", plan)
	}
	queued := m.renderQueued(80)
	if !strings.Contains(queued, "Messages to be submitted after next tool call") || !strings.Contains(queued, "also check docs") {
		t.Fatalf("queued = %q", queued)
	}
}

func TestToolCallRenderingShowsNameAndArguments(t *testing.T) {
	call := provider.ToolCall{Name: "read_file", Arguments: `{"path":"README.md","start_line":1}`}
	rendered := formatToolEvent(agent.Event{Call: &call}, false)
	if !strings.Contains(rendered, "Reading README.md:1") || strings.Contains(rendered, "start_line") || strings.Contains(rendered, "{") {
		t.Fatalf("tool call = %q", rendered)
	}
}

func TestSearchToolRenderingShowsSearchScope(t *testing.T) {
	call := provider.ToolCall{Name: "search_text", Arguments: `{"query":"func \\w+","path":"internal/tui","glob":"*.go","regex":true}`}
	rendered := formatToolEvent(agent.Event{Call: &call}, false)
	if !strings.Contains(rendered, `Searching for "func \\w+" in internal/tui files matching *.go`) {
		t.Fatalf("search rendering = %q", rendered)
	}
}

func TestToolRenderersUseSpecializedPlanCommandAndPatchViews(t *testing.T) {
	plan := provider.ToolCall{Name: "update_plan", Arguments: `{"plan":[{"step":"Inspect","status":"completed"},{"step":"Test","status":"in_progress"}]}`}
	if rendered := formatToolEvent(agent.Event{Call: &plan, Result: &tool.Result{Content: `{}`}}, true); !strings.Contains(rendered, "Updated plan") || strings.Contains(rendered, `"status"`) {
		t.Fatalf("plan rendering = %q", rendered)
	}
	command := provider.ToolCall{Name: "exec_command", Arguments: `{"cmd":"go test ./..."}`}
	if rendered := formatToolEvent(agent.Event{Call: &command, Result: &tool.Result{Content: `{"output":"ok package","running":false}`}}, true); !strings.Contains(rendered, "Ran go test ./...") || !strings.Contains(rendered, "ok package") || strings.Contains(rendered, `"output"`) {
		t.Fatalf("command rendering = %q", rendered)
	}
	if rendered := formatToolEvent(agent.Event{Call: &command, Result: &tool.Result{Content: `{"session_id":1001,"output":"still working","running":true}`}}, true); !strings.Contains(rendered, "Started go test ./...") || !strings.Contains(rendered, "process #1001") || !strings.Contains(rendered, "still working") {
		t.Fatalf("background command rendering = %q", rendered)
	}
	patch := provider.ToolCall{Name: "apply_patch", Arguments: `{"path":"main.go","old_text":"old\n","new_text":"new\n"}`}
	if rendered := formatToolEvent(agent.Event{Call: &patch, Result: &tool.Result{Content: "Updated main.go."}}, true); !strings.Contains(rendered, "Edited main.go (+1 -1)") || !strings.Contains(rendered, "-old") || !strings.Contains(rendered, "+new") {
		t.Fatalf("patch rendering = %q", rendered)
	}
	move := provider.ToolCall{Name: "apply_patch", Arguments: `{"path":"old.go","move_to":"new.go"}`}
	if rendered := formatToolEvent(agent.Event{Call: &move, Result: &tool.Result{Content: "Updated new.go."}}, true); !strings.Contains(rendered, "Moved old.go → new.go") {
		t.Fatalf("move rendering = %q", rendered)
	}
}

func TestPatchRendererNeverTruncatesContent(t *testing.T) {
	newLines := make([]string, 0, 45)
	for line := 1; line <= 44; line++ {
		newLines = append(newLines, fmt.Sprintf("line-%02d", line))
	}
	newLines = append(newLines, strings.Repeat("x", 240)+"-LONG-LINE-END")
	arguments, _ := json.Marshal(map[string]any{"path": "long.go", "new_text": strings.Join(newLines, "\n") + "\n"})
	rendered := formatToolEvent(agent.Event{Call: &provider.ToolCall{Name: "apply_patch", Arguments: string(arguments)}, Result: &tool.Result{Content: "Updated long.go."}}, true)
	if strings.Contains(rendered, "truncated") || !strings.Contains(rendered, "+line-44") || !strings.Contains(rendered, "LONG-LINE-END") {
		t.Fatalf("single-file patch was truncated: %q", rendered)
	}

	diff := "--- a/long.go\n+++ b/long.go\n@@ -1 +1,2 @@\n-old\n+" + strings.Repeat("y", 600) + "-UNIFIED-END\n"
	arguments, _ = json.Marshal(map[string]any{"unified_diff": diff})
	rendered = formatToolEvent(agent.Event{Call: &provider.ToolCall{Name: "apply_patch", Arguments: string(arguments)}, Result: &tool.Result{Content: "Applied."}}, true)
	if strings.Contains(rendered, "truncated") || !strings.Contains(rendered, "UNIFIED-END") {
		t.Fatalf("unified patch was truncated: %q", rendered)
	}

	operations := make([]map[string]any, 0, 7)
	for index := 1; index <= 7; index++ {
		operations = append(operations, map[string]any{"path": fmt.Sprintf("file-%d.go", index), "new_text": fmt.Sprintf("content-%d\n", index)})
	}
	arguments, _ = json.Marshal(map[string]any{"operations": operations})
	rendered = formatToolEvent(agent.Event{Call: &provider.ToolCall{Name: "apply_patch", Arguments: string(arguments)}, Result: &tool.Result{Content: "Updated files."}}, true)
	if strings.Contains(rendered, "more files") || strings.Contains(rendered, "truncated") || !strings.Contains(rendered, "file-7.go") || !strings.Contains(rendered, "+content-7") {
		t.Fatalf("multi-file patch was truncated: %q", rendered)
	}
}

func TestEveryBuiltInToolHasACompactSpecializedRendering(t *testing.T) {
	tests := []struct {
		name, arguments, want string
	}{
		{"list_files", `{"path":"internal"}`, "Listing internal"},
		{"search_text", `{"query":"needle"}`, `Searching for "needle"`},
		{"read_file", `{"path":"README.md"}`, "Reading README.md"},
		{"apply_patch", `{"path":"a.go","old_text":"a","new_text":"b"}`, "Editing a.go"},
		{"run_command", `{"command":"go test ./..."}`, "Running go test ./..."},
		{"exec_command", `{"cmd":"go test ./..."}`, "Running go test ./..."},
		{"write_stdin", `{"session_id":1001,"chars":"y\\n"}`, "Writing to process #1001"},
		{"wait", `{"session_id":1001}`, "Waiting for process #1001"},
		{"list_processes", `{}`, "Listing background processes"},
		{"stop_process", `{"session_id":1001}`, "Stopping process 1001"},
		{"tool_search", `{"query":"database"}`, "Searching tools"},
		{"mcp__demo__query", `{"sql":"select 1"}`, "Calling demo · query"},
		{"git_status", `{}`, "Checking Git status"},
		{"git_diff", `{"staged":true}`, "Inspecting staged changes"},
		{"view_image", `{"path":"shot.png"}`, "Viewing shot.png"},
		{"web__run", `{"weather":[{"location":"Shanghai"}]}`, "Checking weather"},
		{"update_plan", `{"plan":[]}`, "Updating plan"},
		{"create_goal", `{"objective":"Ship"}`, "Creating goal"},
		{"get_goal", `{}`, "Checking goal"},
		{"update_goal", `{"status":"complete"}`, "Marking goal complete"},
		{"spawn_agent", `{"task_name":"tests","message":"Run tests"}`, "Spawning tests"},
		{"send_message", `{"target":"tests","message":"Check race"}`, "Messaging tests"},
		{"followup_task", `{"target":"tests","message":"Check docs"}`, "Following up with tests"},
		{"interrupt_agent", `{"target":"tests"}`, "Interrupting tests"},
		{"list_agents", `{}`, "Listing sub-agents"},
		{"wait_agent", `{"target":"tests"}`, "Waiting for tests"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := provider.ToolCall{Name: test.name, Arguments: test.arguments}
			rendered := formatToolEvent(agent.Event{Call: &call}, false)
			if !strings.Contains(rendered, test.want) {
				t.Fatalf("rendered = %q, want %q", rendered, test.want)
			}
			if strings.Contains(rendered, `{"`) || strings.Contains(rendered, `\n`) {
				t.Fatalf("raw JSON leaked into rendering: %q", rendered)
			}
		})
	}
}

func TestEveryDocumentedSlashCommandHasARoute(t *testing.T) {
	for _, command := range slashCommands {
		if route := routeSlashCommand(command.name); route == routeUnknown {
			t.Errorf("documented command %s has no handler route", command.name)
		}
	}
}

func TestApprovalUsesSelectionWithoutChangingComposer(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.resize(80, 30)
	m.input.SetValue("preserve this draft")
	requestCall := provider.ToolCall{ID: "call-1", Name: "exec_command", Arguments: `{"cmd":"go test ./..."}`}
	m.pendingApproval = &agent.ApprovalRequest{Call: requestCall, Risk: tool.RiskExecute}
	m.input.Blur()
	if panel := m.renderApproval(80); !strings.Contains(panel, "Would you like to run this command?") || !strings.Contains(panel, "Yes, just this once") {
		t.Fatalf("approval panel = %q", panel)
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(model)
	if m.approvalChoice != 1 || m.input.Value() != "preserve this draft" {
		t.Fatalf("choice=%d input=%q", m.approvalChoice, m.input.Value())
	}
}

func TestStreamingConversationUsesMessageHistory(t *testing.T) {
	firstResponse := provider.Response{ID: "resp_first", Text: "first"}
	fake := &fakeProvider{streams: []provider.Stream{
		&fakeStream{events: []provider.StreamEvent{
			{Type: provider.StreamEventTextDelta, Delta: "first"},
			{Type: provider.StreamEventCompleted, Response: &firstResponse},
		}},
		&fakeStream{},
	}}
	m := newModel(context.Background(), fake, Options{Model: "gpt-test", Stream: true, Timeout: time.Second})
	m.resize(80, 24)

	updated, command := m.submit("hello")
	m = updated.(model)
	drainRequest(t, &m, command)
	if len(m.history) != 2 {
		t.Fatalf("history length = %d, want 2", len(m.history))
	}
	if !strings.Contains(m.viewport.GetContent(), "first") {
		t.Fatalf("viewport content = %q, want streamed response", m.viewport.GetContent())
	}

	updated, command = m.submit("continue")
	m = updated.(model)
	drainRequest(t, &m, command)
	if len(fake.requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(fake.requests))
	}
	if len(fake.requests[1].History) != 2 {
		t.Fatalf("second request history = %+v, want two messages", fake.requests[1].History)
	}
	if fake.requests[1].History[0].Role != provider.MessageRoleUser || fake.requests[1].History[0].Content != "hello" {
		t.Fatalf("first history message = %+v", fake.requests[1].History[0])
	}
	if fake.requests[1].History[1].Role != provider.MessageRoleAssistant || fake.requests[1].History[1].Content != "first" {
		t.Fatalf("second history message = %+v", fake.requests[1].History[1])
	}
}

func TestRequestTimeoutDoesNotCancelWholeAgentTurn(t *testing.T) {
	first := provider.Response{ToolCalls: []provider.ToolCall{{ID: "slow", Name: "slow_read", Arguments: `{}`}}}
	second := provider.Response{Text: "finished after tool"}
	fake := &fakeProvider{streams: []provider.Stream{
		&fakeStream{events: []provider.StreamEvent{{Type: provider.StreamEventCompleted, Response: &first}}},
		&fakeStream{events: []provider.StreamEvent{{Type: provider.StreamEventTextDelta, Delta: second.Text}, {Type: provider.StreamEventCompleted, Response: &second}}},
	}}
	registry, _ := tool.NewRegistry(&slowReadTool{delay: 30 * time.Millisecond})
	m := newModel(context.Background(), fake, Options{Model: "test", Stream: true, Timeout: 10 * time.Millisecond, Tools: registry})
	updated, command := m.submit("run a long turn")
	m = updated.(model)
	drainRequest(t, &m, command)
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].content != "finished after tool" {
		t.Fatalf("turn was canceled by the tool duration: messages = %#v", m.messages)
	}
}

func drainRequest(t *testing.T, m *model, command tea.Cmd) {
	t.Helper()
	commands := []tea.Cmd{command}
	for i := 0; i < 32 && len(commands) > 0; i++ {
		current := commands[0]
		commands = commands[1:]
		msg := current()
		if batch, ok := msg.(tea.BatchMsg); ok {
			commands = append(commands, batch...)
			continue
		}
		updated, next := m.Update(msg)
		*m = updated.(model)
		if !m.busy {
			return
		}
		if next != nil {
			commands = append(commands, next)
		}
	}
	t.Fatal("request did not finish")
}
