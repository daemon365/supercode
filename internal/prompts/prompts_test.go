package prompts

import (
	"strings"
	"testing"
	"time"
)

func TestSessionAssemblesStableInstructionLayers(t *testing.T) {
	value := Session(SessionInput{
		Workspace:           "/workspace",
		ProjectInstructions: "Follow AGENTS.md.",
		CustomInstructions:  "Prefer focused tests.",
		ToolNames:           []string{"apply_patch", "spawn_agent"},
	})
	for _, expected := range []string{
		"You are SuperCode",
		"# Apply Patch guidance",
		"same path more than once",
		"# Multi-agent orchestration",
		"# Project instructions",
		"Follow AGENTS.md.",
		"# Additional developer instructions",
		"Prefer focused tests.",
	} {
		if !strings.Contains(value, expected) {
			t.Fatalf("session instructions do not contain %q:\n%s", expected, value)
		}
	}
	if strings.Contains(value, "{{") {
		t.Fatalf("session instructions contain an unresolved placeholder:\n%s", value)
	}
}

func TestTurnAssemblesDynamicContext(t *testing.T) {
	value := Turn(TurnInput{
		Model: "gpt-test", Mode: ModePlan, Approval: "never", SandboxStatus: "workspace-write",
		Workspace: "/workspace", ContextWindowTokens: 272000, AutoCompactTokens: 244800,
		UsableContextTokens: 258400, MaxTurns: 0, Skills: "Use the review skill.",
		Memory: "The user prefers Go.", Plugins: []string{"github"}, Hooks: []string{"pre_tool_use"},
		MCPServers: []string{"docs"}, Goal: "Objective: finish the client", Now: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	for _, expected := range []string{
		"# Collaboration mode: Plan",
		"Do not edit files",
		"# Approval policy: never",
		"Model: gpt-test",
		"Current date: 2026-08-01",
		"Nominal window: 272000 tokens",
		"Automatic compaction threshold: 244800 tokens",
		"Hard usable request limit: 258400 tokens",
		"Maximum model turns: unlimited",
		"# Skills",
		"# Memory",
		"# Enabled plugins",
		"# Enabled hooks",
		"# Connected MCP servers",
		"# Active goal",
	} {
		if !strings.Contains(value, expected) {
			t.Fatalf("turn instructions do not contain %q:\n%s", expected, value)
		}
	}
}

func TestModesAndSpecializedPrompts(t *testing.T) {
	for input, expected := range map[string]Mode{
		"": ModeDefault, "PLAN": ModePlan, "execute": ModeExecute,
		"pair-programming": ModePair, "pair_programming": ModePair, "unknown": ModeDefault,
	} {
		if actual := NormalizeMode(input); actual != expected {
			t.Fatalf("NormalizeMode(%q) = %q, want %q", input, actual, expected)
		}
	}
	values := []string{
		CompactPrompt(), ReviewPrompt("security"), GoalContinuation("ship it", 25, 100),
		AwaiterInstructions(), GuardianPolicy(), RealtimeInstructions(),
		MemoryExtractionPrompt(), MemoryConsolidationPrompt(),
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || strings.Contains(value, "{{") {
			t.Fatalf("specialized prompt is empty or unresolved:\n%s", value)
		}
	}
	if value := GoalContinuation("ship it", 25, 100); !strings.Contains(value, "Tokens remaining: 75") {
		t.Fatalf("goal budget was not rendered:\n%s", value)
	}
}
