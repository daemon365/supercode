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
		request.Model = strings.TrimSpace(model)
		if capabilities, ok := r.options.ModelCatalog.Resolve(request.Model); ok {
			request.ParallelToolCalls = capabilities.ParallelToolCalls
		}
		response, emitted, err := r.modelTurnAttempt(ctx, request, events)
		if err == nil {
			return response, nil
		}
		failures = append(failures, fmt.Errorf("model %s: %w", request.Model, err))
		if emitted || ctx.Err() != nil {
			break
		}
	}
	return provider.Response{}, errors.Join(failures...)
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
		r.options.OnEvent(event)
	}
	return emit(ctx, events, event)
}
