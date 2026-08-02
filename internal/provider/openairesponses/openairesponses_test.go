package openairesponses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/daemon365/supercode/internal/provider"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestGenerateUsesResponsesAPIAndMapsTools(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://responses.example.test/responses" {
			t.Fatalf("URL = %s", request.URL)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "gpt-responses" || body["store"] != false {
			t.Fatalf("request = %#v", body)
		}
		input, _ := body["input"].([]any)
		encoded, _ := json.Marshal(input)
		if !strings.Contains(string(encoded), `"type":"function_call"`) || !strings.Contains(string(encoded), `"type":"function_call_output"`) {
			t.Fatalf("input = %s", encoded)
		}
		responseBody := `{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"gpt-responses","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody)), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://responses.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	model := &Provider{client: client, secrets: []string{"secret"}}
	response, err := model.Generate(context.Background(), provider.Request{
		Model: "gpt-responses", Prompt: "continue",
		History: []provider.Message{
			{Role: provider.MessageRoleAssistant, ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "read_file", Arguments: `{"path":"README.md"}`}}},
			{Role: provider.MessageRoleTool, ToolCallID: "call_1", Content: "contents"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != "done" || response.Usage.TotalTokens != 5 {
		t.Fatalf("response = %+v", response)
	}
}

func TestStreamUsesResponsesEvents(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello \"}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"model\":\"gpt-responses\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello world\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":2,\"total_tokens\":4}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://responses.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	stream, err := (&Provider{client: client, secrets: []string{"secret"}}).Stream(context.Background(), provider.Request{Model: "gpt-responses", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var deltas string
	var completed *provider.Response
	for stream.Next() {
		event := stream.Current()
		if event.Type == provider.StreamEventTextDelta {
			deltas += event.Delta
		}
		if event.Type == provider.StreamEventCompleted {
			completed = event.Response
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if deltas != "hello world" || completed == nil || completed.Text != "hello world" {
		t.Fatalf("deltas = %q, completed = %+v", deltas, completed)
	}
}

func TestGenerateRejectsNullIncompleteAndStreamTruncation(t *testing.T) {
	for name, body := range map[string]string{
		"null":       "null",
		"incomplete": "{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"incomplete\",\"model\":\"gpt-responses\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"incomplete\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial\",\"annotations\":[]}]}]}",
	} {
		t.Run(name, func(t *testing.T) {
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			client := sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://responses.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
			if _, err := (&Provider{client: client}).Generate(context.Background(), provider.Request{Model: "test", Prompt: "hello"}); err == nil {
				t.Fatal("invalid response was accepted")
			}
		})
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://responses.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	stream, err := (&Provider{client: client}).Stream(context.Background(), provider.Request{Model: "test", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next() {
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("truncated stream error = %v", err)
	}
}
