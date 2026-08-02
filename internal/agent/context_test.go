package agent

import (
	"strings"
	"testing"

	"github.com/daemon365/supercode/internal/provider"
)

func TestCompactHistoryPreservesRecentCompleteTurn(t *testing.T) {
	history := []provider.Message{
		{Role: provider.MessageRoleUser, Content: "old request " + strings.Repeat("x", 2_000)},
		{Role: provider.MessageRoleAssistant, ToolCalls: []provider.ToolCall{{ID: "old", Name: "read_file"}}},
		{Role: provider.MessageRoleTool, ToolCallID: "old", Content: strings.Repeat("old output ", 300)},
		{Role: provider.MessageRoleAssistant, Content: "old answer"},
		{Role: provider.MessageRoleUser, Content: "recent request"},
		{Role: provider.MessageRoleAssistant, Content: "recent answer"},
	}
	compacted, changed := CompactHistory(history, 400)
	if !changed {
		t.Fatal("CompactHistory() changed = false")
	}
	if !strings.Contains(compacted[0].Content, "Earlier conversation compacted") {
		t.Fatalf("summary = %q", compacted[0].Content)
	}
	if compacted[len(compacted)-2].Content != "recent request" || compacted[len(compacted)-1].Content != "recent answer" {
		t.Fatalf("recent suffix not preserved: %+v", compacted)
	}
	if compacted[1].Role == provider.MessageRoleTool {
		t.Fatal("compacted history starts retained suffix with an orphaned tool result")
	}
}

func TestBoundToolContentPreservesHeadAndTail(t *testing.T) {
	input := "HEAD-" + strings.Repeat("x", 500) + "-TAIL"
	got := boundToolContent(input, 20)
	if !strings.HasPrefix(got, "HEAD-") || !strings.HasSuffix(got, "-TAIL") || !strings.Contains(got, "tool output truncated") {
		t.Fatalf("boundToolContent() = %q", got)
	}
}

func TestEstimateTextTokensHandlesCJK(t *testing.T) {
	if got := EstimateTextTokens("你好世界"); got < 2 {
		t.Fatalf("EstimateTextTokens() = %d, want at least 2", got)
	}
}
