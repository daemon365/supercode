package session

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
