package hook

import (
	"context"
	"runtime"
	"testing"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/provider"
)

func TestPreToolHookCanRewriteArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	manager, err := New(t.TempDir(), map[string][]config.Hook{
		"pre_tool_use": {{
			Command: []string{"/bin/sh", "-c", `printf '%s' '{"arguments":"{\"path\":\"README.md\"}"}'`},
			Matcher: "^read_file$",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := manager.Run(context.Background(), agent.HookPreToolUse, agent.HookInput{
		Call: &provider.ToolCall{Name: "read_file", Arguments: `{"path":"old"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Arguments != `{"path":"README.md"}` {
		t.Fatalf("arguments = %q", output.Arguments)
	}
}

func TestHookMatcherSkipsCommand(t *testing.T) {
	manager, err := New(t.TempDir(), map[string][]config.Hook{
		"pre_tool_use": {{Command: []string{"command-that-does-not-exist"}, Matcher: "^apply_patch$"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Run(context.Background(), agent.HookPreToolUse, agent.HookInput{Call: &provider.ToolCall{Name: "read_file"}})
	if err != nil {
		t.Fatalf("non-matching hook ran: %v", err)
	}
}
