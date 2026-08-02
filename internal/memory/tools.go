package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

const (
	ToolSearch = "memories_search"
	ToolRead   = "memories_read"
	ToolList   = "memories_list"
	ToolAdd    = "memories_add_ad_hoc_note"
)

func (s *Store) Tools() []tool.Tool {
	if s == nil || !s.Configuration().DedicatedTools {
		return nil
	}
	return []tool.Tool{
		&memoryTool{store: s, name: ToolSearch},
		&memoryTool{store: s, name: ToolRead},
		&memoryTool{store: s, name: ToolList},
		&memoryTool{store: s, name: ToolAdd},
	}
}

type memoryTool struct {
	store *Store
	name  string
}

func (t *memoryTool) Definition() provider.ToolDefinition {
	switch t.name {
	case ToolSearch:
		return provider.ToolDefinition{Name: t.name, Description: "Search generated long-term Markdown memory by text and return file, line, and matching content.", Parameters: json.RawMessage("{\"type\":\"object\",\"properties\":{\"query\":{\"type\":\"string\"},\"max_results\":{\"type\":\"integer\",\"minimum\":1,\"maximum\":200}},\"required\":[\"query\"],\"additionalProperties\":false}")}
	case ToolRead:
		return provider.ToolDefinition{Name: t.name, Description: "Read one memory Markdown file with optional one-based line bounds, up to 20000 estimated tokens.", Parameters: json.RawMessage("{\"type\":\"object\",\"properties\":{\"path\":{\"type\":\"string\"},\"start_line\":{\"type\":\"integer\",\"minimum\":1},\"end_line\":{\"type\":\"integer\",\"minimum\":1},\"max_tokens\":{\"type\":\"integer\",\"minimum\":1,\"maximum\":20000}},\"required\":[\"path\"],\"additionalProperties\":false}")}
	case ToolList:
		return provider.ToolDefinition{Name: t.name, Description: "List files under the memory root without exposing internal metadata or Git state.", Parameters: json.RawMessage("{\"type\":\"object\",\"properties\":{\"path\":{\"type\":\"string\"},\"max_results\":{\"type\":\"integer\",\"minimum\":1,\"maximum\":2000}},\"additionalProperties\":false}")}
	default:
		return provider.ToolDefinition{Name: t.name, Description: "Create one append-only memory note only after the user explicitly asks to remember, forget, or update something.", Parameters: json.RawMessage("{\"type\":\"object\",\"properties\":{\"filename\":{\"type\":\"string\"},\"note\":{\"type\":\"string\"}},\"required\":[\"note\"],\"additionalProperties\":false}")}
	}
}

func (t *memoryTool) Risk(string) tool.Risk {
	if t.name == ToolAdd {
		return tool.RiskWrite
	}
	return tool.RiskRead
}

func (t *memoryTool) ParallelSafe(string) bool { return t.name != ToolAdd }

func (t *memoryTool) Summary(arguments string) string {
	switch t.name {
	case ToolSearch:
		return "search long-term memory"
	case ToolRead:
		return "read long-term memory"
	case ToolList:
		return "list long-term memory"
	default:
		return "add an explicit memory note"
	}
}

func (t *memoryTool) Execute(ctx context.Context, arguments string) (tool.Result, error) {
	switch t.name {
	case ToolSearch:
		return t.search(ctx, arguments)
	case ToolRead:
		return t.read(arguments)
	case ToolList:
		return t.list(arguments)
	default:
		return t.add(arguments)
	}
}

func decodeStrict(arguments string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("arguments must contain one JSON object")
	}
	return nil
}

