// Package memory manages file-backed, cross-session long-term memory.
package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	stateVersion            = 1
	defaultSummaryTokens    = 2_500
	defaultReadTokens       = 20_000
	defaultSearchResults    = 200
	defaultListResults      = 2_000
	maximumArtifactBytes    = 4 * 1024 * 1024
	maximumStateBytes       = 16 * 1024 * 1024
	maximumStateRollouts    = 4096
	maximumStateNotes       = 4096
	maximumPipelineInput    = 900_000
	maximumSelectedRawBytes = maximumPipelineInput / 2
	maximumAdHocNoteBytes   = 64 * 1024
)

// Config controls generation and retrieval without coupling memory to one
// model provider. Empty model names use the active SuperCode model.
type Config struct {
	Generate                       bool
	Use                            bool
	DedicatedTools                 bool
	AutoCapture                    bool
	SummaryTokens                  int
	MaxRolloutsPerStartup          int
	MaxRolloutAgeDays              int
	MinRolloutIdleHours            int
	MaxRawMemoriesForConsolidation int
	MaxUnusedDays                  int
	ExtractModel                   string
	ConsolidationModel             string
}

func DefaultConfig() Config {
	return Config{
		Use: true, DedicatedTools: true, SummaryTokens: defaultSummaryTokens,
		MaxRolloutsPerStartup: 2, MaxRolloutAgeDays: 10, MinRolloutIdleHours: 6,
		MaxRawMemoriesForConsolidation: 256, MaxUnusedDays: 30,
	}
}

func (configuration Config) normalized() Config {
	defaults := DefaultConfig()
	if configuration.SummaryTokens <= 0 {
		configuration.SummaryTokens = defaults.SummaryTokens
	}
	if configuration.MaxRolloutsPerStartup <= 0 {
		configuration.MaxRolloutsPerStartup = defaults.MaxRolloutsPerStartup
	}
	configuration.MaxRolloutsPerStartup = min(configuration.MaxRolloutsPerStartup, 128)
	if configuration.MaxRolloutAgeDays <= 0 {
		configuration.MaxRolloutAgeDays = defaults.MaxRolloutAgeDays
	}
	if configuration.MinRolloutIdleHours <= 0 {
		configuration.MinRolloutIdleHours = defaults.MinRolloutIdleHours
	}
	if configuration.MaxRawMemoriesForConsolidation <= 0 {
		configuration.MaxRawMemoriesForConsolidation = defaults.MaxRawMemoriesForConsolidation
	}
	configuration.MaxRawMemoriesForConsolidation = min(configuration.MaxRawMemoriesForConsolidation, 4096)
	if configuration.MaxUnusedDays <= 0 {
		configuration.MaxUnusedDays = defaults.MaxUnusedDays
	}
	return configuration
}

type Store struct {
	root        string
	legacyPath  string
	mu          sync.Mutex
	pipelineMu  sync.Mutex
	startupMu   sync.Mutex
	startupEnd  context.CancelFunc
	startupDone chan struct{}
	config      Config
}

// NewStore creates a directory-backed memory store. A historical memory.md
// path may be supplied for one-time migration into an ad-hoc note.
func NewStore(path string, legacyPaths ...string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory path is required")
	}
	path = filepath.Clean(path)
	legacyPath := ""
	if strings.EqualFold(filepath.Ext(path), ".md") {
		legacyPath = path
		path = filepath.Join(filepath.Dir(path), "memories")
	}
	if len(legacyPaths) > 0 && strings.TrimSpace(legacyPaths[0]) != "" {
		legacyPath = filepath.Clean(legacyPaths[0])
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve memory root: %w", err)
	}
	if info, statErr := os.Lstat(absolute); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("memory root must not be a symbolic link")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	for _, directory := range []string{
		absolute,
		filepath.Join(absolute, "raw"),
		filepath.Join(absolute, "rollout_summaries"),
		filepath.Join(absolute, "skills"),
		filepath.Join(absolute, "extensions", "ad_hoc", "notes"),
	} {
		if err := secureDirectory(directory); err != nil {
			return nil, err
		}
	}
	store := &Store{root: absolute, legacyPath: legacyPath, config: DefaultConfig()}
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.loadStateUnlocked()
	if err != nil {
		return nil, err
	}
	if err := store.migrateLegacyUnlocked(&state); err != nil {
		return nil, err
	}
	return store, nil
}

func secureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure memory directory: %w", err)
	}
	return nil
}

func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Configure retains the original API while mapping it onto summary injection
// and optional deterministic capture of explicit preference statements.
func (s *Store) Configure(maxTokens int, autoCapture bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	configuration := s.config
	if maxTokens > 0 {
		configuration.SummaryTokens = maxTokens
	}
	configuration.AutoCapture = autoCapture
	s.config = configuration.normalized()
	s.mu.Unlock()
}

func (s *Store) ConfigureAdvanced(configuration Config) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.config = configuration.normalized()
	s.mu.Unlock()
}

