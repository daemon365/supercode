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

const (
	maxProcessBuffer    = 1024 * 1024
	maxManagedProcesses = 64
	maxProcessInput     = 64 * 1024
	processRetention    = 10 * time.Minute
	processCloseTimeout = 5 * time.Second
	processWriteTimeout = 5 * time.Second
	processWaitDelay    = 2 * time.Second
)

type processManager struct {
	workspace workspace
	sandbox   commandSandbox
	nextID    atomic.Int64
	mu        sync.Mutex
	processes map[int64]*managedProcess
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

type managedProcess struct {
	id      int64
	command *exec.Cmd
	input   io.WriteCloser
	output  processBuffer
	done    chan struct{}
	// outputDone is closed after the PTY reader has drained all bytes. Process
	// completion is not published before this channel closes.
	outputDone chan struct{}
	started    time.Time
	mu         sync.Mutex
	inputMu    sync.Mutex
	err        error
	exit       *int
	endedAt    time.Time
	expiry     *time.Timer
}

type processBuffer struct {
	mu           sync.Mutex
	data         []byte
	head         int
	cursor       int64
	total        int64
	nextSub      uint64
	subs         map[uint64]*processSubscriber
	deliveryMu   sync.Mutex
	deliveryCond *sync.Cond
	deliveryNext uint64
	deliveryDone uint64
}

type processSubscriber struct {
	mu       sync.Mutex
	changed  *sync.Cond
	active   bool
	inFlight int
	callback func(string)
}

func newProcessSubscriber(callback func(string)) *processSubscriber {
	result := &processSubscriber{active: true, callback: callback}
	result.changed = sync.NewCond(&result.mu)
	return result
}

func (s *processSubscriber) deliver(delta string) {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.inFlight++
	s.mu.Unlock()

	s.callback(delta)

	s.mu.Lock()
	s.inFlight--
	if s.inFlight == 0 {
		s.changed.Broadcast()
	}
	s.mu.Unlock()
}

func (s *processSubscriber) stop() {
	s.mu.Lock()
	s.active = false
	for s.inFlight > 0 {
		s.changed.Wait()
	}
	s.mu.Unlock()
}

func (b *processBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	original := len(value)
	b.appendLocked(value)
	b.total += int64(original)
	subscribers := make([]*processSubscriber, 0, len(b.subs))
	for _, subscriber := range b.subs {
		subscribers = append(subscribers, subscriber)
	}
	sequence := uint64(0)
	if len(subscribers) > 0 && len(value) > 0 {
		b.deliveryNext++
		sequence = b.deliveryNext
		if b.deliveryCond == nil {
			b.deliveryCond = sync.NewCond(&b.deliveryMu)
		}
	}
	b.mu.Unlock()
	if len(subscribers) == 0 || len(value) == 0 {
		return original, nil
	}
	delta := string(value)
	b.deliveryMu.Lock()
	for sequence != b.deliveryDone+1 {
		b.deliveryCond.Wait()
	}
	for _, subscriber := range subscribers {
		subscriber.deliver(delta)
	}
	b.deliveryDone = sequence
	b.deliveryCond.Broadcast()
	b.deliveryMu.Unlock()
	return original, nil
}

func (b *processBuffer) appendLocked(value []byte) {
	if len(value) == 0 {
		return
	}
	if len(value) >= maxProcessBuffer {
		if cap(b.data) < maxProcessBuffer {
			b.data = make([]byte, maxProcessBuffer)
		} else {
			b.data = b.data[:maxProcessBuffer]
		}
		copy(b.data, value[len(value)-maxProcessBuffer:])
		b.head = 0
		return
	}
	if len(b.data) < maxProcessBuffer {
		remaining := maxProcessBuffer - len(b.data)
		if len(value) <= remaining {
			b.data = append(b.data, value...)
			return
		}
		b.data = append(b.data, value[:remaining]...)
		value = value[remaining:]
	}
	first := min(len(value), maxProcessBuffer-b.head)
	copy(b.data[b.head:b.head+first], value[:first])
	copy(b.data, value[first:])
	b.head = (b.head + len(value)) % maxProcessBuffer
}

func (b *processBuffer) subscribe(subscriber func(string)) func() {
	if subscriber == nil {
		return func() {}
	}
	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[uint64]*processSubscriber)
	}
	b.nextSub++
	id := b.nextSub
	registered := newProcessSubscriber(subscriber)
	b.subs[id] = registered
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
		registered.stop()
	}
}
func (b *processBuffer) take(maximum int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if maximum <= 0 || maximum > maxToolOutput {
		maximum = maxToolOutput
	}
	base := b.total - int64(len(b.data))
	start := b.cursor
	missed := int64(0)
	if start < base {
		missed = base - start
		start = base
	}
	end := b.total
	b.cursor = b.total
	prefix := ""
	if end-start > int64(maximum) {
		missed += end - start - int64(maximum)
		start = end - int64(maximum)
	}
	if missed > 0 {
		prefix = fmt.Sprintf("[earlier output truncated; %d bytes discarded]\n", missed)
	}
	return prefix + string(b.bytesLocked(start-base, end-base))
}

