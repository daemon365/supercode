// Package hook runs trusted lifecycle commands without invoking a shell.
package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/config"
)

const maxHookOutput = 64 * 1024

type Manager struct {
	workspace string
	hooks     map[string][]compiledHook
}

type compiledHook struct {
	command []string
	timeout time.Duration
	matcher *regexp.Regexp
	sha256  string
}

func New(workspace string, definitions map[string][]config.Hook) (*Manager, error) {
	manager := &Manager{workspace: workspace, hooks: make(map[string][]compiledHook, len(definitions))}
	for event, values := range definitions {
		event = strings.ToLower(strings.TrimSpace(event))
		if event == "" {
			return nil, errors.New("hook event name is required")
		}
		for _, value := range values {
			if value.Enabled != nil && !*value.Enabled {
				continue
			}
			if len(value.Command) == 0 || strings.TrimSpace(value.Command[0]) == "" {
				return nil, fmt.Errorf("hook %s command is required", event)
			}
			timeout := 10 * time.Second
			if strings.TrimSpace(value.Timeout) != "" {
				parsed, err := time.ParseDuration(value.Timeout)
				if err != nil || parsed <= 0 {
					return nil, fmt.Errorf("hook %s has invalid timeout %q", event, value.Timeout)
				}
				timeout = parsed
			}
			var matcher *regexp.Regexp
			if value.Matcher != "" {
				parsed, err := regexp.Compile(value.Matcher)
				if err != nil {
					return nil, fmt.Errorf("hook %s matcher: %w", event, err)
				}
				matcher = parsed
			}
			command := append([]string(nil), value.Command...)
			expectedDigest := strings.TrimSpace(value.SHA256)
			if expectedDigest != "" {
				executable, err := resolvePinnedExecutable(workspace, command[0])
				if err != nil {
					return nil, err
				}
				if err := verifyExecutableDigest(executable, expectedDigest); err != nil {
					return nil, err
				}
				// Execute the exact path that was hashed. A bare command name must
				// never be resolved through PATH after a different file was pinned.
				command[0] = executable
			}
			manager.hooks[event] = append(manager.hooks[event], compiledHook{
				command: command, timeout: timeout, matcher: matcher, sha256: expectedDigest,
			})
		}
	}
	return manager, nil
}

func (m *Manager) Empty() bool { return m == nil || len(m.hooks) == 0 }

