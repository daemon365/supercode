// Package openrouter adapts OpenRouter's official Go SDK to SuperCode's
// provider-neutral boundary.
package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	openroutersdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	orstream "github.com/OpenRouterTeam/go-sdk/types/stream"

	"github.com/daemon365/supercode/internal/provider"
)

var ErrMissingAPIKey = errors.New("OpenRouter API key is required")

type Config struct {
	APIKey     string
	BaseURL    string
	Headers    map[string]string
	HTTPClient openroutersdk.HTTPClient
}

type Provider struct {
	client  *openroutersdk.OpenRouter
	secrets []string
}

type responseStream struct {
	stream            *orstream.EventStream[components.ChatStreamingResponse]
	current           provider.StreamEvent
	response          provider.Response
	err               error
	emittedCompletion bool
	toolCallsByIndex  map[int64]*streamToolCall
	text              strings.Builder
	totalToolArgs     int
	terminal          bool
	secrets           []string
}

type streamToolCall struct {
	id        string
	name      strings.Builder
	arguments strings.Builder
}

var _ provider.Provider = (*Provider)(nil)

func New(config Config) (*Provider, error) {
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	options := []openroutersdk.SDKOption{openroutersdk.WithSecurity(apiKey)}
	if baseURL := strings.TrimSpace(config.BaseURL); baseURL != "" {
		options = append(options, openroutersdk.WithServerURL(strings.TrimRight(baseURL, "/")))
	}
	client := config.HTTPClient
	if client == nil {
		client = provider.SecureHTTPClient(nil)
	} else if concrete, ok := client.(*http.Client); ok {
		client = provider.SecureHTTPClient(concrete)
	} else {
		client = provider.SecureHTTPDoer(client)
	}
	secrets := []string{apiKey}
	if len(config.Headers) > 0 {
		client = &headerClient{next: client, headers: cloneHeaders(config.Headers)}
		for _, value := range config.Headers {
			if strings.TrimSpace(value) != "" {
				secrets = append(secrets, value)
			}
		}
	}
	options = append(options, openroutersdk.WithClient(client))
	return &Provider{client: openroutersdk.New(options...), secrets: secrets}, nil
}

func (p *Provider) Generate(ctx context.Context, request provider.Request) (provider.Response, error) {
	if err := request.Validate(); err != nil {
		return provider.Response{}, err
	}
	params, err := newChatRequest(request, false)
	if err != nil {
		return provider.Response{}, err
	}
	value, err := p.client.Chat.Send(ctx, params, nil, operations.WithAcceptHeaderOverride(operations.AcceptHeaderEnumApplicationJson))
	if err != nil {
		return provider.Response{}, redactedError("OpenRouter Chat Completions API", err, p.secrets...)
	}
	if value == nil || value.ChatResult == nil {
		return provider.Response{}, errors.New("OpenRouter Chat Completions API returned no JSON result")
	}
	result, err := mapChatResult(*value.ChatResult)
	if err != nil {
		return provider.Response{}, err
	}
	if result.Text == "" && len(result.ToolCalls) == 0 {
		return provider.Response{}, errors.New("OpenRouter Chat Completions API returned no text or tool calls")
	}
	return result, nil
}

func (p *Provider) Stream(ctx context.Context, request provider.Request) (provider.Stream, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	params, err := newChatRequest(request, true)
	if err != nil {
		return nil, err
	}
	value, err := p.client.Chat.Send(ctx, params, nil, operations.WithAcceptHeaderOverride(operations.AcceptHeaderEnumTextEventStream))
	if err != nil {
		return nil, redactedError("OpenRouter Chat Completions stream", err, p.secrets...)
	}
	if value == nil || value.EventStream == nil {
		return nil, errors.New("OpenRouter Chat Completions API returned no event stream")
	}
	return &responseStream{stream: value.EventStream, toolCallsByIndex: make(map[int64]*streamToolCall), secrets: append([]string(nil), p.secrets...)}, nil
}

