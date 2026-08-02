package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var omissionPlaceholderPattern = regexp.MustCompile(`(?i)^\s*(?:(?://|#|/\*+|\*+)\s*)?(?:\[\s*)?(?:(?:content|code|lines?|sections?)\s+(?:omitted|unchanged)|(?:rest|remaining|existing|unchanged|other)\s+(?:content|code|lines?|sections?)\s+(?:omitted|unchanged)|\.\.\.\s*(?:rest|remaining|existing|unchanged|other)\b.*)(?:\s*\])?(?:\s*\*/)?\s*$`)

type patchRequest struct {
	patchOperation
	Operations  []patchOperation `json:"operations"`
	UnifiedDiff string           `json:"unified_diff"`
}

type patchOperation struct {
	Path           string `json:"path"`
	Old            string `json:"old_text"`
	New            string `json:"new_text"`
	Delete         bool   `json:"delete"`
	ExpectedSHA256 string `json:"expected_sha256"`
	MoveTo         string `json:"move_to"`
}

type preparedPatch struct {
	path, originalHash string
	original, updated  []byte
	mode               fs.FileMode
	existed, delete    bool
	temporary, backup  string
	installed          bool
}

func executePatch(ctx context.Context, workspace workspace, arguments string) (Result, error) {
	var input patchRequest
	if err := decodeArguments(arguments, &input); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(input.UnifiedDiff) != "" {
		return applyUnifiedDiff(ctx, workspace, input.UnifiedDiff)
	}
	operations := input.Operations
	if len(operations) == 0 {
		operations = []patchOperation{{Path: input.Path, Old: input.Old, New: input.New, Delete: input.Delete, ExpectedSHA256: input.ExpectedSHA256, MoveTo: input.MoveTo}}
	}
	if len(operations) == 0 || len(operations) > 100 {
		return Result{}, errors.New("apply_patch requires between 1 and 100 operations")
	}
	transaction := newPatchTransaction(workspace)
	for index, operation := range operations {
		if placeholder := omissionPlaceholder(operation.New); placeholder != "" {
			return Result{}, fmt.Errorf("operation %d (%s): new_text contains an omission placeholder %q; provide the complete intended content", index+1, operation.Path, placeholder)
		}
		if strings.TrimSpace(operation.MoveTo) != "" {
			if err := transaction.move(operation); err != nil {
				return Result{}, fmt.Errorf("operation %d (%s → %s): %w", index+1, operation.Path, operation.MoveTo, err)
			}
			continue
		}
		if err := transaction.apply(operation); err != nil {
			return Result{}, fmt.Errorf("operation %d (%s): %w", index+1, operation.Path, err)
		}
	}
	prepared := transaction.prepared()
	if len(prepared) == 0 {
		return Result{Content: "No file changes were necessary."}, nil
	}
	if err := commitPatches(prepared); err != nil {
		return Result{}, err
	}
	lines := make([]string, 0, len(prepared))
	for _, item := range prepared {
		if item.delete {
			lines = append(lines, fmt.Sprintf("Deleted %s.", workspace.display(item.path)))
		} else {
			hash := sha256.Sum256(item.updated)
			lines = append(lines, fmt.Sprintf("Updated %s (%d bytes, sha256 %x).", workspace.display(item.path), len(item.updated), hash))
		}
	}
	return Result{Content: strings.Join(lines, "\n")}, nil
}

