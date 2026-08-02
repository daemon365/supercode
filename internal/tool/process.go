package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/daemon365/supercode/internal/provider"
)

const maxProcessBuffer = 1024 * 1024

type processManager struct {
	workspace workspace
	sandbox   commandSandbox
	nextID    atomic.Int64
	mu        sync.Mutex
	processes map[int64]*managedProcess
}

type managedProcess struct {
	id      int64
	command *exec.Cmd
	input   io.WriteCloser
	output  processBuffer
	done    chan struct{}
	started time.Time
	mu      sync.Mutex
	err     error
	exit    *int
}

type processBuffer struct {
	mu        sync.Mutex
	data      []byte
	cursor    int
	truncated bool
	nextSub   uint64
	subs      map[uint64]func(string)
}

func (b *processBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	original := len(value)
	remaining := maxProcessBuffer - len(b.data)
	if remaining <= 0 {
		b.truncated = true
		b.mu.Unlock()
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, value...)
	delta := string(append([]byte(nil), value...))
	subscribers := make([]func(string), 0, len(b.subs))
	for _, subscriber := range b.subs {
		subscribers = append(subscribers, subscriber)
	}
	b.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber(delta)
	}
	return original, nil
}

func (b *processBuffer) subscribe(subscriber func(string)) func() {
	if subscriber == nil {
		return func() {}
	}
	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[uint64]func(string))
	}
	b.nextSub++
	id := b.nextSub
	b.subs[id] = subscriber
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}
func (b *processBuffer) take(maximum int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if maximum <= 0 || maximum > maxToolOutput {
		maximum = maxToolOutput
	}
	start, end := b.cursor, len(b.data)
	b.cursor = end
	prefix := ""
	if end-start > maximum {
		start = end - maximum
		prefix = "[earlier output truncated]\n"
	}
	value := prefix + string(b.data[start:end])
	if b.truncated && end == len(b.data) {
		value += "\n[process output limit reached]"
	}
	return value
}

func newProcessManager(workspace workspace, sandbox commandSandbox) *processManager {
	manager := &processManager{workspace: workspace, sandbox: sandbox, processes: make(map[int64]*managedProcess)}
	manager.nextID.Store(1000)
	return manager
}

func (m *processManager) tools() []Tool {
	return []Tool{
		&execCommandTool{manager: m}, &writeStdinTool{manager: m}, &waitTool{manager: m},
		&listProcessesTool{manager: m}, &stopProcessTool{manager: m},
	}
}

type execCommandTool struct{ manager *processManager }

func (*execCommandTool) Category() Category { return CategoryShell }

