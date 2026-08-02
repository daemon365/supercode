// Package provider defines the model-provider boundary used by SuperCode.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

var (
	ErrEmptyModel  = errors.New("model is required")
	ErrEmptyPrompt = errors.New("prompt is required")
)

// MessageRole identifies a portable conversation message role.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
)

// Message is a provider-neutral conversation message.
type Message struct {
	Role       MessageRole `json:"role"`
	Content    string      `json:"content,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Images     []Image     `json:"images,omitempty"`
}

// Image is an inline image returned by a tool or supplied by a user. Data is
// base64 without a data-URL prefix so persistence remains provider neutral.
type Image struct {
	MIMEType string `json:"mime_type"`
	Data     string `json:"data,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// Ref is a session-store relative asset path. Provider adapters receive
	// hydrated Data; they never need to understand this persistence detail.
	Ref string `json:"ref,omitempty"`
}

// ToolDefinition describes a model-callable tool without exposing provider SDK
// types to the agent runtime.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a provider-neutral function call requested by a model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Request is the smallest provider-neutral request needed by the first
// SuperCode version. Provider-specific options must stay in provider adapters.
type Request struct {
	Model        string
	Instructions string
	Prompt       string
	// History contains completed user and assistant messages from previous
	// turns. The current Prompt is appended by the provider adapter.
	History           []Message
	Tools             []ToolDefinition
	Images            []Image
	ReasoningEffort   string
	ServiceTier       string
	ParallelToolCalls *bool
}

// Validate checks fields required by every provider.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return ErrEmptyModel
	}
	if strings.TrimSpace(r.Prompt) == "" && len(r.History) == 0 && len(r.Images) == 0 {
		return ErrEmptyPrompt
	}
	return nil
}

// Usage contains the portable token counters exposed by model providers.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// Response is the provider-neutral result returned to the application.
type Response struct {
	ID           string
	Model        string
	Text         string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// StreamEventType identifies a provider-neutral streaming event.
type StreamEventType string

const (
	StreamEventTextDelta StreamEventType = "text_delta"
	StreamEventCompleted StreamEventType = "completed"
)

// StreamEvent contains either a text delta or the final response.
type StreamEvent struct {
	Type     StreamEventType
	Delta    string
	Response *Response
}

// Stream is a pull-based provider-neutral response stream.
type Stream interface {
	Next() bool
	Current() StreamEvent
	Err() error
	Close() error
}

// Provider supports both collected and streaming generation. Future providers
// implement this interface without leaking their SDK types into the application.
type Provider interface {
	Generate(ctx context.Context, request Request) (Response, error)
	Stream(ctx context.Context, request Request) (Stream, error)
}