// Run implements agent.Hook. Each hook receives one JSON object on stdin and
// may return a partial agent.HookOutput object on stdout. A non-zero exit blocks
// the lifecycle operation, while an empty stdout means no modification.
func (m *Manager) Run(ctx context.Context, event agent.HookEvent, input agent.HookInput) (agent.HookOutput, error) {
	if m == nil {
		return agent.HookOutput{}, nil
	}
	result := agent.HookOutput{}
	for _, item := range m.hooks[string(event)] {
		if item.matcher != nil && !item.matcher.MatchString(hookSubject(input)) {
			continue
		}
		commandPath := item.command[0]
		cleanupCommand := func() {}
		if item.sha256 != "" {
			var stageErr error
			commandPath, cleanupCommand, stageErr = stageVerifiedExecutable(item.command[0], item.sha256)
			if stageErr != nil {
				return result, stageErr
			}
		}
		payload := struct {
			Event     agent.HookEvent `json:"event"`
			Workspace string          `json:"workspace"`
			Input     agent.HookInput `json:"input"`
		}{Event: event, Workspace: m.workspace, Input: input}
		encoded, _ := json.Marshal(payload)
		commandContext, cancel := context.WithTimeout(ctx, item.timeout)
		command := exec.CommandContext(commandContext, commandPath, item.command[1:]...)
		command.Args[0] = item.command[0]
		configureHookCommand(command)
		command.WaitDelay = time.Second
		command.Dir = m.workspace
		command.Env = append(os.Environ(), "SUPERCODE_HOOK_EVENT="+string(event), "SUPERCODE_WORKSPACE="+m.workspace)
		command.Stdin = bytes.NewReader(encoded)
		var stdout, stderr boundedBuffer
		stdout.limit, stderr.limit = maxHookOutput, 8*1024
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		cleanupHookCommand(command)
		timedOut := errors.Is(commandContext.Err(), context.DeadlineExceeded)
		cancel()
		cleanupCommand()
		if stdout.truncated {
			return result, fmt.Errorf("%s hook output exceeded %d bytes", event, maxHookOutput)
		}
		if err != nil {
			message := strings.TrimSpace(stderr.String())
			if timedOut {
				return result, fmt.Errorf("%s hook timed out after %s", event, item.timeout)
			}
			if message != "" {
				return result, fmt.Errorf("%s hook failed: %w: %s", event, err, message)
			}
			return result, fmt.Errorf("%s hook failed: %w", event, err)
		}
		if strings.TrimSpace(stdout.String()) == "" {
			continue
		}
		var output agent.HookOutput
		decoder := json.NewDecoder(strings.NewReader(stdout.String()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&output); err != nil {
			return result, fmt.Errorf("decode %s hook output: %w", event, err)
		}
		mergeOutput(&result, output)
	}
	return result, nil
}

func (m *Manager) Session(ctx context.Context, event string) error {
	if m == nil {
		return nil
	}
	_, err := m.Run(ctx, agent.HookEvent(event), agent.HookInput{})
	return err
}

func hookSubject(input agent.HookInput) string {
	if input.Call != nil {
		return input.Call.Name
	}
	return input.Prompt
}

func mergeOutput(target *agent.HookOutput, value agent.HookOutput) {
	if value.Allow != nil {
		target.Allow = value.Allow
	}
	if value.Message != "" {
		target.Message = value.Message
	}
	if value.Prompt != "" {
		target.Prompt = value.Prompt
	}
	if value.Arguments != "" {
		target.Arguments = value.Arguments
	}
	if value.AdditionalContext != "" {
		target.AdditionalContext = strings.TrimSpace(strings.Join([]string{target.AdditionalContext, value.AdditionalContext}, "\n\n"))
	}
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > len(value) {
		remaining = len(value)
	}
	if remaining > 0 {
		_, _ = b.buffer.Write(value[:remaining])
	}
	b.truncated = b.truncated || remaining < original
	return original, nil
}

func (b *boundedBuffer) Len() int       { return b.buffer.Len() }
func (b *boundedBuffer) String() string { return b.buffer.String() }

// ResolveCommand is used by diagnostics and tests to show the exact executable
// without executing it.
func ResolveCommand(workspace string, command []string) string {
	if len(command) == 0 || filepath.IsAbs(command[0]) {
		return strings.Join(command, " ")
	}
	return strings.Join(append([]string{filepath.Join(workspace, command[0])}, command[1:]...), " ")
}

func resolvePinnedExecutable(workspace, executable string) (string, error) {
	path := strings.TrimSpace(executable)
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve trusted hook %s: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat trusted hook %s: %w", resolved, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("trusted hook %s is not a regular file", resolved)
	}
	return resolved, nil
}

func verifyExecutableDigest(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read trusted hook %s: %w", path, err)
	}
	hasher := sha256.New()
	_, readErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("read trusted hook %s: %w", path, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close trusted hook %s: %w", path, closeErr)
	}
	if !strings.EqualFold(strings.TrimSpace(expected), hex.EncodeToString(hasher.Sum(nil))) {
		return fmt.Errorf("hook %s changed since it was trusted; review it and update sha256", path)
	}
	return nil
}

// stageVerifiedExecutable copies the exact bytes that were hashed into a
// private path and executes that copy. This closes the verify-then-exec rename
// race where a workspace file could otherwise be replaced after verification.
func stageVerifiedExecutable(path, expected string) (string, func(), error) {
	source, err := os.Open(path)
	if err != nil {
		return "", func() {}, fmt.Errorf("read trusted hook %s: %w", path, err)
	}
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = source.Close()
		if err != nil {
			return "", func() {}, fmt.Errorf("stat trusted hook %s: %w", path, err)
		}
		return "", func() {}, fmt.Errorf("trusted hook %s is not a regular file", path)
	}
	directory, err := os.MkdirTemp("", "supercode-hook-*")
	if err != nil {
		_ = source.Close()
		return "", func() {}, fmt.Errorf("stage trusted hook %s: %w", path, err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	destinationPath := filepath.Join(directory, filepath.Base(path))
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = source.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("stage trusted hook %s: %w", path, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(destination, hasher), source)
	sourceCloseErr := source.Close()
	chmodErr := destination.Chmod(info.Mode().Perm())
	syncErr := destination.Sync()
	destinationCloseErr := destination.Close()
	if err := errors.Join(copyErr, sourceCloseErr, chmodErr, syncErr, destinationCloseErr); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("stage trusted hook %s: %w", path, err)
	}
	if !strings.EqualFold(strings.TrimSpace(expected), hex.EncodeToString(hasher.Sum(nil))) {
		cleanup()
		return "", func() {}, fmt.Errorf("hook %s changed since it was trusted; review it and update sha256", path)
	}
	return destinationPath, cleanup, nil
}
