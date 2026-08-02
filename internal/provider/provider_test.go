package provider

import (
	"errors"
	"testing"
)

func TestRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		request Request
		wantErr error
	}{
		{
			name:    "valid",
			request: Request{Model: "gpt-test", Prompt: "hello"},
		},
		{
			name:    "missing model",
			request: Request{Prompt: "hello"},
			wantErr: ErrEmptyModel,
		},
		{
			name:    "missing prompt",
			request: Request{Model: "gpt-test", Prompt: "  "},
			wantErr: ErrEmptyPrompt,
		},
		{
			name: "history without a new prompt",
			request: Request{
				Model:   "gpt-test",
				History: []Message{{Role: MessageRoleTool, Content: "done", ToolCallID: "call_1"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.request.Validate()
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}
