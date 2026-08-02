// Package collaboration exposes a bounded set of sub-agent collaboration tools.
package collaboration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

type Manager struct {
	ctx           context.Context
	runner        *agent.Runner
	mu            sync.Mutex
	tasks         map[string]*task
	maxConcurrent int
	maxDepth      int
}
type task struct {
	name, status string
	role, model  string
	reasoning    string
	cancel       context.CancelFunc
	run          *agent.RunHandle
	history      []provider.Message
	output       strings.Builder
	err          error
	done         chan struct{}
}

type persistedTask struct {
	Name      string             `json:"name"`
	Status    string             `json:"status"`
	Role      string             `json:"role,omitempty"`
	Model     string             `json:"model,omitempty"`
	Reasoning string             `json:"reasoning_effort,omitempty"`
	History   []provider.Message `json:"history,omitempty"`
	Output    string             `json:"output,omitempty"`
	Error     string             `json:"error,omitempty"`
}

func New(ctx context.Context, runner *agent.Runner) *Manager {
	return &Manager{ctx: ctx, runner: runner, tasks: make(map[string]*task), maxConcurrent: 8, maxDepth: 3}
}

type taskPathContextKey struct{}

func roleInstructions(role, reasoning string) string {
	var values []string
	if strings.TrimSpace(role) != "" {
		values = append(values, "Sub-agent role: "+strings.TrimSpace(role)+". Stay within that bounded responsibility.")
	}
	if strings.TrimSpace(reasoning) != "" {
		values = append(values, "Requested reasoning effort: "+strings.TrimSpace(reasoning)+".")
	}
	if strings.EqualFold(strings.TrimSpace(role), "awaiter") {
		values = append(values, prompts.AwaiterInstructions())
	}
	return strings.Join(values, "\n")
}

