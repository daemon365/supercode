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
	nextSequence  uint64
	shuttingDown  bool
	transitioning bool
}
type task struct {
	name, status string
	role, model  string
	reasoning    string
	ctx          context.Context
	cancel       context.CancelFunc
	run          *agent.RunHandle
	history      []provider.Message
	output       string
	err          error
	done         chan struct{}
	sequence     uint64
	interrupted  bool
}

const (
	maxStoredTasks       = 128
	maxTaskOutputBytes   = 128 * 1024
	maxTaskHistoryTokens = 64 * 1024
	maxTaskHistoryBytes  = 1024 * 1024
	maxTaskImageBytes    = 128 * 1024
	maxSnapshotBytes     = 8 * 1024 * 1024
	maxSnapshotError     = 8 * 1024
)

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
	type candidate struct {
		sequence uint64
		value    persistedTask
		encoded  json.RawMessage
	}
	candidates := make([]candidate, 0, len(m.tasks))
	for _, item := range m.tasks {
		value := persistedTask{
			Name: item.name, Status: item.status, Role: item.role, Model: item.model,
			Reasoning: item.reasoning, History: append([]provider.Message(nil), item.history...), Output: item.output,
		}
		if item.err != nil {
			value.Error = boundTaskField(item.err.Error(), maxSnapshotError)
		}
		candidates = append(candidates, candidate{sequence: item.sequence, value: value})
	}
	m.mu.Unlock()

	// Preserve metadata for every task first, then spend the remaining budget
	// on the newest task histories and outputs. This makes persistence bounded
	// without hiding the existence/status of older tasks in normal operation.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].sequence > candidates[j].sequence })
	selected := make([]candidate, 0, len(candidates))
	total := 2 // JSON array brackets.
	for _, item := range candidates {
		metadata := item.value
		metadata.History = nil
		metadata.Output = ""
		encoded, err := json.Marshal(metadata)
		if err != nil {
			continue
		}
		extra := len(encoded)
		if len(selected) > 0 {
			extra++
		}
		if total+extra > maxSnapshotBytes {
			continue
		}
		item.encoded = encoded
		selected = append(selected, item)
		total += extra
	}
	for index := range selected {
		full, err := json.Marshal(selected[index].value)
		if err != nil {
			continue
		}
		if total-len(selected[index].encoded)+len(full) <= maxSnapshotBytes {
			total += len(full) - len(selected[index].encoded)
			selected[index].encoded = full
			continue
		}
		historyOnly := selected[index].value
		historyOnly.Output = ""
		if encoded, marshalErr := json.Marshal(historyOnly); marshalErr == nil && total-len(selected[index].encoded)+len(encoded) <= maxSnapshotBytes {
			total += len(encoded) - len(selected[index].encoded)
			selected[index].encoded = encoded
			continue
		}
		outputOnly := selected[index].value
		outputOnly.History = nil
		if encoded, marshalErr := json.Marshal(outputOnly); marshalErr == nil && total-len(selected[index].encoded)+len(encoded) <= maxSnapshotBytes {
			total += len(encoded) - len(selected[index].encoded)
			selected[index].encoded = encoded
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].value.Name < selected[j].value.Name })
	values := make([]json.RawMessage, len(selected))
	for index := range selected {
		values[index] = selected[index].encoded
	}
	encoded, _ := json.Marshal(values)
	return encoded
}

func (m *Manager) Restore(data json.RawMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.RestoreContext(ctx, data)
}

// RestoreContext cancels and joins tasks from the previous session before
// atomically replacing the persisted task set.
func (m *Manager) RestoreContext(ctx context.Context, data json.RawMessage) error {
	if m == nil {
		return nil
	}
	if len(data) == 0 {
		data = json.RawMessage(`[]`)
	}
	var values []persistedTask
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("restore sub-agents: %w", err)
	}
	if len(values) > maxStoredTasks {
		values = values[len(values)-maxStoredTasks:]
	}
	if err := m.beginTransition(ctx); err != nil {
		return fmt.Errorf("stop previous sub-agents: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transitioning = false
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
			name: value.Name, status: status,
			role: boundTaskField(value.Role, 4*1024), model: boundTaskField(value.Model, 512), reasoning: boundTaskField(value.Reasoning, 64),
			history: boundedTaskHistory(value.History), output: boundTaskOutput(value.Output), done: make(chan struct{}),
		}
		m.nextSequence++
		item.sequence = m.nextSequence
		if value.Error != "" {
			item.err = errors.New(boundTaskField(value.Error, 64*1024))
		}
		close(item.done)
		m.tasks[item.name] = item
	}
	return nil
}

