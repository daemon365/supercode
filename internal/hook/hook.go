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
			if strings.TrimSpace(value.SHA256) != "" {
				path := value.Command[0]
				if !filepath.IsAbs(path) {
					path = filepath.Join(workspace, path)
				}
				content, err := os.ReadFile(path)
				if err != nil {
					return nil, fmt.Errorf("read trusted hook %s: %w", path, err)
				}
				digest := sha256.Sum256(content)
				if !strings.EqualFold(strings.TrimSpace(value.SHA256), hex.EncodeToString(digest[:])) {
					return nil, fmt.Errorf("hook %s changed since it was trusted; review it and update sha256", path)
				}
			}
			manager.hooks[event] = append(manager.hooks[event], compiledHook{
				command: append([]string(nil), value.Command...), timeout: timeout, matcher: matcher,
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
		payload := struct {
			Event     agent.HookEvent `json:"event"`
			Workspace string          `json:"workspace"`
			Input     agent.HookInput `json:"input"`
		}{Event: event, Workspace: m.workspace, Input: input}
		encoded, _ := json.Marshal(payload)
		commandContext, cancel := context.WithTimeout(ctx, item.timeout)
		command := exec.CommandContext(commandContext, item.command[0], item.command[1:]...)
		command.Dir = m.workspace
		command.Env = append(os.Environ(), "SUPERCODE_HOOK_EVENT="+string(event), "SUPERCODE_WORKSPACE="+m.workspace)
		command.Stdin = bytes.NewReader(encoded)
		var stdout, stderr boundedBuffer
		stdout.limit, stderr.limit = maxHookOutput, 8*1024
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		timedOut := errors.Is(commandContext.Err(), context.DeadlineExceeded)
		cancel()
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
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.Len()
	if remaining > len(value) {
		remaining = len(value)
	}
	if remaining > 0 {
		_, _ = b.Buffer.Write(value[:remaining])
	}
	return original, nil
}

// ResolveCommand is used by diagnostics and tests to show the exact executable
// without executing it.
func ResolveCommand(workspace string, command []string) string {
	if len(command) == 0 || filepath.IsAbs(command[0]) {
		return strings.Join(command, " ")
	}
	return strings.Join(append([]string{filepath.Join(workspace, command[0])}, command[1:]...), " ")
}
