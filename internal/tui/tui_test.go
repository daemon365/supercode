package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/collaboration"
	"github.com/daemon365/supercode/internal/memory"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
	"github.com/daemon365/supercode/internal/taskstate"
	"github.com/daemon365/supercode/internal/tool"
)

type fakeProvider struct {
	requests         []provider.Request
	streams          []provider.Stream
	generateRequests []provider.Request
	generated        []provider.Response
}

func (fake *fakeProvider) Generate(_ context.Context, request provider.Request) (provider.Response, error) {
	if len(fake.generated) == 0 {
		return provider.Response{}, errors.New("unexpected Generate call")
	}
	fake.generateRequests = append(fake.generateRequests, request)
	response := fake.generated[0]
	fake.generated = fake.generated[1:]
	return response, nil
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

func TestStartupWarningsRemainVisibleInAlternateScreen(t *testing.T) {
	m := newModel(context.Background(), nil, Options{
		Model: "gpt-test", Timeout: time.Second, AlternateScreen: true,
		StartupWarnings: []string{"MCP server unavailable"},
	})
	if len(m.messages) != 1 || !strings.Contains(m.messages[0].content, "MCP server unavailable") {
		t.Fatalf("startup warning was not added to the transcript: %#v", m.messages)
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

func TestEscapeJoinsActiveTurnBeforeRefocusingComposer(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "gpt-test", Timeout: time.Second})
	requestContext, cancel := context.WithCancel(context.Background())
	events := make(chan agent.Event)
	m.busy = true
	m.cancelCurrentRequest = cancel
	m.agentEvents = events
	m.activePrompt = "question"
	m.turnMessageStart = 0
	m.messages = append(m.messages, message{role: "assistant", content: "partial", streaming: true})

	updated, stopCommand := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(model)
	if requestContext.Err() != context.Canceled {
		t.Fatal("escape did not cancel the active request")
	}
	if !m.busy || !m.cancelling || m.agentEvents != events || m.cancelCurrentRequest != nil {
		t.Fatalf("active turn was not kept behind the join barrier: busy=%t cancelling=%t events=%v cancel=%v", m.busy, m.cancelling, m.agentEvents, m.cancelCurrentRequest)
	}
	if m.input.Placeholder != "Stopping the active turn…" {
		t.Fatalf("composer placeholder = %q", m.input.Placeholder)
	}
	if len(m.messages) == 0 || m.messages[0].streaming {
		t.Fatal("partial assistant output was not finalized")
	}
	if len(m.history) != 2 || m.history[0].Content != "question" || m.history[1].Content != "partial" {
		t.Fatalf("interrupted history = %#v", m.history)
	}

	// The durability job may finish first, but the composer must remain locked
	// until the runner event stream closes.
	if stopCommand == nil {
		t.Fatal("stop did not schedule interrupted-turn persistence")
	}
	messageChannel := make(chan tea.Msg, 1)
	go func() { messageChannel <- stopCommand() }()
	var stopMessage tea.Msg
	select {
	case stopMessage = <-messageChannel:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stop launched a second blocking agent-event reader")
	}
	jobMessage, ok := stopMessage.(sessionJobMsg)
	if !ok {
		t.Fatalf("stop command returned %T, want sessionJobMsg", stopMessage)
	}
	updated, _ = m.Update(jobMessage)
	m = updated.(model)
	if !m.busy || !m.cancelling {
		t.Fatal("session save incorrectly bypassed the runner join barrier")
	}
	before := len(m.messages)
	updated, command := m.submit("must wait")
	m = updated.(model)
	if command != nil || len(m.messages) != before {
		t.Fatal("a new turn started while the cancelled runner was still active")
	}

	updated, _ = m.Update(agentEventMsg{events: events, ok: false})
	m = updated.(model)
	if m.busy || m.cancelling || m.agentEvents != nil {
		t.Fatalf("active turn did not unlock after join: busy=%t cancelling=%t events=%v", m.busy, m.cancelling, m.agentEvents)
	}
	if !m.input.Focused() || m.input.Placeholder != "Ask anything or type /help…" {
		t.Fatalf("composer was not restored: focused=%t placeholder=%q", m.input.Focused(), m.input.Placeholder)
	}
}

func TestFirstControlCStartsVisibleGracefulExitAndSecondForcesQuit(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	updated, command := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(model)
	if command == nil || !m.exiting || !strings.Contains(m.View().Content, "Saving session and memory before exit") {
		t.Fatalf("graceful exit did not become visible: exiting=%t command=%v view=%q", m.exiting, command, m.View().Content)
	}
	updated, command = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(model)
	if command == nil {
		t.Fatal("second Ctrl+C did not force quit")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatalf("second Ctrl+C command returned %T, want tea.QuitMsg", command())
	}
}

func TestExternalExitPreservesPartialTurnBeforeShutdown(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace", SessionStore: store,
	})
	events := make(chan agent.Event)
	m.busy = true
	m.agentEvents = events
	m.activePrompt = "keep the interrupted prompt"
	m.turnMessageStart = 0
	m.messages = []message{{role: "user", content: "keep the interrupted prompt"}, {role: "assistant", streaming: true}}
	m.appendAssistantDelta("keep the partial answer")

	updated, command := m.Update(externalExitMsg{})
	m = updated.(model)
	if command == nil || !m.exiting || !m.cancelling || len(m.history) != 2 {
		t.Fatalf("external exit state: command=%v exiting=%t cancelling=%t history=%#v", command, m.exiting, m.cancelling, m.history)
	}
	if len(m.sessionJobs) != 1 || m.sessionJobs[0].action != sessionActionInterrupt {
		t.Fatalf("interrupted save was not queued: %#v", m.sessionJobs)
	}
	result := m.sessionJobs[0].run()
	updated, _ = m.Update(sessionJobMsg{result: result})
	m = updated.(model)
	loaded, err := store.Load(m.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[0].Content != "keep the interrupted prompt" || loaded.Messages[1].Content != "keep the partial answer" {
		t.Fatalf("persisted interrupted history = %#v", loaded.Messages)
	}
}

