package app

import (
	"archive/zip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/policy"
)

func TestExportDiagnosticsRedactsAllMCPSecretSources(t *testing.T) {
	root := t.TempDir()
	policyStore, err := policy.NewStore(filepath.Join(root, "policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.File{
		Token:        "top-secret-token",
		TokenCommand: []string{"secret-tool", "top-secret-command-argument"},
		Providers: []config.ProviderConfig{{
			Name: "private", Provider: "openai", URL: "https://user:top-secret-provider-password@example.com/v1/top-secret-provider-path?key=top-secret-provider-query",
			Token: "top-secret-provider-key", TokenCommand: []string{"secret-tool", "top-secret-provider-command"}, Models: []string{"private-model"},
			Headers: map[string]string{"Authorization": "top-secret-provider-header"},
		}},
		MCPServers: map[string]config.MCPServer{"example": {
			URL: "https://user:top-secret-password@example.com/mcp/top-secret-path?api_key=top-secret-query#top-secret-fragment",
			Env: map[string]string{"ORDINARY_NAME": "top-secret-env"},
			Headers: map[string]string{
				"X-Api-Key": "top-secret-header", "Cookie": "top-secret-cookie", "X-Ordinary": "top-secret-ordinary-header",
			},
			OAuthTokenCommand: []string{"secret-tool", "top-secret-oauth"},
		}},
	}
	archivePath := filepath.Join(root, "diagnostics.zip")
	if err := exportDiagnostics(archivePath, filepath.Join(root, "config.yaml"), root, configuration, policyStore, func(string) (string, bool) { return "", false }, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	var content string
	for _, entry := range archive.File {
		if entry.Name != "config.redacted.yaml" {
			continue
		}
		file, openErr := entry.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		data, readErr := io.ReadAll(file)
		if readErr != nil {
			file.Close()
			t.Fatal(readErr)
		}
		file.Close()
		content = string(data)
	}
	if content == "" || strings.Contains(content, "top-secret") || !strings.Contains(content, "[redacted]") {
		t.Fatalf("redacted config = %s", content)
	}
	if configuration.MCPServers["example"].Env["ORDINARY_NAME"] != "top-secret-env" {
		t.Fatal("diagnostics export mutated the caller's configuration")
	}
	if configuration.Providers[0].Token != "top-secret-provider-key" || configuration.Providers[0].Headers["Authorization"] != "top-secret-provider-header" {
		t.Fatal("diagnostics export mutated provider configuration")
	}
}

func TestDebugEventLogDoesNotPersistToolSummaryOrErrorText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.jsonl")
	logger, err := newEventLogger(options{debugLog: path, workspace: t.TempDir(), modelName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	logger(agent.Event{Type: agent.EventError, Summary: `write_stdin {"chars":"top-secret"}`, Err: errors.New("top-secret")})
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "top-secret") || !strings.Contains(string(content), "summary_bytes") {
		t.Fatalf("debug log leaked event content: %s", content)
	}
}
