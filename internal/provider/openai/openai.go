// Package openai adapts the official OpenAI Go SDK to SuperCode's Provider
// interface. It uses Chat Completions because that protocol is implemented by
// OpenAI and most OpenAI-compatible services.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	"github.com/daemon365/supercode/internal/provider"
)

var ErrMissingAPIKey = errors.New("OPENAI_API_KEY is required")

// Config contains only connection settings owned by the OpenAI adapter.
type Config struct {
	APIKey     string
	BaseURL    string
	MaxRetries int
}

// Provider implements provider.Provider with the OpenAI Chat Completions API.
type Provider struct {
	client sdk.Client
	secret string
}

type responseStream struct {
	stream            *ssestream.Stream[sdk.ChatCompletionChunk]
	current           provider.StreamEvent
	response          provider.Response
	err               error
	emittedCompletion bool
	toolCallsByIndex  map[int64]*provider.ToolCall
	secret            string
}

var _ provider.Provider = (*Provider)(nil)

// New creates an OpenAI-compatible provider. The API key is intentionally
// supplied by the caller so secret loading stays outside the adapter.
func New(config Config) (*Provider, error) {
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	options := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL := strings.TrimSpace(config.BaseURL); baseURL != "" {
		options = append(options, option.WithBaseURL(baseURL))
	}
	if config.MaxRetries < 0 {
		return nil, errors.New("OpenAI max retries must not be negative")
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 2
	}
	options = append(options, option.WithMaxRetries(config.MaxRetries))
	return &Provider{client: sdk.NewClient(options...), secret: apiKey}, nil
}

// Generate maps a provider-neutral request to Chat Completions.
func (p *Provider) Generate(ctx context.Context, request provider.Request) (provider.Response, error) {
	if err := request.Validate(); err != nil {
		return provider.Response{}, err
	}
	completion, err := p.client.Chat.Completions.New(ctx, newChatParams(request))
	if err != nil {
		return provider.Response{}, redactedError("openai-compatible chat completions API", err, p.secret)
	}
	if len(completion.Choices) == 0 {
		return provider.Response{}, errors.New("openai-compatible chat completions API returned no choices")
	}
	choice := completion.Choices[0]
	toolCalls := mapToolCalls(choice.Message.ToolCalls)
	if choice.Message.Content == "" && len(toolCalls) == 0 {
		return provider.Response{}, errors.New("openai-compatible chat completions API returned no text")
	}

	return provider.Response{
		ID:           completion.ID,
		Model:        completion.Model,
		Text:         choice.Message.Content,
		ToolCalls:    toolCalls,
		FinishReason: choice.FinishReason,
		Usage:        mapUsage(completion.Usage),
	}, nil
}

// Stream starts a streaming Chat Completions request.
func (p *Provider) Stream(ctx context.Context, request provider.Request) (provider.Stream, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return &responseStream{
		stream:           p.client.Chat.Completions.NewStreaming(ctx, newChatParams(request)),
		toolCallsByIndex: make(map[int64]*provider.ToolCall),
		secret:           p.secret,
	}, nil
}

