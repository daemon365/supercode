package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
)

type PipelineReport struct {
	Eligible             int
	Extracted            int
	NoOutput             int
	Failed               int
	Consolidated         bool
	ConsolidationSkipped bool
}

type extractionOutput struct {
	RawMemory      string  `json:"raw_memory"`
	RolloutSummary string  `json:"rollout_summary"`
	RolloutSlug    *string `json:"rollout_slug"`
}

type consolidationSkill struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type consolidationOutput struct {
	MemoryMD        string               `json:"memory_md"`
	MemorySummaryMD string               `json:"memory_summary_md"`
	Skills          []consolidationSkill `json:"skills"`
}

// StartStartup launches the bounded pipeline for idle sessions. It intentionally
// does not block normal chat startup.
func (s *Store) StartStartup(ctx context.Context, modelProvider provider.Provider, sessions *session.Store, currentSessionID, activeModel string) {
	if s == nil || modelProvider == nil || sessions == nil || !s.Configuration().Generate {
		return
	}
	go func() {
		_, _ = s.RunStartup(ctx, modelProvider, sessions, currentSessionID, activeModel)
	}()
}

func (s *Store) RunStartup(ctx context.Context, modelProvider provider.Provider, sessions *session.Store, currentSessionID, activeModel string) (PipelineReport, error) {
	var report PipelineReport
	if s == nil || modelProvider == nil || sessions == nil {
		return report, errors.New("memory pipeline dependencies are unavailable")
	}
	s.pipelineMu.Lock()
	defer s.pipelineMu.Unlock()
	configuration := s.Configuration()
	if !configuration.Generate {
		report.ConsolidationSkipped = true
		return report, nil
	}

	_ = s.prepareGitBaseline(ctx)
	candidates, err := s.eligibleSessions(sessions, currentSessionID, configuration, time.Now().UTC())
	if err != nil {
		s.recordPipelineError(err)
		return report, err
	}
	report.Eligible = len(candidates)
	for _, candidate := range candidates {
		outcome, extractionErr := s.extractSession(ctx, modelProvider, candidate, configuration, activeModel)
		switch outcome {
		case rolloutSucceeded:
			report.Extracted++
		case rolloutNoOutput:
			report.NoOutput++
		default:
			report.Failed++
		}
		if extractionErr != nil {
			continue
		}
	}
	consolidated, err := s.consolidate(ctx, modelProvider, configuration, activeModel)
	if err != nil {
		s.recordPipelineError(err)
		return report, err
	}
	report.Consolidated = consolidated
	report.ConsolidationSkipped = !consolidated
	s.recordPipelineSuccess()
	return report, nil
}

func (s *Store) eligibleSessions(store *session.Store, currentID string, configuration Config, now time.Time) ([]session.Session, error) {
	values, err := store.ListAll("", 0, false)
	if err != nil {
		return nil, err
	}
	maxAge := now.Add(-time.Duration(configuration.MaxRolloutAgeDays) * 24 * time.Hour)
	idleCutoff := now.Add(-time.Duration(configuration.MinRolloutIdleHours) * time.Hour)
	s.mu.Lock()
	state, err := s.loadStateUnlocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var candidates []session.Session
	for _, value := range values {
		if value.ID == currentID || value.ParentID != "" || len(value.Messages) == 0 || value.ArchivedAt != nil {
			continue
		}
		if value.UpdatedAt.Before(maxAge) || value.UpdatedAt.After(idleCutoff) {
			continue
		}
		if previous, ok := state.Rollouts[value.ID]; ok && !previous.SourceUpdatedAt.Before(value.UpdatedAt) {
			continue
		}
		candidates = append(candidates, value)
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].UpdatedAt.After(candidates[right].UpdatedAt)
	})
	if len(candidates) > configuration.MaxRolloutsPerStartup {
		candidates = candidates[:configuration.MaxRolloutsPerStartup]
	}
	return candidates, nil
}

type filteredMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content,omitempty"`
	ToolCalls  []filteredToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

type filteredToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

