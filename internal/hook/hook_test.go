package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/provider"
)

func TestBoundedBufferCannotBypassLimitThroughIOCopy(t *testing.T) {
	var output boundedBuffer
	output.limit = 32
	input := strings.NewReader(strings.Repeat("x", 4096))
	written, err := io.Copy(&output, input)
	if err != nil {
		t.Fatal(err)
	}
	if written != 4096 || output.Len() != 32 || !output.truncated {
		t.Fatalf("written=%d retained=%d truncated=%t", written, output.Len(), output.truncated)
	}
}

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

func TestPinnedHookExecutesExactHashedFileAndRechecksDigest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	workspace := t.TempDir()
	pathDirectory := t.TempDir()
	workspaceHook := filepath.Join(workspace, "hookcmd")
	pathHook := filepath.Join(pathDirectory, "hookcmd")
	workspaceContent := []byte("#!/bin/sh\nprintf '%s' '{\"additional_context\":\"workspace\"}'\n")
	pathContent := []byte("#!/bin/sh\nprintf '%s' '{\"additional_context\":\"path\"}'\n")
	if err := os.WriteFile(workspaceHook, workspaceContent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathHook, pathContent, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	digest := sha256.Sum256(workspaceContent)
	manager, err := New(workspace, map[string][]config.Hook{
		"user_prompt_submit": {{Command: []string{"hookcmd"}, SHA256: hex.EncodeToString(digest[:])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := manager.Run(context.Background(), agent.HookUserPromptSubmit, agent.HookInput{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if output.AdditionalContext != "workspace" {
		t.Fatalf("executed PATH hook instead of pinned file: %+v", output)
	}
	if err := os.WriteFile(workspaceHook, append(workspaceContent, []byte("# changed\n")...), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), agent.HookUserPromptSubmit, agent.HookInput{Prompt: "again"}); err == nil || !strings.Contains(err.Error(), "changed since it was trusted") {
		t.Fatalf("changed hook error = %v", err)
	}
}

func TestStagedPinnedHookKeepsVerifiedBytesAfterSourceReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	path := filepath.Join(t.TempDir(), "hook.sh")
	original := []byte("#!/bin/sh\nprintf verified\n")
	if err := os.WriteFile(path, original, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)
	staged, cleanup, err := stageVerifiedExecutable(path, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf replaced\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(staged).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "verified" {
		t.Fatalf("staged output = %q", output)
	}
}

func TestHookTimeoutTerminatesChildHoldingOutputPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	manager, err := New(t.TempDir(), map[string][]config.Hook{
		"session_start": {{Command: []string{"/bin/sh", "-c", "sleep 30 & wait"}, Timeout: "30ms"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = manager.Run(context.Background(), agent.HookEvent("session_start"), agent.HookInput{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hook timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("hook child kept output pipe open for %s", elapsed)
	}
}