func applyUnifiedDiff(ctx context.Context, workspace workspace, diff string) (Result, error) {
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			if placeholder := omissionPlaceholder(strings.TrimPrefix(line, "+")); placeholder != "" {
				return Result{}, fmt.Errorf("unified diff contains an omission placeholder %q; provide the complete intended content", placeholder)
			}
		}
		if !strings.HasPrefix(line, "+++ ") && !strings.HasPrefix(line, "--- ") {
			continue
		}
		path := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "--- ")), "a/"), "b/")
		if path == "/dev/null" {
			continue
		}
		if filepath.IsAbs(path) || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") || path == ".git" || strings.HasPrefix(path, ".git/") {
			return Result{}, ErrOutsideWorkspace
		}
		if _, err := workspace.resolveWrite(path, true); err != nil {
			return Result{}, err
		}
	}
	run := func(check bool) ([]byte, error) {
		arguments := []string{"apply", "--whitespace=nowarn"}
		if check {
			arguments = append(arguments, "--check")
		}
		arguments = append(arguments, "-")
		command := exec.CommandContext(ctx, "git", arguments...)
		configureProcessTree(command, false)
		command.WaitDelay = time.Second
		defer cleanupProcessTree(command)
		command.Dir = workspace.root
		command.Stdin = strings.NewReader(diff)
		var stdout, stderr limitedBuffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		if err != nil {
			return nil, fmt.Errorf("git apply: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return []byte(stdout.String()), nil
	}
	if _, err := run(true); err != nil {
		return Result{}, err
	}
	if _, err := run(false); err != nil {
		return Result{}, err
	}
	return Result{Content: "Applied unified diff atomically with git apply."}, nil
}

func omissionPlaceholder(value string) string {
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if omissionPlaceholderPattern.MatchString(line) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

type stagedPatch struct {
	prepared preparedPatch
	exists   bool
	content  []byte
}

type patchTransaction struct {
	workspace workspace
	paths     map[string]*stagedPatch
	order     []string
}

func newPatchTransaction(workspace workspace) *patchTransaction {
	return &patchTransaction{workspace: workspace, paths: make(map[string]*stagedPatch)}
}

func (t *patchTransaction) stage(path string) (*stagedPatch, error) {
	base, err := loadPatchPath(t.workspace, path)
	if err != nil {
		return nil, err
	}
	if existing, ok := t.paths[base.path]; ok {
		return existing, nil
	}
	staged := &stagedPatch{prepared: base, exists: base.existed, content: append([]byte(nil), base.original...)}
	t.paths[base.path] = staged
	t.order = append(t.order, base.path)
	return staged, nil
}

func (t *patchTransaction) apply(input patchOperation) error {
	staged, err := t.stage(input.Path)
	if err != nil {
		return err
	}
	if err := verifyStagedHash(staged, input.ExpectedSHA256); err != nil {
		return err
	}
	if input.Delete {
		if input.Old != "" || input.New != "" {
			return errors.New("old_text and new_text must be empty when delete is true")
		}
		if !staged.exists {
			return os.ErrNotExist
		}
		staged.exists, staged.content = false, nil
		return nil
	}
	if !staged.exists {
		if input.Old != "" {
			return errors.New("cannot replace text in a missing file")
		}
		staged.exists, staged.content = true, []byte(input.New)
		return nil
	}
	if input.Old == "" {
		return errors.New("old_text is empty but the file already exists")
	}
	if strings.Count(string(staged.content), input.Old) != 1 {
		return errors.New("old_text must match exactly once")
	}
	staged.content = []byte(strings.Replace(string(staged.content), input.Old, input.New, 1))
	return nil
}

func (t *patchTransaction) move(input patchOperation) error {
	if input.Delete || input.Old != "" || input.New != "" {
		return errors.New("move_to cannot be combined with edit fields")
	}
	source, err := t.stage(input.Path)
	if err != nil {
		return err
	}
	target, err := t.stage(input.MoveTo)
	if err != nil {
		return err
	}
	if source == target {
		return errors.New("move_to must differ from path")
	}
	if err := verifyStagedHash(source, input.ExpectedSHA256); err != nil {
		return err
	}
	if !source.exists {
		return os.ErrNotExist
	}
	if target.exists {
		return errors.New("move destination already exists")
	}
	target.exists, target.content = true, append([]byte(nil), source.content...)
	source.exists, source.content = false, nil
	return nil
}

func (t *patchTransaction) prepared() []preparedPatch {
	items := make([]preparedPatch, 0, len(t.order))
	for _, path := range t.order {
		staged := t.paths[path]
		if staged.prepared.existed == staged.exists && (!staged.exists || bytes.Equal(staged.prepared.original, staged.content)) {
			continue
		}
		item := staged.prepared
		item.delete = !staged.exists
		item.updated = append([]byte(nil), staged.content...)
		items = append(items, item)
	}
	return items
}

func verifyStagedHash(staged *stagedPatch, expectedHash string) error {
	if strings.TrimSpace(expectedHash) == "" {
		return nil
	}
	expected := strings.ToLower(strings.TrimSpace(expectedHash))
	if !staged.exists {
		if expected != "missing" {
			return fmt.Errorf("sha256 conflict: file is missing, expected %s", expected)
		}
		return nil
	}
	current := fmt.Sprintf("%x", sha256.Sum256(staged.content))
	if expected != current {
		return fmt.Errorf("sha256 conflict: current %s, expected %s", current, expected)
	}
	return nil
}

func loadPatchPath(workspace workspace, requestedPath string) (preparedPatch, error) {
	if strings.TrimSpace(requestedPath) == "" {
		return preparedPatch{}, errors.New("path is required")
	}
	candidate := requestedPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspace.root, candidate)
	}
	candidate = filepath.Clean(candidate)
	if !within(workspace.root, candidate) {
		return preparedPatch{}, ErrOutsideWorkspace
	}
	if entry, err := os.Lstat(candidate); err == nil && entry.Mode()&os.ModeSymlink != 0 {
		return preparedPatch{}, errors.New("apply_patch refuses to modify a symbolic link")
	}
	path, err := workspace.resolveWrite(requestedPath, true)
	if err != nil {
		return preparedPatch{}, err
	}
	content, readErr := os.ReadFile(path)
	item := preparedPatch{path: path, mode: 0o644}
	if readErr == nil {
		info, err := os.Stat(path)
		if err != nil {
			return preparedPatch{}, err
		}
		if !info.Mode().IsRegular() {
			return preparedPatch{}, errors.New("apply_patch only supports regular files")
		}
		item.existed, item.mode, item.original = true, info.Mode().Perm(), content
		hash := sha256.Sum256(content)
		item.originalHash = fmt.Sprintf("%x", hash)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return preparedPatch{}, readErr
	}
	return item, nil
}

