package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const indexVersion = 2

type indexFile struct {
	Version  int                          `json:"version"`
	Sessions map[string]indexEntry        `json:"sessions"`
	Invalid  map[string]invalidIndexEntry `json:"invalid,omitempty"`
}

type invalidIndexEntry struct {
	SnapshotSize            int64 `json:"snapshot_size"`
	SnapshotModified        int64 `json:"snapshot_modified"`
	EventSize               int64 `json:"event_size,omitempty"`
	EventModified           int64 `json:"event_modified,omitempty"`
	CompressedEventSize     int64 `json:"compressed_event_size,omitempty"`
	CompressedEventModified int64 `json:"compressed_event_modified,omitempty"`
}

type indexEntry struct {
	ID                      string     `json:"id"`
	ParentID                string     `json:"parent_id,omitempty"`
	Workspace               string     `json:"workspace"`
	Model                   string     `json:"model"`
	Title                   string     `json:"title,omitempty"`
	UpdatedAt               time.Time  `json:"updated_at"`
	ArchivedAt              *time.Time `json:"archived_at,omitempty"`
	SearchText              string     `json:"search_text,omitempty"`
	MessageCount            int        `json:"message_count,omitempty"`
	Revision                uint64     `json:"revision,omitempty"`
	LastEventSequence       uint64     `json:"last_event_sequence,omitempty"`
	SnapshotSize            int64      `json:"snapshot_size,omitempty"`
	SnapshotModified        int64      `json:"snapshot_modified,omitempty"`
	EventSize               int64      `json:"event_size,omitempty"`
	EventModified           int64      `json:"event_modified,omitempty"`
	CompressedEventSize     int64      `json:"compressed_event_size,omitempty"`
	CompressedEventModified int64      `json:"compressed_event_modified,omitempty"`
}

// Metadata is the lightweight session-picker/search representation. It never
// hydrates messages or image assets; Load resolves the selected session.
type Metadata struct {
	ID           string
	ParentID     string
	Title        string
	Workspace    string
	Model        string
	SearchText   string
	UpdatedAt    time.Time
	ArchivedAt   *time.Time
	MessageCount int
}

type RepairReport struct {
	Scanned int      `json:"scanned"`
	Indexed int      `json:"indexed"`
	Invalid []string `json:"invalid,omitempty"`
}

// Repair rebuilds index.json from authoritative snapshots plus every newer
// recovery event. Invalid sessions are reported and left untouched.
func (s *Store) Repair() (RepairReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var report RepairReport
	err := withFileLock(s.storeLockPath, func() error {
		var err error
		report, err = s.repairLocked()
		return err
	})
	return report, err
}

func (s *Store) repairLocked() (RepairReport, error) {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return RepairReport{}, fmt.Errorf("scan sessions for repair: %w", err)
	}
	index := indexFile{Version: indexVersion, Sessions: make(map[string]indexEntry), Invalid: make(map[string]invalidIndexEntry)}
	report := RepairReport{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == filepath.Base(s.indexPath) {
			continue
		}
		report.Scanned++
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			report.Invalid = append(report.Invalid, entry.Name())
			index.Invalid[entry.Name()] = s.invalidIndexEntry(id)
			continue
		}
		var value Session
		loadErr := withFileLock(s.sessionLockPath(id), func() error {
			var err error
			value, _, err = s.loadSessionDiskLocked(id, false)
			return err
		})
		if loadErr != nil {
			report.Invalid = append(report.Invalid, entry.Name())
			index.Invalid[entry.Name()] = s.invalidIndexEntry(id)
			continue
		}
		index.Sessions[id] = s.makeIndexEntry(value)
		report.Indexed++
	}
	sort.Strings(report.Invalid)
	if err := s.writeIndexLocked(index); err != nil {
		return RepairReport{}, err
	}
	return report, nil
}

// ListMetadata performs index-only filtering and sorting. warnings contains
// corrupt snapshot names skipped by an automatic repair.
func (s *Store) ListMetadata(workspace, query string, limit int, includeArchived bool) (items []Metadata, warnings []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = withFileLock(s.storeLockPath, func() error {
		var innerErr error
		items, warnings, innerErr = s.listMetadataLocked(workspace, query, limit, includeArchived)
		return innerErr
	})
	return items, warnings, err
}