func TestExternalExitPreservesCommittedQueuedGuidanceInOrder(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	events := make(chan agent.Event)
	m.busy = true
	m.agentEvents = events
	m.activePrompt = "initial prompt"
	m.turnMessageStart = 0
	m.messages = []message{
		{role: "user", content: "initial prompt", historyContent: "initial prompt"},
		{role: "assistant", streaming: true},
	}
	m.appendAssistantDelta("first assistant segment")
	m.queuedMessages = []string{"visible queued guidance"}
	updated, _ := m.handleAgentEvent(agent.Event{Type: agent.EventQueuedCommitted, Queued: []string{"effective queued guidance"}})
	m = updated.(model)
	if m.messages[1].streaming {
		t.Fatal("queued commit did not finish the preceding assistant segment")
	}
	updated, _ = m.handleAgentEvent(agent.Event{Type: agent.EventTextDelta, Delta: "second assistant segment"})
	m = updated.(model)
	updated, _ = m.Update(externalExitMsg{})
	m = updated.(model)

	want := []struct {
		role    provider.MessageRole
		content string
	}{
		{provider.MessageRoleUser, "initial prompt"},
		{provider.MessageRoleAssistant, "first assistant segment"},
		{provider.MessageRoleUser, "effective queued guidance"},
		{provider.MessageRoleAssistant, "second assistant segment"},
	}
	if len(m.history) != len(want) {
		t.Fatalf("interrupted queued history=%#v", m.history)
	}
	for index, expected := range want {
		if m.history[index].Role != expected.role || m.history[index].Content != expected.content {
			t.Fatalf("history[%d]=%#v, want role=%q content=%q", index, m.history[index], expected.role, expected.content)
		}
	}
}

func TestParentContextCancellationBecomesGracefulExitMessage(t *testing.T) {
	externalContext, cancelExternal := context.WithCancel(context.Background())
	lifecycleContext, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	command := waitExternalExit(externalContext, lifecycleContext)
	cancelExternal()
	if _, ok := command().(externalExitMsg); !ok {
		t.Fatalf("parent cancellation returned an unexpected message")
	}
}

func TestRunConvertsCancelledParentContextIntoDurableExit(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, nil, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace",
		SessionStore: store, Session: current,
	}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(current.ID)
	if err != nil || loaded.ID != current.ID {
		t.Fatalf("cancelled parent bypassed graceful session save: session=%#v err=%v", loaded, err)
	}
}

