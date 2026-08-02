package provider

import (
	"context"
	"errors"
	"testing"
)

type routingProvider struct{ model string }

func (p *routingProvider) Generate(_ context.Context, request Request) (Response, error) {
	p.model = request.Model
	return Response{Model: request.Model, Text: "ok"}, nil
}
func (p *routingProvider) Stream(context.Context, Request) (Stream, error) {
	return nil, errors.New("not implemented")
}

func TestRouterDispatchesQualifiedDuplicateModels(t *testing.T) {
	first, second := &routingProvider{}, &routingProvider{}
	router, err := NewRouter([]Route{
		{Name: "copilot", Models: []string{"gpt-4"}, Provider: first},
		{Name: "openai", Models: []string{"gpt-4"}, Provider: second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Generate(context.Background(), Request{Model: "gpt-4", Prompt: "hi"}); err == nil {
		t.Fatal("ambiguous unqualified model was accepted")
	}
	if _, err := router.Generate(context.Background(), Request{Model: "copilot/gpt-4", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if first.model != "gpt-4" || second.model != "" {
		t.Fatalf("routed models = first %q, second %q", first.model, second.model)
	}
}

func TestRouterResolvesUniqueUnqualifiedModel(t *testing.T) {
	target := &routingProvider{}
	router, err := NewRouter([]Route{{Name: "anthropic", Models: []string{"claude"}, Provider: target}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Generate(context.Background(), Request{Model: "claude", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	if target.model != "claude" {
		t.Fatalf("model = %q", target.model)
	}
}
