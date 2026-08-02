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

const indexVersion = 1

type indexFile struct {
	Version  int                   `json:"version"`
	Sessions map[string]indexEntry `json:"sessions"`
}

type indexEntry struct {
	ID         string     `json:"id"`
	Workspace  string     `json:"workspace"`
	Model      string     `json:"model"`
	Title      string     `json:"title,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	SearchText string     `json:"search_text,omitempty"`
}

type RepairReport struct {
	Scanned int      `json:"scanned"`
	Indexed int      `json:"indexed"`
	Invalid []string `json:"invalid,omitempty"`
}

// Repair rebuilds index.json from authoritative session snapshots. Invalid
// snapshots are reported and left untouched for manual recovery.
func (s *Store) Repair() (RepairReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return RepairReport{}, fmt.Errorf("scan sessions for repair: %w", err)
	}
	index := indexFile{Version: indexVersion, Sessions: make(map[string]indexEntry)}
	report := RepairReport{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == filepath.Base(s.indexPath) {
			continue
		}
		report.Scanned++
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			report.Invalid = append(report.Invalid, entry.Name())
			continue
		}
		value, decodeErr := readSnapshot(s.path(id))
		if decodeErr != nil || migrate(&value) != nil || value.ID != id {
			report.Invalid = append(report.Invalid, entry.Name())
			continue
		}
		index.Sessions[id] = makeIndexEntry(value)
		report.Indexed++
	}
	sort.Strings(report.Invalid)
	if err := s.writeIndexLocked(index); err != nil {
		return RepairReport{}, err
	}
	return report, nil
}

func (s *Store) listIndexed(workspace, query string, limit int, includeArchived bool) ([]Session, error) {
	index, err := s.readIndex()
	if err != nil {
		if _, repairErr := s.Repair(); repairErr != nil {
			return nil, repairErr
		}
		index, err = s.readIndex()
		if err != nil {
			return nil, err
		}
	}
	if s.indexCoverageChanged(index) {
		if _, repairErr := s.Repair(); repairErr != nil {
			return nil, repairErr
		}
		index, err = s.readIndex()
		if err != nil {
			return nil, err
		}
	}
	words := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	entries := make([]indexEntry, 0, len(index.Sessions))
	for _, entry := range index.Sessions {
		if workspace != "" && entry.Workspace != workspace || !includeArchived && entry.ArchivedAt != nil {
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].UpdatedAt.After(entries[j].UpdatedAt) })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	values := make([]Session, 0, len(entries))
	for _, entry := range entries {
		value, loadErr := s.Load(entry.ID)
		if loadErr == nil {
			values = append(values, value)
		}
	}
	return values, nil
}

func (s *Store) indexCoverageChanged(index indexFile) bool {
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
		if _, ok := index.Sessions[id]; !ok {
			return true
		}
		seen++
	}
	return seen != len(index.Sessions)
}

func (s *Store) readIndex() (indexFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readIndexLocked()
}

func (s *Store) readIndexLocked() (indexFile, error) {
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
	return value, nil
}

func (s *Store) updateIndexLocked(value Session) error {
	index, err := s.readIndexLocked()
	if err != nil {
		index = indexFile{Version: indexVersion, Sessions: make(map[string]indexEntry)}
	}
	index.Sessions[value.ID] = makeIndexEntry(value)
	return s.writeIndexLocked(index)
}

func (s *Store) deleteIndexLocked(id string) error {
	index, err := s.readIndexLocked()
	if err != nil {
		return err
	}
	delete(index.Sessions, id)
	return s.writeIndexLocked(index)
}

func (s *Store) writeIndexLocked(index indexFile) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session index: %w", err)
	}
	if err := atomicWrite(s.indexPath, data, 0o600); err != nil {
		return fmt.Errorf("write session index: %w", err)
	}
	return nil
}

func makeIndexEntry(value Session) indexEntry {
	parts := []string{value.ID, value.Title, value.Workspace, value.Model}
	for _, message := range value.Messages {
		if strings.TrimSpace(message.Content) != "" {
			parts = append(parts, message.Content)
		}
	}
	search := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if len(search) > 128*1024 {
		search = search[:128*1024]
		for !utf8.ValidString(search) {
			search = search[:len(search)-1]
		}
	}
	return indexEntry{ID: value.ID, Workspace: value.Workspace, Model: value.Model, Title: value.Title, UpdatedAt: value.UpdatedAt, ArchivedAt: value.ArchivedAt, SearchText: search}
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
	// Versions 1 and 2 used the same snapshot fields. Version 3 adds indexed
	// metadata and external image references without changing message meaning.
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
	return os.Rename(name, path)
}
