package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/daemon365/supercode/internal/provider"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type testClient struct{ transport roundTripFunc }

func (client testClient) Do(request *http.Request) (*http.Response, error) {
	return client.transport(request)
}

func TestGenerateUsesOfficialSDKChatEndpoint(t *testing.T) {
	client := testClient{transport: func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://router.example.test/api/v1/chat/completions" {
			t.Fatalf("URL = %s", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-Test") != "configured" {
			t.Fatalf("headers = %#v", request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "openai/gpt-4o" || body["stream"] != false {
			t.Fatalf("request = %#v", body)
		}
		responseBody := `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hello"}}],"created":0,"id":"gen_1","model":"openai/gpt-4o","object":"chat.completion","system_fingerprint":null,"usage":{"completion_tokens":1,"prompt_tokens":2,"total_tokens":3}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody)), Request: request}, nil
	}}
	model, err := New(Config{APIKey: "secret", BaseURL: "https://router.example.test/api/v1", Headers: map[string]string{"X-Test": "configured"}, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(context.Background(), provider.Request{Model: "openai/gpt-4o", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != "hello" || response.Usage.TotalTokens != 3 {
		t.Fatalf("response = %+v", response)
	}
}

func TestStreamCollectsOpenRouterDeltas(t *testing.T) {
	client := testClient{transport: func(request *http.Request) (*http.Response, error) {
		body := "data: {\"id\":\"gen_stream\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"openai/gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"gen_stream\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"openai/gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	}}
	model, err := New(Config{APIKey: "secret", BaseURL: "https://router.example.test/api/v1", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := model.Stream(context.Background(), provider.Request{Model: "openai/gpt-4o", Prompt: "hello"})
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
	if delta != "hello" || completed == nil || completed.Usage.TotalTokens != 3 {
		t.Fatalf("delta = %q, completed = %+v", delta, completed)
	}
}

func TestStreamRejectsEOFWithoutFinishReason(t *testing.T) {
	client := testClient{transport: func(request *http.Request) (*http.Response, error) {
		body := "data: {\"id\":\"truncated\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	}}
	model, err := New(Config{APIKey: "secret", BaseURL: "https://router.example.test/api/v1", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := model.Stream(context.Background(), provider.Request{Model: "test", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next() {
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "finish reason") {
		t.Fatalf("truncated stream error = %v", err)
	}
}
