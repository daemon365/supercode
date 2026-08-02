// Package session stores resumable SuperCode conversations as local JSON.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/taskstate"
)

type Session struct {
	Version    int                `json:"version,omitempty"`
	ID         string             `json:"id"`
	ParentID   string             `json:"parent_id,omitempty"`
	Title      string             `json:"title,omitempty"`
	Workspace  string             `json:"workspace"`
	Model      string             `json:"model"`
	Mode       string             `json:"mode,omitempty"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
	Messages   []provider.Message `json:"messages"`
	Plan       taskstate.Plan     `json:"plan,omitempty"`
	Goal       *taskstate.Goal    `json:"goal,omitempty"`
	Agents     json.RawMessage    `json:"agents,omitempty"`
	ArchivedAt *time.Time         `json:"archived_at,omitempty"`
}

// Event is an append-only record used for diagnostics and crash recovery.
type Event struct {
	At      time.Time       `json:"at"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Checkpoint struct {
	Messages []provider.Message `json:"messages"`
	Plan     taskstate.Plan     `json:"plan,omitempty"`
	Goal     *taskstate.Goal    `json:"goal,omitempty"`
	Agents   json.RawMessage    `json:"agents,omitempty"`
	Title    string             `json:"title,omitempty"`
	Mode     string             `json:"mode,omitempty"`
}

type Store struct {
	directory       string
	eventsDirectory string
	assetsDirectory string
	indexPath       string
	mu              sync.Mutex
}

const CurrentVersion = 3

func NewStore(directory string) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("session directory is required")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve session directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("secure session directory: %w", err)
	}
	eventsDirectory := filepath.Join(absolute, "events")
	if err := os.MkdirAll(eventsDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create session events directory: %w", err)
	}
	if err := os.Chmod(eventsDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("secure session events directory: %w", err)
	}
	assetsDirectory := filepath.Join(absolute, "assets")
	if err := os.MkdirAll(assetsDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create session assets directory: %w", err)
	}
	store := &Store{directory: absolute, eventsDirectory: eventsDirectory, assetsDirectory: assetsDirectory, indexPath: filepath.Join(absolute, "index.json")}
	if _, err := os.Stat(store.indexPath); errors.Is(err, os.ErrNotExist) {
		if _, repairErr := store.Repair(); repairErr != nil {
			return nil, repairErr
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect session index: %w", err)
	} else if _, indexErr := store.readIndex(); indexErr != nil {
		if _, repairErr := store.Repair(); repairErr != nil {
			return nil, repairErr
		}
	}
	return store, nil
}

func (s *Store) New(workspace, model string) (Session, error) {
	now := time.Now().UTC()
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return Session{}, fmt.Errorf("create session ID: %w", err)
	}
	return Session{
		Version:   CurrentVersion,
		ID:        now.Format("20060102-150405") + "-" + hex.EncodeToString(random),
		Workspace: workspace, Model: model, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Store) Save(value Session) error {
	if !validID(value.ID) {
		return errors.New("invalid session ID")
	}
	if value.CreatedAt.IsZero() {
		value.CreatedAt = time.Now().UTC()
	}
	value.Version = CurrentVersion
	value.UpdatedAt = time.Now().UTC()
	prepared, err := s.externalizeSession(value)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(prepared, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := atomicWrite(s.path(value.ID), data, 0o600); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return s.updateIndexLocked(prepared)
}

func (s *Store) Load(id string) (Session, error) {
	if !validID(id) {
		return Session{}, errors.New("invalid session ID")
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return Session{}, fmt.Errorf("load session %q: %w", id, err)
	}
	var value Session
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Session{}, fmt.Errorf("decode session %q: %w", id, err)
	}
	if value.Version == 0 {
		value.Version = 1
	}
	if err := migrate(&value); err != nil {
		return Session{}, fmt.Errorf("migrate session %q: %w", id, err)
	}
	if checkpoint, at, ok := s.latestCheckpoint(id); ok && at.After(value.UpdatedAt) {
		value.Messages = checkpoint.Messages
		value.Plan = checkpoint.Plan
		value.Goal = checkpoint.Goal
		value.Agents = checkpoint.Agents
		value.Mode = checkpoint.Mode
		if checkpoint.Title != "" {
			value.Title = checkpoint.Title
		}
		value.UpdatedAt = at
	}
	if err := s.hydrateMessages(value.Messages); err != nil {
		return Session{}, fmt.Errorf("hydrate session %q: %w", id, err)
	}
	return value, nil
}

func (s *Store) List(workspace string, limit int) ([]Session, error) {
	return s.listIndexed(workspace, "", limit, false)
}

// ListAll is the session picker backend when archived threads should be shown.
func (s *Store) ListAll(workspace string, limit int, includeArchived bool) ([]Session, error) {
	return s.listIndexed(workspace, "", limit, includeArchived)
}

