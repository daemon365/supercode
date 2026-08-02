package collaboration

import (
	"context"
	"strings"
	"testing"

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
