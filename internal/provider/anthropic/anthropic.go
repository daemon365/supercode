// Package anthropic adapts Anthropic's official Go SDK to SuperCode's
// provider-neutral boundary.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/daemon365/supercode/internal/provider"
)

const defaultMaxTokens = 8192

var ErrMissingAPIKey = errors.New("Anthropic API key is required")

type Config struct {
	APIKey     string
	BaseURL    string
	MaxRetries int
	MaxTokens  int64
	Headers    map[string]string
}

type Provider struct {
	client    sdk.Client
	maxTokens int64
	secrets   []string
}

type responseStream struct {
	stream            *ssestream.Stream[sdk.MessageStreamEventUnion]
	message           sdk.Message
	current           provider.StreamEvent
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
		return nil, errors.New("Anthropic max retries must not be negative")
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 2
	}
	if config.MaxTokens < 0 {
		return nil, errors.New("Anthropic max tokens must not be negative")
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = defaultMaxTokens
	}
	options := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(config.MaxRetries), option.WithHTTPClient(provider.SecureHTTPClient(nil))}
	secrets := []string{apiKey}
	if baseURL := strings.TrimSpace(config.BaseURL); baseURL != "" {
		baseURL = strings.TrimRight(baseURL, "/")
		baseURL = strings.TrimSuffix(baseURL, "/v1")
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
	return &Provider{client: sdk.NewClient(options...), maxTokens: config.MaxTokens, secrets: secrets}, nil
}

func (p *Provider) Generate(ctx context.Context, request provider.Request) (provider.Response, error) {
	if err := request.Validate(); err != nil {
		return provider.Response{}, err
	}
	params, err := newMessageParams(request, p.maxTokens)
	if err != nil {
		return provider.Response{}, err
	}
	message, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return provider.Response{}, redactedError("Anthropic Messages API", err, p.secrets...)
	}
	if message == nil {
		return provider.Response{}, errors.New("Anthropic Messages API returned a null response")
	}
	result := mapMessage(*message)
	if err := validateResponse(result); err != nil {
		return provider.Response{}, err
	}
	if result.Text == "" && len(result.ToolCalls) == 0 {
		return provider.Response{}, errors.New("Anthropic Messages API returned no text or tool calls")
	}
	return result, nil
}

func (p *Provider) Stream(ctx context.Context, request provider.Request) (provider.Stream, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	params, err := newMessageParams(request, p.maxTokens)
	if err != nil {
		return nil, err
	}
	return &responseStream{stream: p.client.Messages.NewStreaming(ctx, params), secrets: append([]string(nil), p.secrets...)}, nil
}

func newMessageParams(request provider.Request, maxTokens int64) (sdk.MessageNewParams, error) {
	params := sdk.MessageNewParams{Model: sdk.Model(request.Model), MaxTokens: maxTokens}
	if instructions := strings.TrimSpace(request.Instructions); instructions != "" {
		params.System = []sdk.TextBlockParam{{Text: instructions}}
	}
	for _, message := range request.History {
		blocks, role, err := anthropicBlocks(message)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		appendMessage(&params.Messages, role, blocks)
	}
	if request.Prompt != "" || len(request.Images) > 0 {
		blocks, role, err := anthropicBlocks(provider.Message{Role: provider.MessageRoleUser, Content: request.Prompt, Images: request.Images})
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		appendMessage(&params.Messages, role, blocks)
	}
	for _, definition := range request.Tools {
		var schema sdk.ToolInputSchemaParam
		if len(definition.Parameters) > 0 {
			if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
				return sdk.MessageNewParams{}, fmt.Errorf("tool %s parameters: %w", definition.Name, err)
			}
		}
		tool := sdk.ToolUnionParamOfTool(schema, definition.Name)
		if definition.Description != "" {
			tool.OfTool.Description = sdk.String(definition.Description)
		}
		params.Tools = append(params.Tools, tool)
	}
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls && len(params.Tools) > 0 {
		params.ToolChoice.OfAuto = &sdk.ToolChoiceAutoParam{DisableParallelToolUse: sdk.Bool(true)}
	}
	if effort := strings.TrimSpace(request.ReasoningEffort); effort != "" {
		params.OutputConfig.Effort = sdk.OutputConfigEffort(effort)
	}
	if tier := strings.TrimSpace(request.ServiceTier); tier == "auto" || tier == "standard_only" {
		params.ServiceTier = sdk.MessageNewParamsServiceTier(tier)
	}
	return params, nil
}

