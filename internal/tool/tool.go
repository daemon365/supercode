// Package tool defines SuperCode's model-callable tool boundary and registry.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/daemon365/supercode/internal/provider"
)

// Risk controls whether a tool call can run without user approval.
type Risk string

const (
	RiskRead       Risk = "read"
	RiskWrite      Risk = "write"
	RiskExecute    Risk = "execute"
	RiskNetwork    Risk = "network"
	RiskPermission Risk = "permission"
)

type Category string

const (
	CategoryTool       Category = "tool"
	CategoryFile       Category = "file"
	CategoryShell      Category = "shell"
	CategoryNetwork    Category = "network"
	CategoryMCP        Category = "mcp"
	CategorySkill      Category = "skill"
	CategoryPermission Category = "permission"
)

type Categorized interface{ Category() Category }

func CategoryOf(item Tool) Category {
	if categorized, ok := item.(Categorized); ok {
		return categorized.Category()
	}
	return CategoryTool
}

// Result is the bounded observation returned to the model.
type Result struct {
	Content string
	IsError bool
	Images  []provider.Image
}

// Tool is implemented by every built-in or future plugin tool.
type Tool interface {
	Definition() provider.ToolDefinition
	Risk(arguments string) Risk
	Summary(arguments string) string
	Execute(ctx context.Context, arguments string) (Result, error)
}

// Registry keeps tool discovery independent from execution orchestration.
type Registry struct {
	tools map[string]Tool
}

func NewRegistry(tools ...Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]Tool, len(tools))}
	if err := registry.Add(tools...); err != nil {
		return nil, err
	}
	return registry, nil
}

// Add installs dynamically discovered tools before a runner starts.
func (r *Registry) Add(tools ...Tool) error {
	if r == nil {
		return errors.New("register tool: registry is nil")
	}
	for _, item := range tools {
		if item == nil {
			return errors.New("register tool: tool is nil")
		}
		name := strings.TrimSpace(item.Definition().Name)
		if name == "" {
			return errors.New("register tool: name is required")
		}
		if _, exists := r.tools[name]; exists {
			return fmt.Errorf("register tool: duplicate name %q", name)
		}
		r.tools[name] = item
	}
	return nil
}

func (r *Registry) Definitions() []provider.ToolDefinition {
	names := r.Names()
	definitions := make([]provider.ToolDefinition, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, r.tools[name].Definition())
	}
	return definitions
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Lookup(name string) (Tool, bool) {
	item, ok := r.tools[name]
	return item, ok
}

func (r *Registry) Len() int { return len(r.tools) }

// SearchTool exposes a compact catalog for models and users when many dynamic
// MCP tools are installed. Execution still goes through the original registry.
func SearchTool(items []Tool) Tool {
	definitions := make([]provider.ToolDefinition, 0, len(items))
	for _, item := range items {
		if item != nil {
			definitions = append(definitions, item.Definition())
		}
	}
	return &toolSearch{definitions: definitions}
}

type toolSearch struct{ definitions []provider.ToolDefinition }

func (*toolSearch) Definition() provider.ToolDefinition {
	return provider.ToolDefinition{
		Name: "tool_search", Description: "Search available built-in and MCP tools by name or description.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["query"],"additionalProperties":false}`),
	}
}
func (*toolSearch) Risk(string) Risk { return RiskRead }
func (*toolSearch) Summary(arguments string) string {
	return "search tools " + strings.Join(strings.Fields(arguments), " ")
}
func (t *toolSearch) Execute(_ context.Context, arguments string) (Result, error) {
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return Result{}, fmt.Errorf("decode tool_search arguments: %w", err)
	}
	query := strings.ToLower(strings.TrimSpace(input.Query))
	if query == "" {
		return Result{}, errors.New("query is required")
	}
	if input.Limit <= 0 || input.Limit > 50 {
		input.Limit = 20
	}
	words := strings.Fields(query)
	type match struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	values := make([]match, 0, input.Limit)
	for _, definition := range t.definitions {
		haystack := strings.ToLower(definition.Name + " " + definition.Description)
		matched := true
		for _, word := range words {
			if !strings.Contains(haystack, word) {
				matched = false
				break
			}
		}
		if matched {
			values = append(values, match{Name: definition.Name, Description: definition.Description})
			if len(values) >= input.Limit {
				break
			}
		}
	}
	encoded, _ := json.MarshalIndent(map[string]any{"query": input.Query, "tools": values}, "", "  ")
	return Result{Content: string(encoded)}, nil
}
