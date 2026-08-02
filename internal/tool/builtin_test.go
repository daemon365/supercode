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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/provider"
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

func TestNoArgumentBuiltinSchemasDeclareProperties(t *testing.T) {
	definitions := []provider.ToolDefinition{
		(&gitTool{name: "git_status"}).Definition(),
		(&listProcessesTool{}).Definition(),
	}
	for _, definition := range definitions {
		t.Run(definition.Name, func(t *testing.T) {
			var schema map[string]json.RawMessage
			if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
				t.Fatal(err)
			}
			properties, exists := schema["properties"]
			if !exists || string(properties) != "{}" {
				t.Fatalf("properties = %s, want {}", properties)
			}
		})
	}
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

func TestDenyRootCanonicalizesSymlinkAlias(t *testing.T) {
	workspaceRoot := t.TempDir()
	readRoot := t.TempDir()
	denied := filepath.Join(readRoot, "secret")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "secret-link")
	if err := os.Symlink(denied, alias); err != nil {
		t.Skip(err)
	}
	workspace, err := newWorkspaceWithOptions(workspaceRoot, SandboxOptions{ReadRoots: []string{readRoot}, DenyRoots: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.resolve(filepath.Join(denied, "hidden.txt"), true); !errors.Is(err, ErrOutsideWorkspace) {
		t.Fatalf("canonical deny alias was bypassed: %v", err)
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

func TestApplyPatchSchemaUsesCompatibleTopLevelObject(t *testing.T) {
	definition := builtinByName(t, t.TempDir(), "apply_patch").Definition()
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if string(schema["type"]) != `"object"` {
		t.Fatalf("top-level schema type = %s, want object", schema["type"])
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf", "enum", "const", "not"} {
		if _, exists := schema[keyword]; exists {
			t.Fatalf("top-level schema contains incompatible %q keyword", keyword)
		}
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(schema["properties"], &properties); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"path", "operations", "unified_diff"} {
		if _, exists := properties[name]; !exists {
			t.Errorf("schema is missing %q property", name)
		}
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

func TestSearchTextStopsRipgrepAfterResultLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ripgrep script is Unix-specific")
	}
	bin := t.TempDir()
	ripgrep := filepath.Join(bin, "rg")
	if err := os.WriteFile(ripgrep, []byte("#!/bin/sh\nwhile :; do printf '/tmp/fake.go:1:needle\\n'; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	started := time.Now()
	result, err := builtinByName(t, root, "search_text").Execute(context.Background(), `{"query":"needle","max_results":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(result.Content, "needle"); count != 3 || !strings.Contains(result.Content, "[results truncated]") {
		t.Fatalf("bounded search result = %q", result.Content)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("ripgrep was not stopped after the result limit: %s", elapsed)
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

func TestProcessBufferKeepsLatestOutputAndContinuesBroadcasting(t *testing.T) {
	var buffer processBuffer
	var streamed strings.Builder
	unsubscribe := buffer.subscribe(func(delta string) { streamed.WriteString(delta) })
	defer unsubscribe()

	first := bytes.Repeat([]byte("a"), maxProcessBuffer)
	second := []byte("latest-output")
	if _, err := buffer.Write(first); err != nil {
		t.Fatal(err)
	}
	_ = buffer.take(maxToolOutput)
	if _, err := buffer.Write(second); err != nil {
		t.Fatal(err)
	}
	if got := buffer.take(maxToolOutput); got != string(second) {
		t.Fatalf("latest output = %q, want %q", got, second)
	}
	if !strings.HasSuffix(streamed.String(), string(second)) {
		t.Fatalf("stream stopped after reaching the retention limit: suffix=%q", streamed.String()[len(streamed.String())-32:])
	}
}

func TestProcessBufferReportsDiscardedOutput(t *testing.T) {
	var buffer processBuffer
	if _, err := buffer.Write(bytes.Repeat([]byte("x"), maxProcessBuffer+128)); err != nil {
		t.Fatal(err)
	}
	got := buffer.take(64)
	if !strings.Contains(got, "bytes discarded") || !strings.HasSuffix(got, strings.Repeat("x", 64)) {
		t.Fatalf("bounded output = %q", got)
	}
}

func TestWriteProcessInputIsBoundedAndCancelable(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	process := &managedProcess{input: writer}
	if err := writeProcessInput(context.Background(), process, strings.Repeat("x", maxProcessInput+1)); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized write error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := writeProcessInput(ctx, process, "blocked")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled write error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("cancelled stdin write remained blocked for %s", elapsed)
	}
}

func TestProcessBufferUsesCircularStorageAfterCapacity(t *testing.T) {
	var buffer processBuffer
	if _, err := buffer.Write(bytes.Repeat([]byte("a"), maxProcessBuffer)); err != nil {
		t.Fatal(err)
	}
	storage := &buffer.data[0]
	_ = buffer.take(maxToolOutput)
	var written []byte
	for index := 0; index < 32; index++ {
		chunk := bytes.Repeat([]byte{byte('b' + index%20)}, 4096)
		written = append(written, chunk...)
		if _, err := buffer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if storage != &buffer.data[0] {
		t.Fatal("rolling output reallocated instead of reusing circular storage")
	}
	got := buffer.take(maxToolOutput)
	if !strings.HasSuffix(got, string(written[len(written)-maxToolOutput:])) {
		t.Fatalf("circular tail mismatch: suffix=%q", got[len(got)-64:])
	}
}

func TestProcessBufferUnsubscribeWaitsForInFlightDelivery(t *testing.T) {
	var buffer processBuffer
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	unsubscribe := buffer.subscribe(func(string) {
		calls.Add(1)
		close(entered)
		<-release
	})
	writeDone := make(chan struct{})
	go func() {
		_, _ = buffer.Write([]byte("first"))
		close(writeDone)
	}()
	<-entered
	unsubscribed := make(chan struct{})
	go func() {
		unsubscribe()
		close(unsubscribed)
	}()
	select {
	case <-unsubscribed:
		t.Fatal("unsubscribe returned while a progress callback was still running")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-writeDone
	<-unsubscribed
	_, _ = buffer.Write([]byte("late"))
	if calls.Load() != 1 {
		t.Fatalf("callback count = %d, want 1", calls.Load())
	}
}

func TestProcessBufferDeliversConcurrentWritesInBufferOrder(t *testing.T) {
	var buffer processBuffer
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var deliveredMu sync.Mutex
	var delivered []string
	unsubscribe := buffer.subscribe(func(delta string) {
		if delta == "first" {
			close(firstEntered)
			<-releaseFirst
		}
		deliveredMu.Lock()
		delivered = append(delivered, delta)
		deliveredMu.Unlock()
	})
	defer unsubscribe()
	firstDone := make(chan struct{})
	go func() {
		_, _ = buffer.Write([]byte("first"))
		close(firstDone)
	}()
	<-firstEntered
	secondDone := make(chan struct{})
	go func() {
		_, _ = buffer.Write([]byte("second"))
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("second progress delivery overtook the first")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	<-secondDone
	deliveredMu.Lock()
	got := strings.Join(delivered, ",")
	deliveredMu.Unlock()
	if got != "first,second" {
		t.Fatalf("progress delivery order = %q", got)
	}
	if output := buffer.take(maxToolOutput); output != "firstsecond" {
		t.Fatalf("buffer order = %q", output)
	}
}

func TestProcessManagerExpiresCompletedSessions(t *testing.T) {
	manager := newProcessManager(workspace{}, commandSandbox{})
	done := make(chan struct{})
	close(done)
	process := &managedProcess{id: 1001, done: done, endedAt: time.Now().Add(-processRetention - time.Second)}
	manager.processes[process.id] = process
	manager.expire(process)
	if _, exists := manager.processes[process.id]; exists {
		t.Fatal("expired process was retained")
	}
}

func TestProcessManagerCapsSessionsAndEvictsOldestCompleted(t *testing.T) {
	manager := newProcessManager(workspace{}, commandSandbox{})
	for index := 0; index < maxManagedProcesses; index++ {
		done := make(chan struct{})
		close(done)
		process := &managedProcess{
			id: int64(index + 1), done: done,
			endedAt: time.Now().Add(time.Duration(index) * time.Second),
		}
		manager.processes[process.id] = process
	}
	newest := &managedProcess{id: 1000, done: make(chan struct{})}
	if err := manager.track(newest); err != nil {
		t.Fatal(err)
	}
	if len(manager.processes) != maxManagedProcesses {
		t.Fatalf("managed process count = %d", len(manager.processes))
	}
	if _, exists := manager.processes[1]; exists {
		t.Fatal("oldest completed process was not evicted")
	}
	if manager.processes[newest.id] != newest {
		t.Fatal("new process was not tracked")
	}

	running := newProcessManager(workspace{}, commandSandbox{})
	for index := 0; index < maxManagedProcesses; index++ {
		process := &managedProcess{id: int64(index + 1), done: make(chan struct{})}
		running.processes[process.id] = process
	}
	if err := running.track(&managedProcess{id: 2000, done: make(chan struct{})}); err == nil || !strings.Contains(err.Error(), "process limit") {
		t.Fatalf("running-process cap error = %v", err)
	}
}

func TestBuiltinLifecycleStopsBackgroundProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell process lifecycle test is Unix-specific")
	}
	items, lifecycle, err := BuiltinsWithLifecycle(t.TempDir(), SandboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var execute Tool
	for _, item := range items {
		if item.Definition().Name == "exec_command" {
			execute = item
			break
		}
	}
	if execute == nil {
		t.Fatal("exec_command tool is unavailable")
	}
	started, err := execute.Execute(context.Background(), `{"cmd":"sleep 30","yield_time_ms":250}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(started.Content, `"running": true`) {
		t.Fatalf("process did not remain running: %s", started.Content)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPTYCompletionIncludesDrainedTailOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY behavior is Unix-specific")
	}
	items, lifecycle, err := BuiltinsWithLifecycle(t.TempDir(), SandboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lifecycle.Close() })
	var execute Tool
	for _, item := range items {
		if item.Definition().Name == "exec_command" {
			execute = item
			break
		}
	}
	for iteration := 0; iteration < 20; iteration++ {
		result, err := execute.Execute(context.Background(), `{"cmd":"i=0; while [ $i -lt 200 ]; do printf 'line-%04d\\n' $i; i=$((i+1)); done; printf '__PTY_TAIL__\\n'","tty":true,"yield_time_ms":30000,"sandbox_permissions":"require-escalated","justification":"PTY tail regression test"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Content, "__PTY_TAIL__") || strings.Contains(result.Content, `"running": true`) {
			t.Fatalf("iteration %d lost PTY tail: %s", iteration, result.Content)
		}
	}
}

func TestManagedProcessTerminationKillsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group behavior is covered here; Windows uses taskkill /T")
	}
	for _, mode := range []string{"wait", "stop", "close", "context"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			items, lifecycle, err := BuiltinsWithLifecycle(root, SandboxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lifecycle.Close() })
			tools := make(map[string]Tool)
			for _, item := range items {
				tools[item.Definition().Name] = item
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, err := tools["exec_command"].Execute(ctx, `{"cmd":"sleep 30 & echo $! > child.pid; wait","yield_time_ms":250,"sandbox_permissions":"require-escalated","justification":"process-tree test"}`)
			if err != nil {
				t.Fatal(err)
			}
			var state struct {
				SessionID int64 `json:"session_id"`
				Running   bool  `json:"running"`
			}
			if err := json.Unmarshal([]byte(started.Content), &state); err != nil || !state.Running {
				t.Fatalf("start state = %s, err=%v", started.Content, err)
			}
			pidBytes, err := os.ReadFile(filepath.Join(root, "child.pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			if err != nil || pid <= 1 {
				t.Fatalf("child pid = %q, err=%v", pidBytes, err)
			}
			switch mode {
			case "wait":
				_, err = tools["wait"].Execute(context.Background(), fmt.Sprintf(`{"session_id":%d,"terminate":true,"yield_time_ms":2000}`, state.SessionID))
			case "stop":
				_, err = tools["stop_process"].Execute(context.Background(), fmt.Sprintf(`{"session_id":%d}`, state.SessionID))
			case "close":
				err = lifecycle.Close()
			case "context":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			waitForProcessExit(t, pid)
		})
	}
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		probe := exec.Command("/bin/sh", "-c", "kill -0 \"$1\" 2>/dev/null", "sh", strconv.Itoa(pid))
		if probe.Run() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived process-tree termination", pid)
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
	if CanRunInParallel(item, `{"path":"pixel.png"}`) {
		t.Fatal("view_image unexpectedly opted into ordinary parallel reads")
	}
}

func TestByteBudgetHonorsCancellationAndRelease(t *testing.T) {
	budget := newByteBudget(10)
	release, err := budget.acquire(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budget.acquire(ctx, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked acquire error = %v", err)
	}
	release()
	release()
	secondRelease, err := budget.acquire(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	secondRelease()
}

func TestLimitedBufferCapsIOCopyAtSource(t *testing.T) {
	var output limitedBuffer
	value := strings.Repeat("x", maxToolOutput*4)
	written, err := io.Copy(&output, io.LimitReader(strings.NewReader(value), int64(len(value))))
	if err != nil || written != int64(len(value)) {
		t.Fatalf("io.Copy() = %d, %v", written, err)
	}
	if output.Len() != maxToolOutput || !output.Truncated() {
		t.Fatalf("buffer len=%d truncated=%t", output.Len(), output.Truncated())
	}
	if _, exposesReadFrom := any(&output).(io.ReaderFrom); exposesReadFrom {
		t.Fatal("limitedBuffer exposes ReadFrom and can bypass Write limits")
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