type wireChatRequest struct {
	Model             string         `json:"model"`
	Messages          []any          `json:"messages"`
	Tools             []wireChatTool `json:"tools,omitempty"`
	Stream            bool           `json:"stream"`
	ReasoningEffort   string         `json:"reasoning_effort,omitempty"`
	ServiceTier       string         `json:"service_tier,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
}

type wireChatTool struct {
	Type     string           `json:"type"`
	Function wireChatFunction `json:"function"`
}

type wireChatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}

func newChatRequest(request provider.Request, stream bool) (components.ChatRequest, error) {
	wire := wireChatRequest{
		Model: request.Model, Stream: stream, ReasoningEffort: request.ReasoningEffort,
		ServiceTier: request.ServiceTier, ParallelToolCalls: request.ParallelToolCalls,
	}
	if instructions := strings.TrimSpace(request.Instructions); instructions != "" {
		wire.Messages = append(wire.Messages, map[string]any{"role": "system", "content": instructions})
	}
	for _, message := range request.History {
		wire.Messages = append(wire.Messages, chatMessage(message))
	}
	if request.Prompt != "" || len(request.Images) > 0 {
		wire.Messages = append(wire.Messages, chatMessage(provider.Message{Role: provider.MessageRoleUser, Content: request.Prompt, Images: request.Images}))
	}
	for _, definition := range request.Tools {
		parameters := any(map[string]any{})
		if len(definition.Parameters) > 0 {
			if err := json.Unmarshal(definition.Parameters, &parameters); err != nil {
				return components.ChatRequest{}, fmt.Errorf("tool %s parameters: %w", definition.Name, err)
			}
		}
		wire.Tools = append(wire.Tools, wireChatTool{Type: "function", Function: wireChatFunction{
			Name: definition.Name, Description: definition.Description, Parameters: parameters,
		}})
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return components.ChatRequest{}, err
	}
	var result components.ChatRequest
	if err := json.Unmarshal(encoded, &result); err != nil {
		return components.ChatRequest{}, fmt.Errorf("build OpenRouter request: %w", err)
	}
	return result, nil
}

func chatMessage(message provider.Message) any {
	switch message.Role {
	case provider.MessageRoleAssistant:
		calls := make([]map[string]any, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			calls = append(calls, map[string]any{
				"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments},
			})
		}
		return map[string]any{"role": "assistant", "content": message.Content, "tool_calls": calls}
	case provider.MessageRoleTool:
		return map[string]any{"role": "tool", "content": message.Content, "tool_call_id": message.ToolCallID}
	default:
		if len(message.Images) == 0 {
			return map[string]any{"role": "user", "content": message.Content}
		}
		content := make([]map[string]any, 0, len(message.Images)+1)
		if message.Content != "" {
			content = append(content, map[string]any{"type": "text", "text": message.Content})
		}
		for _, image := range message.Images {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:" + image.MIMEType + ";base64," + image.Data, "detail": defaultImageDetail(image.Detail),
			}})
		}
		return map[string]any{"role": "user", "content": content}
	}
}

type simpleChatResult struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

func mapChatResult(value components.ChatResult) (provider.Response, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return provider.Response{}, err
	}
	var wire simpleChatResult
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return provider.Response{}, err
	}
	if len(wire.Choices) == 0 {
		return provider.Response{}, errors.New("OpenRouter Chat Completions API returned no choices")
	}
	choice := wire.Choices[0]
	result := provider.Response{
		ID: wire.ID, Model: wire.Model, Text: choice.Message.Content, FinishReason: choice.FinishReason,
		Usage: provider.Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens, TotalTokens: wire.Usage.TotalTokens},
	}
	for _, call := range choice.Message.ToolCalls {
		if call.ID != "" && call.Function.Name != "" {
			result.ToolCalls = append(result.ToolCalls, provider.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
	}
	if err := validateResponse(result); err != nil {
		return provider.Response{}, err
	}
	return result, nil
}

func validateResponse(value provider.Response) error {
	if len(value.Text) > 16*1024*1024 || len(value.ToolCalls) > 64 {
		return errors.New("OpenRouter response exceeds configured text or tool-call limits")
	}
	totalArguments := 0
	for _, call := range value.ToolCalls {
		if len(call.ID) > 1024 || len(call.Name) > 256 || len(call.Arguments) > 1024*1024 || len(call.Arguments) > 8*1024*1024-totalArguments {
			return errors.New("OpenRouter tool call exceeds configured size limits")
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
		chunk := s.stream.Value()
		if chunk == nil {
			continue
		}
		data, err := json.Marshal(chunk.Data)
		if err != nil {
			s.err = err
			return false
		}
		var wire struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Index        int64  `json:"index"`
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int64                            `json:"index"`
						ID       string                           `json:"id"`
						Function struct{ Name, Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			s.err = err
			return false
		}
		if wire.Error != nil {
			s.err = redactedError("OpenRouter Chat Completions stream", errors.New(wire.Error.Message), s.secrets...)
			return false
		}
		if s.response.ID == "" {
			s.response.ID, s.response.Model = wire.ID, wire.Model
		}
		if wire.Usage.TotalTokens > 0 {
			s.response.Usage = provider.Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens, TotalTokens: wire.Usage.TotalTokens}
		}
		for _, choice := range wire.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.FinishReason != "" {
				s.response.FinishReason = choice.FinishReason
				s.terminal = true
			}
			for _, delta := range choice.Delta.ToolCalls {
				if delta.Index < 0 || delta.Index >= 64 {
					s.err = errors.New("OpenRouter stream returned an invalid tool-call index")
					return false
				}
				call := s.toolCallsByIndex[delta.Index]
				if call == nil {
					call = &streamToolCall{}
					s.toolCallsByIndex[delta.Index] = call
				}
				if delta.ID != "" {
					if len(delta.ID) > 1024 {
						s.err = errors.New("OpenRouter stream tool-call ID is too large")
						return false
					}
					call.id = delta.ID
				}
				if len(delta.Function.Name) > 256-call.name.Len() || len(delta.Function.Arguments) > 1024*1024-call.arguments.Len() || len(delta.Function.Arguments) > 8*1024*1024-s.totalToolArgs {
					s.err = errors.New("OpenRouter stream tool call exceeds configured size limits")
					return false
				}
				call.name.WriteString(delta.Function.Name)
				call.arguments.WriteString(delta.Function.Arguments)
				s.totalToolArgs += len(delta.Function.Arguments)
			}
			if choice.Delta.Content != "" {
				if len(choice.Delta.Content) > 16*1024*1024-s.text.Len() {
					s.err = errors.New("OpenRouter stream text exceeds the 16 MiB limit")
					return false
				}
				s.text.WriteString(choice.Delta.Content)
				s.current = provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: choice.Delta.Content}
				return true
			}
		}
	}
	if err := s.stream.Err(); err != nil {
		s.err = redactedError("OpenRouter Chat Completions stream", err, s.secrets...)
		return false
	}
	if !s.terminal {
		s.err = errors.New("OpenRouter Chat Completions stream ended before a finish reason")
		return false
	}
	for index := 0; index < len(s.toolCallsByIndex); index++ {
		call := s.toolCallsByIndex[int64(index)]
		if call == nil || call.id == "" || call.name.Len() == 0 {
			s.err = errors.New("OpenRouter stream returned incomplete or non-contiguous tool calls")
			return false
		}
		s.response.ToolCalls = append(s.response.ToolCalls, provider.ToolCall{ID: call.id, Name: call.name.String(), Arguments: call.arguments.String()})
	}
	s.response.Text = s.text.String()
	if err := validateResponse(s.response); err != nil {
		s.err = err
		return false
	}
	if s.response.Text == "" && len(s.response.ToolCalls) == 0 {
		s.err = errors.New("OpenRouter Chat Completions stream returned no text or tool calls")
		return false
	}
	s.emittedCompletion = true
	response := s.response
	s.current = provider.StreamEvent{Type: provider.StreamEventCompleted, Response: &response}
	return true
}

func defaultImageDetail(value string) string {
	if value == "" || value == "original" {
		return "high"
	}
	return value
}

type headerClient struct {
	next    openroutersdk.HTTPClient
	headers map[string]string
}

func (c *headerClient) Do(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for name, value := range c.headers {
		clone.Header.Set(name, value)
	}
	return c.next.Do(clone)
}
func cloneHeaders(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}
func redactedError(prefix string, err error, secrets ...string) error {
	return provider.RedactedError(prefix, err, secrets...)
}
func (s *responseStream) Current() provider.StreamEvent { return s.current }
func (s *responseStream) Err() error                    { return s.err }
func (s *responseStream) Close() error                  { return s.stream.Close() }
