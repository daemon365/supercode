package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
)

func TestExplicitNotesSummaryInjectionAndClear(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memories")
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remember("Prefer concise answers"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remember("Prefer concise answers"); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadDir(filepath.Join(root, "extensions", "ad_hoc", "notes"))
	if err != nil || len(notes) != 2 {
		t.Fatalf("notes = %v, err = %v", notes, err)
	}
	note, _ := os.ReadFile(filepath.Join(root, "extensions", "ad_hoc", "notes", notes[0].Name()))
	if !strings.Contains(string(note), "Prefer concise answers") {
		t.Fatalf("note = %q", note)
	}
	if value, _ := store.Read(); value != "" {
		t.Fatalf("generated memory changed before consolidation: %q", value)
	}
	if err := atomicWrite(filepath.Join(root, "MEMORY.md"), []byte("# Handbook\nDetailed history\n")); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(root, "memory_summary.md"), []byte("Prefers concise answers.\n")); err != nil {
		t.Fatal(err)
	}
	instructions := store.Instructions()
	if !strings.Contains(instructions, "MEMORY SUMMARY BEGINS") || !strings.Contains(instructions, "Prefers concise answers") || strings.Contains(instructions, "Detailed history") {
		t.Fatalf("instructions = %q", instructions)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if value, _ := store.Read(); value != "" {
		t.Fatalf("memory after clear = %q", value)
	}
	notes, err = os.ReadDir(filepath.Join(root, "extensions", "ad_hoc", "notes"))
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes after clear = %v, err = %v", notes, err)
	}
}

func TestAtomicWriteUsesPrivateModeAndRejectsOversizedData(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "artifact.md")
	if err := atomicWrite(path, []byte("saved\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	rejected := filepath.Join(root, "rejected.md")
	if err := atomicWrite(rejected, make([]byte, maximumArtifactBytes+1)); err == nil {
		t.Fatal("expected oversized write to fail")
	}
	if _, err := os.Stat(rejected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected path exists or returned unexpected error: %v", err)
	}
}

func TestBoundedMemoryReadRejectsGrowthAndSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "memory_summary.md")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openMemoryNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("grew beyond bound"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOpenedPrivateFile(file, info, 3); err == nil {
		t.Fatal("expected bounded reader to reject growth after Stat")
	}
	target := filepath.Join(root, "target.md")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(link, maximumArtifactBytes); err == nil {
		t.Fatal("memory reader followed a symbolic link")
	}
}

func TestStateReadIsBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memories")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), make([]byte, maximumArtifactBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(root); err == nil {
		t.Fatal("expected oversized state.json to be rejected")
	}
}