func serializeSession(value session.Session) (string, error) {
	messages := make([]filteredMessage, 0, len(value.Messages))
	for _, item := range value.Messages {
		message := filteredMessage{Role: string(item.Role), Content: redactSecrets(item.Content), ToolCallID: item.ToolCallID}
		for _, call := range item.ToolCalls {
			message.ToolCalls = append(message.ToolCalls, filteredToolCall{Name: call.Name, Arguments: redactSecrets(call.Arguments)})
		}
		if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
			continue
		}
		messages = append(messages, message)
	}
	data, err := json.Marshal(messages)
	if err != nil {
		return "", err
	}
	valueText := redactSecrets(string(data))
	if len(valueText) > maximumPipelineInput {
		head := maximumPipelineInput / 3
		tail := maximumPipelineInput - head
		valueText = valueText[:head] + "\n[older rollout content truncated]\n" + valueText[len(valueText)-tail:]
	}
	return valueText, nil
}

func (s *Store) extractSession(ctx context.Context, modelProvider provider.Provider, value session.Session, configuration Config, activeModel string) (string, error) {
	contents, err := serializeSession(value)
	if err != nil {
		s.markExtractionFailure(value, err)
		return rolloutFailed, err
	}
	model := defaultString(strings.TrimSpace(configuration.ExtractModel), activeModel)
	if model == "" {
		err := errors.New("memory extraction model is unavailable")
		s.markExtractionFailure(value, err)
		return rolloutFailed, err
	}
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	response, err := modelProvider.Generate(requestContext, provider.Request{
		Model: model, Instructions: prompts.MemoryExtractionPrompt() + "\n\n" + extractionInstructions(),
		Prompt: extractionInput(value.ID, value.Workspace, contents), ReasoningEffort: "low",
	})
	if err != nil {
		s.markExtractionFailure(value, err)
		return rolloutFailed, err
	}
	var output extractionOutput
	if err := decodeModelJSON(response.Text, &output); err != nil {
		s.markExtractionFailure(value, err)
		return rolloutFailed, err
	}
	output.RawMemory = redactSecrets(output.RawMemory)
	output.RolloutSummary = redactSecrets(output.RolloutSummary)
	slug := ""
	if output.RolloutSlug != nil {
		slug = slugify(redactSecrets(*output.RolloutSlug))
	}
	if output.RawMemory == "" || output.RolloutSummary == "" {
		if err := s.markNoExtractionOutput(value); err != nil {
			return rolloutFailed, err
		}
		return rolloutNoOutput, nil
	}
	if err := s.persistExtraction(value, output.RawMemory, output.RolloutSummary, slug); err != nil {
		s.markExtractionFailure(value, err)
		return rolloutFailed, err
	}
	return rolloutSucceeded, nil
}

func decodeModelJSON(value string, destination any) error {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "```") {
		lines := strings.Split(value, "\n")
		if len(lines) >= 3 {
			lines = lines[1 : len(lines)-1]
			value = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode memory model JSON: %w", err)
	}
	return nil
}

func (s *Store) persistExtraction(source session.Session, rawMemory, rolloutSummary, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadStateUnlocked()
	if err != nil {
		return err
	}
	record := state.Rollouts[source.ID]
	if record.RolloutID == "" {
		record.RolloutID, err = newRolloutID()
		if err != nil {
			return err
		}
	}
	record.SessionID = source.ID
	record.SourceUpdatedAt = source.UpdatedAt
	record.GeneratedAt = time.Now().UTC()
	record.Slug = slug
	record.RawPath = filepath.ToSlash(filepath.Join("raw", record.RolloutID+".md"))
	record.SummaryPath = filepath.ToSlash(filepath.Join("rollout_summaries", record.RolloutID+".md"))
	record.Status = rolloutSucceeded
	record.LastError = ""
	raw := fmt.Sprintf("# Raw rollout memory\n\n- rollout_id: %s\n- session_id: %s\n- workspace: %s\n- source_updated_at: %s\n- generated_at: %s\n\n%s\n", record.RolloutID, source.ID, source.Workspace, source.UpdatedAt.Format(time.RFC3339), record.GeneratedAt.Format(time.RFC3339), rawMemory)
	summary := fmt.Sprintf("# Rollout summary\n\n- rollout_id: %s\n- session_id: %s\n- workspace: %s\n- source_updated_at: %s\n- slug: %s\n\n%s\n", record.RolloutID, source.ID, source.Workspace, source.UpdatedAt.Format(time.RFC3339), defaultString(slug, "none"), rolloutSummary)
	if err := atomicWrite(filepath.Join(s.root, filepath.FromSlash(record.RawPath)), []byte(raw)); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(s.root, filepath.FromSlash(record.SummaryPath)), []byte(summary)); err != nil {
		return err
	}
	state.Rollouts[source.ID] = record
	return s.saveStateUnlocked(state)
}

