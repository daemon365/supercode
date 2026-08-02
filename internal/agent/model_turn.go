package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daemon365/supercode/internal/memory"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

func (r *Runner) modelTurn(ctx context.Context, request provider.Request, events chan<- Event) (provider.Response, error) {
	models := append([]string{request.Model}, r.options.FallbackModels...)
	var failures []error
	for _, model := range models {
		if strings.TrimSpace(model) == "" {
			continue
		}
		candidate, err := r.requestForModel(request, strings.TrimSpace(model))
		if err != nil {
			failures = append(failures, fmt.Errorf("model %s: %w", strings.TrimSpace(model), err))
			continue
		}
		response, emitted, err := r.modelTurnAttempt(ctx, candidate, events)
		if err == nil {
			return response, nil
		}
		failures = append(failures, fmt.Errorf("model %s: %w", candidate.Model, err))
		if emitted || ctx.Err() != nil {
			break
		}
	}
	return provider.Response{}, errors.Join(failures...)
}

func (r *Runner) requestForModel(base provider.Request, model string) (provider.Request, error) {
	request := base
	request.Model = model
	if r.options.ModelCatalog == nil {
		return request, nil
	}
	capabilities, known := r.options.ModelCatalog.Resolve(model)
	if !known {
		request.ParallelToolCalls = nil
		return request, nil
	}
	if err := r.options.ModelCatalog.Validate(model, request.ReasoningEffort, request.ServiceTier); err != nil {
		return provider.Request{}, err
	}
	if requestHasImages(request) && len(capabilities.InputModalities) > 0 && !capabilities.Supports("image") {
		return provider.Request{}, errors.New("model does not advertise image input support")
	}
	if capabilities.ToolCalling != nil && !*capabilities.ToolCalling {
		if historyUsesTools(request.History) {
			return provider.Request{}, errors.New("model does not support the tool-call history already in this turn")
		}
		request.Tools = nil
		request.ParallelToolCalls = nil
	} else {
		request.ParallelToolCalls = capabilities.ParallelToolCalls
	}
	_, _, usable := r.options.ModelCatalog.Limits(model, r.options.ContextWindowTokens)
	requestTokens := EstimateMessagesTokens(request.History) + EstimateTextTokens(request.Prompt) + EstimateTextTokens(request.Instructions)
	for _, definition := range request.Tools {
		requestTokens += EstimateTextTokens(definition.Name) + EstimateTextTokens(definition.Description) + EstimateTextTokens(string(definition.Parameters))
	}
	if usable > 0 && requestTokens >= usable {
		return provider.Request{}, fmt.Errorf("estimated request context is %d tokens, exceeding this fallback model's %d-token usable limit", requestTokens, usable)
	}
	return request, nil
}

func requestHasImages(request provider.Request) bool {
	if len(request.Images) > 0 {
		return true
	}
	for _, message := range request.History {
		if len(message.Images) > 0 {
			return true
		}
	}
	return false
}

func historyUsesTools(history []provider.Message) bool {
	for _, message := range history {
		if message.Role == provider.MessageRoleTool || len(message.ToolCalls) > 0 || message.ToolCallID != "" {
			return true
		}
	}
	return false
}

func (r *Runner) modelTurnAttempt(ctx context.Context, request provider.Request, events chan<- Event) (provider.Response, bool, error) {
	requestContext, cancel := context.WithTimeout(ctx, r.options.RequestTimeout)
	defer cancel()
	if !r.options.Stream {
		response, err := r.provider.Generate(requestContext, request)
		if err != nil {
			return provider.Response{}, false, requestError(err, r.options.RequestTimeout)
		}
		visibleText, citationIDs := memory.StripCitations(response.Text)
		response.Text = visibleText
		if len(citationIDs) > 0 && r.options.OnMemoryCitation != nil {
			r.options.OnMemoryCitation(citationIDs)
		}
		if response.Text != "" && !r.emit(ctx, events, Event{Type: EventTextDelta, Delta: response.Text}) {
			return provider.Response{}, true, ctx.Err()
		}
		return response, response.Text != "", nil
	}
	stream, err := r.provider.Stream(requestContext, request)
	if err != nil {
		return provider.Response{}, false, requestError(err, r.options.RequestTimeout)
	}
	defer stream.Close()
	var response *provider.Response
	var citationParser memory.CitationParser
	emitted := false
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case provider.StreamEventTextDelta:
			if event.Delta != "" {
				emitted = true
			}
			visible := citationParser.Push(event.Delta)
			if visible != "" && !r.emit(ctx, events, Event{Type: EventTextDelta, Delta: visible}) {
				return provider.Response{}, emitted, ctx.Err()
			}
		case provider.StreamEventCompleted:
			response = event.Response
		}
	}
	if err := stream.Err(); err != nil {
		return provider.Response{}, emitted, requestError(err, r.options.RequestTimeout)
	}
	if response == nil {
		return provider.Response{}, emitted, errors.New("provider stream ended without a completed response")
	}
	tail, citationIDs := citationParser.Finish()
	if tail != "" && !r.emit(ctx, events, Event{Type: EventTextDelta, Delta: tail}) {
		return provider.Response{}, emitted, ctx.Err()
	}
	visibleText, finalIDs := memory.StripCitations(response.Text)
	response.Text = visibleText
	citationIDs = appendUniqueStrings(citationIDs, finalIDs...)
	if len(citationIDs) > 0 && r.options.OnMemoryCitation != nil {
		r.options.OnMemoryCitation(citationIDs)
	}
	return *response, emitted, nil
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

func requestError(err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("model request exceeded %s; increase timeout in ~/.supercode/config.yaml or with -timeout: %w", timeout, err)
	}
	return err
}

func toolMessage(callID string, result tool.Result) provider.Message {
	prefix := ""
	if result.IsError {
		prefix = "ERROR: "
	}
	return provider.Message{Role: provider.MessageRoleTool, ToolCallID: callID, Content: prefix + result.Content}
}

func emit(ctx context.Context, events chan<- Event, event Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) emit(ctx context.Context, events chan<- Event, event Event) bool {
	if r.options.OnEvent != nil {
		r.eventsMu.Lock()
		r.options.OnEvent(event)
		r.eventsMu.Unlock()
	}
	return emit(ctx, events, event)
}