func TestLegacyMemoryMigratesToAdHocNote(t *testing.T) {
	directory := t.TempDir()
	legacy := filepath.Join(directory, "memory.md")
	if err := os.WriteFile(legacy, []byte("- Prefer Go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(directory, "memories"), legacy)
	if err != nil {
		t.Fatal(err)
	}
	notes, _, err := store.readAdHocNotes()
	if err != nil || !strings.Contains(notes, "Prefer Go") {
		t.Fatalf("migrated notes = %q, err = %v", notes, err)
	}
}

func TestMemoryToolsSearchReadListAndRejectTraversal(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(store.root, "MEMORY.md"), []byte("# Decisions\nUse atomic writes.\n")); err != nil {
		t.Fatal(err)
	}
	var searchResult, readResult, listResult string
	for _, item := range store.Tools() {
		switch item.Definition().Name {
		case ToolSearch:
			result, err := item.Execute(context.Background(), "{\"query\":\"atomic\"}")
			if err != nil {
				t.Fatal(err)
			}
			searchResult = result.Content
		case ToolRead:
			result, err := item.Execute(context.Background(), "{\"path\":\"MEMORY.md\"}")
			if err != nil {
				t.Fatal(err)
			}
			readResult = result.Content
			if _, err := item.Execute(context.Background(), "{\"path\":\"../state.json\"}"); err == nil {
				t.Fatal("memory read allowed traversal")
			}
		case ToolList:
			result, err := item.Execute(context.Background(), "{}")
			if err != nil {
				t.Fatal(err)
			}
			listResult = result.Content
		}
	}
	if !strings.Contains(searchResult, "MEMORY.md:2") || !strings.Contains(readResult, "2: Use atomic writes") || !strings.Contains(listResult, "MEMORY.md") {
		t.Fatalf("search=%q read=%q list=%q", searchResult, readResult, listResult)
	}
}

func TestMemoryToolsListAndSearchIncludeAdHocNotes(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddAdHocNote("widget-note", "remember the blue widget"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddAdHocNote("other-note", "unrelated note"); err != nil {
		t.Fatal(err)
	}
	var searchResult, listRoot, listNotes string
	for _, item := range store.Tools() {
		switch item.Definition().Name {
		case ToolSearch:
			result, err := item.Execute(context.Background(), "{\"query\":\"blue widget\"}")
			if err != nil {
				t.Fatal(err)
			}
			searchResult = result.Content
		case ToolList:
			result, err := item.Execute(context.Background(), "{}")
			if err != nil {
				t.Fatal(err)
			}
			listRoot = result.Content
			result, err = item.Execute(context.Background(), "{\"path\":\"extensions/ad_hoc/notes\"}")
			if err != nil {
				t.Fatal(err)
			}
			listNotes = result.Content
		}
	}
	if !strings.Contains(searchResult, "blue widget") {
		t.Fatalf("search missed ad-hoc notes: %q", searchResult)
	}
	if !strings.Contains(listRoot, "extensions/ad_hoc/notes/") || !strings.Contains(listRoot, "widget-note") {
		t.Fatalf("root list missed notes: %q", listRoot)
	}
	if !strings.Contains(listNotes, "widget-note") || !strings.Contains(listNotes, "other-note") {
		t.Fatalf("notes list = %q", listNotes)
	}
	// Internal trees remain hidden and unlistable.
	for _, item := range store.Tools() {
		if item.Definition().Name != ToolList {
			continue
		}
		result, err := item.Execute(context.Background(), "{\"path\":\"raw\"}")
		if err == nil {
			t.Fatalf("listing internal raw/ should fail, got %q", result.Content)
		}
		result, err = item.Execute(context.Background(), "{\"path\":\"extensions/other\"}")
		if err == nil {
			t.Fatalf("listing hidden extensions branch should fail, got %q", result.Content)
		}
	}
}

type pipelineProvider struct {
	responses []provider.Response
	requests  []provider.Request
}

type failingPipelineProvider struct{ err error }

func (p *failingPipelineProvider) Generate(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, p.err
}
func (*failingPipelineProvider) Stream(context.Context, provider.Request) (provider.Stream, error) {
	panic("unexpected stream")
}

func (p *pipelineProvider) Generate(_ context.Context, request provider.Request) (provider.Response, error) {
	p.requests = append(p.requests, request)
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

func (*pipelineProvider) Stream(context.Context, provider.Request) (provider.Stream, error) {
	panic("unexpected stream")
}

func TestTwoPhasePipelineWritesArtifactsAndRedactsSecrets(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	extractionJSON, _ := json.Marshal(extractionOutput{
		RawMemory:      "Use focused tests. API key sk-abcdefghijklmnop must not persist.",
		RolloutSummary: "Focused Go test workflow.",
	})
	consolidationJSON, _ := json.Marshal(consolidationOutput{
		MemoryMD:        "# Testing\nUse focused Go tests.\n",
		MemorySummaryMD: "Testing workflow is available.",
		Skills:          []consolidationSkill{{Name: "focused-tests", Content: "# Focused tests\n\nRun the narrow package first."}},
	})
	model := &pipelineProvider{responses: []provider.Response{{Text: string(extractionJSON)}, {Text: string(consolidationJSON)}}}
	source := session.Session{
		ID: "session-1", Workspace: "/workspace", Model: "test",
		CreatedAt: time.Now().Add(-8 * time.Hour), UpdatedAt: time.Now().Add(-7 * time.Hour),
		Messages: []provider.Message{
			{Role: provider.MessageRoleUser, Content: "Remember the focused test workflow. token=super-secret-value"},
			{Role: provider.MessageRoleAssistant, Content: "Done."},
		},
	}
	configuration := DefaultConfig()
	if outcome, err := store.extractSession(context.Background(), model, source, configuration, "active-model"); err != nil || outcome != rolloutSucceeded {
		t.Fatalf("extract outcome=%q err=%v", outcome, err)
	}
	consolidated, err := store.consolidate(context.Background(), model, configuration, "active-model")
	if err != nil || !consolidated {
		t.Fatalf("consolidated=%t err=%v", consolidated, err)
	}
	for _, path := range []string{"MEMORY.md", "memory_summary.md", "raw_memories.md", filepath.Join("skills", "focused-tests", "SKILL.md")} {
		if _, err := os.Stat(filepath.Join(store.root, path)); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(store.root, "raw_memories.md"))
	if strings.Contains(string(raw), "sk-abcdefghijklmnop") || strings.Contains(string(raw), "super-secret-value") || !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("raw memory was not redacted: %s", raw)
	}
	if len(model.requests) != 2 || model.requests[0].ReasoningEffort != "low" || model.requests[1].ReasoningEffort != "medium" {
		t.Fatalf("requests = %#v", model.requests)
	}
}

func TestStartupPipelineSkipsCurrentAndChildSessions(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(filepath.Join(directory, "memories"))
	if err != nil {
		t.Fatal(err)
	}
	sessionsDirectory := filepath.Join(directory, "sessions")
	sessions, err := session.NewStore(sessionsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	values := []session.Session{
		{Version: 2, ID: "current", Workspace: "/workspace", CreatedAt: now.Add(-9 * time.Hour), UpdatedAt: now.Add(-8 * time.Hour), Messages: []provider.Message{{Role: provider.MessageRoleUser, Content: "current"}}},
		{Version: 2, ID: "child", ParentID: "parent", Workspace: "/workspace", CreatedAt: now.Add(-9 * time.Hour), UpdatedAt: now.Add(-8 * time.Hour), Messages: []provider.Message{{Role: provider.MessageRoleUser, Content: "child"}}},
		{Version: 2, ID: "root", Workspace: "/workspace", CreatedAt: now.Add(-9 * time.Hour), UpdatedAt: now.Add(-8 * time.Hour), Messages: []provider.Message{{Role: provider.MessageRoleUser, Content: "root preference"}}},
	}
	for _, value := range values {
		data, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := os.WriteFile(filepath.Join(sessionsDirectory, value.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	extractionJSON, _ := json.Marshal(extractionOutput{RawMemory: "Root memory.", RolloutSummary: "Root summary."})
	consolidationJSON, _ := json.Marshal(consolidationOutput{MemoryMD: "# Root\n", MemorySummaryMD: "Root summary."})
	model := &pipelineProvider{responses: []provider.Response{{Text: string(extractionJSON)}, {Text: string(consolidationJSON)}}}
	configuration := DefaultConfig()
	configuration.Generate = true
	store.ConfigureAdvanced(configuration)
	report, err := store.RunStartup(context.Background(), model, sessions, "current", "active-model")
	if err != nil {
		t.Fatal(err)
	}
	if report.Eligible != 1 || report.Extracted != 1 || !report.Consolidated || len(model.requests) != 2 {
		t.Fatalf("report=%+v requests=%d", report, len(model.requests))
	}
}

func TestShutdownPipelineExtractsRecentlyCommittedCurrentSessionOnce(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(filepath.Join(directory, "memories"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := DefaultConfig()
	configuration.Generate = true
	store.ConfigureAdvanced(configuration)
	sessions, err := session.NewStore(filepath.Join(directory, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := sessions.New("/workspace", "active-model")
	if err != nil {
		t.Fatal(err)
	}
	current.Messages = []provider.Message{
		{Role: provider.MessageRoleUser, Content: "Always run the focused package test first."},
		{Role: provider.MessageRoleAssistant, Content: "Understood."},
	}
	if err := sessions.Commit(current); err != nil {
		t.Fatal(err)
	}
	extractionJSON, _ := json.Marshal(extractionOutput{RawMemory: "Run focused package tests first.", RolloutSummary: "Focused testing preference."})
	consolidationJSON, _ := json.Marshal(consolidationOutput{MemoryMD: "# Testing\nRun focused package tests first.\n", MemorySummaryMD: "Focused testing preference."})
	changedExtractionJSON, _ := json.Marshal(extractionOutput{RawMemory: "Run focused package tests and race tests.", RolloutSummary: "Focused and race testing preference."})
	changedConsolidationJSON, _ := json.Marshal(consolidationOutput{MemoryMD: "# Testing\nRun focused package and race tests.\n", MemorySummaryMD: "Focused and race testing preference."})
	model := &pipelineProvider{responses: []provider.Response{
		{Text: string(extractionJSON)}, {Text: string(consolidationJSON)},
		{Text: string(changedExtractionJSON)}, {Text: string(changedConsolidationJSON)},
	}}

	report, err := store.RunShutdown(context.Background(), model, sessions, current.ID, "active-model")
	if err != nil {
		t.Fatal(err)
	}
	if report.Eligible != 1 || report.Extracted != 1 || !report.Consolidated || len(model.requests) != 2 {
		t.Fatalf("report=%+v requests=%d", report, len(model.requests))
	}
	unchanged, err := sessions.Load(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Commit(unchanged); err != nil {
		t.Fatal(err)
	}
	second, err := store.RunShutdown(context.Background(), model, sessions, current.ID, "active-model")
	if err != nil {
		t.Fatal(err)
	}
	if second.Eligible != 0 || len(model.requests) != 2 {
		t.Fatalf("deduplicated report=%+v requests=%d", second, len(model.requests))
	}
	changed, err := sessions.Load(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed.Messages = append(changed.Messages, provider.Message{Role: provider.MessageRoleUser, Content: "Also run the race detector."})
	if err := sessions.Commit(changed); err != nil {
		t.Fatal(err)
	}
	third, err := store.RunShutdown(context.Background(), model, sessions, current.ID, "active-model")
	if err != nil {
		t.Fatal(err)
	}
	if third.Eligible != 1 || third.Extracted != 1 || len(model.requests) != 4 {
		t.Fatalf("changed report=%+v requests=%d", third, len(model.requests))
	}
}

func TestShutdownPipelineReturnsCurrentExtractionFailure(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(filepath.Join(directory, "memories"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := DefaultConfig()
	configuration.Generate = true
	store.ConfigureAdvanced(configuration)
	sessions, err := session.NewStore(filepath.Join(directory, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := sessions.New("/workspace", "active-model")
	if err != nil {
		t.Fatal(err)
	}
	current.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "remember this"}}
	if err := sessions.Commit(current); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("extraction failed")
	report, err := store.RunShutdown(context.Background(), &failingPipelineProvider{err: sentinel}, sessions, current.ID, "active-model")
	if !errors.Is(err, sentinel) || report.Failed != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	extractionJSON, _ := json.Marshal(extractionOutput{RawMemory: "Retried memory.", RolloutSummary: "Retry succeeded."})
	consolidationJSON, _ := json.Marshal(consolidationOutput{MemoryMD: "# Retry\nSucceeded.\n", MemorySummaryMD: "Retry succeeded."})
	retry := &pipelineProvider{responses: []provider.Response{{Text: string(extractionJSON)}, {Text: string(consolidationJSON)}}}
	retried, retryErr := store.RunShutdown(context.Background(), retry, sessions, current.ID, "active-model")
	if retryErr != nil || retried.Extracted != 1 || len(retry.requests) != 2 {
		t.Fatalf("retry report=%+v requests=%d err=%v", retried, len(retry.requests), retryErr)
	}
}

func TestSelectMemoriesBoundsRawBudgetAndDoesNotReadUnusedSummaries(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state := newState()
	for index := 0; index < 3; index++ {
		name := fmt.Sprintf("raw/memory-%d.md", index)
		content := strings.Repeat(fmt.Sprintf("memory-%d ", index), 40_000)
		if err := atomicWrite(filepath.Join(store.root, filepath.FromSlash(name)), []byte(content)); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("session-%d", index)
		state.Rollouts[id] = rolloutRecord{
			SessionID: id, RolloutID: fmt.Sprintf("rollout-%d", index), Status: rolloutSucceeded,
			RawPath: name, SummaryPath: "rollout_summaries/does-not-exist.md",
			SourceUpdatedAt: now.Add(-time.Duration(index) * time.Minute), UsageCount: 3 - index,
		}
	}
	store.mu.Lock()
	err = store.saveStateUnlocked(state)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	configuration := DefaultConfig()
	selected, _, err := store.selectMemories(configuration, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) == 0 {
		t.Fatal("missing summary files incorrectly excluded every raw memory")
	}
	total := 0
	for _, item := range selected {
		total += len("\n\n---\n\n\n") + len(item.raw)
	}
	if total > maximumSelectedRawBytes {
		t.Fatalf("selected raw memory bytes=%d, budget=%d", total, maximumSelectedRawBytes)
	}
}

func TestEligibleSessionsStopsBeforeHydrationWhenContextCancelled(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(filepath.Join(directory, "memories"))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewStore(filepath.Join(directory, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	value, err := sessions.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	value.UpdatedAt = now.Add(-8 * time.Hour)
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "eligible"}}
	if err := sessions.Commit(value); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.eligibleSessions(ctx, sessions, "", DefaultConfig(), now)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("eligibleSessions error=%v, want context.Canceled", err)
	}
}

func TestEligibleSessionsMetadataOnlyBlockersDoNotStarveChangedSession(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(filepath.Join(directory, "memories"))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewStore(filepath.Join(directory, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := DefaultConfig()
	configuration.MaxRolloutsPerStartup = 1

	target, err := sessions.New("/workspace", "test")
	if err != nil {
		t.Fatal(err)
	}
	target.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "target before"}}
	if err := sessions.Commit(target); err != nil {
		t.Fatal(err)
	}
	targetBefore, err := sessions.Load(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetHash, err := sessionSourceHash(targetBefore)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore.Messages = append(targetBefore.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "target changed"})
	if err := sessions.Commit(targetBefore); err != nil {
		t.Fatal(err)
	}

	state := newState()
	state.Rollouts[target.ID] = rolloutRecord{
		SessionID: target.ID, Status: rolloutSucceeded, SourceUpdatedAt: targetBefore.UpdatedAt, SourceHash: targetHash,
	}
	// MaxRollouts=1 yields an overscan window of nine. Put nine newer,
	// metadata-only commits ahead of the genuinely changed target.
	for index := 0; index < 9; index++ {
		blocker, createErr := sessions.New("/workspace", "test")
		if createErr != nil {
			t.Fatal(createErr)
		}
		blocker.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: fmt.Sprintf("unchanged-%d", index)}}
		if err := sessions.Commit(blocker); err != nil {
			t.Fatal(err)
		}
		before, err := sessions.Load(blocker.ID)
		if err != nil {
			t.Fatal(err)
		}
		hash, err := sessionSourceHash(before)
		if err != nil {
			t.Fatal(err)
		}
		state.Rollouts[blocker.ID] = rolloutRecord{
			SessionID: blocker.ID, Status: rolloutSucceeded, SourceUpdatedAt: before.UpdatedAt, SourceHash: hash,
		}
		if err := sessions.Commit(before); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	err = store.saveStateUnlocked(state)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(8 * time.Hour)
	first, err := store.eligibleSessions(context.Background(), sessions, "", configuration, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 {
		t.Fatalf("first bounded scan unexpectedly reached target: %#v", first)
	}
	second, err := store.eligibleSessions(context.Background(), sessions, "", configuration, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != target.ID {
		t.Fatalf("changed target remained starved after blockers were observed: %#v", second)
	}
}

func TestMemoryStatePrunesDeterministicallyBeforeSizeLimit(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	state := newState()
	oldSuccess := "successful-old-record"
	state.Rollouts[oldSuccess] = rolloutRecord{SessionID: oldSuccess, Status: rolloutSucceeded, SourceUpdatedAt: time.Unix(1, 0)}
	sharedLargeError := strings.Repeat("x", 16*1024)
	for index := 0; index < maximumStateRollouts+100; index++ {
		id := fmt.Sprintf("failed-%05d", index)
		state.Rollouts[id] = rolloutRecord{SessionID: id, Status: rolloutFailed, GeneratedAt: time.Unix(int64(index+10), 0), LastError: sharedLargeError}
	}
	state.AppliedNotes["removed-note.md"] = "hash"
	state.LastPipelineError = strings.Repeat("provider failure ", 2*maximumStateBytes)
	store.mu.Lock()
	err = store.saveStateUnlocked(state)
	loaded, loadErr := store.loadStateUnlocked()
	store.mu.Unlock()
	if err != nil || loadErr != nil {
		t.Fatalf("save=%v load=%v", err, loadErr)
	}
	if len(loaded.Rollouts) != maximumStateRollouts {
		t.Fatalf("rollout records = %d, want %d", len(loaded.Rollouts), maximumStateRollouts)
	}
	if _, ok := loaded.Rollouts[oldSuccess]; !ok {
		t.Fatal("successful record was pruned before failed records")
	}
	if len(loaded.AppliedNotes) != 0 {
		t.Fatalf("removed applied notes survived: %#v", loaded.AppliedNotes)
	}
	if info, err := os.Stat(filepath.Join(store.root, "state.json")); err != nil || info.Size() > maximumStateBytes {
		t.Fatalf("state file info=%v err=%v", info, err)
	}
}

func TestCitationParserAndUsageFeedback(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	state, _ := store.loadStateUnlocked()
	state.Rollouts["session"] = rolloutRecord{SessionID: "session", RolloutID: "11111111-1111-4111-8111-111111111111", Status: rolloutSucceeded}
	_ = store.saveStateUnlocked(state)
	store.mu.Unlock()

	var parser CitationParser
	visible := parser.Push("Answer<oai-mem-")
	visible += parser.Push("citation><citation_entries>MEMORY.md:1-1|note=[used]</citation_entries><rollout_ids>\n11111111-1111-4111-8111-111111111111\n</rollout_ids></oai-mem-citation>")
	tail, ids := parser.Finish()
	visible += tail
	if visible != "Answer" || len(ids) != 1 {
		t.Fatalf("visible=%q ids=%v", visible, ids)
	}
	store.RecordUsage(ids)
	store.mu.Lock()
	state, _ = store.loadStateUnlocked()
	store.mu.Unlock()
	record := state.Rollouts["session"]
	if record.UsageCount != 1 || record.LastUsage == nil {
		t.Fatalf("usage record = %#v", record)
	}
}
