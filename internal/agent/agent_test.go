package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/modelcatalog"
	"github.com/daemon365/supercode/internal/permission"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

type fakeProvider struct {
	responses []provider.Response
	requests  []provider.Request
	errors    []error
}

func (f *fakeProvider) Generate(_ context.Context, request provider.Request) (provider.Response, error) {
	f.requests = append(f.requests, request)
	if len(f.errors) > 0 {
		err := f.errors[0]
		f.errors = f.errors[1:]
		if err != nil {
			return provider.Response{}, err
		}
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response, nil
}

func TestRunnerFallsBackBeforeAnyStreamedOutput(t *testing.T) {
	model := &fakeProvider{
		responses: []provider.Response{{Text: "fallback worked"}},
		errors:    []error{errors.New("primary unavailable"), nil},
	}
	registry, _ := tool.NewRegistry()
	runner, _ := New(model, registry, Options{Model: "primary", FallbackModels: []string{"fallback"}, Approval: ApprovalNever})
	for event := range runner.Run(context.Background(), Input{Prompt: "hello"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if len(model.requests) != 2 || model.requests[0].Model != "primary" || model.requests[1].Model != "fallback" {
		t.Fatalf("requests = %#v", model.requests)
	}
}

func TestRunnerAdaptsRequestToFallbackCapabilities(t *testing.T) {
	toolCalling, noToolCalling := true, false
	catalog := modelcatalog.New(nil, map[string]modelcatalog.Capabilities{
		"primary":  {ToolCalling: &toolCalling},
		"fallback": {ToolCalling: &noToolCalling, InputModalities: []string{"text"}},
	})
	model := &fakeProvider{responses: []provider.Response{{Text: "fallback worked"}}, errors: []error{errors.New("primary unavailable"), nil}}
	registry, _ := tool.NewRegistry(&fakeTool{name: "read", risk: tool.RiskRead})
	runner, err := New(model, registry, Options{Model: "primary", FallbackModels: []string{"fallback"}, Approval: ApprovalNever, ModelCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	for event := range runner.Run(context.Background(), Input{Prompt: "hello"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if len(model.requests) != 2 || len(model.requests[0].Tools) == 0 || len(model.requests[1].Tools) != 0 || model.requests[1].ParallelToolCalls != nil {
		t.Fatalf("capability-adapted requests = %#v", model.requests)
	}
}

func TestRunnerSkipsFallbackWithoutRequiredImageModality(t *testing.T) {
	model := &fakeProvider{errors: []error{errors.New("primary unavailable")}}
	catalog := modelcatalog.New(nil, map[string]modelcatalog.Capabilities{
		"primary":  {InputModalities: []string{"text", "image"}},
		"fallback": {InputModalities: []string{"text"}},
	})
	registry, _ := tool.NewRegistry()
	runner, err := New(model, registry, Options{Model: "primary", FallbackModels: []string{"fallback"}, Approval: ApprovalNever, ModelCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for event := range runner.Run(context.Background(), Input{Prompt: "inspect", Images: []provider.Image{{MIMEType: "image/png", Data: "AA=="}}}) {
		if event.Type == EventError {
			runErr = event.Err
		}
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "image input") || len(model.requests) != 1 {
		t.Fatalf("error=%v requests=%d", runErr, len(model.requests))
	}
}

func TestRunnerHidesMemoryCitationAndRecordsUsage(t *testing.T) {
	const rolloutID = "11111111-1111-4111-8111-111111111111"
	model := &fakeProvider{responses: []provider.Response{{Text: "Visible answer<oai-mem-citation><citation_entries>MEMORY.md:1-1|note=[used]</citation_entries><rollout_ids>\n" + rolloutID + "\n</rollout_ids></oai-mem-citation>"}}}
	registry, _ := tool.NewRegistry()
	var cited []string
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever, OnMemoryCitation: func(ids []string) { cited = append(cited, ids...) }})
	var output string
	for event := range runner.Run(context.Background(), Input{Prompt: "hello"}) {
		if event.Type == EventTextDelta {
			output += event.Delta
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if output != "Visible answer" || len(cited) != 1 || cited[0] != rolloutID {
		t.Fatalf("output=%q cited=%v", output, cited)
	}
}

func TestToolSearchLazilyActivatesMCPDefinitions(t *testing.T) {
	remote := &fakeTool{name: "mcp__demo__echo", risk: tool.RiskRead}
	search := tool.SearchTool([]tool.Tool{remote})
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "search", Name: "tool_search", Arguments: `{"query":"echo"}`}}},
		{ToolCalls: []provider.ToolCall{{ID: "call", Name: "mcp__demo__echo", Arguments: `{}`}}},
		{Text: "done"},
	}}
	registry, _ := tool.NewRegistry(remote, search)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalAlways})
	for event := range runner.Run(context.Background(), Input{Prompt: "find echo"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	contains := func(request provider.Request, name string) bool {
		for _, definition := range request.Tools {
			if definition.Name == name {
				return true
			}
		}
		return false
	}
	if contains(model.requests[0], remote.name) || !contains(model.requests[1], remote.name) {
		t.Fatalf("lazy definitions were not activated: first=%#v second=%#v", model.requests[0].Tools, model.requests[1].Tools)
	}
}
func (*fakeProvider) Stream(context.Context, provider.Request) (provider.Stream, error) {
	panic("unexpected Stream")
}

func TestDefaultContextBudgetReservesFivePercent(t *testing.T) {
	registry, _ := tool.NewRegistry()
	runner, err := New(&fakeProvider{}, registry, Options{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if runner.options.ContextWindowTokens != 272_000 || runner.options.AutoCompactTokens != 244_800 || runner.options.UsableContextTokens != 258_400 {
		t.Fatalf("context budget = %d/%d/%d", runner.options.ContextWindowTokens, runner.options.AutoCompactTokens, runner.options.UsableContextTokens)
	}
}

func TestRunnerRejectsRequestsAtUsableContextLimit(t *testing.T) {
	registry, _ := tool.NewRegistry()
	model := &fakeProvider{}
	runner, err := New(model, registry, Options{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for event := range runner.Run(context.Background(), Input{Prompt: strings.Repeat("a", DefaultUsableContextTokens*4)}) {
		if event.Type == EventError {
			runErr = event.Err
		}
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "258400-token usable limit") {
		t.Fatalf("error = %v, want usable context limit", runErr)
	}
	if len(model.requests) != 0 {
		t.Fatalf("provider received %d oversized request(s)", len(model.requests))
	}
}

type fakeTool struct {
	calls int
	name  string
	risk  tool.Risk
}

type categorizedFakeTool struct {
	*fakeTool
	category tool.Category
}

func (t *categorizedFakeTool) Category() tool.Category { return t.category }

type emptyPermissionFakeTool struct{ fakeTool }

func (*emptyPermissionFakeTool) PermissionRequest(string) (permission.Request, error) {
	return permission.Request{}, nil
}

func (f *fakeTool) Definition() provider.ToolDefinition {
	name := f.name
	if name == "" {
		name = "edit"
	}
	return provider.ToolDefinition{Name: name, Parameters: []byte(`{"type":"object"}`)}
}
func (f *fakeTool) Risk(string) tool.Risk {
	if f.risk == "" {
		return tool.RiskWrite
	}
	return f.risk
}
func (*fakeTool) Summary(string) string { return "edit a file" }
func (f *fakeTool) Execute(context.Context, string) (tool.Result, error) {
	f.calls++
	return tool.Result{Content: "edited"}, nil
}

func TestReturnedToolImagesHaveAggregateCountAndByteLimits(t *testing.T) {
	images := []provider.Image{{MIMEType: "image/png", Data: "AA=="}, {MIMEType: "image/png", Data: "AA=="}}
	if _, err := validateReturnedToolImages(images, maxReturnedToolImages-1, 0, maxReturnedToolImages, maxReturnedToolImageBytes); err == nil || !strings.Contains(err.Error(), "image limit") {
		t.Fatalf("count-limit error = %v", err)
	}
	if _, err := validateReturnedToolImages(images[:1], 0, 0, maxReturnedToolImages, 0); err == nil || !strings.Contains(err.Error(), "decoded-data limit") {
		t.Fatalf("byte-limit error = %v", err)
	}
	if added, err := validateReturnedToolImages(images, 0, 0, maxReturnedToolImages, maxReturnedToolImageBytes); err != nil || added != 2 {
		t.Fatalf("valid images added=%d err=%v", added, err)
	}
}

func TestRunnerExecutesApprovedToolAndContinues(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ID: "one", ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "edit", Arguments: `{}`}}},
		{ID: "two", Text: "done"},
	}}
	edit := &fakeTool{risk: tool.RiskExecute}
	registry, _ := tool.NewRegistry(edit)
	runner, err := New(model, registry, Options{Model: "test", Approval: ApprovalOnRequest})
	if err != nil {
		t.Fatal(err)
	}

	var completed []provider.Message
	for event := range runner.Run(context.Background(), Input{Prompt: "change it"}) {
		if event.Type == EventApprovalRequired {
			event.Approval.Decide(true)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		if event.Type == EventCompleted {
			completed = event.History
		}
	}
	if edit.calls != 1 {
		t.Fatalf("tool calls = %d, want 1", edit.calls)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	second := model.requests[1]
	if second.Prompt != "" {
		t.Fatalf("second prompt = %q, want empty", second.Prompt)
	}
	if len(second.History) != 3 {
		t.Fatalf("second history = %+v", second.History)
	}
	if second.History[2].Role != provider.MessageRoleTool || second.History[2].ToolCallID != "call_1" || second.History[2].Content != "edited" {
		t.Fatalf("tool result history = %+v", second.History[2])
	}
	if len(completed) != 4 || completed[3].Content != "done" {
		t.Fatalf("completed history = %+v", completed)
	}
}

func TestRunnerNeverApprovalDeniesWrite(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "edit", Arguments: `{}`}}},
		{Text: "could not edit"},
	}}
	edit := &fakeTool{risk: tool.RiskExecute}
	registry, _ := tool.NewRegistry(edit)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever})
	for event := range runner.Run(context.Background(), Input{Prompt: "change it"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if edit.calls != 0 {
		t.Fatalf("tool calls = %d, want 0", edit.calls)
	}
	if got := model.requests[1].History[2].Content; got != "ERROR: Tool call denied by the user or approval policy." {
		t.Fatalf("denial result = %q", got)
	}
}

func TestRunnerQueuesGuidanceAfterToolBatch(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "edit", Arguments: `{}`}}},
		{Text: "steered"},
	}}
	edit := &fakeTool{risk: tool.RiskExecute}
	registry, _ := tool.NewRegistry(edit)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalOnRequest})
	run := runner.Start(context.Background(), Input{Prompt: "start"})
	committed := false
	for event := range run.Events {
		if event.Type == EventApprovalRequired {
			if !run.Queue("also check tests") {
				t.Fatal("Queue() = false")
			}
			event.Approval.Decide(true)
		}
		if event.Type == EventQueuedCommitted {
			committed = true
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if !committed {
		t.Fatal("queued message was not committed")
	}
	second := model.requests[1].History
	if len(second) != 4 || second[3].Role != provider.MessageRoleUser || second[3].Content != "also check tests" {
		t.Fatalf("second request history = %+v", second)
	}
}

func TestRunnerAutoAllowsWorkspaceWriteWithoutApproval(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "edit", Arguments: `{}`}}},
		{Text: "done"},
	}}
	edit := &fakeTool{risk: tool.RiskWrite}
	registry, _ := tool.NewRegistry(edit)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalOnRequest})
	approvals := 0
	for event := range runner.Run(context.Background(), Input{Prompt: "change it"}) {
		if event.Type == EventApprovalRequired {
			approvals++
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if approvals != 0 || edit.calls != 1 {
		t.Fatalf("approvals=%d calls=%d, want 0 and 1", approvals, edit.calls)
	}
}

func TestRunnerGranularCategoryDenyPrecedesReadAutoApproval(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "read", Arguments: `{}`}}},
		{Text: "denied"},
	}}
	read := &categorizedFakeTool{fakeTool: &fakeTool{name: "read", risk: tool.RiskRead}, category: tool.CategoryFile}
	registry, _ := tool.NewRegistry(read)
	runner, _ := New(model, registry, Options{
		Model: "test", Approval: ApprovalGranular,
		ApprovalCategories: map[tool.Category]bool{tool.CategoryFile: false},
	})
	for event := range runner.Run(context.Background(), Input{Prompt: "read"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if read.calls != 0 {
		t.Fatalf("read calls = %d, want denied", read.calls)
	}
}

func TestRunnerSkipsEmptyPermissionGrantForLocalToolOperation(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "local", Arguments: `{}`}}},
		{Text: "done"},
	}}
	local := &emptyPermissionFakeTool{fakeTool{name: "local", risk: tool.RiskRead}}
	registry, _ := tool.NewRegistry(local)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever})
	for event := range runner.Run(context.Background(), Input{Prompt: "local operation"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if local.calls != 1 {
		t.Fatalf("local calls = %d, want 1", local.calls)
	}
}

func TestRootRunExpiresTurnPermissionsEvenInNeverMode(t *testing.T) {
	manager, err := permission.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	extra := t.TempDir()
	if _, err := manager.Grant(permission.Profile{FileSystem: permission.FileSystem{Read: []string{extra}}}, permission.ScopeTurn); err != nil {
		t.Fatal(err)
	}
	registry, _ := tool.NewRegistry()
	runner, _ := New(&fakeProvider{responses: []provider.Response{{Text: "done"}}}, registry, Options{Model: "test", Approval: ApprovalNever, Permissions: manager})
	for event := range runner.Run(context.Background(), Input{Prompt: "hello"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if snapshot := manager.Snapshot(); len(snapshot.Turn.FileSystem.Read) != 0 {
		t.Fatalf("turn permission survived root turn start: %+v", snapshot)
	}
}

func TestForkDoesNotExpireParentTurnPermissions(t *testing.T) {
	manager, err := permission.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	extra := t.TempDir()
	if _, err := manager.Grant(permission.Profile{FileSystem: permission.FileSystem{Read: []string{extra}}}, permission.ScopeTurn); err != nil {
		t.Fatal(err)
	}
	registry, _ := tool.NewRegistry()
	parent, _ := New(&fakeProvider{responses: []provider.Response{{Text: "done"}}}, registry, Options{Model: "test", Approval: ApprovalNever, Permissions: manager})
	child, err := parent.Fork("", ApprovalNever)
	if err != nil {
		t.Fatal(err)
	}
	for event := range child.Run(context.Background(), Input{Prompt: "hello"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if snapshot := manager.Snapshot(); len(snapshot.Turn.FileSystem.Read) != 1 {
		t.Fatalf("child expired parent turn permission: %+v", snapshot)
	}
}

func TestRunnerSessionApprovalAllowsRepeatedTool(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "exec_command", Arguments: `{"cmd":"go test ./..."}`}}},
		{ToolCalls: []provider.ToolCall{{ID: "call_2", Name: "exec_command", Arguments: `{"cmd":"go vet ./..."}`}}},
		{Text: "done"},
	}}
	command := &fakeTool{name: "exec_command", risk: tool.RiskExecute}
	registry, _ := tool.NewRegistry(command)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalOnRequest})
	approvals := 0
	for event := range runner.Run(context.Background(), Input{Prompt: "check it"}) {
		if event.Type == EventApprovalRequired {
			approvals++
			event.Approval.DecideWithScope(ApprovalAllowSession)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if approvals != 1 || command.calls != 2 {
		t.Fatalf("approvals=%d calls=%d, want 1 and 2", approvals, command.calls)
	}
}

func TestRunnerPrefixApprovalMatchesOnlySimpleCommandPrefix(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "exec_command", Arguments: `{"cmd":"go test ./..."}`}}},
		{ToolCalls: []provider.ToolCall{{ID: "call_2", Name: "exec_command", Arguments: `{"cmd":"go test ./internal/..."}`}}},
		{Text: "done"},
	}}
	command := &fakeTool{name: "exec_command", risk: tool.RiskExecute}
	registry, _ := tool.NewRegistry(command)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalOnRequest})
	approvals := 0
	for event := range runner.Run(context.Background(), Input{Prompt: "test it"}) {
		if event.Type == EventApprovalRequired {
			approvals++
			event.Approval.DecideWithScope(ApprovalAllowPrefix)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if approvals != 1 || command.calls != 2 {
		t.Fatalf("approvals=%d calls=%d, want 1 and 2", approvals, command.calls)
	}
}

func TestRunnerReportsUsageForEveryModelTurn(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{{Text: "done", Usage: provider.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}}}}
	registry, _ := tool.NewRegistry()
	var usage provider.Usage
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever, OnUsage: func(value provider.Usage) {
		usage = value
	}})
	for event := range runner.Run(context.Background(), Input{Prompt: "hello"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

type progressTool struct{}

func (*progressTool) Definition() provider.ToolDefinition {
	return provider.ToolDefinition{Name: "progress", Parameters: []byte(`{"type":"object"}`)}
}

type delayedParallelTool struct {
	name     string
	parallel bool
	delay    time.Duration
	active   *atomic.Int64
	maximum  *atomic.Int64
}

func (t *delayedParallelTool) Definition() provider.ToolDefinition {
	return provider.ToolDefinition{Name: t.name, Parameters: []byte(`{"type":"object"}`)}
}
func (*delayedParallelTool) Risk(string) tool.Risk      { return tool.RiskRead }
func (t *delayedParallelTool) ParallelSafe(string) bool { return t.parallel }
func (t *delayedParallelTool) Summary(string) string    { return "delayed " + t.name }
func (t *delayedParallelTool) Execute(context.Context, string) (tool.Result, error) {
	active := t.active.Add(1)
	for {
		maximum := t.maximum.Load()
		if active <= maximum || t.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	time.Sleep(t.delay)
	t.active.Add(-1)
	return tool.Result{Content: t.name}, nil
}

func TestRunnerExecutesOptedInReadBatchConcurrentlyAndKeepsResultOrder(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{Name: "read_a", Arguments: `{}`}, {Name: "read_b", Arguments: `{}`}}},
		{Text: "done"},
	}}
	var active, maximum atomic.Int64
	first := &delayedParallelTool{name: "read_a", parallel: true, delay: 60 * time.Millisecond, active: &active, maximum: &maximum}
	second := &delayedParallelTool{name: "read_b", parallel: true, delay: 60 * time.Millisecond, active: &active, maximum: &maximum}
	registry, _ := tool.NewRegistry(first, second)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever})
	for event := range runner.Run(context.Background(), Input{Prompt: "read both"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if maximum.Load() < 2 {
		t.Fatalf("maximum concurrency = %d, want at least 2", maximum.Load())
	}
	history := model.requests[1].History
	if len(history) != 4 || history[2].Content != "read_a" || history[3].Content != "read_b" {
		t.Fatalf("tool results lost response order: %+v", history)
	}
	if history[1].ToolCalls[0].ID == "" || history[1].ToolCalls[0].ID != history[2].ToolCallID {
		t.Fatalf("generated tool call IDs are inconsistent: %+v", history)
	}
}

func TestGeneratedToolCallIDsStayUniqueAcrossRuns(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{Name: "read", Arguments: `{}`}}}, {Text: "first"},
		{ToolCalls: []provider.ToolCall{{Name: "read", Arguments: `{}`}}}, {Text: "second"},
	}}
	read := &delayedParallelTool{name: "read", parallel: true, active: &atomic.Int64{}, maximum: &atomic.Int64{}}
	registry, _ := tool.NewRegistry(read)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever})
	var identifiers []string
	for _, prompt := range []string{"one", "two"} {
		for event := range runner.Run(context.Background(), Input{Prompt: prompt}) {
			if event.Type == EventError {
				t.Fatal(event.Err)
			}
			if event.Type == EventCompleted {
				identifiers = append(identifiers, event.History[1].ToolCalls[0].ID)
			}
		}
	}
	if len(identifiers) != 2 || identifiers[0] == "" || identifiers[0] == identifiers[1] {
		t.Fatalf("generated IDs = %v", identifiers)
	}
}

