package tool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func builtinByName(t *testing.T, root, name string) Tool {
	t.Helper()
	items, err := Builtins(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Definition().Name == name {
			return item
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestReadFileRejectsWorkspaceEscape(t *testing.T) {
	root := t.TempDir()
	item := builtinByName(t, root, "read_file")
	_, err := item.Execute(context.Background(), `{"path":"../secret"}`)
	if !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("error = %v, want ErrOutsideWorkspace", err)
	}
}

func TestAdditionalReadWriteAndDenyRoots(t *testing.T) {
	workspaceRoot := t.TempDir()
	readRoot := t.TempDir()
	writeRoot := t.TempDir()
	denied := filepath.Join(readRoot, "secret")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readRoot, "visible.txt"), []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(denied, "hidden.txt"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := newWorkspaceWithOptions(workspaceRoot, SandboxOptions{ReadRoots: []string{readRoot}, WriteRoots: []string{writeRoot}, DenyRoots: []string{denied}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.resolve(filepath.Join(readRoot, "visible.txt"), false); err != nil {
		t.Fatalf("read root rejected: %v", err)
	}
	if _, err := workspace.resolveWrite(filepath.Join(readRoot, "new.txt"), true); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("read-only root write error = %v", err)
	}
	if _, err := workspace.resolveWrite(filepath.Join(writeRoot, "new.txt"), true); err != nil {
		t.Fatalf("write root rejected: %v", err)
	}
	if _, err := workspace.resolve(filepath.Join(denied, "hidden.txt"), false); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("deny root read error = %v", err)
	}
}

func TestReadFileRejectsSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	item := builtinByName(t, root, "read_file")
	_, err := item.Execute(context.Background(), `{"path":"link"}`)
	if !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("error = %v, want ErrOutsideWorkspace", err)
	}
}

func TestApplyPatchCreatesAndEditsFile(t *testing.T) {
	root := t.TempDir()
	item := builtinByName(t, root, "apply_patch")
	if description := item.Definition().Description; !strings.Contains(description, "operations array may repeat a path") {
		t.Fatalf("apply_patch description does not explain repeated paths: %q", description)
	}
	if _, err := item.Execute(context.Background(), `{"path":"hello.txt","old_text":"","new_text":"hello\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := item.Execute(context.Background(), `{"path":"hello.txt","old_text":"hello","new_text":"goodbye"}`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "goodbye\n" {
		t.Fatalf("content = %q", data)
	}
	if _, err := item.Execute(context.Background(), `{"path":"hello.txt","delete":true}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "hello.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file stat error = %v", err)
	}
}

