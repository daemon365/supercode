package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daemon365/supercode/internal/provider"
)

const maxToolOutput = 64 * 1024

// Builtins returns the standard workspace-scoped coding tool set.
func Builtins(root string) ([]Tool, error) {
	return BuiltinsWithOptions(root, SandboxOptions{})
}

func BuiltinsWithOptions(root string, options SandboxOptions) ([]Tool, error) {
	tools, _, err := BuiltinsWithLifecycle(root, options)
	return tools, err
}

// BuiltinsWithLifecycle returns the standard tools together with the owner of
// background process resources. Long-lived hosts must close the lifecycle when
// shutting down; Builtins and BuiltinsWithOptions remain for compatibility.
func BuiltinsWithLifecycle(root string, options SandboxOptions) ([]Tool, io.Closer, error) {
	workspace, err := newWorkspaceWithOptions(root, options)
	if err != nil {
		return nil, nil, err
	}
	sandbox := newCommandSandbox(workspace)
	processes := newProcessManager(workspace, sandbox)
	tools := []Tool{
		&listFilesTool{workspace}, &searchTextTool{workspace}, &readFileTool{workspace},
		&applyPatchTool{workspace},
		&gitTool{workspace: workspace, name: "git_status"},
		&gitTool{workspace: workspace, name: "git_diff"},
	}
	tools = append(tools, processes.tools()...)
	tools = append(tools, &viewImageTool{workspace: workspace})
	tools = append(tools, newWebTool(options.Permissions))
	if options.Permissions != nil {
		tools = append(tools, newRequestPermissionsTool(options.Permissions))
	}
	return tools, processes, nil
}

// SandboxStatus describes the command boundary selected for a workspace.
func SandboxStatus(root string) string {
	return SandboxStatusWithOptions(root, SandboxOptions{})
}

func SandboxStatusWithOptions(root string, options SandboxOptions) string {
	workspace, err := newWorkspaceWithOptions(root, options)
	if err != nil {
		return "unavailable"
	}
	return newCommandSandbox(workspace).status()
}

type listFilesTool struct{ workspace }

func (*listFilesTool) Category() Category { return CategoryFile }

