package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestNewRequiresAPIKey(t *testing.T) {
	_, err := New(Config{})
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("New() error = %v, want %v", err, ErrMissingAPIKey)
	}
}

func TestErrorsRedactAPIKey(t *testing.T) {
	cause := context.Canceled
	err := redactedError("request failed", fmt.Errorf("authorization secret-key was rejected: %w", cause), "secret-key")
	if strings.Contains(err.Error(), "secret-key") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error was not redacted: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("redacted error lost its cause: %v", err)
	}
}

func TestGenerateRejectsNullCompletion(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("null")), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL("https://null.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	model := &Provider{client: client}
	if _, err := model.Generate(context.Background(), provider.Request{Model: "test", Prompt: "hello"}); err == nil || !strings.Contains(err.Error(), "null response") {
		t.Fatalf("null completion error = %v", err)
	}
}

func TestBoundedResponseBodyReturnsExplicitLimitError(t *testing.T) {
	body := provider.NewBoundedResponseBody(io.NopCloser(strings.NewReader("123456")), 5)
	value, err := io.ReadAll(body)
	if !errors.Is(err, provider.ErrHTTPResponseTooLarge) || string(value) != "12345" {
		t.Fatalf("bounded body value=%q err=%v", value, err)
	}
}

func TestGenerateUsesChatCompletionsAPI(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		if request.URL.String() != "https://api.example.test/chat/completions" {
			t.Errorf("URL = %s, want https://api.example.test/chat/completions", request.URL)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", authorization)
		}

		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Model != "gpt-test" {
			t.Errorf("model = %q, want gpt-test", body.Model)
		}
		wantMessages := []struct {
			role    string
			content string
		}{
			{role: "system", content: "be concise"},
			{role: "user", content: "earlier question"},
			{role: "assistant", content: "earlier answer"},
			{role: "user", content: "say hello"},
		}
		if len(body.Messages) != len(wantMessages) {
			t.Fatalf("message count = %d, want %d", len(body.Messages), len(wantMessages))
		}
		for index, want := range wantMessages {
			if body.Messages[index].Role != want.role || body.Messages[index].Content != want.content {
				t.Errorf("message %d = %+v, want role=%q content=%q", index, body.Messages[index], want.role, want.content)
			}
		}

		responseBody := `{
			"id":"chatcmpl_test",
			"object":"chat.completion",
			"created":0,
			"model":"gpt-test",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"hello from openai"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    request,
		}, nil
	})

	client := sdk.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL("https://api.example.test"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	openAIProvider := &Provider{client: client}

	response, err := openAIProvider.Generate(context.Background(), provider.Request{
		Model:        "gpt-test",
		Instructions: "be concise",
		Prompt:       "say hello",
		History: []provider.Message{
			{Role: provider.MessageRoleUser, Content: "earlier question"},
			{Role: provider.MessageRoleAssistant, Content: "earlier answer"},
		},
	})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if response.ID != "chatcmpl_test" {
		t.Errorf("response ID = %q, want chatcmpl_test", response.ID)
	}
	if response.Model != "gpt-test" {
		t.Errorf("response model = %q, want gpt-test", response.Model)
	}
	if response.Text != "hello from openai" {
		t.Errorf("response text = %q, want hello from openai", response.Text)
	}
	if response.Usage != (provider.Usage{InputTokens: 4, OutputTokens: 3, TotalTokens: 7}) {
		t.Errorf("response usage = %+v", response.Usage)
	}
}

func TestStreamUsesChatCompletionsAPI(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://stream.example.test/chat/completions" {
			t.Errorf("URL = %s, want https://stream.example.test/chat/completions", request.URL)
		}

		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Model != "gpt-stream" || !body.Stream {
			t.Errorf("request body = %+v", body)
		}
		if len(body.Messages) != 3 || body.Messages[2].Content != "say hello" {
			t.Errorf("messages = %+v", body.Messages)
		}

		streamBody := `data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","created":0,"model":"gpt-stream","choices":[{"index":0,"delta":{"role":"assistant","content":"hello "},"finish_reason":null}]}

data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","created":0,"model":"gpt-stream","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}