func (s *Store) markNoExtractionOutput(source session.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadStateUnlocked()
	if err != nil {
		return err
	}
	record := state.Rollouts[source.ID]
	record.SessionID = source.ID
	record.SourceUpdatedAt = source.UpdatedAt
	record.GeneratedAt = time.Now().UTC()
	record.Status = rolloutNoOutput
	record.LastError = ""
	state.Rollouts[source.ID] = record
	return s.saveStateUnlocked(state)
}

func (s *Store) markExtractionFailure(source session.Session, failure error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadStateUnlocked()
	if err != nil {
		return
	}
	record := state.Rollouts[source.ID]
	record.SessionID = source.ID
	record.SourceUpdatedAt = source.UpdatedAt
	record.GeneratedAt = time.Now().UTC()
	record.Status = rolloutFailed
	record.LastError = redactSecrets(failure.Error())
	state.Rollouts[source.ID] = record
	_ = s.saveStateUnlocked(state)
}

type selectedMemory struct {
	record  rolloutRecord
	raw     string
	summary string
}

func (s *Store) selectMemories(configuration Config, now time.Time) ([]selectedMemory, stateData, error) {
	s.mu.Lock()
	state, err := s.loadStateUnlocked()
	s.mu.Unlock()
	if err != nil {
		return nil, stateData{}, err
	}
	cutoff := now.Add(-time.Duration(configuration.MaxUnusedDays) * 24 * time.Hour)
	var records []rolloutRecord
	for _, record := range state.Rollouts {
		if record.Status != rolloutSucceeded || record.RolloutID == "" {
			continue
		}
		recency := record.SourceUpdatedAt
		if record.LastUsage != nil {
			recency = *record.LastUsage
		}
		if recency.Before(cutoff) {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].UsageCount != records[right].UsageCount {
			return records[left].UsageCount > records[right].UsageCount
		}
		leftRecency, rightRecency := records[left].SourceUpdatedAt, records[right].SourceUpdatedAt
		if records[left].LastUsage != nil {
			leftRecency = *records[left].LastUsage
		}
		if records[right].LastUsage != nil {
			rightRecency = *records[right].LastUsage
		}
		if !leftRecency.Equal(rightRecency) {
			return leftRecency.After(rightRecency)
		}
		return records[left].SourceUpdatedAt.After(records[right].SourceUpdatedAt)
	})
	if len(records) > configuration.MaxRawMemoriesForConsolidation {
		records = records[:configuration.MaxRawMemoriesForConsolidation]
	}
	selected := make([]selectedMemory, 0, len(records))
	for _, record := range records {
		raw, err := readBoundedFile(filepath.Join(s.root, filepath.FromSlash(record.RawPath)), maximumArtifactBytes)
		if err != nil {
			continue
		}
		summary, err := readBoundedFile(filepath.Join(s.root, filepath.FromSlash(record.SummaryPath)), maximumArtifactBytes)
		if err != nil {
			continue
		}
		selected = append(selected, selectedMemory{record: record, raw: raw, summary: summary})
	}
	return selected, state, nil
}

