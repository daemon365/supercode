package policy

import (
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