func (t *memoryTool) search(ctx context.Context, arguments string) (tool.Result, error) {
	var input struct {
		Query string `json:"query"`
		Max   int    `json:"max_results"`
	}
	if err := decodeStrict(arguments, &input); err != nil {
		return tool.Result{}, err
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" {
		return tool.Result{}, errors.New("query is required")
	}
	if input.Max <= 0 {
		input.Max = defaultSearchResults
	}
	input.Max = min(input.Max, defaultSearchResults)
	query := strings.ToLower(input.Query)
	var matches []string
	err := t.store.walkPublicMarkdown(func(relative, path string) error {
		if len(matches) >= input.Max {
			return fs.SkipAll
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		scanner := bufio.NewScanner(io.LimitReader(file, maximumArtifactBytes))
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for line := 1; scanner.Scan(); line++ {
			if strings.Contains(strings.ToLower(scanner.Text()), query) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", relative, line, scanner.Text()))
				if len(matches) >= input.Max {
					break
				}
			}
		}
		return scanner.Err()
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return tool.Result{}, err
	}
	if len(matches) == 0 {
		return tool.Result{Content: "No memory matches found."}, nil
	}
	return tool.Result{Content: strings.Join(matches, "\n")}, nil
}

func (t *memoryTool) read(arguments string) (tool.Result, error) {
	var input struct {
		Path      string `json:"path"`
		Start     int    `json:"start_line"`
		End       int    `json:"end_line"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := decodeStrict(arguments, &input); err != nil {
		return tool.Result{}, err
	}
	path, relative, err := t.store.resolvePublicPath(input.Path, false)
	if err != nil {
		return tool.Result{}, err
	}
	if !strings.EqualFold(filepath.Ext(path), ".md") {
		return tool.Result{}, errors.New("only memory Markdown files may be read")
	}
	if input.Start <= 0 {
		input.Start = 1
	}
	if input.End > 0 && input.End < input.Start {
		return tool.Result{}, errors.New("end_line must be at least start_line")
	}
	if input.MaxTokens <= 0 {
		input.MaxTokens = defaultReadTokens
	}
	input.MaxTokens = min(input.MaxTokens, defaultReadTokens)
	maximumCharacters := input.MaxTokens * 4
	file, err := os.Open(path)
	if err != nil {
		return tool.Result{}, err
	}
	defer file.Close()
	var output strings.Builder
	fmt.Fprintf(&output, "%s\n", relative)
	scanner := bufio.NewScanner(io.LimitReader(file, maximumArtifactBytes))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		if line < input.Start {
			continue
		}
		if input.End > 0 && line > input.End {
			break
		}
		fmt.Fprintf(&output, "%d: %s\n", line, scanner.Text())
		if output.Len() >= maximumCharacters {
			output.WriteString("[memory read truncated]\n")
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: output.String()}, nil
}

func (t *memoryTool) list(arguments string) (tool.Result, error) {
	var input struct {
		Path string `json:"path"`
		Max  int    `json:"max_results"`
	}
	if err := decodeStrict(arguments, &input); err != nil {
		return tool.Result{}, err
	}
	if input.Max <= 0 {
		input.Max = defaultListResults
	}
	input.Max = min(input.Max, defaultListResults)
	root, relative, err := t.store.resolvePublicPath(input.Path, true)
	if err != nil {
		return tool.Result{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return tool.Result{}, err
	}
	if !info.IsDir() {
		return tool.Result{}, errors.New("memory path is not a directory")
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == root {
			return nil
		}
		child, relErr := filepath.Rel(t.store.root, path)
		if relErr != nil {
			return nil
		}
		relative := filepath.ToSlash(child)
		if entry.IsDir() {
			// Keep descending into directories that may hold public files;
			// internal trees such as raw/ and state.json stay hidden.
			if !publicMemoryPath(relative) && !publicDirectory(relative) {
				return filepath.SkipDir
			}
			if publicMemoryPath(relative) {
				paths = append(paths, relative+"/")
				if len(paths) >= input.Max {
					return fs.SkipAll
				}
			}
			return nil
		}
		if !publicMemoryPath(relative) {
			return nil
		}
		paths = append(paths, relative)
		if len(paths) >= input.Max {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return tool.Result{}, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return tool.Result{Content: "No memory files under " + relative + "."}, nil
	}
	return tool.Result{Content: strings.Join(paths, "\n")}, nil
}

func (t *memoryTool) add(arguments string) (tool.Result, error) {
	var input struct {
		Filename string `json:"filename"`
		Note     string `json:"note"`
	}
	if err := decodeStrict(arguments, &input); err != nil {
		return tool.Result{}, err
	}
	path, err := t.store.AddAdHocNamed(input.Filename, input.Note)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: "Created " + path + ". It will be merged during the next Phase 2 consolidation."}, nil
}

var noteFilename = regexp.MustCompile("^\\d{4}-\\d{2}-\\d{2}T\\d{2}-\\d{2}-\\d{2}-[a-z0-9][a-z0-9-]{0,79}\\.md$")

func (s *Store) AddAdHocNote(slug, note string) (string, error) {
	base := time.Now().UTC().Format("2006-01-02T15-04-05") + "-" + slugify(slug)
	for suffix := 0; suffix < 1000; suffix++ {
		filename := base + ".md"
		if suffix > 0 {
			filename = fmt.Sprintf("%s-%d.md", base, suffix+1)
		}
		path, err := s.AddAdHocNamed(filename, note)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique memory note filename")
}

func (s *Store) AddAdHocNamed(filename, note string) (string, error) {
	if s == nil {
		return "", errors.New("memory storage is unavailable")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return "", errors.New("memory note is required")
	}
	if len(note) > maximumAdHocNoteBytes {
		return "", fmt.Errorf("memory note exceeds %d bytes", maximumAdHocNoteBytes)
	}
	if strings.TrimSpace(filename) == "" {
		filename = time.Now().UTC().Format("2006-01-02T15-04-05") + "-note.md"
	}
	if !noteFilename.MatchString(filename) {
		return "", errors.New("memory note filename must be YYYY-MM-DDTHH-MM-SS-lowercase-slug.md")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "extensions", "ad_hoc", "notes", filename)
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("memory note already exists: %w", os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := atomicWrite(path, []byte(redactSecrets(note)+"\n")); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("extensions", "ad_hoc", "notes", filename)), nil
}

func (s *Store) resolvePublicPath(value string, directory bool) (string, string, error) {
	value = filepath.Clean(strings.TrimSpace(value))
	if value == "" || value == "." {
		value = "."
	}
	if filepath.IsAbs(value) || value == ".." || strings.HasPrefix(value, ".."+string(filepath.Separator)) {
		return "", "", errors.New("memory path must be relative to the memory root")
	}
	relative := filepath.ToSlash(value)
	if relative != "." && !publicMemoryPath(relative) && !(directory && publicDirectory(relative)) {
		return "", "", errors.New("memory path is internal or unavailable")
	}
	path := filepath.Join(s.root, value)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	rootResolved, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errors.New("memory path escapes the memory root")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", err
	}
	if directory && !info.IsDir() {
		return "", "", errors.New("memory path is not a directory")
	}
	return resolved, relative, nil
}

// publicDirectory reports whether relative names a directory that may contain
// public memory files, or is an ancestor of one. Unlike publicMemoryPath,
// directory names are matched without a trailing slash so list can descend
// through them.
func publicDirectory(relative string) bool {
	switch filepath.ToSlash(strings.TrimPrefix(relative, "./")) {
	case "rollout_summaries", "skills", "extensions", "extensions/ad_hoc", "extensions/ad_hoc/notes":
		return true
	}
	return false
}

func publicMemoryPath(relative string) bool {
	relative = filepath.ToSlash(strings.TrimPrefix(relative, "./"))
	if relative == "" || relative == "." {
		return true
	}
	if relative == "MEMORY.md" || relative == "memory_summary.md" {
		return true
	}
	return strings.HasPrefix(relative, "rollout_summaries/") ||
		strings.HasPrefix(relative, "skills/") ||
		strings.HasPrefix(relative, "extensions/ad_hoc/notes/")
}

func (s *Store) walkPublicMarkdown(visitor func(relative, path string) error) error {
	ordered := []string{"MEMORY.md", "memory_summary.md", "rollout_summaries", "skills", filepath.Join("extensions", "ad_hoc", "notes")}
	for _, relative := range ordered {
		path := filepath.Join(s.root, filepath.FromSlash(relative))
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if err := visitor(relative, path); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(path, func(child string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				return nil
			}
			rel, err := filepath.Rel(s.root, child)
			if err != nil {
				return nil
			}
			return visitor(filepath.ToSlash(rel), child)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
