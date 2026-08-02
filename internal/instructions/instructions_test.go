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