func TestApplyPatchMovesAFileWithTopLevelMoveTo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "before.txt"), []byte("complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := builtinByName(t, root, "apply_patch")
	if _, err := item.Execute(context.Background(), `{"path":"before.txt","move_to":"after.txt"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "before.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "after.txt"))
	if err != nil || string(content) != "complete\n" {
		t.Fatalf("moved content = %q, error = %v", content, err)
	}
}

func TestApplyPatchRejectsOmissionPlaceholders(t *testing.T) {
	root := t.TempDir()
	item := builtinByName(t, root, "apply_patch")
	if _, err := item.Execute(context.Background(), `{"path":"partial.go","new_text":"package partial\n\n// ... rest of code unchanged\n"}`); err == nil || !strings.Contains(err.Error(), "complete intended content") {
		t.Fatalf("error = %v, want omission rejection", err)
	}
	if _, err := item.Execute(context.Background(), `{"path":"literal.txt","new_text":"A real sentence can end with an ellipsis…\n"}`); err != nil {
		t.Fatalf("legitimate content was rejected: %v", err)
	}
}

func TestApplyPatchRefusesToDeleteSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Skip(err)
	}
	item := builtinByName(t, root, "apply_patch")
	if _, err := item.Execute(context.Background(), `{"path":"link.txt","delete":true}`); err == nil {
		t.Fatal("expected symbolic-link deletion to be rejected")
	}
	data, err := os.ReadFile(filepath.Join(root, "target.txt"))
	if err != nil || string(data) != "keep" {
		t.Fatalf("target changed: data=%q err=%v", data, err)
	}
}

func TestApplyPatchRunsMultiFileTransaction(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := builtinByName(t, root, "apply_patch")
	arguments := `{"operations":[{"path":"one.txt","old_text":"one","new_text":"ONE"},{"path":"two.txt","old_text":"","new_text":"two\n"}]}`
	result, err := item.Execute(context.Background(), arguments)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "one.txt") || !strings.Contains(result.Content, "two.txt") || !strings.Contains(result.Content, "sha256") {
		t.Fatalf("result = %q", result.Content)
	}
	one, _ := os.ReadFile(filepath.Join(root, "one.txt"))
	two, _ := os.ReadFile(filepath.Join(root, "two.txt"))
	if string(one) != "ONE\n" || string(two) != "two\n" {
		t.Fatalf("one=%q two=%q", one, two)
	}
}

func TestApplyPatchRunsRepeatedPathOperationsInOrder(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "README.md")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := builtinByName(t, root, "apply_patch")
	arguments := `{"operations":[{"path":"README.md","old_text":"alpha","new_text":"ALPHA"},{"path":"README.md","old_text":"beta","new_text":"BETA"},{"path":"README.md","old_text":"ALPHA\nBETA","new_text":"first\nsecond"}]}`
	result, err := item.Execute(context.Background(), arguments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(result.Content, "README.md") != 1 {
		t.Fatalf("result should report the final path once: %q", result.Content)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first\nsecond\ngamma\n" {
		t.Fatalf("operations were not cumulative: %q", content)
	}
}

func TestApplyPatchRepeatedPathFailureLeavesOriginalUntouched(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "README.md")
	original := []byte("alpha\nbeta\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	item := builtinByName(t, root, "apply_patch")
	arguments := `{"operations":[{"path":"README.md","old_text":"alpha","new_text":"ALPHA"},{"path":"README.md","old_text":"missing","new_text":"value"}]}`
	if _, err := item.Execute(context.Background(), arguments); err == nil || !strings.Contains(err.Error(), "operation 2") {
		t.Fatalf("error = %v, want operation 2 failure", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, original) {
		t.Fatalf("failed transaction changed the file: %q", content)
	}
}

func TestApplyPatchRejectsHashConflictWithoutPartialWrites(t *testing.T) {
	root := t.TempDir()
	onePath, twoPath := filepath.Join(root, "one.txt"), filepath.Join(root, "two.txt")
	if err := os.WriteFile(onePath, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(twoPath, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := fmt.Sprintf("%x", sha256.Sum256([]byte("one")))
	item := builtinByName(t, root, "apply_patch")
	arguments := fmt.Sprintf(`{"operations":[{"path":"one.txt","old_text":"one","new_text":"ONE","expected_sha256":%q},{"path":"two.txt","old_text":"two","new_text":"TWO","expected_sha256":"stale"}]}`, valid)
	if _, err := item.Execute(context.Background(), arguments); err == nil || !strings.Contains(err.Error(), "sha256 conflict") {
		t.Fatalf("error = %v, want sha256 conflict", err)
	}
	one, _ := os.ReadFile(onePath)
	two, _ := os.ReadFile(twoPath)
	if string(one) != "one" || string(two) != "two" {
		t.Fatalf("transaction partially changed files: one=%q two=%q", one, two)
	}
}

func TestGitDiffIncludesUntrackedFiles(t *testing.T) {
	root := t.TempDir()
	command := exec.Command("git", "init", "-q")
	command.Dir = root
	if err := command.Run(); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := builtinByName(t, root, "git_diff").Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "new.txt") || !strings.Contains(result.Content, "+hello") {
		t.Fatalf("git diff = %q", result.Content)
	}
}

func TestCommandSandboxAutoAllowsReadsAndConfinesWrites(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if !newCommandSandbox(workspace{root: root}).available() {
		t.Skip("bubblewrap is unavailable")
	}
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	execute := builtinByName(t, root, "exec_command")
	if risk := execute.Risk(`{"cmd":"sed -n '1p' input.txt"}`); risk != RiskRead {
		t.Fatalf("read command risk = %q", risk)
	}
	read, err := execute.Execute(context.Background(), `{"cmd":"sed -n '1p' input.txt","yield_time_ms":2000}`)
	if err != nil || !strings.Contains(read.Content, "hello") {
		t.Fatalf("sandbox read = %s, err=%v", read.Content, err)
	}

	run := execute
	if risk := run.Risk(`{"cmd":"printf changed > output.txt"}`); risk != RiskExecute {
		t.Fatalf("write command risk = %q", risk)
	}
	if _, err := run.Execute(context.Background(), `{"cmd":"printf changed > output.txt","yield_time_ms":2000}`); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "output.txt")); err != nil || string(data) != "changed" {
		t.Fatalf("workspace output = %q, err=%v", data, err)
	}

	escapePath := filepath.Join(outside, "escape.txt")
	arguments, _ := json.Marshal(map[string]any{"cmd": "printf escaped > " + escapePath, "yield_time_ms": 2000})
	result, err := run.Execute(context.Background(), string(arguments))
	if err != nil || !result.IsError {
		t.Fatalf("outside write result = %+v, err=%v", result, err)
	}
	if _, err := os.Stat(escapePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file exists: %v", err)
	}
}

func TestSearchTextReturnsBoundedMatches(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("one\nneedle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := builtinByName(t, root, "search_text")
	result, err := item.Execute(context.Background(), `{"query":"needle"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "a.go:2:needle") {
		t.Fatalf("result = %q", result.Content)
	}
}

func TestExecCommandWriteStdinSession(t *testing.T) {
	root := t.TempDir()
	// Builtins creates a shared process manager per returned set, so retrieve
	// both tools from one set for this interaction.
	items, err := Builtins(root)
	if err != nil {
		t.Fatal(err)
	}
	var execute, write Tool
	for _, item := range items {
		if item.Definition().Name == "exec_command" {
			execute = item
		}
		if item.Definition().Name == "write_stdin" {
			write = item
		}
	}
	started, err := execute.Execute(context.Background(), `{"cmd":"read line; printf 'got:%s' \"$line\"","tty":true,"yield_time_ms":250}`)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		SessionID int64  `json:"session_id"`
		Running   bool   `json:"running"`
		Output    string `json:"output"`
	}
	if err := json.Unmarshal([]byte(started.Content), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.SessionID == 0 {
		t.Fatalf("start = %s", started.Content)
	}
	finished, err := write.Execute(context.Background(), fmt.Sprintf(`{"session_id":%d,"chars":"hello\n","yield_time_ms":2000}`, state.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(finished.Content, "got:hello") {
		t.Fatalf("write result = %s", finished.Content)
	}
}

func TestViewImageReturnsMultimodalContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pixel.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	item := builtinByName(t, root, "view_image")
	result, err := item.Execute(context.Background(), `{"path":"pixel.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || result.Images[0].MIMEType != "image/png" || result.Images[0].Data == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWebRunTimeDoesNotRequireNetwork(t *testing.T) {
	item := builtinByName(t, t.TempDir(), "web__run")
	if got := item.Risk(`{"time":[{"utc_offset":"+08:00"}]}`); got != RiskRead {
		t.Fatalf("time risk = %q, want %q", got, RiskRead)
	}
	if got := item.Risk(`{"search_query":[{"q":"Go"}]}`); got != RiskNetwork {
		t.Fatalf("search risk = %q, want %q", got, RiskNetwork)
	}
	result, err := item.Execute(context.Background(), `{"time":[{"utc_offset":"+08:00"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"utc_offset": "+08:00"`) {
		t.Fatalf("result = %s", result.Content)
	}
}