func (b *processBuffer) bytesLocked(start, end int64) []byte {
	if end <= start || len(b.data) == 0 {
		return nil
	}
	length := int(end - start)
	result := make([]byte, length)
	physical := (b.head + int(start)) % len(b.data)
	first := min(length, len(b.data)-physical)
	copy(result, b.data[physical:physical+first])
	copy(result[first:], b.data[:length-first])
	return result
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
	configureProcessTree(command, input.TTY && runtime.GOOS != "windows")
	command.WaitDelay = processWaitDelay
	process := &managedProcess{id: t.manager.nextID.Add(1), command: command, done: make(chan struct{}), outputDone: make(chan struct{}), started: time.Now()}
	if input.TTY && runtime.GOOS != "windows" {
		terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 40, Cols: 120})
		if err != nil {
			return Result{}, fmt.Errorf("start PTY: %w", err)
		}
		process.input = terminal
		go func() {
			defer close(process.outputDone)
			_, _ = io.Copy(&process.output, terminal)
			_ = terminal.Close()
		}()
	} else {
		close(process.outputDone)
		stdin, err := command.StdinPipe()
		if err != nil {
			return Result{}, err
		}
		process.input = stdin
		command.Stdout = &process.output
		command.Stderr = &process.output
		if err := command.Start(); err != nil {
			_ = stdin.Close()
			return Result{}, err
		}
	}
	if err := t.manager.track(process); err != nil {
		_ = terminateProcessTree(process.command)
		if process.input != nil {
			_ = process.input.Close()
		}
		_ = command.Wait()
		return Result{}, err
	}
	go func() {
		err := command.Wait()
		// A child may outlive a shell that exits without waiting for it. Clean
		// the process group after Wait as well as on explicit cancellation.
		cleanupProcessTree(command)
		waitForManagedProcessOutput(process)
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
		process.endedAt = time.Now()
		process.mu.Unlock()
		t.manager.mu.Lock()
		if !t.manager.closed && t.manager.processes[process.id] == process {
			process.mu.Lock()
			process.expiry = time.AfterFunc(processRetention, func() { t.manager.expire(process) })
			process.mu.Unlock()
		}
		t.manager.mu.Unlock()
		close(process.done)
	}()
	return t.manager.observe(ctx, process, time.Duration(input.YieldMS)*time.Millisecond, input.MaxTokens, false)
}

func waitForManagedProcessOutput(process *managedProcess) {
	if process == nil || process.outputDone == nil {
		return
	}
	timer := time.NewTimer(processWaitDelay)
	defer timer.Stop()
	select {
	case <-process.outputDone:
		return
	case <-timer.C:
		if process.input != nil {
			_ = process.input.Close()
		}
	}
	finalTimer := time.NewTimer(100 * time.Millisecond)
	defer finalTimer.Stop()
	select {
	case <-process.outputDone:
	case <-finalTimer.C:
	}
}

type writeStdinTool struct{ manager *processManager }

func (*writeStdinTool) Category() Category { return CategoryShell }

