package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/daemon365/supercode/internal/provider"
)

const (
	// The nominal context window describes the model capability. SuperCode
	// starts compaction at 90% and never builds a request beyond 95%, leaving
	// the final 5% for system instructions, tool schemas, and model output.
	DefaultContextWindowTokens = 272_000
	DefaultAutoCompactTokens   = 244_800
	DefaultUsableContextTokens = 258_400
	DefaultToolOutputTokens    = 12_000
)

// EstimateTextTokens intentionally uses a conservative, provider-neutral
// approximation. It handles English code and CJK text more safely than a
// byte/4 estimate without coupling the agent layer to a model tokenizer.
func EstimateTextTokens(value string) int {
	if value == "" {
		return 0
	}
	runes := utf8.RuneCountInString(value)
	bytes := len(value)
	// ASCII-heavy source code averages roughly four bytes per token; non-ASCII
	// text is closer to one or two runes per token. Use the larger estimate.
	return maxInt((bytes+3)/4, (runes+1)/2)
}

func EstimateMessagesTokens(messages []provider.Message) int {
	total := 0
	for _, message := range messages {
		total += 6 + EstimateTextTokens(message.Content)
		for _, call := range message.ToolCalls {
			total += 8 + EstimateTextTokens(call.Name) + EstimateTextTokens(call.Arguments)
		}
		for range message.Images {
			// A fixed allowance is safer than counting base64 bytes, which are not
			// sent as text tokens by multimodal providers.
			total += 1_024
		}
	}
	return total
}

// CompactHistory replaces older complete turns with a bounded structural
// summary while preserving a recent suffix that begins at a user boundary.
// It is deterministic so it remains available for custom/local providers.
func CompactHistory(history []provider.Message, targetTokens int) ([]provider.Message, bool) {
	if targetTokens <= 0 {
		targetTokens = DefaultAutoCompactTokens / 2
	}
	if EstimateMessagesTokens(history) <= targetTokens || len(history) < 4 {
		return append([]provider.Message(nil), history...), false
	}

	recentBudget := maxInt(256, targetTokens*2/3)
	start := len(history)
	tokens := 0
	for index := len(history) - 1; index >= 0; index-- {
		tokens += EstimateMessagesTokens(history[index : index+1])
		if tokens > recentBudget {
			break
		}
		if history[index].Role == provider.MessageRoleUser && history[index].ToolCallID == "" {
			start = index
		}
	}
	if start <= 0 || start >= len(history) {
		// Fall back to the latest complete-looking half instead of risking an
		// orphaned tool result at the beginning of the retained context.
		for index := len(history) / 2; index < len(history); index++ {
			if history[index].Role == provider.MessageRoleUser && history[index].ToolCallID == "" {
				start = index
				break
			}
		}
	}
	if start <= 0 || start >= len(history) {
		return append([]provider.Message(nil), history...), false
	}

	summary := summarizeMessages(history[:start], maxInt(256, targetTokens/3))
	result := make([]provider.Message, 0, 1+len(history)-start)
	result = append(result, provider.Message{
		Role: provider.MessageRoleUser,
		Content: "[Earlier conversation compacted by SuperCode]\n" + summary +
			"\n[Continue from the preserved messages below. Treat this summary as context, not as a new request.]",
	})
	result = append(result, history[start:]...)
	return result, true
}

func summarizeMessages(messages []provider.Message, tokenBudget int) string {
	characterBudget := maxInt(1_024, tokenBudget*3)
	var output strings.Builder
	for _, message := range messages {
		label := string(message.Role)
		if message.ToolCallID != "" {
			label += " result " + message.ToolCallID
		}
		text := strings.TrimSpace(message.Content)
		if len(message.ToolCalls) > 0 {
			names := make([]string, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				names = append(names, call.Name)
			}
			text = strings.TrimSpace(text + " [tools: " + strings.Join(names, ", ") + "]")
		}
		text = singleLine(text)
		if utf8.RuneCountInString(text) > 700 {
			text = string([]rune(text)[:700]) + "…"
		}
		line := fmt.Sprintf("- %s: %s\n", label, text)
		if output.Len()+len(line) > characterBudget {
			output.WriteString("- Additional earlier events omitted.\n")
			break
		}
		output.WriteString(line)
	}
	return strings.TrimSpace(output.String())
}

func boundToolContent(value string, maximumTokens int) string {
	if maximumTokens <= 0 {
		maximumTokens = DefaultToolOutputTokens
	}
	maximumCharacters := maximumTokens * 3
	runes := []rune(value)
	if len(runes) <= maximumCharacters {
		return value
	}
	head := maximumCharacters * 3 / 4
	tail := maximumCharacters - head
	return string(runes[:head]) + fmt.Sprintf("\n[tool output truncated: %d characters omitted]\n", len(runes)-maximumCharacters) + string(runes[len(runes)-tail:])
}

func singleLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
