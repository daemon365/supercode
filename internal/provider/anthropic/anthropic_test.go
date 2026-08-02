package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/daemon365/supercode/internal/provider"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestGenerateUsesMessagesAPIAndMapsToolUse(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://anthropic.example.test/v1/messages" {
			t.Fatalf("URL = %s", request.URL)
		}
		if request.Header.Get("X-Api-Key") != "secret" || request.Header.Get("Anthropic-Version") == "" {
			t.Fatalf("headers = %#v", request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "claude-test" || body["max_tokens"] != float64(4096) {
			t.Fatalf("request = %#v", body)
		}
		outputConfig, ok := body["output_config"].(map[string]any)
		if !ok || outputConfig["effort"] != "high" {
			t.Fatalf("output_config = %#v", body["output_config"])
		}
		encoded, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(encoded), `"type":"tool_use"`) || !strings.Contains(string(encoded), `"type":"tool_result"`) || !strings.Contains(string(encoded), `"is_error":true`) {
			t.Fatalf("messages = %s", encoded)
		}
		responseBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":3}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody)), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://anthropic.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	model := &Provider{client: client, maxTokens: 4096, secrets: []string{"secret"}}
	response, err := model.Generate(context.Background(), provider.Request{
		Model: "claude-test", Prompt: "continue", ReasoningEffort: "high",
		History: []provider.Message{
			{Role: provider.MessageRoleAssistant, ToolCalls: []provider.ToolCall{{ID: "old_call", Name: "read_file", Arguments: `{"path":"old"}`}}},
			{Role: provider.MessageRoleTool, ToolCallID: "old_call", Content: "ERROR: old contents"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments != `{"path":"README.md"}` || response.Usage.TotalTokens != 8 {
		t.Fatalf("response = %+v", response)
	}
}

func TestGenerateRejectsNullAndStreamRejectsTruncation(t *testing.T) {
	nullTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("null")), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://anthropic.example.test"), option.WithHTTPClient(&http.Client{Transport: nullTransport}))
	model := &Provider{client: client, maxTokens: 1024}
	if _, err := model.Generate(context.Background(), provider.Request{Model: "claude-test", Prompt: "hello"}); err == nil || !strings.Contains(err.Error(), "null response") {
		t.Fatalf("null response error = %v", err)
	}

	streamTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client = sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://anthropic.example.test"), option.WithHTTPClient(&http.Client{Transport: streamTransport}))
	stream, err := (&Provider{client: client, maxTokens: 1024}).Stream(context.Background(), provider.Request{Model: "claude-test", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next() {
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("truncated stream error = %v", err)
	}
}

func TestStreamUsesMessagesEvents(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("https://anthropic.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	stream, err := (&Provider{client: client, maxTokens: 1024, secrets: []string{"secret"}}).Stream(context.Background(), provider.Request{Model: "claude-test", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var delta string
	var completed *provider.Response
	for stream.Next() {
		if stream.Current().Type == provider.StreamEventTextDelta {
			delta += stream.Current().Delta
		}
		if stream.Current().Type == provider.StreamEventCompleted {
			completed = stream.Current().Response
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if delta != "hello" || completed == nil || completed.Text != "hello" || completed.Usage.TotalTokens != 3 {
		t.Fatalf("delta = %q, completed = %+v", delta, completed)
	}
}
