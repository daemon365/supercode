package taskstate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daemon365/supercode/internal/provider"
)

func TestGetGoalSchemaDeclaresProperties(t *testing.T) {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal((&getGoalTool{}).Definition().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties, exists := schema["properties"]
	if !exists || string(properties) != "{}" {
		t.Fatalf("properties = %s, want {}", properties)
	}
}

func TestPlanAndGoalTools(t *testing.T) {
	state := New(Plan{}, nil)
	tools := state.Tools()
	find := func(name string) int {
		for i, item := range tools {
			if item.Definition().Name == name {
				return i
			}
		}
		return -1
	}
	if _, err := tools[find("update_plan")].Execute(context.Background(), `{"explanation":"Starting","plan":[{"step":"Inspect","status":"in_progress"},{"step":"Test","status":"pending"}]}`); err != nil {
		t.Fatal(err)
	}
	plan, _ := state.Snapshot()
	if len(plan.Steps) != 2 || plan.Steps[0].Status != StatusInProgress {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := tools[find("create_goal")].Execute(context.Background(), `{"objective":"Ship it","token_budget":1000}`); err != nil {
		t.Fatal(err)
	}
	result, err := tools[find("get_goal")].Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Ship it") {
		t.Fatalf("goal = %s", result.Content)
	}
	state.RecordUsage(provider.Usage{InputTokens: 40, OutputTokens: 10})
	result, err = tools[find("get_goal")].Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"total_tokens":50`) || !strings.Contains(result.Content, `"remaining_token_budget":950`) || !strings.Contains(result.Content, `"turns":1`) {
		t.Fatalf("goal usage = %s", result.Content)
	}
	if _, err := tools[find("update_goal")].Execute(context.Background(), `{"status":"complete"}`); err != nil {
		t.Fatal(err)
	}
	_, goal := state.Snapshot()
	if goal == nil || goal.Status != "complete" {
		t.Fatalf("goal = %+v", goal)
	}
}

func TestPlanRejectsTwoActiveSteps(t *testing.T) {
	state := New(Plan{}, nil)
	_, err := state.Tools()[0].Execute(context.Background(), `{"plan":[{"step":"A","status":"in_progress"},{"step":"B","status":"in_progress"}]}`)
	if err == nil {
		t.Fatal("expected validation error")
	}
}
