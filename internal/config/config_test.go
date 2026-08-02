package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureCreatesSecureConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".supercode", "config.yaml")
	created, err := Ensure(path)
	if err != nil {
		t.Fatalf("Ensure() error: %v", err)
	}
	if !created {
		t.Fatal("Ensure() created = false, want true")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load starter config: %v", err)
	}
	if loaded.Model != "openai/gpt-5.6" || len(loaded.Providers) != 1 {
		t.Fatalf("starter provider config = model %q, providers %#v", loaded.Model, loaded.Providers)
	}
	starter := loaded.Providers[0]
	if starter.Name != "openai" || starter.Provider != "openai_responses" || starter.URL != DefaultURL || starter.Token != "${OPENAI_API_KEY}" || len(starter.Models) != 1 || starter.Models[0] != "gpt-5.6" {
		t.Fatalf("starter provider = %#v", starter)
	}
	if loaded.URL != "" || loaded.Token != "" || len(loaded.Models) != 0 {
		t.Fatalf("starter config unexpectedly uses legacy endpoint fields: %#v", loaded)
	}
	if !strings.Contains(string(contents), "memory_generate: false") || !strings.Contains(string(contents), "memory_use: true") {
		t.Fatalf("starter config does not document file-backed memory: %s", contents)
	}

	if runtime.GOOS != "windows" {
		directoryInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat config directory: %v", err)
		}
		if got := directoryInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("config directory mode = %o, want 700", got)
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat config file: %v", err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("config file mode = %o, want 600", got)
		}
	}
}

func TestEnsureDoesNotOverwriteExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("model: custom\n"), 0o644); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	created, err := Ensure(path)
	if err != nil {
		t.Fatalf("Ensure() error: %v", err)
	}
	if created {
		t.Fatal("Ensure() created = true, want false")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(contents) != "model: custom\n" {
		t.Fatalf("config was overwritten: %q", contents)
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "url: http://127.0.0.1:8000/v1\nmodel: local\ntoken: secret\nstream: false\ntimeout: 30s\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.URL != "http://127.0.0.1:8000/v1" || loaded.Model != "local" || loaded.Token != "secret" {
		t.Fatalf("Load() = %#v", loaded)
	}
	if loaded.Stream == nil || *loaded.Stream {
		t.Fatalf("Load().Stream = %v, want false", loaded.Stream)
	}
	if loaded.Timeout != "30s" {
		t.Fatalf("Load().Timeout = %q, want 30s", loaded.Timeout)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("modle: typo\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want unknown field error")
	}
}

func TestLoadMultipleProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `model: copilot/gpt-4
providers:
  - name: copilot
    provider: openai_responses
    url: http://127.0.0.1:3000/v1
    token: ${COPILOT_API_KEY}
    models: [gpt-4, gpt-5-codex]
  - name: claude
    provider: anthropic
    token: ${ANTHROPIC_API_KEY}
    maxTokens: 4096
    models: [claude-sonnet-4-6]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers) != 2 || loaded.Providers[0].URL != "http://127.0.0.1:3000/v1" || loaded.Providers[1].MaxTokens != 4096 {
		t.Fatalf("providers = %#v", loaded.Providers)
	}
}

func TestLoadRejectsLegacyProviderCredentialNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "providers:\n  - {name: one, provider: openai, baseUrl: https://one.example/v1, apiKey: secret, models: [one]}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field baseUrl not found") {
		t.Fatalf("Load() error = %v, want unknown legacy field", err)
	}
}

func TestLoadRejectsDuplicateProviderNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "providers:\n  - {name: same, provider: openai, models: [one]}\n  - {name: same, provider: anthropic, models: [two]}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("duplicate provider names were accepted")
	}
}

func TestMergeProjectConfiguration(t *testing.T) {
	stream := false
	base := File{
		Model: "base", Instructions: "user instructions", TrustedWorkspaces: []string{"/trusted"},
		MCPServers: map[string]MCPServer{"user": {Command: "user-server"}},
	}
	overlay := File{
		Model: "project", Instructions: "project instructions", Stream: &stream,
		MCPServers:        map[string]MCPServer{"project": {Command: "project-server"}},
		TrustedWorkspaces: []string{"/must-not-be-inherited"},
	}
	merged := Merge(base, overlay)
	if merged.Model != "project" || merged.Stream == nil || *merged.Stream {
		t.Fatalf("merged settings = %#v", merged)
	}
	if !strings.Contains(merged.Instructions, "user instructions") || !strings.Contains(merged.Instructions, "project instructions") {
		t.Fatalf("merged instructions = %q", merged.Instructions)
	}
	if len(merged.MCPServers) != 2 {
		t.Fatalf("MCP servers = %#v", merged.MCPServers)
	}
	if len(merged.TrustedWorkspaces) != 1 || merged.TrustedWorkspaces[0] != "/trusted" {
		t.Fatalf("project modified trust roots: %#v", merged.TrustedWorkspaces)
	}
}

func TestTrustWorkspacePersistsCanonicalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config_version: 1\nmodel: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := TrustWorkspace(path, workspace); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !IsWorkspaceTrusted(loaded, workspace) {
		t.Fatalf("workspace was not trusted: %#v", loaded.TrustedWorkspaces)
	}
}