func (m *Manager) Snapshot() json.RawMessage {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	values := make([]persistedTask, 0, len(m.tasks))
	for _, item := range m.tasks {
		value := persistedTask{
			Name: item.name, Status: item.status, Role: item.role, Model: item.model,
			Reasoning: item.reasoning, History: append([]provider.Message(nil), item.history...), Output: item.output.String(),
		}
		if item.err != nil {
			value.Error = item.err.Error()
		}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	encoded, _ := json.Marshal(values)
	return encoded
}

func (m *Manager) Restore(data json.RawMessage) error {
	if m == nil || len(data) == 0 {
		return nil
	}
	var values []persistedTask
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("restore sub-agents: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.tasks {
		if existing.status == "running" {
			existing.cancel()
		}
	}
	m.tasks = make(map[string]*task, len(values))
	for _, value := range values {
		if !validPath(value.Name) {
			continue
		}
		status := value.Status
		if status == "running" {
			status = "interrupted"
		}
		item := &task{
			name: value.Name, status: status, role: value.Role, model: value.Model, reasoning: value.Reasoning,
			history: append([]provider.Message(nil), value.History...), done: make(chan struct{}),
		}
		item.output.WriteString(value.Output)
		if value.Error != "" {
			item.err = errors.New(value.Error)
		}
		close(item.done)
		m.tasks[item.name] = item
	}
	return nil
}
func (m *Manager) Tools() []tool.Tool {
	return []tool.Tool{&spawnTool{m}, &messageTool{m, false}, &messageTool{m, true}, &interruptTool{m}, &listTool{m}, &waitTool{m}}
}

func (m *Manager) start(parent, name, prompt, role, model, reasoning string, history []provider.Message) error {
	path := name
	if parent != "" {
		path = strings.TrimSuffix(parent, "/") + "/" + name
	}
	if strings.Count(path, "/")+1 > m.maxDepth {
		return fmt.Errorf("sub-agent depth limit of %d reached", m.maxDepth)
	}
	m.mu.Lock()
	if existing := m.tasks[path]; existing != nil && existing.status == "running" {
		m.mu.Unlock()
		return errors.New("agent is already running")
	}
	running := 0
	for _, candidate := range m.tasks {
		if candidate.status == "running" {
			running++
		}
	}
	if running >= m.maxConcurrent {
		m.mu.Unlock()
		return errors.New("sub-agent concurrency limit reached")
	}
	subRunner, err := m.runner.Fork(model, agent.ApprovalNever)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.WithValue(m.ctx, taskPathContextKey{}, path))
	item := &task{name: path, status: "running", role: role, model: model, reasoning: reasoning, cancel: cancel, history: append([]provider.Message(nil), history...), done: make(chan struct{})}
	item.run = subRunner.Start(ctx, agent.Input{Prompt: prompt, History: item.history, Instructions: roleInstructions(role, reasoning)})
	m.tasks[path] = item
	m.mu.Unlock()
	_ = subRunner.NotifyHook(ctx, agent.HookSubagentStart, agent.HookInput{Prompt: path})
	go m.consume(subRunner, item)
	return nil
}
func (m *Manager) consume(subRunner *agent.Runner, item *task) {
	for event := range item.run.Events {
		m.mu.Lock()
		switch event.Type {
		case agent.EventTextDelta:
			item.output.WriteString(event.Delta)
		case agent.EventToolStarted:
			item.output.WriteString("\n[tool] " + event.Summary + "\n")
		case agent.EventApprovalRequired:
			event.Approval.Decide(false)
		case agent.EventCompleted:
			item.history = event.History
			item.status = "completed"
		case agent.EventError:
			item.err = event.Err
			if errors.Is(event.Err, context.Canceled) {
				item.status = "interrupted"
			} else {
				item.status = "error"
			}
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	if item.status == "running" {
		item.status = "completed"
	}
	close(item.done)
	m.mu.Unlock()
	_ = subRunner.NotifyHook(context.Background(), agent.HookSubagentStop, agent.HookInput{Prompt: item.name})
}
func (m *Manager) snapshot(name string) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.tasks[name]
	if item == nil {
		return nil, errors.New("agent not found")
	}
	value := map[string]any{"name": item.name, "status": item.status, "role": item.role, "model": item.model, "reasoning_effort": item.reasoning, "output": item.output.String()}
	if item.err != nil {
		value["error"] = item.err.Error()
	}
	return value, nil
}

type spawnTool struct{ m *Manager }

func (*spawnTool) Definition() provider.ToolDefinition {
	return def("spawn_agent", "Start a bounded sub-agent task, up to three levels deep, with an optional role, compatible model, and reasoning-effort hint. Workspace file edits remain sandboxed; commands and network calls cannot self-approve.", `{"type":"object","properties":{"task_name":{"type":"string"},"message":{"type":"string"},"role":{"type":"string"},"model":{"type":"string"},"reasoning_effort":{"type":"string","enum":["low","medium","high","xhigh"]}},"required":["task_name","message"],"additionalProperties":false}`)
}
func (*spawnTool) Risk(string) tool.Risk      { return tool.RiskRead }
func (*spawnTool) Summary(args string) string { return summary("spawn sub-agent", args) }
func (t *spawnTool) Execute(ctx context.Context, args string) (tool.Result, error) {
	var input struct {
		TaskName        string `json:"task_name"`
		Message         string `json:"message"`
		Role            string `json:"role"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := decode(args, &input); err != nil {
		return tool.Result{}, err
	}
	if !validName(input.TaskName) {
		return tool.Result{}, errors.New("task_name must contain only letters, digits, underscores, or dashes")
	}
	if strings.TrimSpace(input.Message) == "" {
		return tool.Result{}, errors.New("message is required")
	}
	parent, _ := ctx.Value(taskPathContextKey{}).(string)
	if err := t.m.start(parent, input.TaskName, input.Message, input.Role, input.Model, input.ReasoningEffort, nil); err != nil {
		return tool.Result{}, err
	}
	path := input.TaskName
	if parent != "" {
		path = strings.TrimSuffix(parent, "/") + "/" + input.TaskName
	}
	return jsonResult(map[string]any{"task_name": path, "status": "running", "role": input.Role, "model": input.Model, "reasoning_effort": input.ReasoningEffort}), nil
}

type messageTool struct {
	m      *Manager
	follow bool
}

func (t *messageTool) Definition() provider.ToolDefinition {
	if t.follow {
		return def("followup_task", "Queue a follow-up for a running sub-agent, or start another turn with its completed history.", `{"type":"object","properties":{"target":{"type":"string"},"message":{"type":"string"}},"required":["target","message"],"additionalProperties":false}`)
	}
	return def("send_message", "Send steering input to a running sub-agent.", `{"type":"object","properties":{"target":{"type":"string"},"message":{"type":"string"}},"required":["target","message"],"additionalProperties":false}`)
}
func (t *messageTool) Risk(string) tool.Risk {
	return tool.RiskRead
}
func (t *messageTool) Summary(args string) string {
	if t.follow {
		return summary("follow up with sub-agent", args)
	}
	return summary("message sub-agent", args)
}
func (t *messageTool) Execute(_ context.Context, args string) (tool.Result, error) {
	var input struct{ Target, Message string }
	var raw struct {
		Target  string `json:"target"`
		Message string `json:"message"`
	}
	if err := decode(args, &raw); err != nil {
		return tool.Result{}, err
	}
	input.Target, input.Message = raw.Target, raw.Message
	t.m.mu.Lock()
	item := t.m.tasks[input.Target]
	if item == nil {
		t.m.mu.Unlock()
		return tool.Result{}, errors.New("agent not found")
	}
	if item.status == "running" {
		ok := item.run.Queue(input.Message)
		t.m.mu.Unlock()
		if !ok {
			return tool.Result{}, errors.New("agent message queue is full")
		}
		return jsonResult(map[string]any{"target": input.Target, "queued": true}), nil
	}
	history := append([]provider.Message(nil), item.history...)
	t.m.mu.Unlock()
	if !t.follow {
		return tool.Result{}, errors.New("agent is not running; use followup_task")
	}
	if err := t.m.start("", input.Target, input.Message, item.role, item.model, item.reasoning, history); err != nil {
		return tool.Result{}, err
	}
	return jsonResult(map[string]any{"target": input.Target, "status": "running"}), nil
}

type interruptTool struct{ m *Manager }

func (*interruptTool) Definition() provider.ToolDefinition {
	return def("interrupt_agent", "Interrupt a running sub-agent.", `{"type":"object","properties":{"target":{"type":"string"}},"required":["target"],"additionalProperties":false}`)
}
func (*interruptTool) Risk(string) tool.Risk      { return tool.RiskRead }
func (*interruptTool) Summary(args string) string { return summary("interrupt sub-agent", args) }
func (t *interruptTool) Execute(_ context.Context, args string) (tool.Result, error) {
	var input struct {
		Target string `json:"target"`
	}
	if err := decode(args, &input); err != nil {
		return tool.Result{}, err
	}
	t.m.mu.Lock()
	item := t.m.tasks[input.Target]
	if item == nil {
		t.m.mu.Unlock()
		return tool.Result{}, errors.New("agent not found")
	}
	if item.status != "running" {
		t.m.mu.Unlock()
		return tool.Result{}, errors.New("agent is not running")
	}
	item.cancel()
	t.m.mu.Unlock()
	return jsonResult(map[string]any{"target": input.Target, "interrupted": true}), nil
}

type listTool struct{ m *Manager }

func (*listTool) Definition() provider.ToolDefinition {
	return def("list_agents", "List all sub-agent tasks and statuses.", `{"type":"object","additionalProperties":false}`)
}
func (*listTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*listTool) Summary(string) string { return "list sub-agents" }
func (t *listTool) Execute(_ context.Context, args string) (tool.Result, error) {
	var empty struct{}
	if err := decode(args, &empty); err != nil {
		return tool.Result{}, err
	}
	t.m.mu.Lock()
	names := make([]string, 0, len(t.m.tasks))
	for name := range t.m.tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]map[string]any, 0, len(names))
	for _, name := range names {
		item := t.m.tasks[name]
		values = append(values, map[string]any{"name": name, "status": item.status, "role": item.role, "model": item.model, "reasoning_effort": item.reasoning})
	}
	t.m.mu.Unlock()
	return jsonResult(values), nil
}

type waitTool struct{ m *Manager }

func (*waitTool) Definition() provider.ToolDefinition {
	return def("wait_agent", "Wait for a sub-agent update or completion and return its current output.", `{"type":"object","properties":{"target":{"type":"string"},"timeout_ms":{"type":"integer","minimum":100,"maximum":60000}},"required":["target"],"additionalProperties":false}`)
}
func (*waitTool) Risk(string) tool.Risk      { return tool.RiskRead }
func (*waitTool) Summary(args string) string { return summary("wait for sub-agent", args) }
func (t *waitTool) Execute(ctx context.Context, args string) (tool.Result, error) {
	var input struct {
		Target  string `json:"target"`
		Timeout int    `json:"timeout_ms"`
	}
	if err := decode(args, &input); err != nil {
		return tool.Result{}, err
	}
	if input.Timeout <= 0 {
		input.Timeout = 30000
	}
	if input.Timeout > 60000 {
		input.Timeout = 60000
	}
	t.m.mu.Lock()
	item := t.m.tasks[input.Target]
	if item == nil {
		t.m.mu.Unlock()
		return tool.Result{}, errors.New("agent not found")
	}
	done := item.done
	t.m.mu.Unlock()
	timer := time.NewTimer(time.Duration(input.Timeout) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
		return tool.Result{}, ctx.Err()
	}
	value, err := t.m.snapshot(input.Target)
	if err != nil {
		return tool.Result{}, err
	}
	return jsonResult(value), nil
}

func def(name, description, schema string) provider.ToolDefinition {
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
func summary(action, args string) string {
	compact := strings.Join(strings.Fields(args), " ")
	runes := []rune(compact)
	if len(runes) > 200 {
		compact = string(runes[:200]) + "..."
	}
	return action + ": " + compact
}
func jsonResult(value any) tool.Result {
	data, _ := json.MarshalIndent(value, "", "  ")
	return tool.Result{Content: string(data)}
}
func validName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validPath(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) == 0 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if !validName(part) {
			return false
		}
	}
	return true
}