func newChatParams(request provider.Request) sdk.ChatCompletionNewParams {
	messages := make([]sdk.ChatCompletionMessageParamUnion, 0, len(request.History)+2)
	if instructions := strings.TrimSpace(request.Instructions); instructions != "" {
		// System messages have the widest support across compatible providers.
		messages = append(messages, sdk.SystemMessage(instructions))
	}
	for _, historyMessage := range request.History {
		switch historyMessage.Role {
		case provider.MessageRoleUser:
			if len(historyMessage.Images) == 0 {
				messages = append(messages, sdk.UserMessage(historyMessage.Content))
				continue
			}
			parts := make([]sdk.ChatCompletionContentPartUnionParam, 0, len(historyMessage.Images)+1)
			if historyMessage.Content != "" {
				parts = append(parts, sdk.TextContentPart(historyMessage.Content))
			}
			for _, image := range historyMessage.Images {
				detail := image.Detail
				if detail == "original" {
					detail = "high"
				}
				if detail == "" {
					detail = "high"
				}
				parts = append(parts, sdk.ImageContentPart(sdk.ChatCompletionContentPartImageImageURLParam{
					URL: "data:" + image.MIMEType + ";base64," + image.Data, Detail: detail,
				}))
			}
			messages = append(messages, sdk.UserMessage(parts))
		case provider.MessageRoleAssistant:
			if len(historyMessage.ToolCalls) == 0 {
				messages = append(messages, sdk.AssistantMessage(historyMessage.Content))
				continue
			}
			assistant := sdk.ChatCompletionAssistantMessageParam{}
			if historyMessage.Content != "" {
				assistant.Content.OfString = sdk.String(historyMessage.Content)
			}
			for _, call := range historyMessage.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, sdk.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &sdk.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: sdk.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: call.Name, Arguments: call.Arguments,
						},
					},
				})
			}
			messages = append(messages, sdk.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		case provider.MessageRoleTool:
			messages = append(messages, sdk.ToolMessage(historyMessage.Content, historyMessage.ToolCallID))
		}
	}
	if request.Prompt != "" || len(request.Images) > 0 {
		if len(request.Images) == 0 {
			messages = append(messages, sdk.UserMessage(request.Prompt))
		} else {
			parts := make([]sdk.ChatCompletionContentPartUnionParam, 0, len(request.Images)+1)
			if request.Prompt != "" {
				parts = append(parts, sdk.TextContentPart(request.Prompt))
			}
			for _, image := range request.Images {
				detail := defaultImageDetail(image.Detail)
				parts = append(parts, sdk.ImageContentPart(sdk.ChatCompletionContentPartImageImageURLParam{URL: "data:" + image.MIMEType + ";base64," + image.Data, Detail: detail}))
			}
			messages = append(messages, sdk.UserMessage(parts))
		}
	}

	tools := make([]sdk.ChatCompletionToolUnionParam, 0, len(request.Tools))
	for _, definition := range request.Tools {
		var parameters shared.FunctionParameters
		if len(definition.Parameters) > 0 {
			_ = json.Unmarshal(definition.Parameters, &parameters)
		}
		tools = append(tools, sdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name: definition.Name, Description: sdk.String(definition.Description), Parameters: parameters,
		}))
	}

	params := sdk.ChatCompletionNewParams{
		Model:    sdk.ChatModel(request.Model),
		Messages: messages,
		Tools:    tools,
	}
	if request.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(request.ReasoningEffort)
	}
	if request.ServiceTier != "" {
		params.ServiceTier = sdk.ChatCompletionNewParamsServiceTier(request.ServiceTier)
	}
	if request.ParallelToolCalls != nil {
		params.ParallelToolCalls = sdk.Bool(*request.ParallelToolCalls)
	}
	return params
}

func defaultImageDetail(detail string) string {
	if detail == "" || detail == "original" {
		return "high"
	}
	return detail
}

func mapToolCalls(calls []sdk.ChatCompletionMessageToolCallUnion) []provider.ToolCall {
	result := make([]provider.ToolCall, 0, len(calls))
	for _, call := range calls {
		function := call.AsFunction()
		if function.ID == "" || function.Function.Name == "" {
			continue
		}
		result = append(result, provider.ToolCall{
			ID: function.ID, Name: function.Function.Name, Arguments: function.Function.Arguments,
		})
	}
	return result
}

func mapUsage(usage sdk.CompletionUsage) provider.Usage {
	return provider.Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}
}

func (s *responseStream) Next() bool {
	if s.err != nil || s.emittedCompletion {
		return false
	}

	for s.stream.Next() {
		chunk := s.stream.Current()
		if s.response.ID == "" {
			s.response.ID = chunk.ID
			s.response.Model = chunk.Model
		}
		if chunk.Usage.TotalTokens > 0 {
			s.response.Usage = mapUsage(chunk.Usage)
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				s.response.FinishReason = choice.FinishReason
			}
			for _, deltaCall := range choice.Delta.ToolCalls {
				call := s.toolCallsByIndex[deltaCall.Index]
				if call == nil {
					call = &provider.ToolCall{}
					s.toolCallsByIndex[deltaCall.Index] = call
				}
				if deltaCall.ID != "" {
					call.ID = deltaCall.ID
				}
				if deltaCall.Function.Name != "" {
					call.Name += deltaCall.Function.Name
				}
				call.Arguments += deltaCall.Function.Arguments
			}
			if choice.Delta.Content == "" {
				continue
			}
			s.response.Text += choice.Delta.Content
			s.current = provider.StreamEvent{
				Type:  provider.StreamEventTextDelta,
				Delta: choice.Delta.Content,
			}
			return true
		}
	}

	if err := s.stream.Err(); err != nil {
		s.err = redactedError("openai-compatible chat completions stream", err, s.secret)
		return false
	}
	for index := int64(0); index < int64(len(s.toolCallsByIndex)); index++ {
		if call := s.toolCallsByIndex[index]; call != nil {
			s.response.ToolCalls = append(s.response.ToolCalls, *call)
		}
	}
	if s.response.Text == "" && len(s.response.ToolCalls) == 0 {
		s.err = errors.New("openai-compatible chat completions stream returned no text")
		return false
	}

	s.emittedCompletion = true
	response := s.response
	s.current = provider.StreamEvent{
		Type:     provider.StreamEventCompleted,
		Response: &response,
	}
	return true
}

func redactedError(prefix string, err error, secrets ...string) error {
	message := err.Error()
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return fmt.Errorf("%s: %s", prefix, message)
}

func (s *responseStream) Current() provider.StreamEvent {
	return s.current
}

func (s *responseStream) Err() error {
	return s.err
}

func (s *responseStream) Close() error {
	return s.stream.Close()
}
