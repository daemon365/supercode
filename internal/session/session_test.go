package session

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/attachment"
	"github.com/daemon365/supercode/internal/provider"
)

func TestStoreSavesLoadsAndListsSessions(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.New("/workspace", "model")
	if err != nil {
		t.Fatal(err)
	}
	value.Title = "A task"
	value.Messages = []provider.Message{
		{Role: provider.MessageRoleUser, Content: "hello"},
		{Role: provider.MessageRoleTool, Content: "image", Images: []provider.Image{{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("image-data"))}}},
	}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != "A task" || len(loaded.Messages) != 2 {
		t.Fatalf("loaded = %+v", loaded)
	}
	if len(loaded.Messages[1].Images) != 1 || loaded.Messages[1].Images[0].Data != value.Messages[1].Images[0].Data || loaded.Messages[1].Images[0].Ref == "" {
		t.Fatalf("persisted image message = %+v", loaded.Messages[1])
	}
	if len(value.Messages[1].Images) != 1 {
		t.Fatal("Save mutated the caller's session")
	}
	values, err := store.List("/workspace", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].ID != value.ID {
		t.Fatalf("sessions = %+v", values)
	}
	other, err := store.List("/other", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other sessions = %+v", other)
	}
}

func TestEventCheckpointRecoversNewerHistory(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New(t.TempDir(), "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "old"}}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{Messages: []provider.Message{{Role: provider.MessageRoleUser, Content: "recovered"}}, Title: "Recovered"}
	if err := store.Append(value.ID, "checkpoint", checkpoint); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "recovered" || loaded.Title != "Recovered" {
		t.Fatalf("recovered session = %+v", loaded)
	}
}

func TestCommitAppendsDeltaAndLoadReplaysIt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "first"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}

	value.Messages = append(value.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "second"})
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.path(value.ID))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Session
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 {
		t.Fatalf("snapshot was rewritten before the checkpoint interval: messages=%d", len(snapshot.Messages))
	}
	reopened, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "second" {
		t.Fatalf("replayed session = %+v", loaded.Messages)
	}
	found, err := store.Search("/workspace", "second", 10, false)
	if err != nil || len(found) != 1 {
		t.Fatalf("incremental index search = %+v, %v", found, err)
	}
}

func TestMetadataOnlyCommitDoesNotRewriteSnapshot(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "unchanged"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.path(value.ID))
	if err != nil {
		t.Fatal(err)
	}
	value.Title = "updated metadata"
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.path(value.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("metadata-only commit rewrote the full session snapshot")
	}
	loaded, err := store.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != value.Title || len(loaded.Messages) != 1 {
		t.Fatalf("loaded metadata-only commit = %+v", loaded)
	}
}

func TestCommitPeriodicallyRefreshesSnapshot(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "zero"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < checkpointSnapshotEvery; index++ {
		value.Messages = append(value.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "next"})
		if err := store.Commit(value); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := readSnapshot(store.path(value.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != len(value.Messages) {
		t.Fatalf("snapshot messages = %d, want %d", len(snapshot.Messages), len(value.Messages))
	}
	if info, err := os.Stat(store.eventPath(value.ID)); err != nil || info.Size() != 0 {
		t.Fatalf("active WAL after snapshot rotation = %v, %v", info, err)
	}
	if _, err := os.Stat(store.eventGzipPath(value.ID)); err != nil {
		t.Fatalf("rotated WAL segment: %v", err)
	}
}

func TestIncrementalIndexRetainsNewestTextAtSizeLimit(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: strings.Repeat("old-content ", 16*1024)}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	value.Messages = append(value.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "newest-search-marker"})
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	found, err := store.Search("/workspace", "newest-search-marker", 10, false)
	if err != nil || len(found) != 1 || found[0].ID != value.ID {
		t.Fatalf("latest bounded search = %+v, %v", found, err)
	}
}

