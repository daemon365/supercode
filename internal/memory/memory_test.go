package memory

import (
	"context"
	"encoding/json"
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
