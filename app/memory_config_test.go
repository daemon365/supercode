package app

import (
	"testing"

	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/memory"
)

func TestApplyMemoryConfigOverridesOnlyConfiguredFields(t *testing.T) {
	enabled, disabled := true, false
	base := memory.DefaultConfig()
	result := applyMemoryConfig(base, config.File{
		MemoryAutoCapture:            &enabled,
		MemoryMaxTokens:              4096,
		MemoryGenerate:               &enabled,
		MemoryUse:                    &disabled,
		MemoryDedicatedTools:         &enabled,
		MemoryMaxRolloutsPerStartup:  4,
		MemoryMaxRolloutAgeDays:      20,
		MemoryMinRolloutIdleHours:    12,
		MemoryMaxRawForConsolidation: 128,
		MemoryMaxUnusedDays:          60,
		MemoryExtractModel:           "extract-model",
		MemoryConsolidationModel:     "consolidation-model",
	})
	if !result.AutoCapture || !result.Generate || result.Use || !result.DedicatedTools {
		t.Fatalf("boolean overrides were not applied: %+v", result)
	}
	if result.SummaryTokens != 4096 || result.MaxRolloutsPerStartup != 4 || result.MaxRolloutAgeDays != 20 || result.MinRolloutIdleHours != 12 {
		t.Fatalf("rollout limits were not applied: %+v", result)
	}
	if result.MaxRawMemoriesForConsolidation != 128 || result.MaxUnusedDays != 60 {
		t.Fatalf("consolidation limits were not applied: %+v", result)
	}
	if result.ExtractModel != "extract-model" || result.ConsolidationModel != "consolidation-model" {
		t.Fatalf("model overrides were not applied: %+v", result)
	}

	preserved := applyMemoryConfig(base, config.File{})
	if preserved.SummaryTokens != base.SummaryTokens || preserved.MaxRolloutsPerStartup != base.MaxRolloutsPerStartup {
		t.Fatalf("zero values unexpectedly replaced defaults: base=%+v result=%+v", base, preserved)
	}
}