func TestSessionLifecycleRenameForkArchiveDelete(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	value, _ := store.New(workspace, "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "hello"}}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	renamed, err := store.Rename(value.ID, "Useful title")
	if err != nil || renamed.Title != "Useful title" {
		t.Fatalf("Rename() = %+v, %v", renamed, err)
	}
	forked, err := store.Fork(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if forked.ParentID != value.ID || len(forked.Messages) != 1 {
		t.Fatalf("Fork() = %+v", forked)
	}
	archived, err := store.Archive(value.ID)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("Archive() = %+v, %v", archived, err)
	}
	active, err := store.List(workspace, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != forked.ID {
		t.Fatalf("active sessions = %+v", active)
	}
	all, err := store.ListAll(workspace, 10, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAll() = %+v, %v", all, err)
	}
	if err := store.Delete(forked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(forked.ID); !errors.Is(err, os.ErrNotExist) && (err == nil || !strings.Contains(err.Error(), "no such file")) {
		t.Fatalf("Load(deleted) error = %v", err)
	}
}

func TestSessionIndexSearchRepairAndArchiveCompression(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Title = "Dependency audit"
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "find the hidden semaphore regression"}}
	if err := store.Append(value.ID, "checkpoint", Checkpoint{Messages: value.Messages}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	found, err := store.Search("/workspace", "semaphore", 10, false)
	if err != nil || len(found) != 1 || found[0].ID != value.ID {
		t.Fatalf("Search() = %+v, %v", found, err)
	}
	if err := os.WriteFile(store.indexPath, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := store.Repair()
	if err != nil || report.Indexed != 1 || len(report.Invalid) != 0 {
		t.Fatalf("Repair() = %+v, %v", report, err)
	}
	if _, err := store.Archive(value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.eventGzipPath(value.ID)); err != nil {
		t.Fatalf("compressed event log: %v", err)
	}
	if _, err := os.Stat(store.eventPath(value.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plain event log still exists: %v", err)
	}
	events, err := store.Events(value.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("Events() after compression = %+v, %v", events, err)
	}
}

func TestCommitReplacesChangedPrefixEvenWhenHistoryGrows(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{
		{Role: provider.MessageRoleUser, Content: "old-a"},
		{Role: provider.MessageRoleAssistant, Content: "old-b"},
		{Role: provider.MessageRoleUser, Content: "keep-c"},
		{Role: provider.MessageRoleAssistant, Content: "keep-d"},
	}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	value.Messages = []provider.Message{
		{Role: provider.MessageRoleUser, Content: "[compacted summary]"},
		{Role: provider.MessageRoleUser, Content: "keep-c"},
		{Role: provider.MessageRoleAssistant, Content: "keep-d"},
		{Role: provider.MessageRoleUser, Content: "new-user"},
		{Role: provider.MessageRoleAssistant, Content: "new-assistant"},
	}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != len(value.Messages) {
		t.Fatalf("messages = %+v", loaded.Messages)
	}
	for index := range value.Messages {
		if loaded.Messages[index].Content != value.Messages[index].Content {
			t.Fatalf("message %d = %q, want %q", index, loaded.Messages[index].Content, value.Messages[index].Content)
		}
	}
}

func TestConcurrentStoresRejectDivergentSessionCommit(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	first, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := first.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := first.Commit(value); err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	firstView, err := first.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondView, err := second.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstView.Messages = append(firstView.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "first-writer"})
	secondView.Messages = append(secondView.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "second-writer"})
	if err := first.Commit(firstView); err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(secondView); !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("second Commit error = %v, want ErrSessionConflict", err)
	}
	loaded, err := first.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Messages[len(loaded.Messages)-1].Content; got != "first-writer" {
		t.Fatalf("last message = %q", got)
	}
}

func TestStaleIndexReconcilesFromSnapshotAndWAL(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	staleIndex, err := os.ReadFile(store.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	value.Messages = append(value.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "wal-search-marker"})
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.indexPath, staleIndex, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := reopened.ListMetadata("/workspace", "wal-search-marker", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != value.ID || items[0].MessageCount != 2 {
		t.Fatalf("metadata = %+v", items)
	}
	if err := os.Remove(reopened.indexPath); err != nil {
		t.Fatal(err)
	}
	repaired, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err = repaired.ListMetadata("/workspace", "wal-search-marker", 10, false)
	if err != nil || len(items) != 1 {
		t.Fatalf("metadata after index rebuild = %+v, %v", items, err)
	}
}

func TestAppendRepairsIncompleteEventTail(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.eventPath(value.ID), []byte(`{"sequence":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	value.Messages = append(value.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "after-crash"})
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "after-crash" {
		t.Fatalf("messages = %+v", loaded.Messages)
	}
}

func TestAppendPreservesCompleteEventMissingOnlyNewline(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	eventData, _ := json.Marshal(Event{Sequence: 1, Revision: 1, At: time.Now().UTC(), Type: "diagnostic"})
	if err := os.WriteFile(store.eventPath(value.ID), eventData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(value.ID, "next", nil); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("events = %+v", events)
	}
}

func TestLoadRejectsCorruptCompleteEventRecord(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.eventPath(value.ID), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(value.ID); err == nil || !strings.Contains(err.Error(), "corrupt session event log") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLargeCheckpointEventLoadsAfterSnapshotRotation(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", 16*1024*1024+1024)
	value.Messages = append(value.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: large})
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || len(loaded.Messages[1].Content) != len(large) {
		t.Fatalf("loaded large message size = %d", len(loaded.Messages[1].Content))
	}
}

func TestSequencedCheckpointIgnoresWallClockRollback(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readSnapshot(store.path(value.ID))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := Checkpoint{
		BaseMessageCount: 1, BaseRevision: snapshot.Revision, Revision: snapshot.Revision + 1,
		Messages: []provider.Message{{Role: provider.MessageRoleAssistant, Content: "after-clock-rollback"}},
	}
	payload, _ := json.Marshal(checkpoint)
	eventData, _ := json.Marshal(Event{
		Sequence: snapshot.LastEventSequence + 1, Revision: checkpoint.Revision,
		At: snapshot.UpdatedAt.Add(-time.Hour), Type: checkpointEventType, Payload: payload,
	})
	if err := os.WriteFile(store.eventPath(value.ID), append(eventData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "after-clock-rollback" {
		t.Fatalf("messages = %+v", loaded.Messages)
	}
}

func TestLoadRejectsSequencedWALGap(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Commit(value); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := readSnapshot(store.path(value.ID))
	eventData, _ := json.Marshal(Event{
		Sequence: snapshot.LastEventSequence + 2, Revision: snapshot.Revision, At: time.Now().UTC(), Type: "diagnostic",
	})
	if err := os.WriteFile(store.eventPath(value.ID), append(eventData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(value.ID); err == nil || !strings.Contains(err.Error(), "event sequence") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLegacyDeltaCheckpointStillLoads(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := readSnapshot(store.path(value.ID))
	checkpoint := Checkpoint{
		BaseMessageCount: 1,
		Messages:         []provider.Message{{Role: provider.MessageRoleAssistant, Content: "legacy-delta"}},
	}
	payload, _ := json.Marshal(checkpoint)
	eventData, _ := json.Marshal(Event{At: snapshot.UpdatedAt.Add(time.Second), Type: checkpointEventType, Payload: payload})
	if err := os.WriteFile(store.eventPath(value.ID), append(eventData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Content != "legacy-delta" {
		t.Fatalf("messages = %+v", loaded.Messages)
	}
}

func TestLegacyWALChangeAutomaticallyRefreshesCurrentIndex(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "test")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "base"}}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := readSnapshot(store.path(value.ID))
	checkpoint := Checkpoint{
		BaseMessageCount: 1,
		Messages:         []provider.Message{{Role: provider.MessageRoleAssistant, Content: "legacy-index-refresh-marker"}},
	}
	payload, _ := json.Marshal(checkpoint)
	eventData, _ := json.Marshal(Event{At: snapshot.UpdatedAt.Add(time.Second), Type: checkpointEventType, Payload: payload})
	if err := os.WriteFile(store.eventPath(value.ID), append(eventData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := store.Search("/workspace", "legacy-index-refresh-marker", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != value.ID {
		t.Fatalf("Search() after legacy WAL change = %+v", found)
	}
}

func TestConcurrentStoresPreserveIndependentIndexUpdates(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "sessions")
	first, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	firstValue, _ := first.New("/workspace", "test")
	firstValue.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "first-base"}}
	if err := first.Commit(firstValue); err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	secondValue, _ := second.New("/workspace", "test")
	secondValue.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "second-base"}}
	if err := second.Commit(secondValue); err != nil {
		t.Fatal(err)
	}
	firstValue.Messages = append(firstValue.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "first-index-marker"})
	secondValue.Messages = append(secondValue.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: "second-index-marker"})
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 2)
	wait.Add(2)
	go func() { defer wait.Done(); errorsChannel <- first.Commit(firstValue) }()
	go func() { defer wait.Done(); errorsChannel <- second.Commit(secondValue) }()
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"first-index-marker", "second-index-marker"} {
		items, _, err := reopened.ListMetadata("/workspace", marker, 10, false)
		if err != nil || len(items) != 1 {
			t.Fatalf("metadata for %q = %+v, %v", marker, items, err)
		}
	}
}

func TestListMetadataWarnsAboutStableCorruptSnapshot(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path("corrupt"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Repair(); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := store.ListMetadata("", "", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "corrupt.json") {
		t.Fatalf("warnings = %+v", warnings)
	}
}

func TestHydrateRejectsOversizedAndSymlinkedAssetsBeforeReading(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "model")
	value.Messages = []provider.Message{{
		Role:   provider.MessageRoleUser,
		Images: []provider.Image{{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("image"))}},
	}}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readSnapshot(store.path(value.ID))
	if err != nil {
		t.Fatal(err)
	}
	assetPath := filepath.Join(store.directory, filepath.FromSlash(snapshot.Messages[0].Images[0].Ref))
	if err := os.WriteFile(assetPath, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(value.ID); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Load() modified asset error = %v", err)
	}
	if err := os.Truncate(assetPath, maxSessionImageBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(value.ID); err == nil || !strings.Contains(err.Error(), "exceeds 20 MiB") {
		t.Fatalf("Load() oversized asset error = %v", err)
	}
	if err := os.Remove(assetPath); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.png")
	if err := os.WriteFile(external, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, assetPath); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if _, err := store.Load(value.ID); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Load() symlinked asset error = %v", err)
	}
}

func TestSessionImagesUseAttachmentLimitAndRejectOversizedBase64BeforeDecode(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if maxSessionImageBytes != attachment.MaxImageBytes {
		t.Fatalf("session image limit = %d, attachment limit = %d", maxSessionImageBytes, attachment.MaxImageBytes)
	}
	value, _ := store.New("/workspace", "model")
	value.Messages = []provider.Message{{
		Role: provider.MessageRoleUser,
		Images: []provider.Image{{
			MIMEType: "image/png",
			Data:     strings.Repeat("A", base64.StdEncoding.EncodedLen(maxSessionImageBytes)+4),
		}},
	}}
	if err := store.Commit(value); err == nil || !strings.Contains(err.Error(), "exceeds 20 MiB") {
		t.Fatalf("Commit() oversized base64 error = %v", err)
	}
}

func TestHydrateRejectsAggregateSessionImageCount(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.assetsDirectory, "aggregate")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	message := provider.Message{Role: provider.MessageRoleUser}
	for index := 0; index <= maxSessionHydratedImages; index++ {
		name := fmt.Sprintf("image-%02d.png", index)
		if err := os.WriteFile(filepath.Join(directory, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		message.Images = append(message.Images, provider.Image{MIMEType: "image/png", Ref: filepath.ToSlash(filepath.Join("assets", "aggregate", name))})
	}
	if err := store.hydrateMessages([]provider.Message{message}); err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("aggregate hydrate error = %v", err)
	}
}

func TestEventReadersEnforceBoundedRecordsAndStreams(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("12345\n"), 2)
	total := int64(0)
	if _, err := readEventRecordLimited(reader, &total, 32, 4); !errors.Is(err, errEventRecordTooLarge) {
		t.Fatalf("oversized event record error = %v", err)
	}

	reader = bufio.NewReaderSize(strings.NewReader("12\n34\n"), 2)
	total = 0
	if _, err := readEventRecordLimited(reader, &total, 4, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := readEventRecordLimited(reader, &total, 4, 8); !errors.Is(err, errEventLogTooLarge) {
		t.Fatalf("oversized event stream error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "tail.jsonl")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, tailErr := readLastUnterminatedLimit(file, 5, 4)
	_ = file.Close()
	if !errors.Is(tailErr, errEventRecordTooLarge) {
		t.Fatalf("oversized event tail error = %v", tailErr)
	}

	var destination bytes.Buffer
	total = 0
	if err := copyEventDataLimited(&destination, strings.NewReader("12345"), &total, 4); !errors.Is(err, errEventLogTooLarge) {
		t.Fatalf("oversized compressed stream error = %v", err)
	}
}

func TestAppendAfterArchiveKeepsCompressedHistorySegmented(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	value, _ := store.New("/workspace", "model")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "hello"}}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(value.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(value.ID, "after_archive", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.eventGzipPath(value.ID)); err != nil {
		t.Fatalf("compressed history was removed: %v", err)
	}
	plain, err := os.ReadFile(store.eventPath(value.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, []byte("after_archive")) || bytes.Contains(plain, []byte("archived")) {
		t.Fatalf("active WAL contains unexpected history: %s", plain)
	}
	events, err := store.Events(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "archived" || events[1].Type != "after_archive" {
		t.Fatalf("segmented events = %+v", events)
	}
}

func TestListDoesNotRewriteIndexForEveryLoadedSession(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 8; index++ {
		value, _ := store.New("/workspace", "model")
		value.Title = fmt.Sprintf("session %d", index)
		value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: value.Title}}
		if err := store.Save(value); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(store.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	values, err := store.ListAll("/workspace", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 8 {
		t.Fatalf("ListAll() returned %d sessions", len(values))
	}
	after, err := os.Stat(store.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("ListAll() replaced index.json while hydrating an already-current index")
	}
}

func BenchmarkStoreCommitLongSession(b *testing.B) {
	store, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	value, _ := store.New("/workspace", "benchmark")
	value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: "start"}}
	if err := store.Commit(value); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		value.Messages = append(value.Messages, provider.Message{Role: provider.MessageRoleAssistant, Content: strings.Repeat("result ", 64)})
		if err := store.Commit(value); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListSessionsWithoutIndexRewrites(b *testing.B) {
	store, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 100; index++ {
		value, _ := store.New("/workspace", "benchmark")
		value.Messages = []provider.Message{{Role: provider.MessageRoleUser, Content: strings.Repeat("message ", 32)}}
		if err := store.Save(value); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := store.ListAll("/workspace", 0, true); err != nil {
			b.Fatal(err)
		}
	}
}
