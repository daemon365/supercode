// Package modelcatalog keeps model capabilities out of provider adapters and
// lets OpenAI-compatible endpoints describe their own model IDs in YAML.
package modelcatalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

type Capabilities struct {
	Provider            string   `json:"provider,omitempty" yaml:"provider,omitempty"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty" yaml:"context_window_tokens,omitempty"`
	AutoCompactTokens   int      `json:"auto_compact_tokens,omitempty" yaml:"auto_compact_tokens,omitempty"`
	UsableContextTokens int      `json:"usable_context_tokens,omitempty" yaml:"usable_context_tokens,omitempty"`
	ReasoningEfforts    []string `json:"reasoning_efforts,omitempty" yaml:"reasoning_efforts,omitempty"`
	ServiceTiers        []string `json:"service_tiers,omitempty" yaml:"service_tiers,omitempty"`
	InputModalities     []string `json:"input_modalities,omitempty" yaml:"input_modalities,omitempty"`
	ToolCalling         *bool    `json:"tool_calling,omitempty" yaml:"tool_calling,omitempty"`
	ParallelToolCalls   *bool    `json:"parallel_tool_calls,omitempty" yaml:"parallel_tool_calls,omitempty"`
}

type Catalog struct{ models map[string]Capabilities }

func New(names []string, configured map[string]Capabilities) *Catalog {
	models := make(map[string]Capabilities, len(names)+len(configured)+1)
	// These limits are SuperCode's project defaults, not a remote capability
	// discovery claim. Users can override every field in model_catalog.
	models["gpt-5.6"] = normalize(Capabilities{
		Provider: "openai-compatible", ContextWindowTokens: 272000,
		AutoCompactTokens: 244800, UsableContextTokens: 258400,
		InputModalities: []string{"text", "image"}, ToolCalling: boolPointer(true), ParallelToolCalls: boolPointer(true),
	})
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			if _, exists := models[name]; !exists {
				models[name] = Capabilities{}
			}
		}
	}
	for name, capabilities := range configured {
		name = strings.TrimSpace(name)
		if name != "" {
			models[name] = normalize(capabilities)
		}
	}
	return &Catalog{models: models}
}

func (c *Catalog) Resolve(model string) (Capabilities, bool) {
	if c == nil {
		return Capabilities{}, false
	}
	value, ok := c.models[strings.TrimSpace(model)]
	return value, ok
}

func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	values := make([]string, 0, len(c.models))
	for name := range c.models {
		values = append(values, name)
	}
	sort.Strings(values)
	return values
}

func (c *Catalog) Validate(model, reasoningEffort, serviceTier string) error {
	capabilities, ok := c.Resolve(model)
	if !ok {
		return nil
	}
	if value := strings.TrimSpace(reasoningEffort); value != "" && len(capabilities.ReasoningEfforts) > 0 && !contains(capabilities.ReasoningEfforts, value) {
		return fmt.Errorf("model %s does not advertise reasoning effort %q (supported: %s)", model, value, strings.Join(capabilities.ReasoningEfforts, ", "))
	}
	if value := strings.TrimSpace(serviceTier); value != "" && len(capabilities.ServiceTiers) > 0 && !contains(capabilities.ServiceTiers, value) {
		return fmt.Errorf("model %s does not advertise service tier %q (supported: %s)", model, value, strings.Join(capabilities.ServiceTiers, ", "))
	}
	return nil
}

func (c *Catalog) Limits(model string, fallbackContext int) (contextWindow, compact, usable int) {
	capabilities, _ := c.Resolve(model)
	contextWindow = capabilities.ContextWindowTokens
	if contextWindow <= 0 {
		contextWindow = fallbackContext
	}
	if contextWindow <= 0 {
		contextWindow = 272000
	}
	compact = capabilities.AutoCompactTokens
	if compact <= 0 {
		compact = contextWindow * 90 / 100
	}
	usable = capabilities.UsableContextTokens
	if usable <= 0 {
		usable = contextWindow * 95 / 100
	}
	return contextWindow, compact, usable
}

func (c Capabilities) Supports(modality string) bool {
	modality = strings.ToLower(strings.TrimSpace(modality))
	return contains(c.InputModalities, modality)
}

func normalize(value Capabilities) Capabilities {
	value.Provider = strings.TrimSpace(value.Provider)
	value.ReasoningEfforts = normalizeStrings(value.ReasoningEfforts)
	value.ServiceTiers = normalizeStrings(value.ServiceTiers)
	value.InputModalities = normalizeStrings(value.InputModalities)
	if value.ContextWindowTokens < 0 || value.AutoCompactTokens < 0 || value.UsableContextTokens < 0 {
		value.ContextWindowTokens, value.AutoCompactTokens, value.UsableContextTokens = 0, 0, 0
	}
	return value
}

func ValidateCapabilities(value Capabilities) error {
	if value.ContextWindowTokens < 0 || value.AutoCompactTokens < 0 || value.UsableContextTokens < 0 {
		return errors.New("model token limits must not be negative")
	}
	contextWindow, compact, usable := value.ContextWindowTokens, value.AutoCompactTokens, value.UsableContextTokens
	if contextWindow > 0 && usable > contextWindow {
		return errors.New("usable_context_tokens must not exceed context_window_tokens")
	}
	if compact > 0 && usable > 0 && compact >= usable {
		return errors.New("auto_compact_tokens must be smaller than usable_context_tokens")
	}
	return nil
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func contains(values []string, wanted string) bool {
	wanted = strings.ToLower(strings.TrimSpace(wanted))
	for _, value := range values {
		if strings.ToLower(value) == wanted {
			return true
		}
	}
	return false
}

func boolPointer(value bool) *bool { return &value }