func (*writeStdinTool) Definition() provider.ToolDefinition {
	return definition("write_stdin", "Write characters to an existing exec_command session, or poll it with an empty chars value, and return new output.", `{"type":"object","properties":{"session_id":{"type":"integer"},"chars":{"type":"string","maxLength":65536},"yield_time_ms":{"type":"integer","minimum":0,"maximum":300000},"max_output_tokens":{"type":"integer","minimum":1,"maximum":50000}},"required":["session_id"],"additionalProperties":false}`)
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
		if err := writeProcessInput(ctx, process, input.Chars); err != nil {
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

type processWriteDeadliner interface {
	SetWriteDeadline(time.Time) error
}

func writeProcessInput(ctx context.Context, process *managedProcess, value string) error {
	if len(value) > maxProcessInput {
		return fmt.Errorf("chars exceeds the %d-byte limit", maxProcessInput)
	}
	if process == nil || process.input == nil {
		return errors.New("process stdin is unavailable")
	}
	process.inputMu.Lock()
	defer process.inputMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(processWriteTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if writer, ok := process.input.(processWriteDeadliner); ok && writer.SetWriteDeadline(deadline) == nil {
		_, err := io.WriteString(process.input, value)
		_ = writer.SetWriteDeadline(time.Time{})
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return err
	}
	result := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(process.input, value)
		result <- writeErr
	}()
	timer := time.NewTimer(processWriteTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = process.input.Close()
		return ctx.Err()
	case <-timer.C:
		_ = process.input.Close()
		return fmt.Errorf("stdin write timed out after %s", processWriteTimeout)
	}
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
	if input.Terminate {
		_ = terminateProcessTree(process.command)
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
	if m.closed {
		return nil, errors.New("process manager is closed")
	}
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
		m.removeLocked(p)
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
	return definition("list_processes", "List background exec_command sessions and their current status.", `{"type":"object","properties":{},"additionalProperties":false}`)
}
func (*listProcessesTool) Risk(string) Risk      { return RiskRead }
func (*listProcessesTool) Summary(string) string { return "list background processes" }
func (t *listProcessesTool) Execute(_ context.Context, arguments string) (Result, error) {
	var empty struct{}
	if err := decodeArguments(arguments, &empty); err != nil {
		return Result{}, err
	}
	t.manager.mu.Lock()
	if t.manager.closed {
		t.manager.mu.Unlock()
		return Result{}, errors.New("process manager is closed")
	}
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
	if t.manager.closed {
		t.manager.mu.Unlock()
		return Result{}, errors.New("process manager is closed")
	}
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
			if err := terminateProcessTree(process.command); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return Result{}, fmt.Errorf("stop process %d: %w", process.id, err)
			}
			stopped = append(stopped, process.id)
		}
	}
	encoded, _ := json.MarshalIndent(map[string]any{"stopped": stopped}, "", "  ")
	return Result{Content: string(encoded)}, nil
}

func (m *processManager) track(process *managedProcess) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("process manager is closed")
	}
	for len(m.processes) >= maxManagedProcesses {
		var oldest *managedProcess
		for _, candidate := range m.processes {
			select {
			case <-candidate.done:
				candidate.mu.Lock()
				endedAt := candidate.endedAt
				candidate.mu.Unlock()
				if oldest == nil {
					oldest = candidate
					continue
				}
				oldest.mu.Lock()
				oldestEndedAt := oldest.endedAt
				oldest.mu.Unlock()
				if endedAt.Before(oldestEndedAt) {
					oldest = candidate
				}
			default:
			}
		}
		if oldest == nil {
			return fmt.Errorf("background process limit of %d reached", maxManagedProcesses)
		}
		m.removeLocked(oldest)
	}
	m.processes[process.id] = process
	return nil
}

func (m *processManager) expire(process *managedProcess) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.processes[process.id] != process {
		return
	}
	process.mu.Lock()
	expired := !process.endedAt.IsZero() && time.Since(process.endedAt) >= processRetention
	process.mu.Unlock()
	if expired {
		m.removeLocked(process)
	}
}

func (m *processManager) removeLocked(process *managedProcess) {
	if process == nil || m.processes[process.id] != process {
		return
	}
	delete(m.processes, process.id)
	process.mu.Lock()
	if process.expiry != nil {
		process.expiry.Stop()
		process.expiry = nil
	}
	process.mu.Unlock()
}

func (m *processManager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() { m.closeErr = m.closeProcesses() })
	return m.closeErr
}

func (m *processManager) closeProcesses() error {
	m.mu.Lock()
	m.closed = true
	processes := make([]*managedProcess, 0, len(m.processes))
	for _, process := range m.processes {
		processes = append(processes, process)
		m.removeLocked(process)
	}
	m.mu.Unlock()

	var failures []error
	for _, process := range processes {
		if process.command.Process != nil {
			if err := terminateProcessTree(process.command); err != nil && !errors.Is(err, os.ErrProcessDone) {
				failures = append(failures, fmt.Errorf("stop process %d: %w", process.id, err))
			}
		}
		if process.input != nil {
			_ = process.input.Close()
		}
	}
	deadline := time.NewTimer(processCloseTimeout)
	defer deadline.Stop()
	for _, process := range processes {
		select {
		case <-process.done:
		case <-deadline.C:
			failures = append(failures, fmt.Errorf("timed out waiting for background processes to stop after %s", processCloseTimeout))
			return errors.Join(failures...)
		}
	}
	return errors.Join(failures...)
}
