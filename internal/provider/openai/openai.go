// Package openai adapts the official OpenAI Go SDK to SuperCode's Provider
// interface. It uses Chat Completions because that protocol is implemented by
// OpenAI and most OpenAI-compatible services.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	Headers    map[string]string
}

// Provider implements provider.Provider with the OpenAI Chat Completions API.
type Provider struct {
	client  sdk.Client
	secrets []string
}

const (
	maxResponseTextBytes  = 16 * 1024 * 1024
	maxResponseToolCalls  = 64
	maxToolCallNameBytes  = 256
	maxToolCallIDBytes    = 1024
	maxToolCallArgsBytes  = 1024 * 1024
	maxTotalToolArgsBytes = 8 * 1024 * 1024
)

type responseStream struct {
	stream            *ssestream.Stream[sdk.ChatCompletionChunk]
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

// New creates an OpenAI-compatible provider. The API key is intentionally
// supplied by the caller so secret loading stays outside the adapter.
func New(config Config) (*Provider, error) {
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	httpClient := provider.SecureHTTPClient(nil)
	options := []option.RequestOption{option.WithAPIKey(apiKey), option.WithHTTPClient(httpClient)}
	secrets := []string{apiKey}
	if baseURL := strings.TrimSpace(config.BaseURL); baseURL != "" {
		options = append(options, option.WithBaseURL(baseURL))
	}
	if config.MaxRetries < 0 {
		return nil, errors.New("OpenAI max retries must not be negative")
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 2
	}
	for name, value := range config.Headers {
		if strings.TrimSpace(name) != "" {
			options = append(options, option.WithHeader(name, value))
			if strings.TrimSpace(value) != "" {
				secrets = append(secrets, value)
			}
		}
	}
	options = append(options, option.WithMaxRetries(config.MaxRetries))
	return &Provider{client: sdk.NewClient(options...), secrets: secrets}, nil
}

// Generate maps a provider-neutral request to Chat Completions.
func (p *Provider) Generate(ctx context.Context, request provider.Request) (provider.Response, error) {
	if err := request.Validate(); err != nil {
		return provider.Response{}, err
	}
	completion, err := p.client.Chat.Completions.New(ctx, newChatParams(request))
	if err != nil {
		return provider.Response{}, redactedError("openai-compatible chat completions API", err, p.secrets...)
	}
	if completion == nil {
		return provider.Response{}, errors.New("openai-compatible chat completions API returned a null response")
	}
	if len(completion.Choices) == 0 {
		return provider.Response{}, errors.New("openai-compatible chat completions API returned no choices")
	}
	choice := completion.Choices[0]
	if len(choice.Message.Content) > maxResponseTextBytes {
		return provider.Response{}, fmt.Errorf("openai-compatible response text exceeds the %d MiB limit", maxResponseTextBytes/(1024*1024))
	}
	toolCalls, err := mapToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return provider.Response{}, err
	}
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
		toolCallsByIndex: make(map[int64]*streamToolCall),
		secrets:          append([]string(nil), p.secrets...),
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

func mapToolCalls(calls []sdk.ChatCompletionMessageToolCallUnion) ([]provider.ToolCall, error) {
	if len(calls) > maxResponseToolCalls {
		return nil, fmt.Errorf("openai-compatible response exceeds the %d tool-call limit", maxResponseToolCalls)
	}
	result := make([]provider.ToolCall, 0, len(calls))
	totalArguments := 0
	for _, call := range calls {
		function := call.AsFunction()
		if function.ID == "" || function.Function.Name == "" {
			continue
		}
		if len(function.ID) > maxToolCallIDBytes || len(function.Function.Name) > maxToolCallNameBytes || len(function.Function.Arguments) > maxToolCallArgsBytes || len(function.Function.Arguments) > maxTotalToolArgsBytes-totalArguments {
			return nil, errors.New("openai-compatible response tool call exceeds configured size limits")
		}
		totalArguments += len(function.Function.Arguments)
		result = append(result, provider.ToolCall{
			ID: function.ID, Name: function.Function.Name, Arguments: function.Function.Arguments,
		})
	}
	return result, nil
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
			if choice.Index != 0 {
				continue
			}
			if choice.FinishReason != "" {
				s.response.FinishReason = choice.FinishReason
				s.terminal = true
			}
			for _, deltaCall := range choice.Delta.ToolCalls {
				if deltaCall.Index < 0 || deltaCall.Index >= maxResponseToolCalls {
					s.err = errors.New("openai-compatible stream returned an invalid tool-call index")
					return false
				}
				call := s.toolCallsByIndex[deltaCall.Index]
				if call == nil {
					if len(s.toolCallsByIndex) >= maxResponseToolCalls {
						s.err = fmt.Errorf("openai-compatible stream exceeds the %d tool-call limit", maxResponseToolCalls)
						return false
					}
					call = &streamToolCall{}
					s.toolCallsByIndex[deltaCall.Index] = call
				}
				if deltaCall.ID != "" {
					if len(deltaCall.ID) > maxToolCallIDBytes {
						s.err = errors.New("openai-compatible stream tool-call ID is too large")
						return false
					}
					call.id = deltaCall.ID
				}
				if deltaCall.Function.Name != "" {
					if call.name.Len()+len(deltaCall.Function.Name) > maxToolCallNameBytes {
						s.err = errors.New("openai-compatible stream tool-call name is too large")
						return false
					}
					call.name.WriteString(deltaCall.Function.Name)
				}
				if len(deltaCall.Function.Arguments) > maxToolCallArgsBytes-call.arguments.Len() || len(deltaCall.Function.Arguments) > maxTotalToolArgsBytes-s.totalToolArgs {
					s.err = errors.New("openai-compatible stream tool-call arguments are too large")
					return false
				}
				call.arguments.WriteString(deltaCall.Function.Arguments)
				s.totalToolArgs += len(deltaCall.Function.Arguments)
			}
			if choice.Delta.Content == "" {
				continue
			}
			if len(choice.Delta.Content) > maxResponseTextBytes-s.text.Len() {
				s.err = fmt.Errorf("openai-compatible stream text exceeds the %d MiB limit", maxResponseTextBytes/(1024*1024))
				return false
			}
			s.text.WriteString(choice.Delta.Content)
			s.current = provider.StreamEvent{
				Type:  provider.StreamEventTextDelta,
				Delta: choice.Delta.Content,
			}
			return true
		}
	}

	if err := s.stream.Err(); err != nil {
		s.err = redactedError("openai-compatible chat completions stream", err, s.secrets...)
		return false
	}
	if !s.terminal {
		s.err = errors.New("openai-compatible chat completions stream ended before a finish reason")
		return false
	}
	indices := make([]int, 0, len(s.toolCallsByIndex))
	for index := range s.toolCallsByIndex {
		indices = append(indices, int(index))
	}
	sort.Ints(indices)
	for expected, index := range indices {
		if index != expected {
			s.err = errors.New("openai-compatible stream returned non-contiguous tool-call indexes")
			return false
		}
		call := s.toolCallsByIndex[int64(index)]
		if call == nil || call.id == "" || call.name.Len() == 0 {
			s.err = errors.New("openai-compatible stream returned an incomplete tool call")
			return false
		}
		s.response.ToolCalls = append(s.response.ToolCalls, provider.ToolCall{ID: call.id, Name: call.name.String(), Arguments: call.arguments.String()})
	}
	s.response.Text = s.text.String()
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
	return provider.RedactedError(prefix, err, secrets...)
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