data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","created":0,"model":"gpt-stream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","created":0,"model":"gpt-stream","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}

data: [DONE]

`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(streamBody)),
			Request:    request,
		}, nil
	})

	client := sdk.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL("https://stream.example.test"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	openAIProvider := &Provider{client: client}

	stream, err := openAIProvider.Stream(context.Background(), provider.Request{
		Model:  "gpt-stream",
		Prompt: "say hello",
		History: []provider.Message{
			{Role: provider.MessageRoleUser, Content: "earlier question"},
			{Role: provider.MessageRoleAssistant, Content: "earlier answer"},
		},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var completed *provider.Response
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case provider.StreamEventTextDelta:
			text.WriteString(event.Delta)
		case provider.StreamEventCompleted:
			completed = event.Response
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if text.String() != "hello world" {
		t.Errorf("streamed text = %q, want hello world", text.String())
	}
	if completed == nil {
		t.Fatal("completed response = nil")
	}
	if completed.ID != "chatcmpl_stream" || completed.Text != "hello world" {
		t.Errorf("completed response = %+v", completed)
	}
	if completed.Usage.TotalTokens != 4 {
		t.Errorf("total tokens = %d, want 4", completed.Usage.TotalTokens)
	}
}

func TestStreamCollectsFragmentedToolCalls(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tools) != 1 || body.Tools[0].Function.Name != "read_file" {
			t.Fatalf("tools = %+v", body.Tools)
		}
		streamBody := `data: {"id":"chatcmpl_tool","object":"chat.completion.chunk","created":0,"model":"gpt-tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl_tool","object":"chat.completion.chunk","created":0,"model":"gpt-tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(streamBody)), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL("https://tool.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	model := &Provider{client: client}
	stream, err := model.Stream(context.Background(), provider.Request{
		Model: "gpt-tool", Prompt: "read it",
		Tools: []provider.ToolDefinition{{Name: "read_file", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var completed *provider.Response
	for stream.Next() {
		if stream.Current().Type == provider.StreamEventCompleted {
			completed = stream.Current().Response
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if completed == nil || len(completed.ToolCalls) != 1 {
		t.Fatalf("completed = %+v", completed)
	}
	call := completed.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "read_file" || call.Arguments != `{"path":"README.md"}` {
		t.Fatalf("tool call = %+v", call)
	}
	if completed.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", completed.FinishReason)
	}
}

func TestStreamRejectsEOFWithoutFinishReason(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `data: {"id":"truncated","object":"chat.completion.chunk","created":0,"model":"test","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}

data: [DONE]

`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client := sdk.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL("https://truncated.example.test"), option.WithHTTPClient(&http.Client{Transport: transport}))
	model := &Provider{client: client}
	stream, err := model.Stream(context.Background(), provider.Request{Model: "test", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for stream.Next() {
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "finish reason") {
		t.Fatalf("truncated stream error = %v", err)
	}
}

func TestChatParamsMapToolImageAsUserImageContent(t *testing.T) {
	params := newChatParams(provider.Request{Model: "vision-test", History: []provider.Message{
		{Role: provider.MessageRoleAssistant, ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "view_image", Arguments: `{"path":"x.png"}`}}},
		{Role: provider.MessageRoleTool, ToolCallID: "call_1", Content: "loaded"},
		{Role: provider.MessageRoleUser, Content: "Image returned by tool.", Images: []provider.Image{{MIMEType: "image/png", Data: "YWJj", Detail: "high"}}},
	}})
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if !strings.Contains(value, `"type":"image_url"`) || !strings.Contains(value, `data:image/png;base64,YWJj`) {
		t.Fatalf("params = %s", value)
	}
}
