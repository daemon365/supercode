package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daemon365/supercode/internal/provider"
)

func TestPersistentCommandPrefixRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.AddCommandPrefix("go test")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	call := provider.ToolCall{Name: "exec_command", Arguments: `{"cmd":"go test ./internal/..."}`}
	if matched, ok := reloaded.Allows(call); !ok || matched.ID != rule.ID {
		t.Fatalf("matched=%#v ok=%t", matched, ok)
	}
	if removed, err := reloaded.Remove(rule.ID); err != nil || !removed {
		t.Fatalf("remove=%t err=%v", removed, err)
	}
}

func TestPolicyRejectsShellControlSyntax(t *testing.T) {
	if _, ok := ParseCommandPrefix("go test; curl example.com"); ok {
		t.Fatal("unsafe command prefix was accepted")
	}
}

func TestFailedSaveDoesNotActivateRuleInMemory(t *testing.T) {
	root := t.TempDir()
	blockedParent := filepath.Join(root, "not-a-directory")
	store, err := NewStore(filepath.Join(blockedParent, "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedParent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTool("exec_command"); err == nil {
		t.Fatal("AddTool() error = nil")
	}
	if len(store.List()) != 0 {
		t.Fatalf("failed rule remained active: %#v", store.List())
	}
}