func (*execCommandTool) Definition() provider.ToolDefinition {
	return definition("exec_command", "Run a shell command, returning output immediately or a live session_id. Read-only commands run automatically in a read-only sandbox. Other commands require approval and run in the workspace-write sandbox. Use require-escalated only when necessary.", `{"type":"object","properties":{"cmd":{"type":"string"},"workdir":{"type":"string"},"shell":{"type":"string"},"login":{"type":"boolean"},"tty":{"type":"boolean"},"yield_time_ms":{"type":"integer","minimum":250,"maximum":30000},"max_output_tokens":{"type":"integer","minimum":1,"maximum":50000},"sandbox_permissions":{"type":"string","enum":["workspace-write","require-escalated"]},"justification":{"type":"string"}},"required":["cmd"],"additionalProperties":false}`)
}
func (t *execCommandTool) Risk(arguments string) Risk {
	command, escalated := sandboxRequest(arguments, "cmd")
	return t.manager.sandbox.riskFor(command, escalated)
}
func (*execCommandTool) Summary(arguments string) string {
	return argumentSummary("execute command", arguments)
}
func (t *execCommandTool) Execute(ctx context.Context, arguments string) (Result, error) {
	var input struct {
		Cmd           string `json:"cmd"`
		Workdir       string `json:"workdir"`
		Shell         string `json:"shell"`
		Login         bool   `json:"login"`
		TTY           bool   `json:"tty"`
		YieldMS       int    `json:"yield_time_ms"`
		MaxTokens     int    `json:"max_output_tokens"`
		Permissions   string `json:"sandbox_permissions"`
		Justification string `json:"justification"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(input.Cmd) == "" {
		return Result{}, errors.New("cmd is required")
	}
	if input.Permissions == "require-escalated" && strings.TrimSpace(input.Justification) == "" {
		return Result{}, errors.New("justification is required for require-escalated")
	}
	if input.YieldMS <= 0 {
		input.YieldMS = 1000
	}
	if input.YieldMS < 250 {
		input.YieldMS = 250
	}
	if input.YieldMS > 30000 {
		input.YieldMS = 30000
	}
	workdir := t.manager.workspace.root
	if input.Workdir != "" {
		resolved, err := t.manager.workspace.resolve(input.Workdir, false)
		if err != nil {
			return Result{}, err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return Result{}, err
		}
		if !info.IsDir() {
			return Result{}, errors.New("workdir is not a directory")
		}
		workdir = resolved
	}
	shell := strings.TrimSpace(input.Shell)
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "powershell.exe"
		} else {
			shell = "/bin/sh"
		}
	}
	args := []string{"-lc", input.Cmd}
	if runtime.GOOS == "windows" {
		args = []string{"-NoProfile", "-Command", input.Cmd}
	}
	escalated := input.Permissions == "require-escalated"
	readOnly := readOnlyShellCommand(input.Cmd) && !escalated
	command := t.manager.sandbox.command(ctx, shell, args, workdir, !readOnly, escalated)
	process := &managedProcess{id: t.manager.nextID.Add(1), command: command, done: make(chan struct{}), started: time.Now()}
	if input.TTY && runtime.GOOS != "windows" {
		terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 40, Cols: 120})
		if err != nil {
			return Result{}, fmt.Errorf("start PTY: %w", err)
		}
		process.input = terminal
		go func() { _, _ = io.Copy(&process.output, terminal); _ = terminal.Close() }()
	} else {
		stdin, err := command.StdinPipe()
		if err != nil {
			return Result{}, err
		}
		process.input = stdin
		command.Stdout = &process.output
		command.Stderr = &process.output
		if err := command.Start(); err != nil {
			return Result{}, err
		}
	}
	t.manager.mu.Lock()
	t.manager.processes[process.id] = process
	t.manager.mu.Unlock()
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		code := 0
		if err != nil {
			code = -1
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			}
		}
		process.exit = &code
		process.mu.Unlock()
		close(process.done)
	}()
	return t.manager.observe(ctx, process, time.Duration(input.YieldMS)*time.Millisecond, input.MaxTokens, false)
}

type writeStdinTool struct{ manager *processManager }

func (*writeStdinTool) Category() Category { return CategoryShell }

func (*writeStdinTool) Definition() provider.ToolDefinition {
	return definition("write_stdin", "Write characters to an existing exec_command session, or poll it with an empty chars value, and return new output.", `{"type":"object","properties":{"session_id":{"type":"integer"},"chars":{"type":"string"},"yield_time_ms":{"type":"integer","minimum":0,"maximum":300000},"max_output_tokens":{"type":"integer","minimum":1,"maximum":50000}},"required":["session_id"],"additionalProperties":false}`)
}
func (*writeStdinTool) Risk(_ string) Risk {
	// Transport for an exec_command that already passed approval. It does not
	// start a new process.
	return RiskRead
}
func (*writeStdinTool) Summary(arguments string) string {
	return argumentSummary("write to process", arguments)
}
func (t *writeStdinTool) Execute(ctx context.Context, arguments string) (Result, error) {
	var input struct {
		SessionID int64  `json:"session_id"`
		Chars     string `json:"chars"`
		YieldMS   int    `json:"yield_time_ms"`
		MaxTokens int    `json:"max_output_tokens"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	process, err := t.manager.get(input.SessionID)
	if err != nil {
		return Result{}, err
	}
	if input.Chars != "" {
		if _, err := io.WriteString(process.input, input.Chars); err != nil {
			return Result{}, fmt.Errorf("write stdin: %w", err)
		}
	}
	if input.YieldMS <= 0 {
		if input.Chars == "" {
			input.YieldMS = 5000
		} else {
			input.YieldMS = 250
		}
	}
	if input.YieldMS > 300000 {
		input.YieldMS = 300000
	}
	return t.manager.observe(ctx, process, time.Duration(input.YieldMS)*time.Millisecond, input.MaxTokens, false)
}

type waitTool struct{ manager *processManager }

func (*waitTool) Category() Category { return CategoryShell }

func (*waitTool) Definition() provider.ToolDefinition {
	return definition("wait", "Wait for more output from an exec_command session, optionally terminating it.", `{"type":"object","properties":{"session_id":{"type":"integer"},"yield_time_ms":{"type":"integer","minimum":1,"maximum":300000},"max_output_tokens":{"type":"integer","minimum":1,"maximum":50000},"terminate":{"type":"boolean"}},"required":["session_id"],"additionalProperties":false}`)
}
func (*waitTool) Risk(arguments string) Risk {
	var value struct {
		Terminate bool `json:"terminate"`
	}
	_ = json.Unmarshal([]byte(arguments), &value)
	if value.Terminate {
		return RiskExecute
	}
	return RiskRead
}
func (*waitTool) Summary(arguments string) string {
	return argumentSummary("wait for process", arguments)
}
func (t *waitTool) Execute(ctx context.Context, arguments string) (Result, error) {
	var input struct {
		SessionID          int64 `json:"session_id"`
		YieldMS, MaxTokens int
		Terminate          bool `json:"terminate"`
	}
	var raw struct {
		SessionID int64 `json:"session_id"`
		YieldMS   int   `json:"yield_time_ms"`
		MaxTokens int   `json:"max_output_tokens"`
		Terminate bool  `json:"terminate"`
	}
	if err := decodeArguments(arguments, &raw); err != nil {
		return Result{}, err
	}
	input.SessionID, input.YieldMS, input.MaxTokens, input.Terminate = raw.SessionID, raw.YieldMS, raw.MaxTokens, raw.Terminate
	process, err := t.manager.get(input.SessionID)
	if err != nil {
		return Result{}, err
	}
	if input.Terminate && process.command.Process != nil {
		_ = process.command.Process.Kill()
	}
	if input.YieldMS <= 0 {
		input.YieldMS = 10000
	}
	if input.YieldMS > 300000 {
		input.YieldMS = 300000
	}
	return t.manager.observe(ctx, process, time.Duration(input.YieldMS)*time.Millisecond, input.MaxTokens, input.Terminate)
}

func (m *processManager) get(id int64) (*managedProcess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.processes[id]
	if process == nil {
		return nil, fmt.Errorf("exec session %d not found", id)
	}
	return process, nil
}
func (m *processManager) observe(ctx context.Context, p *managedProcess, yield time.Duration, maxTokens int, terminate bool) (Result, error) {
	unsubscribe := p.output.subscribe(func(delta string) {
		ReportProgress(ctx, Progress{Delta: delta, SessionID: p.id})
	})
	defer unsubscribe()
	timer := time.NewTimer(yield)
	defer timer.Stop()
	completed := false
	select {
	case <-p.done:
		completed = true
	case <-timer.C:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	maximum := maxTokens * 4
	if maxTokens <= 0 {
		maximum = maxToolOutput
	}
	output := p.output.take(maximum)
	response := map[string]any{"session_id": p.id, "output": output, "wall_time_seconds": time.Since(p.started).Seconds(), "running": !completed}
	if completed {
		p.mu.Lock()
		response["exit_code"] = p.exit
		if p.err != nil {
			response["error"] = p.err.Error()
		}
		p.mu.Unlock()
		m.mu.Lock()
		delete(m.processes, p.id)
		m.mu.Unlock()
	} else if terminate {
		response["terminating"] = true
	}
	encoded, _ := json.MarshalIndent(response, "", "  ")
	return Result{Content: string(encoded), IsError: completed && p.err != nil}, nil
}

type listProcessesTool struct{ manager *processManager }

func (*listProcessesTool) Category() Category { return CategoryShell }

func (*listProcessesTool) Definition() provider.ToolDefinition {
	return definition("list_processes", "List background exec_command sessions and their current status.", `{"type":"object","additionalProperties":false}`)
}
func (*listProcessesTool) Risk(string) Risk      { return RiskRead }
func (*listProcessesTool) Summary(string) string { return "list background processes" }
func (t *listProcessesTool) Execute(_ context.Context, arguments string) (Result, error) {
	var empty struct{}
	if err := decodeArguments(arguments, &empty); err != nil {
		return Result{}, err
	}
	t.manager.mu.Lock()
	values := make([]map[string]any, 0, len(t.manager.processes))
	for _, process := range t.manager.processes {
		process.mu.Lock()
		value := map[string]any{
			"session_id": process.id, "command": strings.Join(process.command.Args, " "),
			"started_at": process.started.Format(time.RFC3339), "running": process.exit == nil,
		}
		if process.exit != nil {
			value["exit_code"] = *process.exit
		}
		process.mu.Unlock()
		values = append(values, value)
	}
	t.manager.mu.Unlock()
	sort.Slice(values, func(i, j int) bool { return values[i]["session_id"].(int64) < values[j]["session_id"].(int64) })
	encoded, _ := json.MarshalIndent(map[string]any{"processes": values}, "", "  ")
	return Result{Content: string(encoded)}, nil
}

type stopProcessTool struct{ manager *processManager }

func (*stopProcessTool) Category() Category { return CategoryShell }

func (*stopProcessTool) Definition() provider.ToolDefinition {
	return definition("stop_process", "Stop one background exec_command session, or all sessions.", `{"type":"object","properties":{"session_id":{"type":"integer"},"all":{"type":"boolean"}},"additionalProperties":false}`)
}
func (*stopProcessTool) Risk(string) Risk { return RiskExecute }
func (*stopProcessTool) Summary(arguments string) string {
	return argumentSummary("stop background process", arguments)
}
func (t *stopProcessTool) Execute(_ context.Context, arguments string) (Result, error) {
	var input struct {
		SessionID int64 `json:"session_id"`
		All       bool  `json:"all"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	if input.SessionID == 0 && !input.All {
		return Result{}, errors.New("session_id is required unless all is true")
	}
	t.manager.mu.Lock()
	var targets []*managedProcess
	for id, process := range t.manager.processes {
		if input.All || id == input.SessionID {
			targets = append(targets, process)
		}
	}
	t.manager.mu.Unlock()
	if len(targets) == 0 {
		return Result{}, errors.New("no matching background process")
	}
	stopped := make([]int64, 0, len(targets))
	for _, process := range targets {
		if process.command.Process != nil {
			if err := process.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return Result{}, fmt.Errorf("stop process %d: %w", process.id, err)
			}
			stopped = append(stopped, process.id)
		}
	}
	encoded, _ := json.MarshalIndent(map[string]any{"stopped": stopped}, "", "  ")
	return Result{Content: string(encoded)}, nil
}