func (s *Store) Configuration() Config {
	if s == nil {
		return DefaultConfig()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

func (s *Store) Read() (string, error) {
	if s == nil {
		return "", errors.New("memory storage is unavailable")
	}
	return readBoundedFile(filepath.Join(s.root, "MEMORY.md"), maximumArtifactBytes)
}

func (s *Store) Summary() (string, error) {
	if s == nil {
		return "", errors.New("memory storage is unavailable")
	}
	return readBoundedFile(filepath.Join(s.root, "memory_summary.md"), maximumArtifactBytes)
}

func readBoundedFile(path string, maximum int64) (string, error) {
	data, err := readPrivateFile(path, maximum)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read memory artifact: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func readPrivateFile(path string, maximum int64) ([]byte, error) {
	file, err := openMemoryNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return readOpenedPrivateFile(file, info, maximum)
}

func readOpenedPrivateFile(file *os.File, info os.FileInfo, maximum int64) ([]byte, error) {
	if !info.Mode().IsRegular() {
		return nil, errors.New("memory artifact is not a regular file")
	}
	if info.Size() > maximum {
		return nil, fmt.Errorf("memory artifact exceeds %d bytes", maximum)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("memory artifact exceeds %d bytes", maximum)
	}
	return data, nil
}

func (s *Store) Remember(value string) error {
	return s.RememberWithSource(value, "user")
}

func (s *Store) RememberWithSource(value, source string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("memory text is required")
	}
	note := fmt.Sprintf("# Remember\n\nSource: %s\nDate: %s\n\n%s\n", strings.TrimSpace(source), time.Now().UTC().Format(time.RFC3339), value)
	_, err := s.AddAdHocNote("remember-"+slugify(value), note)
	return err
}

// Forget queues a user-authorized deletion or update request for the next
// global consolidation pass instead of editing generated MEMORY.md directly.
func (s *Store) Forget(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "Remove all previously consolidated long-term memory. Keep only later explicit notes and future rollout-derived memory."
	}
	note := fmt.Sprintf("# Forget or update memory\n\nDate: %s\n\n%s\n", time.Now().UTC().Format(time.RFC3339), value)
	_, err := s.AddAdHocNote("forget-"+slugify(value), note)
	return err
}

func (s *Store) AutoCapture(prompt string) (bool, error) {
	if s == nil || !s.Configuration().AutoCapture {
		return false, nil
	}
	value := strings.TrimSpace(strings.Join(strings.Fields(prompt), " "))
	lower := strings.ToLower(value)
	for _, trigger := range []string{"remember that ", "i prefer ", "always use ", "please remember "} {
		if strings.Contains(lower, trigger) {
			return true, s.RememberWithSource(value, "explicit-auto-capture")
		}
	}
	return false, nil
}

// Clear is an explicit hard reset retained for programmatic compatibility.
// It removes generated artifacts, source extracts, notes, generated Skills,
// metadata, and the private Git baseline. The TUI's /forget command uses
// Forget and therefore flows through Phase 2 instead.
func (s *Store) Clear() error {
	if s == nil {
		return errors.New("memory storage is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range []string{"MEMORY.md", "memory_summary.md", "raw_memories.md", ".gitignore"} {
		if err := os.Remove(filepath.Join(s.root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, name := range []string{"raw", "rollout_summaries", "skills", filepath.Join("extensions", "ad_hoc", "notes"), ".skills-next", ".skills-previous", ".git"} {
		if err := os.RemoveAll(filepath.Join(s.root, name)); err != nil {
			return err
		}
	}
	for _, name := range []string{"raw", "rollout_summaries", "skills", filepath.Join("extensions", "ad_hoc", "notes")} {
		if err := secureDirectory(filepath.Join(s.root, name)); err != nil {
			return err
		}
	}
	state := newState()
	// A hard reset must not re-import the same legacy memory.md on restart.
	state.LegacyMigrated = true
	return s.saveStateUnlocked(state)
}

func (s *Store) Instructions() string {
	if s == nil {
		return ""
	}
	configuration := s.Configuration()
	if !configuration.Use {
		return ""
	}
	summary, err := s.Summary()
	if err != nil || summary == "" {
		return ""
	}
	return readPathInstructions(s.root, truncateText(summary, configuration.SummaryTokens), configuration.DedicatedTools)
}

func (s *Store) Status() string {
	if s == nil {
		return "Memory storage is unavailable."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadStateUnlocked()
	if err != nil {
		return "Memory state error: " + err.Error()
	}
	return fmt.Sprintf("Memory root: %s\nGeneration: %t\nUse: %t\nDedicated tools: %t\nRollouts: %d\nLast pipeline run: %s\nLast pipeline error: %s", s.root, s.config.Generate, s.config.Use, s.config.DedicatedTools, successfulRollouts(state), formatTime(state.LastPipelineRun), defaultString(state.LastPipelineError, "none"))
}

func successfulRollouts(state stateData) int {
	count := 0
	for _, record := range state.Rollouts {
		if record.Status == rolloutSucceeded {
			count++
		}
	}
	return count
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "never"
	}
	return value.Local().Format(time.RFC3339)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func truncateText(value string, maximumTokens int) string {
	if maximumTokens <= 0 {
		return value
	}
	runes := []rune(value)
	maximumRunes := maximumTokens * 4
	if len(runes) <= maximumRunes {
		return value
	}
	return string(runes[:maximumRunes]) + "\n[summary truncated]"
}
