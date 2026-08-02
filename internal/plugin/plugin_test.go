package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverPluginContributions(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(directory, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `name: demo
version: 1.0.0
instructions: Use the demo extension.
mcp_servers:
  demo:
    transport: stdio
    command: demo-server
hooks:
  session_start:
    - command: [demo-hook]
`
	if err := os.WriteFile(filepath.Join(directory, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Names) != 1 || bundle.Names[0] != "demo" {
		t.Fatalf("names = %#v", bundle.Names)
	}
	if bundle.Overlay.MCPServers["demo"].Command != "demo-server" || len(bundle.Overlay.Hooks["session_start"]) != 1 {
		t.Fatalf("overlay = %#v", bundle.Overlay)
	}
	if len(bundle.SkillRoots) != 1 || bundle.SkillRoots[0] != filepath.Join(directory, "skills") {
		t.Fatalf("skill roots = %#v", bundle.SkillRoots)
	}
}