func TestGracefulExitWaitsForPendingAndFinalSessionSave(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	_, err := m.enqueueSessionSave(sessionActionSave, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.beginExit()
	m = updated.(model)
	updated, _ = m.Update(exitVisibleMsg{})
	m = updated.(model)
	if m.exitSessionStarted {
		t.Fatal("final exit save started before the pending session job completed")
	}
	first := m.sessionJobs[0].run()
	updated, _ = m.Update(sessionJobMsg{result: first})
	m = updated.(model)
	if !m.exitSessionStarted || len(m.sessionJobs) != 1 || m.sessionJobs[0].action != sessionActionExit {
		t.Fatalf("final exit save was not queued after pending work: started=%t jobs=%#v", m.exitSessionStarted, m.sessionJobs)
	}
	final := m.sessionJobs[0].run()
	updated, _ = m.Update(sessionJobMsg{result: final})
	m = updated.(model)
	if !m.exitSessionSaved || !m.exitQuitScheduled {
		t.Fatalf("exit did not wait for final save: saved=%t quitScheduled=%t", m.exitSessionSaved, m.exitQuitScheduled)
	}
}

func TestGracefulExitRunsMemoryAfterFinalSessionSave(t *testing.T) {
	directory := t.TempDir()
	sessions, err := session.NewStore(filepath.Join(directory, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	memoryStore, err := memory.NewStore(filepath.Join(directory, "memories"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := memory.DefaultConfig()
	configuration.Generate = true
	memoryStore.ConfigureAdvanced(configuration)
	modelProvider := &fakeProvider{generated: []provider.Response{
		{Text: `{"raw_memory":"Keep focused tests.","rollout_summary":"Focused tests.","rollout_slug":null}`},
		{Text: `{"memory_md":"# Testing\nKeep focused tests.\n","memory_summary_md":"Focused tests.","skills":[]}`},
	}}
	m := newModel(context.Background(), modelProvider, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace",
		SessionStore: sessions, Memory: memoryStore,
	})
	m.history = []provider.Message{
		{Role: provider.MessageRoleUser, Content: "Always run focused tests."},
		{Role: provider.MessageRoleAssistant, Content: "Understood."},
	}
	updated, _ := m.beginExit()
	m = updated.(model)
	updated, _ = m.Update(exitVisibleMsg{})
	m = updated.(model)
	if len(m.sessionJobs) != 1 || m.sessionJobs[0].action != sessionActionExit {
		t.Fatalf("final session save was not queued: %#v", m.sessionJobs)
	}
	result := m.sessionJobs[0].run()
	updated, command := m.Update(sessionJobMsg{result: result})
	m = updated.(model)
	if !m.exitSessionSaved || !m.exitMemoryPending || command == nil {
		t.Fatalf("memory did not start after final save: saved=%t pending=%t command=%v", m.exitSessionSaved, m.exitMemoryPending, command)
	}
	var memoryResult *exitMemoryMsg
	for _, message := range collectCommandMessages(command) {
		if value, ok := message.(exitMemoryMsg); ok {
			copy := value
			memoryResult = &copy
		}
	}
	if memoryResult == nil {
		t.Fatal("exit memory command did not return a result")
	}
	updated, _ = m.Update(*memoryResult)
	m = updated.(model)
	if memoryResult.err != nil || !m.exitQuitScheduled {
		t.Fatalf("memory exit result=%v quitScheduled=%t", memoryResult.err, m.exitQuitScheduled)
	}
	if value, err := memoryStore.Read(); err != nil || !strings.Contains(value, "Keep focused tests") {
		t.Fatalf("saved memory=%q err=%v", value, err)
	}
}

func collectCommandMessages(command tea.Cmd) []tea.Msg {
	if command == nil {
		return nil
	}
	message := command()
	batch, ok := message.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{message}
	}
	var messages []tea.Msg
	for _, child := range batch {
		messages = append(messages, collectCommandMessages(child)...)
	}
	return messages
}

func TestGracefulExitTimeoutShowsErrorBeforeSchedulingQuit(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.exiting = true
	cancelled := false
	m.exitCancel = func() { cancelled = true }
	updated, command := m.Update(exitTimeoutMsg{})
	m = updated.(model)
	if !cancelled || !m.exitQuitScheduled || command == nil {
		t.Fatalf("timeout state: cancelled=%t scheduled=%t command=%v", cancelled, m.exitQuitScheduled, command)
	}
	if len(m.messages) == 0 || !strings.Contains(m.messages[len(m.messages)-1].content, "timed out") {
		t.Fatalf("timeout warning was not made visible: %#v", m.messages)
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

func TestSubmitPreparesTurnAsynchronouslyBeforeStartingRunner(t *testing.T) {
	fake := &fakeProvider{streams: []provider.Stream{&fakeStream{}}}
	m := newModel(context.Background(), fake, Options{Model: "test", Stream: true, Timeout: time.Second})
	updated, command := m.submit("prepare asynchronously")
	m = updated.(model)
	if !m.busy || !m.turnPreparing || m.activeRun != nil || len(fake.requests) != 0 {
		t.Fatalf("submit performed turn work inline: busy=%t preparing=%t run=%v requests=%d", m.busy, m.turnPreparing, m.activeRun, len(fake.requests))
	}
	batch, ok := command().(tea.BatchMsg)
	if !ok {
		t.Fatalf("submit command returned %T, want tea.BatchMsg", command())
	}
	var prepared turnPreparedMsg
	found := false
	for _, child := range batch {
		messageChannel := make(chan tea.Msg, 1)
		go func(command tea.Cmd) { messageChannel <- command() }(child)
		select {
		case message := <-messageChannel:
			if value, ok := message.(turnPreparedMsg); ok {
				prepared, found = value, true
			}
		case <-time.After(time.Second):
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("asynchronous turn preparation result was not scheduled")
	}
	updated, _ = m.Update(prepared)
	m = updated.(model)
	if m.turnPreparing || m.activeRun == nil {
		t.Fatalf("runner did not start after preparation: preparing=%t run=%v", m.turnPreparing, m.activeRun)
	}
}

func TestNextAgentEventCoalescesTextBurstAndPreservesControlEvent(t *testing.T) {
	events := make(chan agent.Event, 4)
	events <- agent.Event{Type: agent.EventTextDelta, Delta: "one"}
	events <- agent.Event{Type: agent.EventTextDelta, Delta: " two"}
	events <- agent.Event{Type: agent.EventTextDelta, Delta: " three"}
	events <- agent.Event{Type: agent.EventToolStarted, Summary: "tool"}

	message := nextAgentEvent(events)().(agentEventMsg)
	if message.event.Delta != "one two three" {
		t.Fatalf("coalesced delta = %q", message.event.Delta)
	}
	if message.trailing == nil || message.trailing.Type != agent.EventToolStarted {
		t.Fatalf("trailing event = %+v", message.trailing)
	}
}

func TestNextAgentEventCoalescesOnlyMatchingToolOutputStream(t *testing.T) {
	firstCall := &provider.ToolCall{ID: "call-1", Name: "exec_command"}
	secondCall := &provider.ToolCall{ID: "call-2", Name: "exec_command"}
	events := make(chan agent.Event, 3)
	events <- agent.Event{Type: agent.EventToolOutputDelta, Call: firstCall, SessionID: 7, Delta: "one"}
	events <- agent.Event{Type: agent.EventToolOutputDelta, Call: firstCall, SessionID: 7, Delta: " two"}
	events <- agent.Event{Type: agent.EventToolOutputDelta, Call: secondCall, SessionID: 8, Delta: "other"}

	message := nextAgentEvent(events)().(agentEventMsg)
	if message.event.Delta != "one two" {
		t.Fatalf("coalesced tool delta = %q", message.event.Delta)
	}
	if message.trailing == nil || message.trailing.Call == nil || message.trailing.Call.ID != "call-2" {
		t.Fatalf("trailing tool event = %+v", message.trailing)
	}
}

func TestTranscriptCachesStablePrefixWhileStreamingTailChanges(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.resize(80, 24)
	for index := 0; index < 40; index++ {
		m.messages = append(m.messages, message{role: "status", content: fmt.Sprintf("stable-%d", index)})
	}
	m.messages = append(m.messages, message{role: "assistant", content: "live", streaming: true})
	m.refreshMessages(true)
	if m.transcriptPrefixSize != 40 || m.transcriptPrefix == "" {
		t.Fatalf("prefix size=%d content=%q", m.transcriptPrefixSize, m.transcriptPrefix)
	}
	prefix := m.transcriptPrefix
	m.appendAssistantDelta(" update")
	m.refreshMessages(true)
	if m.transcriptPrefix != prefix || m.transcriptPrefixSize != 40 {
		t.Fatal("streaming delta rebuilt the stable transcript prefix")
	}
	m.finishStreamingAssistant()
	m.refreshMessages(true)
	if m.transcriptPrefixSize != len(m.messages) {
		t.Fatalf("completed prefix size=%d, want %d", m.transcriptPrefixSize, len(m.messages))
	}
}

func TestStreamingAssistantAndStableTranscriptViewsStayBounded(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.resize(100, 30)
	m.messages = []message{{role: "assistant", streaming: true}}
	for index := 0; index < 48; index++ {
		chunk := strings.Repeat(fmt.Sprintf("chunk-%02d ", index), 4*1024)
		if index == 0 {
			chunk = "OLDEST_STREAM_SENTINEL " + chunk
		}
		m.appendAssistantDelta(chunk)
		m.refreshMessages(true)
		if len(m.messages[0].streamTail) > streamingAssistantBytes {
			t.Fatalf("stream tail grew to %d bytes", len(m.messages[0].streamTail))
		}
		if size := len(m.viewport.GetContent()); size > 256*1024 {
			t.Fatalf("streaming viewport grew to %d bytes", size)
		}
	}
	if strings.Contains(m.viewport.GetContent(), "OLDEST_STREAM_SENTINEL") || !strings.Contains(m.viewport.GetContent(), "older response hidden") {
		t.Fatalf("streaming viewport did not use a bounded recent window: %q", m.viewport.GetContent()[:min(300, len(m.viewport.GetContent()))])
	}
	m.finishStreamingAssistant()
	if !strings.Contains(m.messages[0].content, "OLDEST_STREAM_SENTINEL") {
		t.Fatal("final assistant content lost the hidden streaming prefix")
	}

	m.messages = []message{
		{role: "status", content: "OLDEST_TRANSCRIPT_SENTINEL " + strings.Repeat("a", transcriptViewBytes)},
		{role: "status", content: strings.Repeat("b", transcriptViewBytes)},
		{role: "status", content: "newest"},
	}
	m.resetTranscriptCache()
	m.refreshMessages(true)
	if len(m.transcriptPrefix) > transcriptViewBytes || !m.transcriptPrefixCut {
		t.Fatalf("stable transcript cache bytes=%d cut=%t", len(m.transcriptPrefix), m.transcriptPrefixCut)
	}
	if strings.Contains(m.viewport.GetContent(), "OLDEST_TRANSCRIPT_SENTINEL") || !strings.Contains(m.viewport.GetContent(), "use /raw") {
		t.Fatal("stable viewport was not bounded with an explicit full-transcript hint")
	}
	if !strings.Contains(m.rawTranscript(), "OLDEST_TRANSCRIPT_SENTINEL") {
		t.Fatal("view bounding truncated the copy/raw transcript")
	}
}

func TestCompletedStreamingAssistantRemainsMarkdownRendered(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.resize(80, 24)
	m.messages = []message{{role: "assistant", streaming: true}}
	m.appendAssistantDelta("# Rendered heading\n\nThis is **bold** and `code`.")
	m.refreshMessages(true)
	m.finishStreamingAssistant()
	m.refreshMessages(true)

	if m.rawMode || m.messages[0].streaming || m.messages[0].content == "" {
		t.Fatalf("completion state: raw=%t streaming=%t content=%q", m.rawMode, m.messages[0].streaming, m.messages[0].content)
	}
	view := ansi.Strip(m.viewport.GetContent())
	if !strings.Contains(view, "Rendered heading") || strings.Contains(view, "# Rendered heading") || strings.Contains(view, "**bold**") {
		t.Fatalf("completed assistant reverted to raw Markdown: %q", view)
	}
}

func TestRawModeShowsBoundedStreamingAssistantTail(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.resize(80, 24)
	m.rawMode = true
	m.messages = []message{{role: "assistant", streaming: true}}
	m.appendAssistantDelta("old sentinel " + strings.Repeat("x", streamingAssistantBytes))
	m.appendAssistantDelta(" recent sentinel")
	m.refreshMessages(true)
	view := m.viewport.GetContent()
	if !strings.Contains(view, "recent sentinel") || !strings.Contains(view, "older response hidden") {
		t.Fatalf("raw streaming tail was empty or unbounded: %q", view[:min(300, len(view))])
	}
}

func TestInputHistoryNavigatesAndRestoresDraft(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.rememberInput("first prompt")
	m.rememberInput("second prompt")
	m.input.SetValue("unfinished draft")
	m.input.MoveToEnd()

	for _, step := range []struct {
		key  rune
		want string
	}{{tea.KeyUp, "second prompt"}, {tea.KeyUp, "first prompt"}, {tea.KeyDown, "second prompt"}, {tea.KeyDown, "unfinished draft"}} {
		updated, _ := m.Update(tea.KeyPressMsg{Code: step.key})
		m = updated.(model)
		if got := m.input.Value(); got != step.want {
			t.Fatalf("history %v produced %q, want %q", step.key, got, step.want)
		}
	}
	if m.inputHistoryCursor != -1 {
		t.Fatalf("history cursor=%d after restoring draft", m.inputHistoryCursor)
	}
}

func TestInputHistoryPrioritizesMultilineCursorMovement(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.rememberInput("older prompt")
	m.input.SetValue("first line\nsecond line")
	m.input.MoveToEnd()
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(model)
	if m.inputHistoryCursor != -1 || m.input.Value() != "first line\nsecond line" || m.input.Line() != 0 {
		t.Fatalf("multiline Up recalled history before moving cursor: cursor=%d line=%d value=%q", m.inputHistoryCursor, m.input.Line(), m.input.Value())
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(model)
	if m.input.Value() != "older prompt" {
		t.Fatalf("Up at first line did not recall history: %q", m.input.Value())
	}
}

func TestSubmittedInputHistoryIsDeduplicatedAndBounded(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	for count := 0; count < 2; count++ {
		m.input.SetValue("/help")
		updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(model)
	}
	if len(m.inputHistory) != 1 || m.inputHistory[0] != "/help" {
		t.Fatalf("submitted history = %#v", m.inputHistory)
	}
	for index := 0; index < maximumInputHistoryEntries+20; index++ {
		m.rememberInput(fmt.Sprintf("prompt-%03d", index))
	}
	if len(m.inputHistory) != maximumInputHistoryEntries || m.inputHistory[0] != "prompt-020" {
		t.Fatalf("bounded history len=%d first=%q", len(m.inputHistory), m.inputHistory[0])
	}
}

func TestLoadedSessionSeedsInputHistory(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second})
	m.loadHistory([]provider.Message{
		{Role: provider.MessageRoleUser, Content: "older session prompt"},
		{Role: provider.MessageRoleAssistant, Content: "older answer"},
		{Role: provider.MessageRoleUser, Content: "newer session prompt"},
	})

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(model)
	if got := m.input.Value(); got != "newer session prompt" {
		t.Fatalf("loaded session history recalled %q", got)
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(model)
	if got := m.input.Value(); got != "older session prompt" {
		t.Fatalf("loaded session history recalled %q", got)
	}
}

func TestExecuteToolForStatusDefersSlowWorkToCommand(t *testing.T) {
	registry, err := tool.NewRegistry(&slowReadTool{delay: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, Options{Model: "test", Timeout: time.Second, Tools: registry})
	started := time.Now()
	command := m.executeToolForStatus("slow_read", `{}`)
	if command == nil {
		t.Fatal("slow tool command is nil")
	}
	if elapsed := time.Since(started); elapsed >= 20*time.Millisecond {
		t.Fatalf("tool executed synchronously in Update path: %s", elapsed)
	}
	message := command().(toolStatusMsg)
	if message.err != nil || message.result.Content != "done" {
		t.Fatalf("tool result = %#v err=%v", message.result, message.err)
	}
}

func TestSessionPickerLoadsMetadataThenSelectedSession(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.New("/workspace", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	stored.Title = "Metadata only"
	stored.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "large conversation text"}}
	if err := store.Commit(stored); err != nil {
		t.Fatal(err)
	}

	m := newModel(context.Background(), nil, Options{
		Model: "test-model", Timeout: time.Second, Workspace: "/workspace", SessionStore: store,
	})
	command := m.openSessionPicker(false)
	if !m.sessionPickerLoading || len(m.sessionChoices) != 0 || command == nil {
		t.Fatalf("picker did not begin with async metadata load: loading=%t choices=%d command=%v", m.sessionPickerLoading, len(m.sessionChoices), command)
	}
	updated, _ := m.Update(command())
	m = updated.(model)
	if m.sessionPickerLoading || len(m.sessionChoices) != 1 || m.sessionChoices[0].MessageCount != 1 {
		t.Fatalf("metadata choices = %#v loading=%t", m.sessionChoices, m.sessionPickerLoading)
	}

	updated, command = m.updateSessionPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	if command == nil || !m.sessionPickerLoading {
		t.Fatal("selected session was not loaded asynchronously")
	}
	updated, _ = m.Update(command())
	m = updated.(model)
	if !m.sessionPickerActivating || len(m.sessionJobs) != 1 || m.sessionJobs[0].action != sessionActionActivate {
		t.Fatalf("selected session did not begin asynchronous agent activation: activating=%t jobs=%#v", m.sessionPickerActivating, m.sessionJobs)
	}
	activation := m.sessionJobs[0].run()
	updated, _ = m.Update(sessionJobMsg{result: activation})
	m = updated.(model)
	if m.showSessionPicker || m.session.ID != stored.ID || len(m.history) != 1 {
		t.Fatalf("resumed session=%q history=%d picker=%t", m.session.ID, len(m.history), m.showSessionPicker)
	}
}

func TestSessionPickerDiscardedLoadDoesNotActivateSession(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	stored.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "selected"}}
	if err := store.Commit(stored); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace", SessionStore: store,
	})
	currentID := m.session.ID
	listCommand := m.openSessionPicker(false)
	updated, _ := m.Update(listCommand())
	m = updated.(model)
	updated, loadCommand := m.updateSessionPicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	loadResult := loadCommand()
	updated, _ = m.updateSessionPicker(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(model)
	updated, _ = m.Update(loadResult)
	m = updated.(model)
	if m.session.ID != currentID || m.sessionPickerActivating || len(m.sessionJobs) != 0 {
		t.Fatalf("stale load activated a session: current=%q want=%q activating=%t jobs=%#v", m.session.ID, currentID, m.sessionPickerActivating, m.sessionJobs)
	}
}

func TestSessionActivationSavesCurrentInMemoryStateBeforeSwitching(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(current); err != nil {
		t.Fatal(err)
	}
	target, err := store.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(target); err != nil {
		t.Fatal(err)
	}
	state := taskstate.New(taskstate.Plan{
		Explanation: "changed after the last turn",
		Steps:       []taskstate.Step{{Step: "preserve this plan", Status: taskstate.StatusInProgress}},
	}, nil)
	manager := collaboration.New(context.Background(), nil)
	m := newModel(context.Background(), nil, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace",
		SessionStore: store, Session: current, TaskState: state, Collaboration: manager,
	})
	if err := manager.Restore(json.RawMessage(`[{"name":"late-agent","status":"completed","output":"finished after the main turn"}]`)); err != nil {
		t.Fatal(err)
	}
	m.history = []provider.Message{{Role: provider.MessageRoleUser, Content: "current unsaved state"}}
	m.setCollaborationMode(prompts.ModePlan)
	m, command := m.beginSessionActivation(target, sessionActivationContext{})
	if command == nil || len(m.sessionJobs) != 1 {
		t.Fatalf("activation was not queued: command=%v jobs=%#v", command, m.sessionJobs)
	}
	result := m.sessionJobs[0].run()
	updated, _ := m.Update(sessionJobMsg{result: result})
	m = updated.(model)
	if m.session.ID != target.ID {
		t.Fatalf("activated session=%q, want %q", m.session.ID, target.ID)
	}
	saved, err := store.Load(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Messages) != 1 || saved.Messages[0].Content != "current unsaved state" || saved.Mode != string(prompts.ModePlan) || len(saved.Plan.Steps) != 1 || !strings.Contains(string(saved.Agents), "late-agent") {
		t.Fatalf("current session state was lost before activation: %#v", saved)
	}
}

func TestDraftImagesHaveAggregateCountAndByteLimits(t *testing.T) {
	images := make([]provider.Image, maxDraftImages+1)
	for index := range images {
		images[index] = provider.Image{MIMEType: "image/png", Data: "AA=="}
	}
	if err := validateDraftImages(nil, images); err == nil || !strings.Contains(err.Error(), "image limit") {
		t.Fatalf("draft count error = %v", err)
	}
	if err := validateDraftImages(nil, images[:1]); err != nil {
		t.Fatalf("valid draft image error = %v", err)
	}
	if err := validateDraftImagesWithLimits([]provider.Image{{Data: "AAAA"}}, []provider.Image{{Data: "AAAA"}}, maxDraftImages, 5); err == nil || !strings.Contains(err.Error(), "aggregate") {
		t.Fatalf("draft byte error = %v", err)
	}
}

func TestSessionPickerDisplaysCorruptSnapshotWarnings(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(directory, "20260101-010101-deadbeef.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace", SessionStore: store,
	})
	command := m.openSessionPicker(false)
	updated, _ := m.Update(command())
	m = updated.(model)
	if len(m.sessionWarnings) == 0 || !strings.Contains(m.renderSessionPicker(80), "Warning:") {
		t.Fatalf("warnings=%#v picker=%q", m.sessionWarnings, m.renderSessionPicker(80))
	}
}

func TestSessionToolEventsPersistOnlySafeMetadata(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace", SessionStore: store, Session: stored,
	})
	call := &provider.ToolCall{ID: "call-secret", Name: "write_stdin", Arguments: `{"chars":"ARG_SENTINEL_SECRET"}`}
	updated, _ := m.handleAgentEvent(agent.Event{Type: agent.EventToolStarted, Call: call, Risk: tool.RiskExecute})
	m = updated.(model)
	result := m.sessionJobs[0].run()
	updated, _ = m.Update(sessionJobMsg{result: result})
	m = updated.(model)
	updated, _ = m.handleAgentEvent(agent.Event{
		Type: agent.EventToolFinished, Call: call,
		Result: &tool.Result{Content: "RESULT_SENTINEL_SECRET"},
	})
	m = updated.(model)
	result = m.sessionJobs[0].run()
	updated, _ = m.Update(sessionJobMsg{result: result})
	m = updated.(model)

	events, err := os.ReadFile(filepath.Join(directory, "events", stored.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(events)
	if strings.Contains(text, "ARG_SENTINEL_SECRET") || strings.Contains(text, "RESULT_SENTINEL_SECRET") {
		t.Fatalf("session diagnostic events leaked tool data: %s", text)
	}
	if !strings.Contains(text, "argument_bytes") || !strings.Contains(text, "result_bytes") {
		t.Fatalf("safe event metadata missing: %s", text)
	}
}

func TestProviderErrorPersistsPromptAndPartialAssistantBeforeUnlock(t *testing.T) {
	directory := t.TempDir()
	store, err := session.NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), nil, Options{
		Model: "test", Timeout: time.Second, Workspace: "/workspace", SessionStore: store, Session: stored,
	})
	m.busy = true
	m.activePrompt = "keep this prompt"
	m.turnMessageStart = 0
	m.messages = []message{{role: "assistant", content: "keep partial answer", streaming: true}}
	updated, _ := m.handleAgentEvent(agent.Event{Type: agent.EventError, Err: errors.New("ERROR_SENTINEL_SECRET")})
	m = updated.(model)
	if !m.busy || !m.pendingCompletion || len(m.history) != 2 {
		t.Fatalf("failed turn unlocked before durability: busy=%t pending=%t history=%#v", m.busy, m.pendingCompletion, m.history)
	}
	result := m.sessionJobs[0].run()
	updated, _ = m.Update(sessionJobMsg{result: result})
	m = updated.(model)
	if m.busy || m.pendingCompletion {
		t.Fatalf("failed turn stayed locked after save: busy=%t pending=%t", m.busy, m.pendingCompletion)
	}
	loaded, err := store.Load(stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[0].Content != "keep this prompt" || loaded.Messages[1].Content != "keep partial answer" {
		t.Fatalf("persisted failed history = %#v", loaded.Messages)
	}
	events, err := os.ReadFile(filepath.Join(directory, "events", stored.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), "ERROR_SENTINEL_SECRET") {
		t.Fatalf("turn_error event leaked provider error: %s", events)
	}
}

