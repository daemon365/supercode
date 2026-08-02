package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverAndExplicitInjection(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "review")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: review\ndescription: Review code carefully\n---\n# Review\nCheck edge cases.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	plain := catalog.Instructions("hello")
	if !strings.Contains(plain, "$review") || strings.Contains(plain, "Check edge cases") {
		t.Fatalf("plain instructions = %q", plain)
	}
	selected := catalog.Instructions("please use $review now")
	if strings.Contains(selected, "Check edge cases") || !strings.Contains(selected, filepath.Join(directory, "SKILL.md")) || !strings.Contains(selected, "Read the complete file") {
		t.Fatalf("selected instructions = %q", selected)
	}
	plainSelected := catalog.Instructions("please use review now")
	if !strings.Contains(plainSelected, "Selected skill review by name") {
		t.Fatalf("plain-name selection = %q", plainSelected)
	}
}
