package instructions

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverLoadsRootToCWDWithOverrideAndFallback(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "AGENTS.md"):                      "root",
		filepath.Join(root, "packages", "AGENTS.md"):          "ignored",
		filepath.Join(root, "packages", "AGENTS.override.md"): "override",
		filepath.Join(child, "PROJECT.md"):                    "child",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, err := Discover(Options{Root: root, CWD: child, FallbackNames: []string{"PROJECT.md"}})
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, document := range set.Documents {
		contents = append(contents, document.Content)
	}
	if !reflect.DeepEqual(contents, []string{"root", "override", "child"}) {
		t.Fatalf("contents = %#v", contents)
	}
}

func TestDiscoverEnforcesCombinedLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(Options{Root: root, CWD: root, MaxBytes: 3}); err == nil {
		t.Fatal("expected byte limit error")
	}
}

func TestReadInstructionBoundsFileThatGrowsAfterStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openInstructionNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("content that grew beyond the limit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstruction(file, info, 3, 3, path); err == nil {
		t.Fatal("expected the bounded reader to reject a file that grew after Stat")
	}
}

func TestDiscoverDoesNotFollowInstructionSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "secret.md")
	if err := os.WriteFile(target, []byte("must not load"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	set, err := Discover(Options{Root: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Documents) != 0 {
		t.Fatalf("loaded symlinked instructions: %#v", set.Documents)
	}
}

func TestFindProjectRootRecognizesGitIndirectionFile(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../git/worktrees/example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("worktree instructions"), 0o600); err != nil {
		t.Fatal(err)
	}

	found := FindProjectRoot(child)
	if found != root {
		t.Fatalf("FindProjectRoot() = %q, want %q", found, root)
	}
	set, err := Discover(Options{Root: found, CWD: child})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Documents) != 1 || set.Documents[0].Content != "worktree instructions" {
		t.Fatalf("documents = %#v", set.Documents)
	}
}
