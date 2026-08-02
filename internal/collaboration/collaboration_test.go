package collaboration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

type fakeProvider struct{}

func (*fakeProvider) Generate(_ context.Context, request provider.Request) (provider.Response, error) {
	return provider.Response{Text: "sub-agent: " + request.Prompt}, nil
}
func (*fakeProvider) Stream(context.Context, provider.Request) (provider.Stream, error) {
	panic("unexpected stream")
}

func TestListAgentsSchemaDeclaresProperties(t *testing.T) {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal((&listTool{}).Definition().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties, exists := schema["properties"]
	if !exists || string(properties) != "{}" {
		t.Fatalf("properties = %s, want {}", properties)
	}
}

func TestSpawnListAndWaitAgent(t *testing.T) {
	registry, _ := tool.NewRegistry()
	runner, _ := agent.New(&fakeProvider{}, registry, agent.Options{Model: "test", Stream: false, Approval: agent.ApprovalNever})
	manager := New(context.Background(), runner)
	items := manager.Tools()
	find := func(name string) tool.Tool {
		for _, item := range items {
			if item.Definition().Name == name {
				return item
			}
		}
		t.Fatalf("missing %s", name)
		return nil
	}
	if _, err := find("spawn_agent").Execute(context.Background(), `{"task_name":"research","message":"inspect"}`); err != nil {
		t.Fatal(err)
	}
	result, err := find("wait_agent").Execute(context.Background(), `{"target":"research","timeout_ms":2000}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "sub-agent: inspect") || !strings.Contains(result.Content, "completed") {
		t.Fatalf("wait = %s", result.Content)
	}
	listed, err := find("list_agents").Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.Content, "research") {
		t.Fatalf("list = %s", listed.Content)
	}
}

func TestAwaiterRoleUsesSpecializedInstructions(t *testing.T) {
	value := roleInstructions("awaiter", "low")
	if !strings.Contains(value, "awaiter sub-agent") || !strings.Contains(value, "Requested reasoning effort: low") {
		t.Fatalf("awaiter instructions = %q", value)
	}
}

type captureProvider struct{ requests chan provider.Request }

func (p *captureProvider) Generate(_ context.Context, request provider.Request) (provider.Response, error) {
	p.requests <- request
	return provider.Response{Text: "done"}, nil
}
func (*captureProvider) Stream(context.Context, provider.Request) (provider.Stream, error) {
	panic("unexpected stream")
}

func TestSpawnAppliesReasoningEffortToProviderRequest(t *testing.T) {
	model := &captureProvider{requests: make(chan provider.Request, 1)}
	registry, _ := tool.NewRegistry()
	runner, _ := agent.New(model, registry, agent.Options{Model: "test", Stream: false, Approval: agent.ApprovalNever})
	manager := New(context.Background(), runner)
	spawn := manager.Tools()[0]
	if _, err := spawn.Execute(context.Background(), `{"task_name":"reasoning","message":"inspect","reasoning_effort":"high"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-model.requests:
		if request.ReasoningEffort != "high" {
			t.Fatalf("reasoning effort = %q", request.ReasoningEffort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sub-agent request did not start")
	}
}

type blockingProvider struct{ started chan struct{} }

func (p *blockingProvider) Generate(ctx context.Context, _ provider.Request) (provider.Response, error) {
	close(p.started)
	<-ctx.Done()
	return provider.Response{}, ctx.Err()
}
func (*blockingProvider) Stream(context.Context, provider.Request) (provider.Stream, error) {
	panic("unexpected stream")
}

func TestCanceledSubAgentCannotBeMarkedCompletedWhenErrorEventIsDropped(t *testing.T) {
	model := &blockingProvider{started: make(chan struct{})}
	registry, _ := tool.NewRegistry()
	runner, _ := agent.New(model, registry, agent.Options{Model: "test", Stream: false, Approval: agent.ApprovalNever})
	manager := New(context.Background(), runner)
	items := manager.Tools()
	if _, err := items[0].Execute(context.Background(), `{"task_name":"blocked","message":"wait"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sub-agent did not start")
	}
	if _, err := items[3].Execute(context.Background(), `{"target":"blocked"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := items[5].Execute(context.Background(), `{"target":"blocked","timeout_ms":2000}`); err != nil {
		t.Fatal(err)
	}
	state, err := manager.snapshot("blocked")
	if err != nil {
		t.Fatal(err)
	}
	if state["status"] != "interrupted" {
		t.Fatalf("status = %v, want interrupted", state["status"])
	}
}

func TestShutdownCancelsAndJoinsRunningSubAgents(t *testing.T) {
	model := &blockingProvider{started: make(chan struct{})}
	registry, _ := tool.NewRegistry()
	runner, _ := agent.New(model, registry, agent.Options{Model: "test", Stream: false, Approval: agent.ApprovalNever})
	manager := New(context.Background(), runner)
	if _, err := manager.Tools()[0].Execute(context.Background(), `{"task_name":"blocked","message":"wait"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("sub-agent did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := manager.snapshot("blocked")
	if err != nil {
		t.Fatal(err)
	}
	if state["status"] != "interrupted" {
		t.Fatalf("status = %v", state["status"])
	}
	if _, err := manager.Tools()[0].Execute(context.Background(), `{"task_name":"later","message":"run"}`); err == nil {
		t.Fatal("spawn succeeded after shutdown")
	}
}

func TestRestoreCancelsOldRunningSubAgentsBeforeReplacingState(t *testing.T) {
	model := &blockingProvider{started: make(chan struct{})}
	registry, _ := tool.NewRegistry()
	runner, _ := agent.New(model, registry, agent.Options{Model: "test", Stream: false, Approval: agent.ApprovalNever})
	manager := New(context.Background(), runner)
	if _, err := manager.Tools()[0].Execute(context.Background(), `{"task_name":"old","message":"wait"}`); err != nil {
		t.Fatal(err)
	}
	<-model.started
	if err := manager.Restore(json.RawMessage(`[{"name":"saved","status":"completed"}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.snapshot("old"); err == nil {
		t.Fatal("old running task survived restore")
	}
	if state, err := manager.snapshot("saved"); err != nil || state["status"] != "completed" {
		t.Fatalf("restored task=%v err=%v", state, err)
	}
}

func TestSubAgentOutputAndHistoryAreBounded(t *testing.T) {
	value := boundTaskOutput(strings.Repeat("界", maxTaskOutputBytes))
	if len(value) > maxTaskOutputBytes || !strings.HasPrefix(value, "[earlier sub-agent output truncated]") {
		t.Fatalf("bounded output bytes=%d prefix=%q", len(value), value[:min(40, len(value))])
	}
	var history []provider.Message
	for range 400 {
		history = append(history,
			provider.Message{Role: provider.MessageRoleUser, Content: strings.Repeat("question ", 400)},
			provider.Message{Role: provider.MessageRoleAssistant, Content: strings.Repeat("answer ", 400)},
		)
	}
	bounded := boundedTaskHistory(history)
	if agent.EstimateMessagesTokens(bounded) > maxTaskHistoryTokens+1024 {
		t.Fatalf("bounded history still has %d tokens", agent.EstimateMessagesTokens(bounded))
	}
}

func TestSnapshotHasAggregateBudgetAndPrioritizesRecentTaskHistory(t *testing.T) {
	manager := New(context.Background(), nil)
	for index := range 12 {
		name := fmt.Sprintf("task-%02d", index)
		manager.tasks[name] = &task{
			name: name, status: "completed", sequence: uint64(index + 1), done: make(chan struct{}),
			history: []provider.Message{{Role: provider.MessageRoleAssistant, Content: strings.Repeat(string(rune('a'+index)), 900*1024)}},
			output:  strings.Repeat("output", 1024),
		}
	}
	snapshot := manager.Snapshot()
	if len(snapshot) > maxSnapshotBytes {
		t.Fatalf("snapshot bytes = %d, limit = %d", len(snapshot), maxSnapshotBytes)
	}
	var values []persistedTask
	if err := json.Unmarshal(snapshot, &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 12 {
		t.Fatalf("snapshot task count = %d, want metadata for 12", len(values))
	}
	byName := make(map[string]persistedTask, len(values))
	for _, value := range values {
		byName[value.Name] = value
	}
	if len(byName["task-11"].History) == 0 {
		t.Fatal("newest task history was not retained")
	}
	if len(byName["task-00"].History) != 0 {
		t.Fatal("oldest task history should yield to the aggregate budget")
	}
	restored := New(context.Background(), nil)
	if err := restored.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
}
