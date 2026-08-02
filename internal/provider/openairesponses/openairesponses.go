// Package openairesponses adapts the official OpenAI Go SDK Responses API to
// SuperCode's provider-neutral boundary.
package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"

	"github.com/daemon365/supercode/internal/provider"
)

var ErrMissingAPIKey = errors.New("OpenAI Responses API key is required")

type Config struct {
	APIKey     string
	BaseURL    string
	MaxRetries int
	Headers    map[string]string
}

type Provider struct {
	client  sdk.Client
	secrets []string
}

type responseStream struct {
	stream            *ssestream.Stream[responses.ResponseStreamEventUnion]
	current           provider.StreamEvent
	response          provider.Response
	err               error
	emittedCompletion bool
	secrets           []string
	textBytes         int
}

var _ provider.Provider = (*Provider)(nil)

func New(config Config) (*Provider, error) {
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	if config.MaxRetries < 0 {
		return nil, errors.New("OpenAI Responses max retries must not be negative")
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 2
	}
	options := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(config.MaxRetries), option.WithHTTPClient(provider.SecureHTTPClient(nil))}
	secrets := []string{apiKey}
	if baseURL := strings.TrimSpace(config.BaseURL); baseURL != "" {
		options = append(options, option.WithBaseURL(baseURL))
	}
	for name, value := range config.Headers {
		if strings.TrimSpace(name) != "" {
			options = append(options, option.WithHeader(name, value))
			if strings.TrimSpace(value) != "" {
				secrets = append(secrets, value)
			}
		}
	}
	return &Provider{client: sdk.NewClient(options...), secrets: secrets}, nil
}

func (p *Provider) Generate(ctx context.Context, request provider.Request) (provider.Response, error) {
	if err := request.Validate(); err != nil {
		return provider.Response{}, err
	}
	params, err := newResponseParams(request)
	if err != nil {
		return provider.Response{}, err
	}
	value, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return provider.Response{}, redactedError("openai-compatible responses API", err, p.secrets...)
	}
	if value == nil {
		return provider.Response{}, errors.New("openai-compatible responses API returned a null response")
	}
	if string(value.Status) != "completed" {
		return provider.Response{}, fmt.Errorf("openai-compatible responses API returned non-completed status %q", value.Status)
	}
	result := mapResponse(*value)
	if err := validateResponse(result); err != nil {
		return provider.Response{}, err
	}
	if result.Text == "" && len(result.ToolCalls) == 0 {
		return provider.Response{}, errors.New("openai-compatible responses API returned no text or tool calls")
	}
	return result, nil
}

func (p *Provider) Stream(ctx context.Context, request provider.Request) (provider.Stream, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	params, err := newResponseParams(request)
	if err != nil {
		return nil, err
	}
	return &responseStream{stream: p.client.Responses.NewStreaming(ctx, params), secrets: append([]string(nil), p.secrets...)}, nil
}

type wireRequest struct {
	Model             string      `json:"model"`
	Instructions      string      `json:"instructions,omitempty"`
	Input             []any       `json:"input"`
	Tools             []wireTool  `json:"tools,omitempty"`
	ParallelToolCalls *bool       `json:"parallel_tool_calls,omitempty"`
	Reasoning         *wireEffort `json:"reasoning,omitempty"`
	ServiceTier       string      `json:"service_tier,omitempty"`
	Store             bool        `json:"store"`
}

type wireTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}

type wireEffort struct {
	Effort string `json:"effort"`
}

