// Package userinput provides a blocking model tool backed by an interactive UI.
package userinput

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type Question struct {
	Header   string   `json:"header"`
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Options  []Option `json:"options"`
}

type Request struct {
	Questions []Question
	once      sync.Once
	answer    chan map[string]string
}

func (r *Request) Decide(answers map[string]string) {
	r.once.Do(func() { r.answer <- answers })
}

type Manager struct{ requests chan *Request }

func New() *Manager                          { return &Manager{requests: make(chan *Request)} }
func (m *Manager) Requests() <-chan *Request { return m.requests }
func (m *Manager) Tool() tool.Tool           { return &requestTool{manager: m} }

type requestTool struct{ manager *Manager }

func (*requestTool) Definition() provider.ToolDefinition {
	return provider.ToolDefinition{Name: "request_user_input", Description: "Ask the user one to three short structured questions and wait for their selections. Each question should offer two or three mutually exclusive choices; the client also permits a custom answer.", Parameters: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"object","properties":{"header":{"type":"string","maxLength":12},"id":{"type":"string","pattern":"^[a-z][a-z0-9_]*$"},"question":{"type":"string"},"options":{"type":"array","minItems":2,"maxItems":3,"items":{"type":"object","properties":{"label":{"type":"string"},"description":{"type":"string"}},"required":["label","description"],"additionalProperties":false}}},"required":["header","id","question","options"],"additionalProperties":false}}},"required":["questions"],"additionalProperties":false}`)}
}
func (*requestTool) Risk(string) tool.Risk { return tool.RiskRead }
func (*requestTool) Summary(string) string { return "ask user a structured question" }
func (t *requestTool) Execute(ctx context.Context, arguments string) (tool.Result, error) {
	var input struct {
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return tool.Result{}, err
	}
	if len(input.Questions) < 1 || len(input.Questions) > 3 {
		return tool.Result{}, errors.New("questions must contain one to three items")
	}
	seen := make(map[string]bool)
	for _, question := range input.Questions {
		if strings.TrimSpace(question.ID) == "" || seen[question.ID] || strings.TrimSpace(question.Question) == "" {
			return tool.Result{}, errors.New("each question requires a unique id and text")
		}
		if len(question.Options) < 2 || len(question.Options) > 3 {
			return tool.Result{}, errors.New("each question requires two or three options")
		}
		seen[question.ID] = true
	}
	request := &Request{Questions: input.Questions, answer: make(chan map[string]string, 1)}
	select {
	case t.manager.requests <- request:
	case <-ctx.Done():
		return tool.Result{}, ctx.Err()
	}
	select {
	case answers := <-request.answer:
		encoded, _ := json.Marshal(map[string]any{"answers": answers})
		return tool.Result{Content: string(encoded)}, nil
	case <-ctx.Done():
		return tool.Result{}, ctx.Err()
	}
}