func anthropicBlocks(message provider.Message) ([]sdk.ContentBlockParamUnion, sdk.MessageParamRole, error) {
	role := sdk.MessageParamRoleUser
	var blocks []sdk.ContentBlockParamUnion
	switch message.Role {
	case provider.MessageRoleAssistant:
		role = sdk.MessageParamRoleAssistant
		if message.Content != "" {
			blocks = append(blocks, sdk.NewTextBlock(message.Content))
		}
		for _, call := range message.ToolCalls {
			var input any = map[string]any{}
			if strings.TrimSpace(call.Arguments) != "" {
				if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
					return nil, role, fmt.Errorf("tool call %s arguments: %w", call.Name, err)
				}
			}
			blocks = append(blocks, sdk.NewToolUseBlock(call.ID, input, call.Name))
		}
	case provider.MessageRoleTool:
		blocks = append(blocks, sdk.NewToolResultBlock(message.ToolCallID, message.Content, strings.HasPrefix(message.Content, "ERROR: ")))
	case provider.MessageRoleUser:
		if message.Content != "" {
			blocks = append(blocks, sdk.NewTextBlock(message.Content))
		}
		for _, image := range message.Images {
			blocks = append(blocks, sdk.NewImageBlockBase64(image.MIMEType, image.Data))
		}
	default:
		return nil, role, fmt.Errorf("unsupported message role %q", message.Role)
	}
	return blocks, role, nil
}

func appendMessage(messages *[]sdk.MessageParam, role sdk.MessageParamRole, blocks []sdk.ContentBlockParamUnion) {
	if len(blocks) == 0 {
		return
	}
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == role {
		(*messages)[len(*messages)-1].Content = append((*messages)[len(*messages)-1].Content, blocks...)
		return
	}
	if role == sdk.MessageParamRoleAssistant {
		*messages = append(*messages, sdk.NewAssistantMessage(blocks...))
	} else {
		*messages = append(*messages, sdk.NewUserMessage(blocks...))
	}
}

func mapMessage(message sdk.Message) provider.Response {
	inputTokens := message.Usage.InputTokens + message.Usage.CacheCreationInputTokens + message.Usage.CacheReadInputTokens
	result := provider.Response{
		ID: message.ID, Model: string(message.Model), FinishReason: string(message.StopReason),
		Usage: provider.Usage{InputTokens: inputTokens, OutputTokens: message.Usage.OutputTokens, TotalTokens: inputTokens + message.Usage.OutputTokens},
	}
	var text strings.Builder
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			if block.ID != "" && block.Name != "" {
				result.ToolCalls = append(result.ToolCalls, provider.ToolCall{ID: block.ID, Name: block.Name, Arguments: string(block.Input)})
			}
		}
	}
	result.Text = text.String()
	return result
}

func validateResponse(value provider.Response) error {
	if len(value.Text) > 16*1024*1024 || len(value.ToolCalls) > 64 {
		return errors.New("Anthropic response exceeds configured text or tool-call limits")
	}
	totalArguments := 0
	for _, call := range value.ToolCalls {
		if len(call.ID) > 1024 || len(call.Name) > 256 || len(call.Arguments) > 1024*1024 || len(call.Arguments) > 8*1024*1024-totalArguments {
			return errors.New("Anthropic response tool call exceeds configured size limits")
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
		if err := s.message.Accumulate(event); err != nil {
			s.err = redactedError("Anthropic Messages stream", err, s.secrets...)
			return false
		}
		if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			if len(event.Delta.Text) > 16*1024*1024-s.textBytes {
				s.err = errors.New("Anthropic Messages stream text exceeds the 16 MiB limit")
				return false
			}
			s.textBytes += len(event.Delta.Text)
			s.current = provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: event.Delta.Text}
			return true
		}
		if event.Type == "message_stop" {
			return s.complete()
		}
	}
	if err := s.stream.Err(); err != nil {
		s.err = redactedError("Anthropic Messages stream", err, s.secrets...)
		return false
	}
	s.err = errors.New("Anthropic Messages stream ended before message_stop")
	return false
}

func (s *responseStream) complete() bool {
	response := mapMessage(s.message)
	if err := validateResponse(response); err != nil {
		s.err = err
		return false
	}
	if response.Text == "" && len(response.ToolCalls) == 0 {
		s.err = errors.New("Anthropic Messages stream returned no text or tool calls")
		return false
	}
	s.emittedCompletion = true
	s.current = provider.StreamEvent{Type: provider.StreamEventCompleted, Response: &response}
	return true
}

func redactedError(prefix string, err error, secrets ...string) error {
	return provider.RedactedError(prefix, err, secrets...)
}

func (s *responseStream) Current() provider.StreamEvent { return s.current }
func (s *responseStream) Err() error                    { return s.err }
func (s *responseStream) Close() error                  { return s.stream.Close() }
