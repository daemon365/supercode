package credential

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolverPrecedenceAndCommand(t *testing.T) {
	resolver := Resolver{LookupEnv: func(name string) (string, bool) {
		if name == "OPENAI_API_KEY" {
			return "environment-key", true
		}
		return "", false
	}}
	key, err := resolver.Resolve(context.Background(), Source{Token: "static-key", Command: []string{"must-not-run"}})
	if err != nil || key != "environment-key" {
		t.Fatalf("environment key = %q, err = %v", key, err)
	}

	resolver.LookupEnv = func(string) (string, bool) { return "", false }
	key, err = resolver.Resolve(context.Background(), Source{Token: "static-key", Command: []string{"/bin/sh", "-c", "printf command-key"}})
	if err != nil || key != "command-key" {
		t.Fatalf("command key = %q, err = %v", key, err)
	}
}

func TestResolverBoundsCommandError(t *testing.T) {
	resolver := Resolver{LookupEnv: func(string) (string, bool) { return "", false }}
	_, err := resolver.Resolve(context.Background(), Source{Command: []string{"/bin/sh", "-c", "printf 'credential-secret-%020000d' 0 >&2; exit 1"}})
	if err == nil {
		t.Fatal("Resolve() error = nil")
	}
	if strings.Contains(err.Error(), "credential-secret") || strings.Count(err.Error(), "0") != 0 {
		t.Fatalf("credential command stderr leaked: %q", err)
	}
}

func TestResolverRejectsTruncatedCredential(t *testing.T) {
	resolver := Resolver{
		LookupEnv:   func(string) (string, bool) { return "", false },
		StdoutLimit: 4,
	}
	_, err := resolver.Resolve(context.Background(), Source{Command: []string{"/bin/sh", "-c", "printf abcdef"}})
	if err == nil || !strings.Contains(err.Error(), "exceeded 4 bytes") {
		t.Fatalf("oversized credential error = %v", err)
	}
}

func TestResolverTimeoutTerminatesChildHoldingOutputPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	resolver := Resolver{
		LookupEnv:      func(string) (string, bool) { return "", false },
		CommandTimeout: 30 * time.Millisecond,
	}
	started := time.Now()
	_, err := resolver.Resolve(context.Background(), Source{Command: []string{"/bin/sh", "-c", "sleep 30 & wait"}})
	if err == nil {
		t.Fatal("Resolve() timeout error = nil")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("credential child kept output pipe open for %s", elapsed)
	}
}
