package memory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	rolloutSucceeded = "succeeded"
	rolloutNoOutput  = "no_output"
	rolloutFailed    = "failed"
)

type rolloutRecord struct {
	SessionID       string     `json:"session_id"`
	RolloutID       string     `json:"rollout_id,omitempty"`
	SourceUpdatedAt time.Time  `json:"source_updated_at"`
	GeneratedAt     time.Time  `json:"generated_at,omitempty"`
	Slug            string     `json:"slug,omitempty"`
	RawPath         string     `json:"raw_path,omitempty"`
	SummaryPath     string     `json:"summary_path,omitempty"`
	UsageCount      int        `json:"usage_count,omitempty"`
	LastUsage       *time.Time `json:"last_usage,omitempty"`
	Status          string     `json:"status"`
	LastError       string     `json:"last_error,omitempty"`
}

type stateData struct {
	Version           int                      `json:"version"`
	Rollouts          map[string]rolloutRecord `json:"rollouts"`
	Phase2InputHash   string                   `json:"phase2_input_hash,omitempty"`
	AppliedNotes      map[string]string        `json:"applied_notes,omitempty"`
	LegacyMigrated    bool                     `json:"legacy_migrated,omitempty"`
	LastPipelineRun   time.Time                `json:"last_pipeline_run,omitempty"`
	LastPipelineError string                   `json:"last_pipeline_error,omitempty"`
}

func newState() stateData {
	return stateData{Version: stateVersion, Rollouts: make(map[string]rolloutRecord), AppliedNotes: make(map[string]string)}
}

func (s *Store) loadStateUnlocked() (stateData, error) {
	path := filepath.Join(s.root, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		state := newState()
		if err := s.saveStateUnlocked(state); err != nil {
			return stateData{}, err
		}
		return state, nil
	}
	if err != nil {
		return stateData{}, fmt.Errorf("read memory state: %w", err)
	}
	var state stateData
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return stateData{}, fmt.Errorf("decode memory state: %w", err)
	}
	if state.Version != stateVersion {
		return stateData{}, fmt.Errorf("unsupported memory state version %d", state.Version)
	}
	if state.Rollouts == nil {
		state.Rollouts = make(map[string]rolloutRecord)
	}
	if state.AppliedNotes == nil {
		state.AppliedNotes = make(map[string]string)
	}
	return state, nil
}

func (s *Store) saveStateUnlocked(state stateData) error {
	state.Version = stateVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode memory state: %w", err)
	}
	return atomicWrite(filepath.Join(s.root, "state.json"), append(data, '\n'))
}

func atomicWrite(path string, data []byte) error {
	if len(data) > maximumArtifactBytes {
		return fmt.Errorf("memory artifact exceeds %d bytes", maximumArtifactBytes)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".memory-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
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
	return os.Chmod(path, 0o600)
}

func (s *Store) migrateLegacyUnlocked(state *stateData) error {
	if state.LegacyMigrated || strings.TrimSpace(s.legacyPath) == "" {
		return nil
	}
	content, err := readBoundedFile(s.legacyPath, maximumAdHocNoteBytes)
	if err != nil {
		return err
	}
	if content != "" {
		filename := time.Now().UTC().Format("2006-01-02T15-04-05") + "-legacy-memory.md"
		path := filepath.Join(s.root, "extensions", "ad_hoc", "notes", filename)
		note := "# Imported legacy memory.md\n\n" + redactSecrets(content) + "\n"
		if err := atomicWrite(path, []byte(note)); err != nil {
			return err
		}
	}
	state.LegacyMigrated = true
	return s.saveStateUnlocked(*state)
}

var invalidSlug = regexp.MustCompile("[^a-z0-9]+")

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = invalidSlug.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 48 {
		value = strings.Trim(value[:48], "-")
	}
	if value == "" {
		value = "note"
	}
	return value
}

func newRolloutID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
