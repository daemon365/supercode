// Package session stores resumable SuperCode conversations as local JSON.
package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/taskstate"
)

type Session struct {
	Version           int                `json:"version,omitempty"`
	ID                string             `json:"id"`
	ParentID          string             `json:"parent_id,omitempty"`
	Title             string             `json:"title,omitempty"`
	Workspace         string             `json:"workspace"`
	Model             string             `json:"model"`
	Mode              string             `json:"mode,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	Revision          uint64             `json:"revision,omitempty"`
	LastEventSequence uint64             `json:"last_event_sequence,omitempty"`
	Messages          []provider.Message `json:"messages"`
	Plan              taskstate.Plan     `json:"plan,omitempty"`
	Goal              *taskstate.Goal    `json:"goal,omitempty"`
	Agents            json.RawMessage    `json:"agents,omitempty"`
	ArchivedAt        *time.Time         `json:"archived_at,omitempty"`
}

// Event is an append-only record used for diagnostics and crash recovery.
type Event struct {
	Sequence uint64          `json:"sequence,omitempty"`
	Revision uint64          `json:"revision,omitempty"`
	At       time.Time       `json:"at"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

type Checkpoint struct {
	BaseMessageCount int                `json:"base_message_count,omitempty"`
	BaseRevision     uint64             `json:"base_revision,omitempty"`
	Revision         uint64             `json:"revision,omitempty"`
	Replace          bool               `json:"replace,omitempty"`
	Messages         []provider.Message `json:"messages"`
	Plan             taskstate.Plan     `json:"plan,omitempty"`
	Goal             *taskstate.Goal    `json:"goal,omitempty"`
	Agents           json.RawMessage    `json:"agents,omitempty"`
	Title            string             `json:"title,omitempty"`
	Mode             string             `json:"mode,omitempty"`
}

type Store struct {
	directory       string
	eventsDirectory string
	assetsDirectory string
	locksDirectory  string
	indexPath       string
	indexLockPath   string
	storeLockPath   string
	mu              sync.Mutex
	commits         map[string]commitState
}

type commitState struct {
	messages         []provider.Message
	revision         uint64
	sequence         uint64
	deltas           int
	walBytes         int64
	snapshotSize     int64
	snapshotModified int64
}

const CurrentVersion = 4

const (
	checkpointEventType     = "checkpoint_delta"
	checkpointSnapshotEvery = 64
	checkpointSnapshotBytes = 4 * 1024 * 1024
	checkpointSnapshotMax   = 64 * 1024 * 1024
)

var ErrSessionConflict = errors.New("session changed in another process")

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
	if err := os.Chmod(assetsDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("secure session assets directory: %w", err)
	}
	locksDirectory := filepath.Join(absolute, "locks")
	if err := os.MkdirAll(locksDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create session locks directory: %w", err)
	}
	if err := os.Chmod(locksDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("secure session locks directory: %w", err)
	}
	store := &Store{
		directory: absolute, eventsDirectory: eventsDirectory, assetsDirectory: assetsDirectory,
		locksDirectory: locksDirectory, indexPath: filepath.Join(absolute, "index.json"),
		indexLockPath: filepath.Join(locksDirectory, "index.lock"), storeLockPath: filepath.Join(locksDirectory, "store.lock"),
		commits: make(map[string]commitState),
	}
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
	value := Session{
		Version:   CurrentVersion,
		ID:        now.Format("20060102-150405") + "-" + hex.EncodeToString(random),
		Workspace: workspace, Model: model, CreatedAt: now, UpdatedAt: now,
	}
	s.mu.Lock()
	s.commits[value.ID] = commitState{}
	s.mu.Unlock()
	return value, nil
}

func (s *Store) Save(value Session) error {
	if !validID(value.ID) {
		return errors.New("invalid session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(s.storeLockPath, func() error {
		return withFileLock(s.sessionLockPath(value.ID), func() error {
			state, err := s.currentStateLocked(value.ID)
			if err != nil {
				return err
			}
			if err := s.refreshAndVerifyStateLocked(value.ID, &state); err != nil {
				return err
			}
			value.Revision = state.revision + 1
			value.LastEventSequence = state.sequence
			return s.writeSnapshotLocked(value, &state, true)
		})
	})
}

