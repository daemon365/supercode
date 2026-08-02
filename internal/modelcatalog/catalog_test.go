package modelcatalog

import "testing"

func TestConfiguredCapabilitiesDriveLimitsAndValidation(t *testing.T) {
	catalog := New(nil, map[string]Capabilities{
		"custom": {
			ContextWindowTokens: 100000, ReasoningEfforts: []string{"low", "high"}, ServiceTiers: []string{"priority"},
			InputModalities: []string{"text"}, ToolCalling: boolPointer(false),
		},
	})
	contextWindow, compact, usable := catalog.Limits("custom", 1)
	if contextWindow != 100000 || compact != 90000 || usable != 95000 {
		t.Fatalf("limits = %d/%d/%d", contextWindow, compact, usable)
	}
	if err := catalog.Validate("custom", "medium", "priority"); err == nil {
		t.Fatal("unsupported reasoning effort was accepted")
	}
	if err := catalog.Validate("custom", "high", "priority"); err != nil {
		t.Fatal(err)
	}
}