func commitPatches(items []preparedPatch) (returnErr error) {
	for index := range items {
		item := &items[index]
		if item.delete {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			cleanupPreparedPatches(items)
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(item.path), ".supercode-patch-*")
		if err != nil {
			cleanupPreparedPatches(items)
			return err
		}
		item.temporary = file.Name()
		if err := file.Chmod(item.mode); err != nil {
			file.Close()
			cleanupPreparedPatches(items)
			return err
		}
		if _, err := file.Write(item.updated); err != nil {
			file.Close()
			cleanupPreparedPatches(items)
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			cleanupPreparedPatches(items)
			return err
		}
		if err := file.Close(); err != nil {
			cleanupPreparedPatches(items)
			return err
		}
	}
	// Verify the complete read set again before changing any destination.
	for index := range items {
		item := &items[index]
		content, err := os.ReadFile(item.path)
		if item.existed {
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(content)) != item.originalHash {
				cleanupPreparedPatches(items)
				return fmt.Errorf("patch conflict: %s changed after it was read", item.path)
			}
		} else if err == nil || !errors.Is(err, os.ErrNotExist) {
			cleanupPreparedPatches(items)
			return fmt.Errorf("patch conflict: %s was created after it was checked", item.path)
		}
	}
	defer func() {
		if returnErr != nil {
			rollbackPreparedPatches(items)
		}
		cleanupPreparedPatches(items)
	}()
	for index := range items {
		item := &items[index]
		if item.existed {
			backup, err := os.CreateTemp(filepath.Dir(item.path), ".supercode-backup-*")
			if err != nil {
				return err
			}
			item.backup = backup.Name()
			if err := backup.Close(); err != nil {
				return err
			}
			if err := os.Remove(item.backup); err != nil {
				return err
			}
			if err := os.Rename(item.path, item.backup); err != nil {
				return err
			}
		}
		if !item.delete {
			if err := os.Rename(item.temporary, item.path); err != nil {
				return err
			}
			item.temporary = ""
			item.installed = true
		}
	}
	for index := range items {
		if items[index].backup != "" {
			if err := os.Remove(items[index].backup); err != nil {
				return err
			}
			items[index].backup = ""
		}
	}
	return nil
}

func rollbackPreparedPatches(items []preparedPatch) {
	for index := len(items) - 1; index >= 0; index-- {
		item := &items[index]
		if item.installed {
			_ = os.Remove(item.path)
		}
		if item.backup != "" {
			_ = os.Rename(item.backup, item.path)
			item.backup = ""
		}
	}
}

func cleanupPreparedPatches(items []preparedPatch) {
	for index := range items {
		if items[index].temporary != "" {
			_ = os.Remove(items[index].temporary)
		}
		if items[index].backup != "" {
			_ = os.Remove(items[index].backup)
		}
	}
}