// Commit durably records a completed turn without rewriting the full session
// history every time. A full snapshot is written on the first commit and then
// periodically; intervening commits append only the new messages and update
// the lightweight search index. Load replays those deltas after the snapshot.
func (s *Store) Commit(value Session) error {
	if !validID(value.ID) {
		return errors.New("invalid session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(s.storeLockPath, func() error {
		return withFileLock(s.sessionLockPath(value.ID), func() error {
			state, err := s.currentStateLocked(value.ID)
			if err != nil {
				return err
			}
			if err := s.refreshAndVerifyStateLocked(value.ID, &state); err != nil {
				return err
			}
			if state.snapshotSize == 0 {
				value.Revision = state.revision + 1
				value.LastEventSequence = state.sequence
				return s.writeSnapshotLocked(value, &state, true)
			}
			if len(value.Messages) < len(state.messages) || !messagesPrefixEqual(state.messages, value.Messages) {
				value.Revision = state.revision + 1
				value.LastEventSequence = state.sequence
				return s.writeSnapshotLocked(value, &state, true)
			}

			value.Version = CurrentVersion
			if value.CreatedAt.IsZero() {
				value.CreatedAt = time.Now().UTC()
			}
			value.UpdatedAt = time.Now().UTC()
			value.Revision = state.revision + 1
			value.LastEventSequence = state.sequence + 1
			checkpoint := Checkpoint{
				BaseMessageCount: len(state.messages), BaseRevision: state.revision, Revision: value.Revision,
				Messages: append([]provider.Message(nil), value.Messages[len(state.messages):]...),
				Plan:     value.Plan, Goal: value.Goal, Agents: value.Agents, Title: value.Title, Mode: value.Mode,
			}
			prepared, err := s.externalizeCheckpoint(value.ID, checkpoint)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(prepared)
			if err != nil {
				return fmt.Errorf("encode session checkpoint: %w", err)
			}
			event := Event{
				Sequence: value.LastEventSequence, Revision: value.Revision, At: value.UpdatedAt,
				Type: checkpointEventType, Payload: payload,
			}
			written, err := s.appendEventRecordLocked(value.ID, event)
			if err != nil {
				return err
			}
			previousMessageCount := len(state.messages)
			state.messages = append(state.messages, cloneMessages(value.Messages[previousMessageCount:])...)
			state.revision = value.Revision
			state.sequence = value.LastEventSequence
			state.deltas++
			state.walBytes += written
			s.commits[value.ID] = state
			if err := s.writeHeadLocked(value.ID, state); err != nil {
				return err
			}
			if shouldRefreshSnapshot(state) {
				return s.writeSnapshotLocked(value, &state, true)
			}
			return s.updateIndexCommitLocked(value, previousMessageCount)
		})
	})
}