func (s *Store) listMetadataLocked(workspace, query string, limit int, includeArchived bool) (items []Metadata, warnings []string, err error) {
	index, err := s.readIndexLocked()
	if err != nil {
		report, repairErr := s.repairLocked()
		if repairErr != nil {
			return nil, nil, repairErr
		}
		warnings = repairWarnings(report)
		index, err = s.readIndexLocked()
		if err != nil {
			return nil, warnings, err
		}
	}
	if s.indexCoverageChangedLocked(index) {
		report, repairErr := s.repairLocked()
		if repairErr != nil {
			return nil, warnings, repairErr
		}
		warnings = append(warnings, repairWarnings(report)...)
		index, err = s.readIndexLocked()
		if err != nil {
			return nil, warnings, err
		}
	}
	words := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	entries := make([]indexEntry, 0, len(index.Sessions))
	for _, entry := range index.Sessions {
		if (workspace != "" && entry.Workspace != workspace) || (!includeArchived && entry.ArchivedAt != nil) {
			continue
		}
		haystack := strings.ToLower(entry.SearchText)
		matched := true
		for _, word := range words {
			if !strings.Contains(haystack, word) {
				matched = false
				break
			}
		}
		if matched {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].UpdatedAt.After(entries[right].UpdatedAt) })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	items = make([]Metadata, 0, len(entries))
	for _, entry := range entries {
		items = append(items, Metadata{
			ID: entry.ID, ParentID: entry.ParentID, Title: entry.Title, Workspace: entry.Workspace,
			Model: entry.Model, SearchText: entry.SearchText, UpdatedAt: entry.UpdatedAt,
			ArchivedAt: entry.ArchivedAt, MessageCount: entry.MessageCount,
		})
	}
	warnings = mergeWarnings(warnings, warningsFromInvalid(index))
	return items, warnings, nil
}

func mergeWarnings(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, group := range groups {
		for _, warning := range group {
			if _, exists := seen[warning]; exists {
				continue
			}
			seen[warning] = struct{}{}
			result = append(result, warning)
		}
	}
	sort.Strings(result)
	return result
}

func repairWarnings(report RepairReport) []string {
	warnings := make([]string, 0, len(report.Invalid))
	for _, name := range report.Invalid {
		warnings = append(warnings, "Skipped corrupt session "+name)
	}
	return warnings
}

func warningsFromInvalid(index indexFile) []string {
	names := make([]string, 0, len(index.Invalid))
	for name := range index.Invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	warnings := make([]string, 0, len(names))
	for _, name := range names {
		warnings = append(warnings, "Skipped corrupt session "+name)
	}
	return warnings
}

func (s *Store) listIndexed(workspace, query string, limit int, includeArchived bool) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var values []Session
	err := withFileLock(s.storeLockPath, func() error {
		metadata, _, err := s.listMetadataLocked(workspace, query, limit, includeArchived)
		if err != nil {
			return err
		}
		values = make([]Session, 0, len(metadata))
		for _, item := range metadata {
			var value Session
			var state commitState
			if err := withFileLock(s.sessionLockPath(item.ID), func() error {
				var loadErr error
				value, state, loadErr = s.loadSessionDiskLocked(item.ID, true)
				return loadErr
			}); err != nil {
				return err
			}
			s.commits[item.ID] = state
			values = append(values, value)
		}
		return nil
	})
	return values, err
}

func (s *Store) indexCoverageChangedLocked(index indexFile) bool {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return true
	}
	seen := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == filepath.Base(s.indexPath) {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		indexed, ok := index.Sessions[id]
		if !ok {
			invalid, knownInvalid := index.Invalid[entry.Name()]
			size, modified, _, statErr := snapshotFingerprint(s.path(id))
			eventSize, eventModified, _, eventErr := snapshotFingerprint(s.eventPath(id))
			compressedSize, compressedModified, _, compressedErr := snapshotFingerprint(s.eventGzipPath(id))
			if !knownInvalid || statErr != nil || eventErr != nil || compressedErr != nil ||
				invalid.SnapshotSize != size || invalid.SnapshotModified != modified ||
				invalid.EventSize != eventSize || invalid.EventModified != eventModified ||
				invalid.CompressedEventSize != compressedSize || invalid.CompressedEventModified != compressedModified {
				return true
			}
			seen++
			continue
		}
		size, modified, _, statErr := snapshotFingerprint(s.path(id))
		eventSize, eventModified, _, eventErr := snapshotFingerprint(s.eventPath(id))
		compressedSize, compressedModified, _, compressedErr := snapshotFingerprint(s.eventGzipPath(id))
		if statErr != nil || eventErr != nil || compressedErr != nil || indexed.SnapshotSize != size || indexed.SnapshotModified != modified ||
			indexed.EventSize != eventSize || indexed.EventModified != eventModified ||
			indexed.CompressedEventSize != compressedSize || indexed.CompressedEventModified != compressedModified {
			return true
		}
		seen++
	}
	return seen != len(index.Sessions)+len(index.Invalid)
}