// Quiesce cancels and joins active tasks while keeping their final state and
// allowing the manager to be reused. Session switches use it to take a durable
// snapshot before replacing the task set.
func (m *Manager) Quiesce(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if err := m.beginTransition(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.transitioning = false
	m.mu.Unlock()
	return nil
}

// Shutdown prevents new sub-agents, interrupts all active tasks, and joins
// their consumers without holding the manager lock while waiting.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.shuttingDown = true
	done := m.interruptRunningLocked()
	m.mu.Unlock()
	return waitTasks(ctx, done)
}

func (m *Manager) beginTransition(ctx context.Context) error {
	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return errors.New("sub-agent manager is shutting down")
	}
	m.transitioning = true
	done := m.interruptRunningLocked()
	m.mu.Unlock()
	if err := waitTasks(ctx, done); err != nil {
		m.mu.Lock()
		m.transitioning = false
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) interruptRunningLocked() []<-chan struct{} {
	done := make([]<-chan struct{}, 0, len(m.tasks))
	for _, item := range m.tasks {
		select {
		case <-item.done:
			continue
		default:
		}
		item.interrupted = true
		item.status = "interrupted"
		if item.cancel != nil {
			item.cancel()
		}
		done = append(done, item.done)
	}
	return done
}

func waitTasks(ctx context.Context, tasks []<-chan struct{}) error {
	for _, done := range tasks {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
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
	if m.shuttingDown || m.transitioning {
		m.mu.Unlock()
		return errors.New("sub-agent manager is unavailable during shutdown or session restore")
	}
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
	if existing := m.tasks[path]; existing == nil && len(m.tasks) >= maxStoredTasks && !m.pruneOldestLocked() {
		m.mu.Unlock()
		return errors.New("sub-agent task history limit reached")
	}
	subRunner, err := m.runner.ForkConfigured(model, agent.ApprovalNever, reasoning)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.WithValue(m.ctx, taskPathContextKey{}, path))
	m.nextSequence++
	item := &task{
		name: path, status: "running", role: boundTaskField(role, 4*1024), model: boundTaskField(model, 512), reasoning: boundTaskField(reasoning, 64),
		ctx: ctx, cancel: cancel, history: boundedTaskHistory(history), done: make(chan struct{}), sequence: m.nextSequence,
	}
	item.run = subRunner.Start(ctx, agent.Input{Prompt: prompt, History: item.history, Instructions: roleInstructions(item.role, item.reasoning)})
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
			item.output = appendTaskOutput(item.output, event.Delta)
		case agent.EventToolStarted:
			name := "unknown"
			if event.Call != nil {
				name = event.Call.Name
			}
			item.output = appendTaskOutput(item.output, "\n[tool] "+name+"\n")
		case agent.EventApprovalRequired:
			event.Approval.Decide(false)
		case agent.EventCompleted:
			item.history = boundedTaskHistory(event.History)
			if item.interrupted || item.ctx.Err() != nil {
				item.status = "interrupted"
			} else {
				item.status = "completed"
			}
		case agent.EventError:
			item.err = errors.New(boundTaskField(event.Err.Error(), 64*1024))
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
		if item.interrupted || item.ctx.Err() != nil {
			item.status = "interrupted"
		} else {
			item.status = "completed"
		}
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
	value := map[string]any{"name": item.name, "status": item.status, "role": item.role, "model": item.model, "reasoning_effort": item.reasoning, "output": item.output}
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
	item.interrupted = true
	item.cancel()
	t.m.mu.Unlock()
	return jsonResult(map[string]any{"target": input.Target, "interrupted": true}), nil
}

type listTool struct{ m *Manager }

