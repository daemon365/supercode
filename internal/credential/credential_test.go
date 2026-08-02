package credential

import (
	"context"
	"strings"
	"testing"
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
	_, err := resolver.Resolve(context.Background(), Source{Command: []string{"/bin/sh", "-c", "printf '%020000d' 0 >&2; exit 1"}})
	if err == nil {
		t.Fatal("Resolve() error = nil")
	}
	if strings.Count(err.Error(), "0") > defaultStderrLimit {
		t.Fatalf("command error was not bounded: %d bytes", len(err.Error()))
	}
}