func (s *Store) readIndex() (indexFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readIndexLocked()
}

func (s *Store) readIndexLocked() (result indexFile, resultErr error) {
	err := withFileLock(s.indexLockPath, func() error {
		result, resultErr = s.readIndexFile()
		return resultErr
	})
	if err != nil {
		return indexFile{}, err
	}
	return result, nil
}

func (s *Store) readIndexFile() (indexFile, error) {
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		return indexFile{}, fmt.Errorf("read session index: %w", err)
	}
	var value indexFile
	if err := json.Unmarshal(data, &value); err != nil {
		return indexFile{}, fmt.Errorf("decode session index: %w", err)
	}
	if value.Version != indexVersion || value.Sessions == nil {
		return indexFile{}, errors.New("session index has an unsupported format")
	}
	if value.Invalid == nil {
		value.Invalid = make(map[string]invalidIndexEntry)
	}
	return value, nil
}

func (s *Store) updateIndexLocked(value Session) error {
	return withFileLock(s.indexLockPath, func() error {
		index, err := s.readIndexFile()
		if err != nil {
			index = indexFile{Version: indexVersion, Sessions: make(map[string]indexEntry), Invalid: make(map[string]invalidIndexEntry)}
		}
		index.Sessions[value.ID] = s.makeIndexEntry(value)
		delete(index.Invalid, value.ID+".json")
		return s.writeIndexFile(index)
	})
}

func (s *Store) reconcileIndexLocked(value Session) error {
	return withFileLock(s.indexLockPath, func() error {
		index, err := s.readIndexFile()
		if err != nil {
			index = indexFile{Version: indexVersion, Sessions: make(map[string]indexEntry), Invalid: make(map[string]invalidIndexEntry)}
		}
		entry, exists := index.Sessions[value.ID]
		size, modified, _, statErr := snapshotFingerprint(s.path(value.ID))
		eventSize, eventModified, _, eventErr := snapshotFingerprint(s.eventPath(value.ID))
		compressedSize, compressedModified, _, compressedErr := snapshotFingerprint(s.eventGzipPath(value.ID))
		if err := errors.Join(statErr, eventErr, compressedErr); err != nil {
			return err
		}
		if exists && entry.ID == value.ID && entry.ParentID == value.ParentID && entry.Workspace == value.Workspace &&
			entry.Model == value.Model && entry.Title == value.Title && entry.UpdatedAt.Equal(value.UpdatedAt) &&
			timesEqual(entry.ArchivedAt, value.ArchivedAt) && entry.MessageCount == len(value.Messages) &&
			entry.Revision == value.Revision && entry.LastEventSequence == value.LastEventSequence &&
			entry.SnapshotSize == size && entry.SnapshotModified == modified &&
			entry.EventSize == eventSize && entry.EventModified == eventModified &&
			entry.CompressedEventSize == compressedSize && entry.CompressedEventModified == compressedModified {
			return nil
		}
		index.Sessions[value.ID] = s.makeIndexEntry(value)
		delete(index.Invalid, value.ID+".json")
		return s.writeIndexFile(index)
	})
}

func timesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func (s *Store) updateIndexCommitLocked(value Session, previousMessageCount int) error {
	return withFileLock(s.indexLockPath, func() error {
		index, err := s.readIndexFile()
		if err != nil {
			index = indexFile{Version: indexVersion, Sessions: make(map[string]indexEntry), Invalid: make(map[string]invalidIndexEntry)}
		}
		entry, exists := index.Sessions[value.ID]
		if !exists || previousMessageCount < 0 || previousMessageCount > len(value.Messages) || entry.MessageCount != previousMessageCount {
			entry = s.makeIndexEntry(value)
		} else {
			parts := []string{value.Title}
			for _, message := range value.Messages[previousMessageCount:] {
				if strings.TrimSpace(message.Content) != "" {
					parts = append(parts, message.Content)
				}
			}
			entry.ID, entry.ParentID, entry.Workspace, entry.Model, entry.Title = value.ID, value.ParentID, value.Workspace, value.Model, value.Title
			entry.UpdatedAt, entry.ArchivedAt, entry.MessageCount = value.UpdatedAt, value.ArchivedAt, len(value.Messages)
			entry.Revision, entry.LastEventSequence = value.Revision, value.LastEventSequence
			addition := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
			if addition != "" {
				entry.SearchText = boundSearchText(strings.TrimSpace(entry.SearchText + " " + addition))
			}
			s.decorateStorageFingerprint(value.ID, &entry)
		}
		index.Sessions[value.ID] = entry
		delete(index.Invalid, value.ID+".json")
		return s.writeIndexFile(index)
	})
}