func TestRunnerSerializesBatchWhenAnyToolDoesNotOptIn(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "a", Name: "read_a", Arguments: `{}`}, {ID: "b", Name: "stateful", Arguments: `{}`}}},
		{Text: "done"},
	}}
	var active, maximum atomic.Int64
	first := &delayedParallelTool{name: "read_a", parallel: true, delay: 10 * time.Millisecond, active: &active, maximum: &maximum}
	second := &delayedParallelTool{name: "stateful", parallel: false, delay: 10 * time.Millisecond, active: &active, maximum: &maximum}
	registry, _ := tool.NewRegistry(first, second)
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever})
	for event := range runner.Run(context.Background(), Input{Prompt: "read safely"}) {
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
	}
	if maximum.Load() != 1 {
		t.Fatalf("stateful read overlapped another call: maximum concurrency=%d", maximum.Load())
	}
}

func TestParallelToolSlotAcquisitionHonorsCancellation(t *testing.T) {
	var active, maximum atomic.Int64
	item := &delayedParallelTool{name: "read", parallel: true, active: &active, maximum: &maximum}
	registry, _ := tool.NewRegistry(item)
	runner, _ := New(&fakeProvider{}, registry, Options{Model: "test", Approval: ApprovalNever})
	runner.toolSlots = make(chan struct{}, 1)
	runner.toolSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := []preparedToolCall{{call: provider.ToolCall{Name: "read", Arguments: `{}`}, item: item}}
	runner.executePreparedCalls(ctx, make(chan Event, 1), calls)
	if !calls[0].finished || !calls[0].result.IsError || !strings.Contains(calls[0].result.Content, "canceled") || item.active.Load() != 0 {
		t.Fatalf("canceled call = %+v active=%d", calls[0], item.active.Load())
	}
	<-runner.toolSlots
}
func (*progressTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*progressTool) Summary(string) string { return "stream progress" }
func (*progressTool) Execute(ctx context.Context, _ string) (tool.Result, error) {
	tool.ReportProgress(ctx, tool.Progress{Delta: "first\n", SessionID: 42})
	tool.ReportProgress(ctx, tool.Progress{Delta: "second\n", SessionID: 42})
	return tool.Result{Content: "done"}, nil
}

func TestRunnerStreamsToolProgressBeforeCompletion(t *testing.T) {
	model := &fakeProvider{responses: []provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "call_progress", Name: "progress", Arguments: `{}`}}},
		{Text: "done"},
	}}
	registry, _ := tool.NewRegistry(&progressTool{})
	runner, _ := New(model, registry, Options{Model: "test", Approval: ApprovalNever})
	var deltas string
	finished := false
	for event := range runner.Run(context.Background(), Input{Prompt: "run"}) {
		switch event.Type {
		case EventToolOutputDelta:
			if finished {
				t.Fatal("progress arrived after tool completion")
			}
			deltas += event.Delta
			if event.SessionID != 42 {
				t.Fatalf("session ID = %d", event.SessionID)
			}
		case EventToolFinished:
			finished = true
		case EventError:
			t.Fatal(event.Err)
		}
	}
	if deltas != "first\nsecond\n" || !finished {
		t.Fatalf("deltas=%q finished=%t", deltas, finished)
	}
}
