package permission

import (
	"path/filepath"
	"testing"
)

func TestManagerExpiresTurnGrantsAndKeepsSessionGrants(t *testing.T) {
	workspace := t.TempDir()
	readRoot := t.TempDir()
	writeRoot := t.TempDir()
	manager, err := NewManager(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Grant(Profile{FileSystem: FileSystem{Read: []string{readRoot}}, Network: Network{Domains: []string{"example.com"}, Protocols: []string{"https"}}}, ScopeTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Grant(Profile{FileSystem: FileSystem{Write: []string{writeRoot}}}, ScopeSession); err != nil {
		t.Fatal(err)
	}
	if !contains(manager.ReadRoots(), filepath.Clean(readRoot)) || !manager.AllowsURL("https://api.example.com/v1") {
		t.Fatal("turn grants were not applied")
	}
	manager.BeginTurn()
	if contains(manager.ReadRoots(), filepath.Clean(readRoot)) || manager.AllowsURL("https://example.com") {
		t.Fatal("turn grants survived BeginTurn")
	}
	if !contains(manager.WriteRoots(), filepath.Clean(writeRoot)) {
		t.Fatal("session grant expired with the turn")
	}
}

func TestManagerRequiresExplicitNetworkWildcardForShell(t *testing.T) {
	manager, _ := NewManager(t.TempDir())
	_, _ = manager.Grant(Profile{Network: Network{Domains: []string{"example.com"}, Protocols: []string{"https"}}}, ScopeSession)
	if manager.AllowsUnrestrictedNetwork() {
		t.Fatal("domain grant opened unrestricted shell networking")
	}
	_, _ = manager.Grant(Profile{Network: Network{Domains: []string{"*"}, Protocols: []string{"*"}}}, ScopeSession)
	if !manager.AllowsUnrestrictedNetwork() {
		t.Fatal("explicit wildcard grant was not recognized")
	}
}
