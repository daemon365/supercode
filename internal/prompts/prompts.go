package prompts

import (
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"
)

//go:embed templates
var templates embed.FS

type Mode string

const (
	ModeDefault Mode = "default"
	ModePlan    Mode = "plan"
	ModeExecute Mode = "execute"
	ModePair    Mode = "pair"
)

func NormalizeMode(value string) Mode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "plan":
		return ModePlan
	case "execute":
		return ModeExecute
	case "pair", "pair-programming", "pair_programming":
		return ModePair
	default:
		return ModeDefault
	}
}

type SessionInput struct {
	Workspace           string
	ProjectInstructions string
	CustomInstructions  string
	ToolNames           []string
}

type TurnInput struct {
	Model               string
	Mode                Mode
	Approval            string
	SandboxStatus       string
	Workspace           string
	ContextWindowTokens int
	AutoCompactTokens   int
	UsableContextTokens int
	MaxTurns            int
	Skills              string
	Memory              string
	Plugins             []string
	Hooks               []string
	MCPServers          []string
	Goal                string
	Additional          string
	Now                 time.Time
}

func Session(input SessionInput) string {
	sections := []string{template("base.md")}
	if contains(input.ToolNames, "apply_patch") {
		sections = append(sections, template("special/apply_patch.md"))
	}
	if contains(input.ToolNames, "spawn_agent") {
		sections = append(sections, template("special/orchestrator.md"))
	}
	if value := strings.TrimSpace(input.ProjectInstructions); value != "" {
		sections = append(sections, "# Project instructions\n\n"+value)
	}
	if value := strings.TrimSpace(input.CustomInstructions); value != "" {
		sections = append(sections, "# Additional developer instructions\n\n"+value)
	}
	return join(sections...)
}

func Turn(input TurnInput) string {
	if input.Now.IsZero() {
		input.Now = time.Now()
	}
	input.Mode = NormalizeMode(string(input.Mode))
	sections := []string{
		template("modes/" + string(input.Mode) + ".md"),
		permissionInstructions(input.Approval),
		fmt.Sprintf("# Runtime environment\n\n- Model: %s\n- Current date: %s\n- Workspace: %s\n- Sandbox: %s", fallback(input.Model, "provider default"), input.Now.Format("2006-01-02"), fallback(input.Workspace, "unknown"), fallback(input.SandboxStatus, "approval-only status unknown")),
		fmt.Sprintf("# Context budget\n\n- Nominal window: %d tokens\n- Automatic compaction threshold: %d tokens\n- Hard usable request limit: %d tokens\n- Maximum model turns: %s\n\nKeep tool output and explanations proportionate to the remaining context. Never evade these limits by dropping required file content.", input.ContextWindowTokens, input.AutoCompactTokens, input.UsableContextTokens, turnLimit(input.MaxTurns)),
	}
	sections = appendOptionalList(sections, "Enabled plugins", input.Plugins)
	sections = appendOptionalList(sections, "Enabled hooks", input.Hooks)
	sections = appendOptionalList(sections, "Connected MCP servers", input.MCPServers)
	if value := strings.TrimSpace(input.Skills); value != "" {
		sections = append(sections, "# Skills\n\n"+value)
	}
	if value := strings.TrimSpace(input.Memory); value != "" {
		sections = append(sections, "# Memory\n\n"+value)
	}
	if value := strings.TrimSpace(input.Goal); value != "" {
		sections = append(sections, "# Active goal\n\n"+value)
	}
	if value := strings.TrimSpace(input.Additional); value != "" {
		sections = append(sections, value)
	}
	return join(sections...)
}

func CompactPrompt() string { return template("special/compact.md") }

func ReviewPrompt(focus string) string {
	value := template("special/review.md")
	if focus = strings.TrimSpace(focus); focus != "" {
		value += "\n\nReview focus supplied by the user:\n" + focus
	}
	return value
}

func GoalContinuation(objective string, used, budget int64) string {
	return render(template("special/goal_continuation.md"), map[string]string{
		"OBJECTIVE": strings.TrimSpace(objective),
		"USED":      fmt.Sprint(used),
		"BUDGET":    budgetText(budget),
		"REMAINING": remainingText(used, budget),
	})
}

func AwaiterInstructions() string       { return template("special/awaiter.md") }
func GuardianPolicy() string            { return template("special/guardian.md") }
func RealtimeInstructions() string      { return template("special/realtime.md") }
func MemoryExtractionPrompt() string    { return template("special/memory_extract.md") }
func MemoryConsolidationPrompt() string { return template("special/memory_consolidate.md") }

func permissionInstructions(value string) string {
	name := strings.ToLower(strings.TrimSpace(value))
	if name != "always" && name != "never" {
		name = "on-request"
	}
	return template("permissions/" + name + ".md")
}

func template(name string) string {
	content, err := templates.ReadFile("templates/" + name)
	if err != nil {
		panic("missing embedded prompt template: " + name)
	}
	return strings.TrimSpace(string(content))
}

func render(value string, replacements map[string]string) string {
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value = strings.ReplaceAll(value, "{{"+key+"}}", replacements[key])
	}
	return value
}

func join(values ...string) string {
	var result []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return strings.Join(result, "\n\n")
}

func appendOptionalList(sections []string, title string, values []string) []string {
	if len(values) == 0 {
		return sections
	}
	items := append([]string(nil), values...)
	sort.Strings(items)
	return append(sections, "# "+title+"\n\n- "+strings.Join(items, "\n- "))
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func fallback(value, replacement string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return replacement
}

func turnLimit(value int) string {
	if value == 0 {
		return "unlimited (context, time, cancellation, and goal budgets still apply)"
	}
	return fmt.Sprint(value)
}

func budgetText(value int64) string {
	if value <= 0 {
		return "unlimited"
	}
	return fmt.Sprint(value)
}

func remainingText(used, budget int64) string {
	if budget <= 0 {
		return "unlimited"
	}
	return fmt.Sprint(max(int64(0), budget-used))
}
