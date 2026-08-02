package diagnostic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerRedactsSecretFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.jsonl")
	logger, err := NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	logger.Log("test", map[string]any{"api_key": "secret", "model": "test"})
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "secret") || !strings.Contains(string(content), "[redacted]") {
		t.Fatalf("log = %s", content)
	}
}