// Search uses the file-backed index and includes message text in addition to
// title, ID, model, and workspace metadata.
func (s *Store) Search(workspace, query string, limit int, includeArchived bool) ([]Session, error) {
	return s.listIndexed(workspace, query, limit, includeArchived)
}

// Append writes and fsyncs one event without rewriting the session snapshot.
func (s *Store) Append(id, eventType string, payload any) error {
	if !validID(id) {
		return errors.New("invalid session ID")
	}
	if strings.TrimSpace(eventType) == "" {
		return errors.New("session event type is required")
	}
	if checkpoint, ok := payload.(Checkpoint); ok {
		prepared, err := s.externalizeCheckpoint(id, checkpoint)
		if err != nil {
			return err
		}
		payload = prepared
	}
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode session event: %w", err)
		}
		raw = data
	}
	data, err := json.Marshal(Event{At: time.Now().UTC(), Type: eventType, Payload: raw})
	if err != nil {
		return fmt.Errorf("encode session event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.restoreCompressedEventsLocked(id); err != nil {
		return err
	}
	file, err := os.OpenFile(s.eventPath(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open session event log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("append session event: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync session event: %w", err)
	}
	return file.Close()
}

func (s *Store) Events(id string) ([]Event, error) {
	if !validID(id) {
		return nil, errors.New("invalid session ID")
	}
	file, closeReader, err := s.openEventReader(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open session event log: %w", err)
	}
	defer closeReader()
	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			// A crash can leave only the final append incomplete. Ignore that tail
			// while retaining every previously fsynced event.
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session event log: %w", err)
	}
	return events, nil
}

func (s *Store) Rename(id, title string) (Session, error) {
	value, err := s.Load(id)
	if err != nil {
		return Session{}, err
	}
	value.Title = strings.TrimSpace(title)
	if value.Title == "" {
		return Session{}, errors.New("session title is required")
	}
	if err := s.Append(id, "renamed", map[string]string{"title": value.Title}); err != nil {
		return Session{}, err
	}
	return value, s.Save(value)
}

func (s *Store) Fork(id string) (Session, error) {
	source, err := s.Load(id)
	if err != nil {
		return Session{}, err
	}
	value, err := s.New(source.Workspace, source.Model)
	if err != nil {
		return Session{}, err
	}
	value.ParentID = source.ID
	value.Title = strings.TrimSpace(source.Title + " (fork)")
	value.Mode = source.Mode
	value.Messages = append([]provider.Message(nil), source.Messages...)
	value.Plan = source.Plan
	value.Agents = append(json.RawMessage(nil), source.Agents...)
	if source.Goal != nil {
		goal := *source.Goal
		value.Goal = &goal
	}
	if err := s.Append(value.ID, "forked", map[string]string{"parent_id": source.ID}); err != nil {
		return Session{}, err
	}
	return value, s.Save(value)
}

func (s *Store) Archive(id string) (Session, error) {
	value, err := s.Load(id)
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	value.ArchivedAt = &now
	if err := s.Append(id, "archived", nil); err != nil {
		return Session{}, err
	}
	if err := s.Save(value); err != nil {
		return Session{}, err
	}
	if err := s.compressEvents(id); err != nil {
		return Session{}, err
	}
	return value, nil
}

func (s *Store) Delete(id string) error {
	if !validID(id) {
		return errors.New("invalid session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete session: %w", err)
	}
	if err := os.Remove(s.eventPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete session event log: %w", err)
	}
	if err := os.Remove(s.eventGzipPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete compressed session event log: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(s.assetsDirectory, id)); err != nil {
		return fmt.Errorf("delete session assets: %w", err)
	}
	return s.deleteIndexLocked(id)
}

func (s *Store) latestCheckpoint(id string) (Checkpoint, time.Time, bool) {
	events, err := s.Events(id)
	if err != nil {
		return Checkpoint{}, time.Time{}, false
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != "checkpoint" {
			continue
		}
		var checkpoint Checkpoint
		if json.Unmarshal(events[index].Payload, &checkpoint) == nil {
			return checkpoint, events[index].At, true
		}
	}
	return Checkpoint{}, time.Time{}, false
}

func (s *Store) Latest(workspace string) (Session, error) {
	values, err := s.List(workspace, 1)
	if err != nil {
		return Session{}, err
	}
	if len(values) == 0 {
		return Session{}, os.ErrNotExist
	}
	return values[0], nil
}

func (s *Store) path(id string) string      { return filepath.Join(s.directory, id+".json") }
func (s *Store) eventPath(id string) string { return filepath.Join(s.eventsDirectory, id+".jsonl") }
func (s *Store) eventGzipPath(id string) string {
	return filepath.Join(s.eventsDirectory, id+".jsonl.gz")
}

func validID(id string) bool {
	if id == "" || len(id) > 80 {
		return false
	}
	for _, character := range id {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}