func (s *Store) Load(id string) (Session, error) {
	if !validID(id) {
		return Session{}, errors.New("invalid session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Session
	err := withFileLock(s.storeLockPath, func() error {
		return withFileLock(s.sessionLockPath(id), func() error {
			value, state, err := s.loadSessionDiskLocked(id, true)
			if err != nil {
				return err
			}
			result = value
			s.commits[id] = state
			if err := s.reconcileHeadLocked(id, state); err != nil {
				return err
			}
			return s.reconcileIndexLocked(value)
		})
	})
	return result, err
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(s.storeLockPath, func() error {
		return withFileLock(s.sessionLockPath(id), func() error {
			state, err := s.currentStateLocked(id)
			if err != nil {
				return err
			}
			if err := s.refreshAndVerifyStateLocked(id, &state); err != nil {
				return err
			}
			var raw json.RawMessage
			if checkpoint, ok := payload.(Checkpoint); ok {
				checkpoint.BaseRevision = state.revision
				checkpoint.Revision = state.revision + 1
				checkpoint.Replace = true
				prepared, err := s.externalizeCheckpoint(id, checkpoint)
				if err != nil {
					return err
				}
				data, err := json.Marshal(prepared)
				if err != nil {
					return fmt.Errorf("encode session event: %w", err)
				}
				raw = data
				state.messages = cloneMessages(checkpoint.Messages)
				state.revision = checkpoint.Revision
				state.deltas = 0
			} else if payload != nil {
				data, err := json.Marshal(payload)
				if err != nil {
					return fmt.Errorf("encode session event: %w", err)
				}
				raw = data
			}
			state.sequence++
			event := Event{Sequence: state.sequence, Revision: state.revision, At: time.Now().UTC(), Type: eventType, Payload: raw}
			written, err := s.appendEventRecordLocked(id, event)
			if err != nil {
				return err
			}
			state.walBytes += written
			s.commits[id] = state
			return s.writeHeadLocked(id, state)
		})
	})
}

func (s *Store) Events(id string) ([]Event, error) {
	if !validID(id) {
		return nil, errors.New("invalid session ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []Event
	err := withFileLock(s.storeLockPath, func() error {
		return withFileLock(s.sessionLockPath(id), func() error {
			var err error
			events, err = s.readEventsLocked(id)
			return err
		})
	})
	return events, err
}

func (s *Store) currentStateLocked(id string) (commitState, error) {
	if state, ok := s.commits[id]; ok {
		return state, nil
	}
	if _, err := os.Stat(s.path(id)); err == nil {
		_, state, err := s.loadSessionDiskLocked(id, true)
		if err != nil {
			return commitState{}, err
		}
		s.commits[id] = state
		return state, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return commitState{}, err
	}
	state := commitState{}
	if head, ok, err := s.readHeadLocked(id); err != nil {
		return commitState{}, err
	} else if ok {
		state.sequence, state.revision = head.Sequence, head.Revision
	}
	if event, ok, err := s.lastPlainEventLocked(id); err != nil {
		return commitState{}, err
	} else if ok {
		state.sequence = max(state.sequence, event.Sequence)
		state.revision = max(state.revision, event.Revision)
	}
	state.walBytes = eventLogSize(s.eventPath(id))
	s.commits[id] = state
	return state, nil
}

func (s *Store) refreshAndVerifyStateLocked(id string, state *commitState) error {
	size, modified, exists, err := snapshotFingerprint(s.path(id))
	if err != nil {
		return err
	}
	if state.snapshotSize == 0 {
		if exists {
			return fmt.Errorf("%w: snapshot was created externally", ErrSessionConflict)
		}
	} else if !exists || size != state.snapshotSize || modified != state.snapshotModified {
		return fmt.Errorf("%w: snapshot was replaced externally", ErrSessionConflict)
	}
	diskRevision, diskSequence := state.revision, state.sequence
	if head, ok, err := s.readHeadLocked(id); err != nil {
		return err
	} else if ok {
		diskRevision = max(diskRevision, head.Revision)
		diskSequence = max(diskSequence, head.Sequence)
	}
	if event, ok, err := s.lastPlainEventLocked(id); err != nil {
		return err
	} else if ok {
		diskRevision = max(diskRevision, event.Revision)
		diskSequence = max(diskSequence, event.Sequence)
	}
	if diskRevision != state.revision {
		return fmt.Errorf("%w: expected revision %d, found %d", ErrSessionConflict, state.revision, diskRevision)
	}
	state.sequence = diskSequence
	state.walBytes = eventLogSize(s.eventPath(id))
	return nil
}

func (s *Store) loadSessionDiskLocked(id string, hydrate bool) (Session, commitState, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return Session{}, commitState{}, fmt.Errorf("load session %q: %w", id, err)
	}
	var value Session
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Session{}, commitState{}, fmt.Errorf("decode session %q: %w", id, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Session{}, commitState{}, fmt.Errorf("decode session %q trailing data: %w", id, err)
	}
	if value.ID != id {
		return Session{}, commitState{}, corruption("snapshot %q contains ID %q", id, value.ID)
	}
	if value.Version == 0 {
		value.Version = 1
	}
	if err := migrate(&value); err != nil {
		return Session{}, commitState{}, fmt.Errorf("migrate session %q: %w", id, err)
	}
	events, err := s.readEventsLocked(id)
	if err != nil {
		return Session{}, commitState{}, fmt.Errorf("recover session %q: %w", id, err)
	}
	deltas, err := recoverCheckpoints(&value, events)
	if err != nil {
		return Session{}, commitState{}, fmt.Errorf("recover session %q: %w", id, err)
	}
	size, modified, _, err := snapshotFingerprint(s.path(id))
	if err != nil {
		return Session{}, commitState{}, err
	}
	if hydrate {
		if err := s.hydrateMessages(value.Messages); err != nil {
			return Session{}, commitState{}, fmt.Errorf("hydrate session %q: %w", id, err)
		}
	}
	state := commitState{
		messages: cloneMessages(value.Messages), revision: value.Revision, sequence: value.LastEventSequence,
		deltas: deltas, walBytes: eventLogSize(s.eventPath(id)), snapshotSize: size, snapshotModified: modified,
	}
	return value, state, nil
}

func (s *Store) writeSnapshotLocked(value Session, state *commitState, rotate bool) error {
	if value.CreatedAt.IsZero() {
		value.CreatedAt = time.Now().UTC()
	}
	value.Version = CurrentVersion
	value.UpdatedAt = time.Now().UTC()
	value.LastEventSequence = state.sequence
	if value.Revision == 0 {
		value.Revision = state.revision
	}
	prepared, err := s.externalizeSession(value)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(prepared, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	if err := atomicWrite(s.path(value.ID), data, 0o600); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	size, modified, _, err := snapshotFingerprint(s.path(value.ID))
	if err != nil {
		return err
	}
	state.messages = cloneMessages(value.Messages)
	state.revision = value.Revision
	state.sequence = value.LastEventSequence
	state.deltas = 0
	state.snapshotSize, state.snapshotModified = size, modified
	s.commits[value.ID] = *state
	if err := s.writeHeadLocked(value.ID, *state); err != nil {
		return err
	}
	if rotate {
		if err := s.rotateEventsLocked(value.ID); err != nil {
			return fmt.Errorf("rotate session events: %w", err)
		}
		state.walBytes = eventLogSize(s.eventPath(value.ID))
		s.commits[value.ID] = *state
	}
	return s.updateIndexLocked(prepared)
}

func shouldRefreshSnapshot(state commitState) bool {
	byteThreshold := min(max(int64(checkpointSnapshotBytes), state.snapshotSize/2), int64(checkpointSnapshotMax))
	deltaThreshold := max(checkpointSnapshotEvery, len(state.messages)/2)
	return state.walBytes >= byteThreshold || state.deltas >= deltaThreshold
}

func messagesPrefixEqual(previous, current []provider.Message) bool {
	if len(current) < len(previous) {
		return false
	}
	for index := range previous {
		left, right := previous[index], current[index]
		if left.Role != right.Role || left.Content != right.Content || left.ToolCallID != right.ToolCallID ||
			!slices.Equal(left.ToolCalls, right.ToolCalls) || !slices.Equal(left.Images, right.Images) {
			return false
		}
	}
	return true
}

func snapshotFingerprint(path string) (size, modified int64, exists bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return info.Size(), info.ModTime().UnixNano(), true, nil
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
	return withFileLock(s.storeLockPath, func() error {
		return withFileLock(s.sessionLockPath(id), func() error {
			if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("delete session: %w", err)
			}
			if err := syncDirectory(s.directory); err != nil {
				return fmt.Errorf("sync session directory after delete: %w", err)
			}
			for _, path := range []string{s.eventPath(id), s.eventGzipPath(id), s.headPath(id)} {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("delete session event data: %w", err)
				}
			}
			if err := syncDirectory(s.eventsDirectory); err != nil {
				return fmt.Errorf("sync session events directory after delete: %w", err)
			}
			if err := os.RemoveAll(filepath.Join(s.assetsDirectory, id)); err != nil {
				return fmt.Errorf("delete session assets: %w", err)
			}
			if err := syncDirectory(s.assetsDirectory); err != nil {
				return fmt.Errorf("sync session assets directory after delete: %w", err)
			}
			delete(s.commits, id)
			return s.deleteIndexLocked(id)
		})
	})
}

func recoverCheckpoints(value *Session, events []Event) (int, error) {
	deltas := 0
	expectedSequence := value.LastEventSequence
	for _, event := range events {
		if event.Sequence == 0 {
			if event.Type != "checkpoint" && event.Type != checkpointEventType || !event.At.After(value.UpdatedAt) {
				continue
			}
			var checkpoint Checkpoint
			if err := json.Unmarshal(event.Payload, &checkpoint); err != nil {
				return 0, corruption("legacy checkpoint payload: %v", err)
			}
			if event.Type == "checkpoint" {
				value.Messages = checkpoint.Messages
				deltas = 0
			} else {
				if checkpoint.BaseMessageCount != len(value.Messages) {
					return 0, corruption("legacy checkpoint base is %d, expected %d", checkpoint.BaseMessageCount, len(value.Messages))
				}
				value.Messages = append(value.Messages, checkpoint.Messages...)
				deltas++
			}
			value.Revision++
			applyCheckpointState(value, checkpoint, true)
			value.UpdatedAt = event.At
			continue
		}
		if event.Sequence <= value.LastEventSequence {
			continue
		}
		if event.Sequence != expectedSequence+1 {
			return 0, corruption("event sequence is %d, expected %d", event.Sequence, expectedSequence+1)
		}
		expectedSequence = event.Sequence
		value.LastEventSequence = event.Sequence
		if event.Type != "checkpoint" && event.Type != checkpointEventType {
			continue
		}
		var checkpoint Checkpoint
		if err := json.Unmarshal(event.Payload, &checkpoint); err != nil {
			return 0, corruption("checkpoint sequence %d payload: %v", event.Sequence, err)
		}
		if checkpoint.BaseRevision != value.Revision {
			return 0, corruption("checkpoint sequence %d base revision is %d, expected %d", event.Sequence, checkpoint.BaseRevision, value.Revision)
		}
		targetRevision := checkpoint.Revision
		if targetRevision == 0 {
			targetRevision = event.Revision
		}
		if targetRevision != value.Revision+1 || event.Revision != targetRevision {
			return 0, corruption("checkpoint sequence %d revision is %d, expected %d", event.Sequence, targetRevision, value.Revision+1)
		}
		if event.Type == "checkpoint" || checkpoint.Replace {
			value.Messages = checkpoint.Messages
			deltas = 0
		} else {
			if checkpoint.BaseMessageCount != len(value.Messages) {
				return 0, corruption("checkpoint sequence %d base message count is %d, expected %d", event.Sequence, checkpoint.BaseMessageCount, len(value.Messages))
			}
			value.Messages = append(value.Messages, checkpoint.Messages...)
			deltas++
		}
		value.Revision = targetRevision
		applyCheckpointState(value, checkpoint, false)
		value.UpdatedAt = event.At
	}
	return deltas, nil
}

func applyCheckpointState(value *Session, checkpoint Checkpoint, legacy bool) {
	value.Plan = checkpoint.Plan
	value.Goal = checkpoint.Goal
	value.Agents = checkpoint.Agents
	value.Mode = checkpoint.Mode
	if !legacy || checkpoint.Title != "" {
		value.Title = checkpoint.Title
	}
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
func (s *Store) headPath(id string) string { return filepath.Join(s.eventsDirectory, id+".head.json") }
func (s *Store) sessionLockPath(id string) string {
	return filepath.Join(s.locksDirectory, id+".lock")
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