func (*listFilesTool) Definition() provider.ToolDefinition {
	return definition("list_files", "List files under the workspace or another configured read root. Hidden files are included; .git is excluded.", `{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative path or an absolute path inside a configured read root (default: .)"},"max_entries":{"type":"integer","minimum":1,"maximum":1000}},"additionalProperties":false}`)
}
func (*listFilesTool) Risk(string) Risk         { return RiskRead }
func (*listFilesTool) ParallelSafe(string) bool { return true }
func (*listFilesTool) Summary(arguments string) string {
	return argumentSummary("list files", arguments)
}
func (t *listFilesTool) Execute(_ context.Context, arguments string) (Result, error) {
	var input struct {
		Path       string `json:"path"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	if input.MaxEntries <= 0 {
		input.MaxEntries = 300
	}
	if input.MaxEntries > 1000 {
		input.MaxEntries = 1000
	}
	root, err := t.resolve(input.Path, false)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return Result{}, err
	}
	if !info.IsDir() {
		return Result{}, errors.New("path is not a directory")
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules") && path != root {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		if len(paths) >= input.MaxEntries {
			return fs.SkipAll
		}
		name := t.display(path)
		if entry.IsDir() {
			name += "/"
		}
		paths = append(paths, name)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	sort.Strings(paths)
	return Result{Content: strings.Join(paths, "\n")}, nil
}

type readFileTool struct{ workspace }

func (*readFileTool) Category() Category { return CategoryFile }

func (*readFileTool) Definition() provider.ToolDefinition {
	return definition("read_file", "Read a UTF-8 text file from the workspace or a configured read root, with optional one-based line bounds.", `{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`)
}
func (*readFileTool) Risk(string) Risk                { return RiskRead }
func (*readFileTool) ParallelSafe(string) bool        { return true }
func (*readFileTool) Summary(arguments string) string { return argumentSummary("read file", arguments) }
func (t *readFileTool) Execute(_ context.Context, arguments string) (Result, error) {
	var input struct {
		Path  string `json:"path"`
		Start int    `json:"start_line"`
		End   int    `json:"end_line"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	file, _, err := t.openRead(input.Path)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	if input.Start <= 0 {
		input.Start = 1
	}
	if input.End <= 0 {
		input.End = input.Start + 499
	}
	if input.End < input.Start {
		return Result{}, errors.New("end_line must be greater than or equal to start_line")
	}
	var output strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(file, 4*1024*1024))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		if line < input.Start {
			continue
		}
		if line > input.End {
			break
		}
		text := scanner.Text()
		if strings.IndexByte(text, 0) >= 0 {
			return Result{}, errors.New("file appears to be binary")
		}
		fmt.Fprintf(&output, "%d: %s\n", line, text)
		if output.Len() >= maxToolOutput {
			output.WriteString("[output truncated]\n")
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, err
	}
	return Result{Content: output.String()}, nil
}

type searchTextTool struct{ workspace }

func (*searchTextTool) Category() Category { return CategoryFile }

func (*searchTextTool) Definition() provider.ToolDefinition {
	return definition("search_text", "Search text with ripgrep-compatible regex, glob, case, and hidden-file controls.", `{"type":"object","properties":{"query":{"type":"string"},"path":{"type":"string","description":"Workspace-relative path (default: .)"},"regex":{"type":"boolean","description":"interpret query as a regular expression"},"glob":{"type":"string","description":"include/exclude glob such as *.go or !vendor/**"},"case_sensitive":{"type":"boolean"},"hidden":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":500}},"required":["query"],"additionalProperties":false}`)
}
func (*searchTextTool) Risk(string) Risk         { return RiskRead }
func (*searchTextTool) ParallelSafe(string) bool { return true }
func (*searchTextTool) Summary(arguments string) string {
	return argumentSummary("search text", arguments)
}
func (t *searchTextTool) Execute(ctx context.Context, arguments string) (Result, error) {
	var input struct {
		Query         string `json:"query"`
		Path          string `json:"path"`
		Regex         bool   `json:"regex"`
		Glob          string `json:"glob"`
		CaseSensitive bool   `json:"case_sensitive"`
		Hidden        bool   `json:"hidden"`
		Max           int    `json:"max_results"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	if input.Query == "" {
		return Result{}, errors.New("query is required")
	}
	if input.Max <= 0 {
		input.Max = 100
	}
	if input.Max > 500 {
		input.Max = 500
	}
	root, err := t.resolve(input.Path, false)
	if err != nil {
		return Result{}, err
	}
	if ripgrep, lookupErr := exec.LookPath("rg"); lookupErr == nil {
		arguments := []string{"--line-number", "--with-filename", "--color", "never", "--no-heading", "--glob", "!.git/**"}
		if !input.Regex {
			arguments = append(arguments, "--fixed-strings")
		}
		if input.CaseSensitive {
			arguments = append(arguments, "--case-sensitive")
		} else {
			arguments = append(arguments, "--ignore-case")
		}
		if input.Hidden {
			arguments = append(arguments, "--hidden")
		}
		if strings.TrimSpace(input.Glob) != "" {
			arguments = append(arguments, "--glob", input.Glob)
		}
		arguments = append(arguments, "--", input.Query, root)
		command := exec.CommandContext(ctx, ripgrep, arguments...)
		configureProcessTree(command, false)
		command.WaitDelay = time.Second
		defer cleanupProcessTree(command)
		stdout, pipeErr := command.StdoutPipe()
		if pipeErr != nil {
			return Result{}, fmt.Errorf("ripgrep output pipe: %w", pipeErr)
		}
		var stderr limitedBuffer
		command.Stderr = &stderr
		if startErr := command.Start(); startErr != nil {
			return Result{}, fmt.Errorf("start ripgrep search: %w", startErr)
		}
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 2*maxToolOutput)
		var output limitedBuffer
		matches := 0
		resultLimitReached := false
		stoppedEarly := false
		for scanner.Scan() {
			if matches >= input.Max {
				resultLimitReached = true
				stoppedEarly = true
				_ = terminateProcessTree(command)
				break
			}
			line := strings.TrimPrefix(scanner.Text(), root+string(filepath.Separator))
			if matches > 0 {
				_, _ = output.Write([]byte("\n"))
			}
			_, _ = output.Write([]byte(line))
			matches++
			if output.Truncated() {
				stoppedEarly = true
				_ = terminateProcessTree(command)
				break
			}
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			_ = terminateProcessTree(command)
		}
		runErr := command.Wait()
		if scanErr != nil {
			return Result{}, fmt.Errorf("read ripgrep output: %w", scanErr)
		}
		if runErr != nil {
			if stoppedEarly {
				runErr = nil
			}
			if exitErr, ok := runErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return Result{Content: "No matches found."}, nil
			}
			if runErr != nil {
				message := strings.TrimSpace(stderr.String())
				if message != "" {
					return Result{}, fmt.Errorf("ripgrep search: %w: %s", runErr, message)
				}
				return Result{}, fmt.Errorf("ripgrep search: %w", runErr)
			}
		}
		if matches == 0 {
			return Result{Content: "No matches found."}, nil
		}
		content := strings.TrimSpace(output.String())
		if resultLimitReached && !output.Truncated() {
			content += "\n[results truncated]"
		}
		return Result{Content: content}, nil
	}
	if input.Regex || input.Glob != "" || input.Hidden {
		return Result{}, errors.New("advanced search requires ripgrep (rg) on PATH")
	}
	var matches []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules") && path != root {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for line := 1; scanner.Scan(); line++ {
			value := scanner.Text()
			if strings.IndexByte(value, 0) >= 0 {
				break
			}
			candidate, query := value, input.Query
			if !input.CaseSensitive {
				candidate, query = strings.ToLower(candidate), strings.ToLower(query)
			}
			if strings.Contains(candidate, query) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", t.display(path), line, value))
				if len(matches) >= input.Max {
					break
				}
			}
		}
		_ = file.Close()
		if len(matches) >= input.Max {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if len(matches) == 0 {
		return Result{Content: "No matches found."}, nil
	}
	return Result{Content: truncate(strings.Join(matches, "\n"))}, nil
}

type applyPatchTool struct{ workspace }

func (*applyPatchTool) Category() Category { return CategoryFile }

func (*applyPatchTool) Definition() provider.ToolDefinition {
	return definition("apply_patch", "Atomically create, edit, delete, move, or apply unified-diff hunks to workspace files. Provide path for one edit, operations for a batch, or unified_diff for a unified diff. An operations array may repeat a path: entries run in array order against the cumulative in-memory result, then commit atomically. Supply complete literal content for every created or replaced section; never use omission placeholders. expected_sha256 rejects stale changes.", `{"type":"object","properties":{"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"},"delete":{"type":"boolean"},"move_to":{"type":"string"},"expected_sha256":{"type":"string"},"unified_diff":{"type":"string"},"operations":{"type":"array","minItems":1,"maxItems":100,"items":{"type":"object","properties":{"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"},"delete":{"type":"boolean"},"move_to":{"type":"string"},"expected_sha256":{"type":"string"}},"required":["path"],"additionalProperties":false}}},"additionalProperties":false}`)
}
func (*applyPatchTool) Risk(string) Risk { return RiskWrite }
func (*applyPatchTool) Summary(arguments string) string {
	return argumentSummary("edit file", arguments)
}
func (t *applyPatchTool) Execute(ctx context.Context, arguments string) (Result, error) {
	return executePatch(ctx, t.workspace, arguments)
}

type runCommandTool struct {
	workspace workspace
	sandbox   commandSandbox
}

func (*runCommandTool) Definition() provider.ToolDefinition {
	return definition("run_command", "Run a non-interactive shell command. Read-only commands run automatically in a read-only sandbox. Other commands require approval and run in the workspace-write sandbox. Use require-escalated only when the sandbox blocks necessary work.", `{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600},"sandbox_permissions":{"type":"string","enum":["workspace-write","require-escalated"]},"justification":{"type":"string"}},"required":["command"],"additionalProperties":false}`)
}
func (t *runCommandTool) Risk(arguments string) Risk {
	command, escalated := sandboxRequest(arguments, "command")
	return t.sandbox.riskFor(command, escalated)
}
func (*runCommandTool) Summary(arguments string) string {
	return argumentSummary("run command", arguments)
}
func (t *runCommandTool) Execute(ctx context.Context, arguments string) (Result, error) {
	var input struct {
		Command       string `json:"command"`
		Timeout       int    `json:"timeout_seconds"`
		Permissions   string `json:"sandbox_permissions"`
		Justification string `json:"justification"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(input.Command) == "" {
		return Result{}, errors.New("command is required")
	}
	if input.Permissions == "require-escalated" && strings.TrimSpace(input.Justification) == "" {
		return Result{}, errors.New("justification is required for require-escalated")
	}
	if input.Timeout <= 0 {
		input.Timeout = 120
	}
	if input.Timeout > 600 {
		input.Timeout = 600
	}
	commandContext, cancel := context.WithTimeout(ctx, time.Duration(input.Timeout)*time.Second)
	defer cancel()
	escalated := input.Permissions == "require-escalated"
	readOnly := readOnlyShellCommand(input.Command) && !escalated
	command := t.sandbox.command(commandContext, "/bin/sh", []string{"-lc", input.Command}, t.workspace.root, !readOnly, escalated)
	configureProcessTree(command, false)
	command.WaitDelay = time.Second
	defer cleanupProcessTree(command)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	content := strings.TrimSpace(output.String())
	if content == "" {
		content = "(no output)"
	}
	if commandContext.Err() != nil {
		return Result{Content: content, IsError: true}, fmt.Errorf("command timed out: %w", commandContext.Err())
	}
	if err != nil {
		return Result{Content: content, IsError: true}, fmt.Errorf("command failed: %w", err)
	}
	return Result{Content: content}, nil
}

