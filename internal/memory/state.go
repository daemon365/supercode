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
	"sort"
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
	SourceHash      string     `json:"source_hash,omitempty"`
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
	data, err := readPrivateFile(path, maximumStateBytes)
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
	s.pruneStateUnlocked(&state)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode memory state: %w", err)
	}
	for len(data)+1 > maximumStateBytes && len(state.Rollouts) > 0 {
		order := stateRolloutRetentionOrder(state.Rollouts)
		drop := min(64, len(order))
		for _, id := range order[len(order)-drop:] {
			delete(state.Rollouts, id)
		}
		data, err = json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fmt.Errorf("encode pruned memory state: %w", err)
		}
	}
	return atomicWriteBounded(filepath.Join(s.root, "state.json"), append(data, '\n'), maximumStateBytes)
}

func (s *Store) pruneStateUnlocked(state *stateData) {
	if state.Rollouts == nil {
		state.Rollouts = make(map[string]rolloutRecord)
	}
	if state.AppliedNotes == nil {
		state.AppliedNotes = make(map[string]string)
	}
	for id, record := range state.Rollouts {
		record.SessionID = boundStateValue(record.SessionID, 256)
		record.RolloutID = boundStateValue(record.RolloutID, 128)
		record.SourceHash = boundStateValue(record.SourceHash, 128)
		record.Slug = boundStateValue(record.Slug, 128)
		record.RawPath = boundStateValue(record.RawPath, 1024)
		record.SummaryPath = boundStateValue(record.SummaryPath, 1024)
		record.LastError = boundStateValue(record.LastError, 2*1024)
		state.Rollouts[id] = record
	}
	state.Phase2InputHash = boundStateValue(state.Phase2InputHash, 128)
	state.LastPipelineError = boundStateValue(state.LastPipelineError, 16*1024)
	if len(state.Rollouts) > maximumStateRollouts {
		order := stateRolloutRetentionOrder(state.Rollouts)
		for _, id := range order[maximumStateRollouts:] {
			delete(state.Rollouts, id)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(s.root, "extensions", "ad_hoc", "notes")); err == nil {
		present := make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				present[entry.Name()] = struct{}{}
			}
		}
		for name := range state.AppliedNotes {
			if _, ok := present[name]; !ok {
				delete(state.AppliedNotes, name)
			}
		}
	}
	if len(state.AppliedNotes) > maximumStateNotes {
		names := make([]string, 0, len(state.AppliedNotes))
		for name := range state.AppliedNotes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names[:len(names)-maximumStateNotes] {
			delete(state.AppliedNotes, name)
		}
	}
	for name, hash := range state.AppliedNotes {
		if len(name) > 256 {
			delete(state.AppliedNotes, name)
			continue
		}
		state.AppliedNotes[name] = boundStateValue(hash, 128)
	}
}

func stateRolloutRetentionOrder(rollouts map[string]rolloutRecord) []string {
	type candidate struct {
		id      string
		record  rolloutRecord
		recency time.Time
	}
	values := make([]candidate, 0, len(rollouts))
	for id, record := range rollouts {
		recency := record.SourceUpdatedAt
		if record.GeneratedAt.After(recency) {
			recency = record.GeneratedAt
		}
		if record.LastUsage != nil && record.LastUsage.After(recency) {
			recency = *record.LastUsage
		}
		values = append(values, candidate{id: id, record: record, recency: recency})
	}
	rank := func(status string) int {
		switch status {
		case rolloutSucceeded:
			return 2
		case rolloutNoOutput:
			return 1
		default:
			return 0
		}
	}
	sort.Slice(values, func(i, j int) bool {
		left, right := rank(values[i].record.Status), rank(values[j].record.Status)
		if left != right {
			return left > right
		}
		if !values[i].recency.Equal(values[j].recency) {
			return values[i].recency.After(values[j].recency)
		}
		return values[i].id < values[j].id
	})
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].id
	}
	return result
}

func boundStateValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && value[end]&0xc0 == 0x80 {
		end--
	}
	return value[:end]
}

func atomicWrite(path string, data []byte) error {
	return atomicWriteBounded(path, data, maximumArtifactBytes)
}

func atomicWriteBounded(path string, data []byte, limit int) error {
	if len(data) > limit {
		return fmt.Errorf("memory artifact exceeds %d bytes", limit)
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
	return syncMemoryDirectory(filepath.Dir(path))
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
