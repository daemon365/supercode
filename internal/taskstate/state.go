// Package taskstate provides model-managed plans and explicit long-term goals.
package taskstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

type StepStatus string

const (
	StatusPending    StepStatus = "pending"
	StatusInProgress StepStatus = "in_progress"
	StatusCompleted  StepStatus = "completed"
)

type Step struct {
	Step   string     `json:"step"`
	Status StepStatus `json:"status"`
}
type Plan struct {
	Explanation string    `json:"explanation,omitempty"`
	Steps       []Step    `json:"plan,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}
type Goal struct {
	Objective    string    `json:"objective"`
	TokenBudget  int       `json:"token_budget,omitempty"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	TotalTokens  int64     `json:"total_tokens,omitempty"`
	Turns        int       `json:"turns,omitempty"`
}

type State struct {
	mu   sync.RWMutex
	plan Plan
	goal *Goal
}

func New(plan Plan, goal *Goal) *State { return &State{plan: plan, goal: goal} }
func (s *State) Snapshot() (Plan, *Goal) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	plan := s.plan
	plan.Steps = append([]Step(nil), plan.Steps...)
	var goal *Goal
	if s.goal != nil {
		copy := *s.goal
		goal = &copy
	}
	return plan, goal
}
func (s *State) Reset() { s.mu.Lock(); defer s.mu.Unlock(); s.plan = Plan{}; s.goal = nil }
func (s *State) Restore(plan Plan, goal *Goal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plan = plan
	if goal == nil {
		s.goal = nil
	} else {
		copy := *goal
		s.goal = &copy
	}
}
func (s *State) RecordUsage(usage provider.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal == nil || s.goal.Status != "active" {
		return
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	s.goal.InputTokens += usage.InputTokens
	s.goal.OutputTokens += usage.OutputTokens
	s.goal.TotalTokens += total
	s.goal.Turns++
	s.goal.UpdatedAt = time.Now().UTC()
}
func (s *State) Tools() []tool.Tool {
	return []tool.Tool{&updatePlanTool{s}, &createGoalTool{s}, &getGoalTool{s}, &updateGoalTool{s}}
}

type updatePlanTool struct{ state *State }

func (*updatePlanTool) Definition() provider.ToolDefinition {
	return definition("update_plan", "Create or update the task plan. At most one step may be in_progress.", `{"type":"object","properties":{"explanation":{"type":"string"},"plan":{"type":"array","items":{"type":"object","properties":{"step":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["step","status"],"additionalProperties":false}}},"required":["plan"],"additionalProperties":false}`)
}
func (*updatePlanTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*updatePlanTool) Summary(string) string { return "update task plan" }
func (t *updatePlanTool) Execute(_ context.Context, args string) (tool.Result, error) {
	var input struct {
		Explanation string `json:"explanation"`
		Plan        []Step `json:"plan"`
	}
	if err := decode(args, &input); err != nil {
		return tool.Result{}, err
	}
	active := 0
	for _, step := range input.Plan {
		if strings.TrimSpace(step.Step) == "" {
			return tool.Result{}, errors.New("plan step text is required")
		}
		switch step.Status {
		case StatusPending, StatusInProgress, StatusCompleted:
		default:
			return tool.Result{}, fmt.Errorf("invalid plan status %q", step.Status)
		}
		if step.Status == StatusInProgress {
			active++
		}
	}
	if active > 1 {
		return tool.Result{}, errors.New("at most one plan step may be in_progress")
	}
	t.state.mu.Lock()
	t.state.plan = Plan{Explanation: input.Explanation, Steps: append([]Step(nil), input.Plan...), UpdatedAt: time.Now().UTC()}
	plan := t.state.plan
	t.state.mu.Unlock()
	data, _ := json.Marshal(plan)
	return tool.Result{Content: string(data)}, nil
}

type createGoalTool struct{ state *State }

func (*createGoalTool) Definition() provider.ToolDefinition {
	return definition("create_goal", "Create a long-term goal only when the user explicitly requests one. Fails while an unfinished goal exists.", `{"type":"object","properties":{"objective":{"type":"string"},"token_budget":{"type":"integer","minimum":1}},"required":["objective"],"additionalProperties":false}`)
}
func (*createGoalTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*createGoalTool) Summary(string) string { return "create long-term goal" }
func (t *createGoalTool) Execute(_ context.Context, args string) (tool.Result, error) {
	var input struct {
		Objective   string `json:"objective"`
		TokenBudget int    `json:"token_budget"`
	}
	if err := decode(args, &input); err != nil {
		return tool.Result{}, err
	}
	if strings.TrimSpace(input.Objective) == "" {
		return tool.Result{}, errors.New("goal objective is required")
	}
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	if t.state.goal != nil && t.state.goal.Status == "active" {
		return tool.Result{}, errors.New("an unfinished goal already exists")
	}
	now := time.Now().UTC()
	t.state.goal = &Goal{Objective: strings.TrimSpace(input.Objective), TokenBudget: input.TokenBudget, Status: "active", CreatedAt: now, UpdatedAt: now}
	value := goalView(t.state.goal)
	data, _ := json.Marshal(value)
	return tool.Result{Content: string(data)}, nil
}

type getGoalTool struct{ state *State }

func (*getGoalTool) Definition() provider.ToolDefinition {
	return definition("get_goal", "Get the active or most recent long-term goal and its status.", `{"type":"object","additionalProperties":false}`)
}
func (*getGoalTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*getGoalTool) Summary(string) string { return "get goal status" }
func (t *getGoalTool) Execute(_ context.Context, args string) (tool.Result, error) {
	var empty struct{}
	if err := decode(args, &empty); err != nil {
		return tool.Result{}, err
	}
	t.state.mu.RLock()
	defer t.state.mu.RUnlock()
	if t.state.goal == nil {
		return tool.Result{Content: "No goal exists."}, nil
	}
	data, _ := json.Marshal(goalView(t.state.goal))
	return tool.Result{Content: string(data)}, nil
}

type updateGoalTool struct{ state *State }

func (*updateGoalTool) Definition() provider.ToolDefinition {
	return definition("update_goal", "Mark the existing long-term goal complete or blocked. Use complete only when achieved and blocked only at a genuine impasse.", `{"type":"object","properties":{"status":{"type":"string","enum":["complete","blocked"]}},"required":["status"],"additionalProperties":false}`)
}
func (*updateGoalTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*updateGoalTool) Summary(string) string { return "update goal status" }
func (t *updateGoalTool) Execute(_ context.Context, args string) (tool.Result, error) {
	var input struct {
		Status string `json:"status"`
	}
	if err := decode(args, &input); err != nil {
		return tool.Result{}, err
	}
	if input.Status != "complete" && input.Status != "blocked" {
		return tool.Result{}, errors.New("goal status must be complete or blocked")
	}
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	if t.state.goal == nil {
		return tool.Result{}, errors.New("no goal exists")
	}
	if t.state.goal.Status != "active" {
		return tool.Result{}, errors.New("goal is already finished")
	}
	t.state.goal.Status = input.Status
	t.state.goal.UpdatedAt = time.Now().UTC()
	data, _ := json.Marshal(goalView(t.state.goal))
	return tool.Result{Content: string(data)}, nil
}

func definition(name, description, schema string) provider.ToolDefinition {
	return provider.ToolDefinition{Name: name, Description: description, Parameters: json.RawMessage(schema)}
}
func decode(args string, target any) error {
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func goalView(goal *Goal) map[string]any {
	data, _ := json.Marshal(goal)
	var value map[string]any
	_ = json.Unmarshal(data, &value)
	value["elapsed_seconds"] = max(0, time.Since(goal.CreatedAt).Seconds())
	if goal.TokenBudget > 0 {
		value["remaining_token_budget"] = max(0, goal.TokenBudget-int(goal.TotalTokens))
	}
	return value
}