type gitTool struct {
	workspace
	name string
}

func (t *gitTool) Definition() provider.ToolDefinition {
	if t.name == "git_status" {
		return definition(t.name, "Show concise Git workspace status.", `{"type":"object","properties":{},"additionalProperties":false}`)
	}
	return definition(t.name, "Show the current Git diff. Unstaged output includes untracked text files by default.", `{"type":"object","properties":{"staged":{"type":"boolean"},"include_untracked":{"type":"boolean"}},"additionalProperties":false}`)
}
func (*gitTool) Risk(string) Risk         { return RiskRead }
func (*gitTool) ParallelSafe(string) bool { return true }
func (t *gitTool) Summary(arguments string) string {
	return argumentSummary(strings.ReplaceAll(t.name, "_", " "), arguments)
}
func (t *gitTool) Execute(ctx context.Context, arguments string) (Result, error) {
	var input struct {
		Staged           bool  `json:"staged"`
		IncludeUntracked *bool `json:"include_untracked"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	args := []string{"status", "--short", "--branch"}
	if t.name == "git_diff" {
		args = []string{"diff", "--no-ext-diff"}
		if input.Staged {
			args = append(args, "--cached")
		}
	}
	command := exec.CommandContext(ctx, "git", args...)
	configureProcessTree(command, false)
	command.WaitDelay = time.Second
	defer cleanupProcessTree(command)
	command.Dir = t.root
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return Result{Content: output.String(), IsError: true}, fmt.Errorf("git command failed: %w", err)
	}
	if t.name == "git_diff" && !input.Staged && (input.IncludeUntracked == nil || *input.IncludeUntracked) {
		if err := t.appendUntrackedDiffs(ctx, &output); err != nil {
			return Result{Content: output.String(), IsError: true}, err
		}
	}
	content := strings.TrimSpace(output.String())
	if content == "" {
		content = "No changes."
	}
	return Result{Content: content}, nil
}

func (t *gitTool) appendUntrackedDiffs(ctx context.Context, output *limitedBuffer) error {
	list := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard", "-z")
	configureProcessTree(list, false)
	list.WaitDelay = time.Second
	list.Dir = t.root
	stdout, err := list.StdoutPipe()
	if err != nil {
		return fmt.Errorf("list untracked files output pipe: %w", err)
	}
	var stderr limitedBuffer
	list.Stderr = &stderr
	if err := list.Start(); err != nil {
		return fmt.Errorf("start listing untracked files: %w", err)
	}
	waited := false
	defer func() {
		if !waited {
			if list.Process != nil {
				_ = terminateProcessTree(list)
			}
			_ = list.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Split(scanNUL)
	scanner.Buffer(make([]byte, 4096), maxToolOutput)
	stoppedEarly := false
	for scanner.Scan() {
		path := scanner.Text()
		if path == "" || output.Truncated() {
			if output.Truncated() {
				stoppedEarly = true
				_ = terminateProcessTree(list)
				break
			}
			continue
		}
		absolute, err := t.resolve(path, false)
		if err != nil {
			continue
		}
		info, err := os.Stat(absolute)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
			continue
		}
		command := exec.CommandContext(ctx, "git", "diff", "--no-index", "--no-ext-diff", "--", os.DevNull, path)
		configureProcessTree(command, false)
		command.WaitDelay = time.Second
		command.Dir = t.root
		command.Stdout, command.Stderr = output, output
		runErr := command.Run()
		cleanupProcessTree(command)
		if err := runErr; err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				return fmt.Errorf("diff untracked file %s: %w", path, err)
			}
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = terminateProcessTree(list)
	}
	waitErr := list.Wait()
	waited = true
	if scanErr != nil {
		return fmt.Errorf("read untracked file list: %w", scanErr)
	}
	if waitErr != nil && !stoppedEarly {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("list untracked files: %w: %s", waitErr, message)
		}
		return fmt.Errorf("list untracked files: %w", waitErr)
	}
	return nil
}

func scanNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if index := bytes.IndexByte(data, 0); index >= 0 {
		return index + 1, data[:index], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func definition(name, description, schema string) provider.ToolDefinition {
	return provider.ToolDefinition{Name: name, Description: description, Parameters: json.RawMessage(schema)}
}

func decodeArguments(arguments string, target any) error {
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("invalid tool arguments: multiple JSON values")
	}
	return nil
}

func argumentSummary(action, arguments string) string {
	compact := strings.Join(strings.Fields(arguments), " ")
	if compact == "" || compact == "{}" {
		return action
	}
	runes := []rune(compact)
	if len(runes) > 160 {
		compact = string(runes[:157]) + "..."
	}
	return action + ": " + compact
}

func truncate(value string) string {
	if len(value) <= maxToolOutput {
		return value
	}
	return value[:maxToolOutput] + "\n[output truncated]"
}

type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Len()
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(value)
	remaining := maxToolOutput - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(value)
	return original, nil
}
func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.buffer.String()
	if b.truncated {
		value += "\n[output truncated]"
	}
	return value
}