func (s *Store) consolidate(ctx context.Context, modelProvider provider.Provider, configuration Config, activeModel string) (bool, error) {
	selected, state, err := s.selectMemories(configuration, time.Now().UTC())
	if err != nil {
		return false, err
	}
	var raw strings.Builder
	for _, item := range selected {
		fmt.Fprintf(&raw, "\n\n---\n\n%s\n", item.raw)
	}
	notes, noteHashes, err := s.readAdHocNotes()
	if err != nil {
		return false, err
	}
	if len(selected) == 0 && strings.TrimSpace(notes) == "" {
		return false, nil
	}
	rawValue := strings.TrimSpace(raw.String())
	if err := atomicWrite(filepath.Join(s.root, "raw_memories.md"), []byte(rawValue+"\n")); err != nil {
		return false, err
	}
	inputHash := hashText(rawValue + "\n" + notes)
	existingMemory, err := s.Read()
	if err != nil {
		return false, err
	}
	existingSummary, err := s.Summary()
	if err != nil {
		return false, err
	}
	if state.Phase2InputHash == inputHash && existingMemory != "" && existingSummary != "" {
		return false, nil
	}
	diff := s.memoryGitDiff(ctx)
	input := consolidationInput(existingMemory, existingSummary, rawValue, notes, diff)
	if len(input) > maximumPipelineInput {
		input = input[:maximumPipelineInput/2] + "\n[consolidation input truncated]\n" + input[len(input)-maximumPipelineInput/2:]
	}
	model := defaultString(strings.TrimSpace(configuration.ConsolidationModel), activeModel)
	if model == "" {
		return false, errors.New("memory consolidation model is unavailable")
	}
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	response, err := modelProvider.Generate(requestContext, provider.Request{
		Model: model, Instructions: prompts.MemoryConsolidationPrompt() + "\n\n" + consolidationInstructions(), Prompt: input, ReasoningEffort: "medium",
	})
	if err != nil {
		return false, err
	}
	var output consolidationOutput
	if err := decodeModelJSON(response.Text, &output); err != nil {
		return false, err
	}
	output.MemoryMD = redactSecrets(output.MemoryMD)
	output.MemorySummaryMD = redactSecrets(output.MemorySummaryMD)
	if output.MemoryMD == "" || output.MemorySummaryMD == "" {
		return false, errors.New("memory consolidation returned empty required artifacts")
	}
	if err := atomicWrite(filepath.Join(s.root, "MEMORY.md"), []byte(output.MemoryMD+"\n")); err != nil {
		return false, err
	}
	if err := atomicWrite(filepath.Join(s.root, "memory_summary.md"), []byte(output.MemorySummaryMD+"\n")); err != nil {
		return false, err
	}
	stagedSkills := filepath.Join(s.root, ".skills-next")
	if err := os.RemoveAll(stagedSkills); err != nil {
		return false, err
	}
	if err := secureDirectory(stagedSkills); err != nil {
		return false, err
	}
	defer os.RemoveAll(stagedSkills)
	for _, skill := range output.Skills {
		name := slugify(skill.Name)
		if name != strings.TrimSpace(skill.Name) || strings.TrimSpace(skill.Content) == "" {
			continue
		}
		directory := filepath.Join(stagedSkills, name)
		if err := secureDirectory(directory); err != nil {
			return false, err
		}
		if err := atomicWrite(filepath.Join(directory, "SKILL.md"), []byte(redactSecrets(skill.Content)+"\n")); err != nil {
			return false, err
		}
	}
	skillsDirectory := filepath.Join(s.root, "skills")
	previousSkills := filepath.Join(s.root, ".skills-previous")
	if err := os.RemoveAll(previousSkills); err != nil {
		return false, err
	}
	if err := os.Rename(skillsDirectory, previousSkills); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Rename(stagedSkills, skillsDirectory); err != nil {
		_ = os.Rename(previousSkills, skillsDirectory)
		return false, err
	}
	if err := os.RemoveAll(previousSkills); err != nil {
		return false, err
	}
	s.mu.Lock()
	state, err = s.loadStateUnlocked()
	if err == nil {
		state.Phase2InputHash = inputHash
		for name, hash := range noteHashes {
			state.AppliedNotes[name] = hash
		}
		err = s.saveStateUnlocked(state)
	}
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	_ = s.commitGitBaseline(ctx)
	return true, nil
}

func (s *Store) readAdHocNotes() (string, map[string]string, error) {
	root := filepath.Join(s.root, "extensions", "ad_hoc", "notes")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", nil, err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	hashes := make(map[string]string)
	var output strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		value, err := readBoundedFile(filepath.Join(root, entry.Name()), maximumAdHocNoteBytes)
		if err != nil {
			return "", nil, err
		}
		hashes[entry.Name()] = hashText(value)
		fmt.Fprintf(&output, "\n\n## %s\n\n%s", entry.Name(), value)
	}
	return strings.TrimSpace(output.String()), hashes, nil
}

func hashText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (s *Store) recordPipelineSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadStateUnlocked()
	if err != nil {
		return
	}
	state.LastPipelineRun = time.Now().UTC()
	state.LastPipelineError = ""
	_ = s.saveStateUnlocked(state)
}

func (s *Store) recordPipelineError(failure error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadStateUnlocked()
	if err != nil {
		return
	}
	state.LastPipelineRun = time.Now().UTC()
	state.LastPipelineError = redactSecrets(failure.Error())
	_ = s.saveStateUnlocked(state)
}