func (s *Store) deleteIndexLocked(id string) error {
	return withFileLock(s.indexLockPath, func() error {
		index, err := s.readIndexFile()
		if err != nil {
			index = indexFile{Version: indexVersion, Sessions: make(map[string]indexEntry), Invalid: make(map[string]invalidIndexEntry)}
		}
		delete(index.Sessions, id)
		delete(index.Invalid, id+".json")
		return s.writeIndexFile(index)
	})
}

func (s *Store) writeIndexLocked(index indexFile) error {
	return withFileLock(s.indexLockPath, func() error { return s.writeIndexFile(index) })
}

func (s *Store) writeIndexFile(index indexFile) error {
	index.Version = indexVersion
	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("encode session index: %w", err)
	}
	if err := atomicWrite(s.indexPath, data, 0o600); err != nil {
		return fmt.Errorf("write session index: %w", err)
	}
	return nil
}

func (s *Store) makeIndexEntry(value Session) indexEntry {
	parts := []string{value.ID, value.Title, value.Workspace, value.Model}
	for _, message := range value.Messages {
		if strings.TrimSpace(message.Content) != "" {
			parts = append(parts, message.Content)
		}
	}
	entry := indexEntry{
		ID: value.ID, ParentID: value.ParentID, Workspace: value.Workspace, Model: value.Model,
		Title: value.Title, UpdatedAt: value.UpdatedAt, ArchivedAt: value.ArchivedAt,
		SearchText:   boundSearchText(strings.Join(strings.Fields(strings.Join(parts, " ")), " ")),
		MessageCount: len(value.Messages), Revision: value.Revision, LastEventSequence: value.LastEventSequence,
	}
	s.decorateStorageFingerprint(value.ID, &entry)
	return entry
}

func (s *Store) decorateStorageFingerprint(id string, entry *indexEntry) {
	entry.SnapshotSize, entry.SnapshotModified, _, _ = snapshotFingerprint(s.path(id))
	entry.EventSize, entry.EventModified, _, _ = snapshotFingerprint(s.eventPath(id))
	entry.CompressedEventSize, entry.CompressedEventModified, _, _ = snapshotFingerprint(s.eventGzipPath(id))
}

func (s *Store) invalidIndexEntry(id string) invalidIndexEntry {
	size, modified, _, _ := snapshotFingerprint(s.path(id))
	eventSize, eventModified, _, _ := snapshotFingerprint(s.eventPath(id))
	compressedSize, compressedModified, _, _ := snapshotFingerprint(s.eventGzipPath(id))
	return invalidIndexEntry{
		SnapshotSize: size, SnapshotModified: modified,
		EventSize: eventSize, EventModified: eventModified,
		CompressedEventSize: compressedSize, CompressedEventModified: compressedModified,
	}
}

func boundSearchText(search string) string {
	const maximum = 128 * 1024
	if len(search) <= maximum {
		return search
	}
	const separator = " … "
	headSize := maximum / 4
	tailSize := maximum - headSize - len(separator)
	head := search[:headSize]
	for !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	tail := search[len(search)-tailSize:]
	for !utf8.ValidString(tail) {
		tail = tail[1:]
	}
	return head + separator + tail
}

func readSnapshot(path string) (Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, err
	}
	var value Session
	if err := json.Unmarshal(data, &value); err != nil {
		return Session{}, err
	}
	if value.Version == 0 {
		value.Version = 1
	}
	return value, nil
}

func migrate(value *Session) error {
	if value.Version < 1 || value.Version > CurrentVersion {
		return fmt.Errorf("unsupported version %d", value.Version)
	}
	value.Version = CurrentVersion
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".supercode-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}
