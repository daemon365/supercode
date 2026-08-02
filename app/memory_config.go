package app

import (
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/memory"
)

func applyMemoryConfig(result memory.Config, source config.File) memory.Config {
	if source.MemoryAutoCapture != nil {
		result.AutoCapture = *source.MemoryAutoCapture
	}
	if source.MemoryMaxTokens > 0 {
		result.SummaryTokens = source.MemoryMaxTokens
	}
	if source.MemoryGenerate != nil {
		result.Generate = *source.MemoryGenerate
	}
	if source.MemoryUse != nil {
		result.Use = *source.MemoryUse
	}
	if source.MemoryDedicatedTools != nil {
		result.DedicatedTools = *source.MemoryDedicatedTools
	}
	if source.MemoryMaxRolloutsPerStartup > 0 {
		result.MaxRolloutsPerStartup = source.MemoryMaxRolloutsPerStartup
	}
	if source.MemoryMaxRolloutAgeDays > 0 {
		result.MaxRolloutAgeDays = source.MemoryMaxRolloutAgeDays
	}
	if source.MemoryMinRolloutIdleHours > 0 {
		result.MinRolloutIdleHours = source.MemoryMinRolloutIdleHours
	}
	if source.MemoryMaxRawForConsolidation > 0 {
		result.MaxRawMemoriesForConsolidation = source.MemoryMaxRawForConsolidation
	}
	if source.MemoryMaxUnusedDays > 0 {
		result.MaxUnusedDays = source.MemoryMaxUnusedDays
	}
	result.ExtractModel = source.MemoryExtractModel
	result.ConsolidationModel = source.MemoryConsolidationModel
	return result
}