func BenchmarkStreamingMarkdownRender(b *testing.B) {
	m := newModel(context.Background(), nil, Options{Model: "benchmark", Timeout: time.Second})
	m.resize(100, 32)
	m.messages = []message{{role: "assistant", streaming: true}}
	chunk := "A streamed sentence with `code` and **emphasis**.\n\n"
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		m.appendAssistantDelta(chunk)
		m.refreshMessages(true)
		if m.messages[0].streamBytes > 128*1024 {
			m.messages[0].streamChunks = nil
			m.messages[0].streamTail = ""
			m.messages[0].streamBytes = 0
		}
	}
}

func BenchmarkLongStreamingTailRender(b *testing.B) {
	m := newModel(context.Background(), nil, Options{Model: "benchmark", Timeout: time.Second})
	m.resize(100, 32)
	m.messages = []message{
		{role: "status", content: strings.Repeat("stable history ", 300_000)},
		{role: "assistant", streaming: true},
	}
	m.refreshMessages(true)
	chunk := strings.Repeat("next streamed chunk ", 32)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		m.appendAssistantDelta(chunk)
		m.refreshMessages(true)
		if len(m.messages[len(m.messages)-1].streamChunks) > 4096 {
			m.messages[len(m.messages)-1].streamChunks = nil
			m.messages[len(m.messages)-1].streamTail = ""
			m.messages[len(m.messages)-1].streamBytes = 0
		}
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