func newResponseParams(request provider.Request) (responses.ResponseNewParams, error) {
	wire := wireRequest{
		Model: request.Model, Instructions: strings.TrimSpace(request.Instructions), Store: false,
		ParallelToolCalls: request.ParallelToolCalls, ServiceTier: strings.TrimSpace(request.ServiceTier),
	}
	if effort := strings.TrimSpace(request.ReasoningEffort); effort != "" {
		wire.Reasoning = &wireEffort{Effort: effort}
	}
	for _, message := range request.History {
		appendResponseMessage(&wire.Input, message)
	}
	if request.Prompt != "" || len(request.Images) > 0 {
		appendResponseMessage(&wire.Input, provider.Message{Role: provider.MessageRoleUser, Content: request.Prompt, Images: request.Images})
	}
	for _, definition := range request.Tools {
		parameters := any(map[string]any{})
		if len(definition.Parameters) > 0 {
			if err := json.Unmarshal(definition.Parameters, &parameters); err != nil {
				return responses.ResponseNewParams{}, fmt.Errorf("tool %s parameters: %w", definition.Name, err)
			}
		}
		wire.Tools = append(wire.Tools, wireTool{Type: "function", Name: definition.Name, Description: definition.Description, Parameters: parameters})
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	var params responses.ResponseNewParams
	if err := json.Unmarshal(encoded, &params); err != nil {
		return responses.ResponseNewParams{}, fmt.Errorf("build Responses request: %w", err)
	}
	return params, nil
}

func appendResponseMessage(input *[]any, message provider.Message) {
	switch message.Role {
	case provider.MessageRoleAssistant:
		if message.Content != "" {
			*input = append(*input, map[string]any{"type": "message", "role": "assistant", "content": message.Content})
		}
		for _, call := range message.ToolCalls {
			*input = append(*input, map[string]any{
				"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments,
			})
		}
	case provider.MessageRoleTool:
		*input = append(*input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content})
	case provider.MessageRoleUser:
		if len(message.Images) == 0 {
			*input = append(*input, map[string]any{"type": "message", "role": "user", "content": message.Content})
			return
		}
		content := make([]map[string]any, 0, len(message.Images)+1)
		if message.Content != "" {
			content = append(content, map[string]any{"type": "input_text", "text": message.Content})
		}
		for _, image := range message.Images {
			content = append(content, map[string]any{
				"type": "input_image", "image_url": "data:" + image.MIMEType + ";base64," + image.Data,
				"detail": defaultImageDetail(image.Detail),
			})
		}
		*input = append(*input, map[string]any{"type": "message", "role": "user", "content": content})
	}
}

func defaultImageDetail(value string) string {
	if value == "" || value == "original" {
		return "high"
	}
	return value
}

func mapResponse(value responses.Response) provider.Response {
	result := provider.Response{
		ID: value.ID, Model: string(value.Model), Text: value.OutputText(), FinishReason: string(value.Status),
		Usage: provider.Usage{InputTokens: value.Usage.InputTokens, OutputTokens: value.Usage.OutputTokens, TotalTokens: value.Usage.TotalTokens},
	}
	for _, item := range value.Output {
		if item.Type != "function_call" {
			continue
		}
		call := item.AsFunctionCall()
		if call.CallID != "" && call.Name != "" {
			result.ToolCalls = append(result.ToolCalls, provider.ToolCall{ID: call.CallID, Name: call.Name, Arguments: call.Arguments})
		}
	}
	return result
}

func validateResponse(value provider.Response) error {
	if len(value.Text) > 16*1024*1024 || len(value.ToolCalls) > 64 {
		return errors.New("openai-compatible responses output exceeds configured text or tool-call limits")
	}
	totalArguments := 0
	for _, call := range value.ToolCalls {
		if len(call.ID) > 1024 || len(call.Name) > 256 || len(call.Arguments) > 1024*1024 || len(call.Arguments) > 8*1024*1024-totalArguments {
			return errors.New("openai-compatible responses tool call exceeds configured size limits")
		}
		totalArguments += len(call.Arguments)
	}
	return nil
}

func (s *responseStream) Next() bool {
	if s.err != nil || s.emittedCompletion {
		return false
	}
	for s.stream.Next() {
		event := s.stream.Current()
		switch event.Type {
		case "response.output_text.delta":
			if len(event.Delta) > 16*1024*1024-s.textBytes {
				s.err = errors.New("openai-compatible responses stream text exceeds the 16 MiB limit")
				return false
			}
			s.textBytes += len(event.Delta)
			s.current = provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: event.Delta}
			return true
		case "response.completed":
			s.response = mapResponse(event.Response)
			return s.complete()
		case "response.failed", "response.incomplete":
			s.err = redactedError("openai-compatible responses stream", errors.New(event.Response.Error.Message), s.secrets...)
			return false
		case "error":
			s.err = redactedError("openai-compatible responses stream", errors.New(event.Message), s.secrets...)
			return false
		}
	}
	if err := s.stream.Err(); err != nil {
		s.err = redactedError("openai-compatible responses stream", err, s.secrets...)
		return false
	}
	s.err = errors.New("openai-compatible responses stream ended before response.completed")
	return false
}

func (s *responseStream) complete() bool {
	if err := validateResponse(s.response); err != nil {
		s.err = err
		return false
	}
	if s.response.Text == "" && len(s.response.ToolCalls) == 0 {
		s.err = errors.New("openai-compatible responses stream returned no text or tool calls")
		return false
	}
	s.emittedCompletion = true
	response := s.response
	s.current = provider.StreamEvent{Type: provider.StreamEventCompleted, Response: &response}
	return true
}

func redactedError(prefix string, err error, secrets ...string) error {
	return provider.RedactedError(prefix, err, secrets...)
}

func (s *responseStream) Current() provider.StreamEvent { return s.current }
func (s *responseStream) Err() error                    { return s.err }
func (s *responseStream) Close() error                  { return s.stream.Close() }