func (*listTool) Definition() provider.ToolDefinition {
	return def("list_agents", "List all sub-agent tasks and statuses.", `{"type":"object","properties":{},"additionalProperties":false}`)
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

func (m *Manager) pruneOldestLocked() bool {
	var oldest *task
	for _, item := range m.tasks {
		if item.status == "running" || oldest != nil && oldest.sequence <= item.sequence {
			continue
		}
		oldest = item
	}
	if oldest == nil {
		return false
	}
	delete(m.tasks, oldest.name)
	return true
}

func boundedTaskHistory(history []provider.Message) []provider.Message {
	copyOfHistory := make([]provider.Message, len(history))
	for index, message := range history {
		copyOfHistory[index] = message
		copyOfHistory[index].Content = boundTaskField(message.Content, 256*1024)
		copyOfHistory[index].ToolCalls = append([]provider.ToolCall(nil), message.ToolCalls...)
		for callIndex := range copyOfHistory[index].ToolCalls {
			copyOfHistory[index].ToolCalls[callIndex].Arguments = boundTaskField(copyOfHistory[index].ToolCalls[callIndex].Arguments, 128*1024)
		}
		copyOfHistory[index].Images = make([]provider.Image, 0, len(message.Images))
		for _, image := range message.Images {
			if len(image.Data) > maxTaskImageBytes {
				copyOfHistory[index].Content = strings.TrimSpace(copyOfHistory[index].Content + "\n[large sub-agent image omitted from persisted history]")
				if image.Ref == "" {
					continue
				}
				image.Data = ""
			}
			copyOfHistory[index].Images = append(copyOfHistory[index].Images, image)
		}
	}
	if agent.EstimateMessagesTokens(copyOfHistory) <= maxTaskHistoryTokens {
		if encoded, _ := json.Marshal(copyOfHistory); len(encoded) <= maxTaskHistoryBytes {
			return copyOfHistory
		}
	}
	compacted, _ := agent.CompactHistory(copyOfHistory, maxTaskHistoryTokens)
	if encoded, _ := json.Marshal(compacted); len(encoded) <= maxTaskHistoryBytes && agent.EstimateMessagesTokens(compacted) <= maxTaskHistoryTokens {
		return compacted
	}
	budget := maxTaskHistoryBytes * 3 / 4
	tokenBudget := maxTaskHistoryTokens * 3 / 4
	start, used, usedTokens := len(compacted), 0, 0
	for index := len(compacted) - 1; index >= 0; index-- {
		encoded, _ := json.Marshal(compacted[index])
		messageTokens := agent.EstimateMessagesTokens(compacted[index : index+1])
		if used+len(encoded) > budget || usedTokens+messageTokens > tokenBudget {
			break
		}
		used += len(encoded)
		usedTokens += messageTokens
		if compacted[index].Role == provider.MessageRoleUser && compacted[index].ToolCallID == "" {
			start = index
		}
	}
	marker := provider.Message{Role: provider.MessageRoleUser, Content: "[Earlier sub-agent history omitted to keep persisted state bounded.]"}
	if start >= len(compacted) {
		return []provider.Message{marker}
	}
	result := make([]provider.Message, 0, 1+len(compacted)-start)
	result = append(result, marker)
	return append(result, compacted[start:]...)
}

func appendTaskOutput(current, addition string) string {
	if len(current)+len(addition) <= maxTaskOutputBytes {
		return current + addition
	}
	if len(addition) >= maxTaskOutputBytes {
		return boundTaskOutput(addition)
	}
	const marker = "[earlier sub-agent output truncated]\n"
	keep := maxTaskOutputBytes - len(marker) - len(addition)
	start := len(current) - keep
	if start < 0 {
		start = 0
	}
	for start < len(current) && current[start]&0xc0 == 0x80 {
		start++
	}
	return marker + current[start:] + addition
}

func boundTaskOutput(value string) string {
	const marker = "[earlier sub-agent output truncated]\n"
	if len(value) <= maxTaskOutputBytes {
		return value
	}
	start := len(value) - (maxTaskOutputBytes - len(marker))
	for start < len(value) && value[start]&0xc0 == 0x80 {
		start++
	}
	return marker + value[start:]
}

func boundTaskField(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	const marker = "\n[field truncated]\n"
	head := (limit - len(marker)) * 3 / 4
	tail := limit - len(marker) - head
	for head > 0 && value[head]&0xc0 == 0x80 {
		head--
	}
	start := len(value) - tail
	for start < len(value) && value[start]&0xc0 == 0x80 {
		start++
	}
	return value[:head] + marker + value[start:]
}
