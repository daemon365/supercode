// Package agent orchestrates model turns, tool approvals, and tool results.
package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daemon365/supercode/internal/modelcatalog"
	"github.com/daemon365/supercode/internal/permission"
	"github.com/daemon365/supercode/internal/policy"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

const (
	maxReturnedToolImages     = 8
	maxReturnedToolImageBytes = int64(64 * 1024 * 1024)
)

type ApprovalMode string

const (
	ApprovalOnRequest ApprovalMode = "on-request"
	ApprovalAlways    ApprovalMode = "always"
	ApprovalNever     ApprovalMode = "never"
	ApprovalGranular  ApprovalMode = "granular"
)

// ApprovalDecision records both whether a call is allowed and how long that
// decision should remain effective.
type ApprovalDecision string

const (
	ApprovalDeny                  ApprovalDecision = "deny"
	ApprovalAllowOnce             ApprovalDecision = "allow_once"
	ApprovalAllowSession          ApprovalDecision = "allow_session"
	ApprovalAllowPrefix           ApprovalDecision = "allow_prefix"
	ApprovalAllowPersistentPrefix ApprovalDecision = "allow_persistent_prefix"
)

func ParseApprovalMode(value string) (ApprovalMode, error) {
	mode := ApprovalMode(strings.TrimSpace(value))
	switch mode {
	case ApprovalOnRequest, ApprovalAlways, ApprovalNever, ApprovalGranular:
		return mode, nil
	default:
		return "", fmt.Errorf("approval must be one of on-request, always, never, or granular")
	}
}

type Options struct {
	Model               string
	Instructions        string
	Stream              bool
	MaxTurns            int
	Approval            ApprovalMode
	ContextWindowTokens int
	AutoCompactTokens   int
	UsableContextTokens int
	ToolOutputTokens    int
	OnUsage             func(provider.Usage)
	Hook                Hook
	FallbackModels      []string
	RequestTimeout      time.Duration
	Policy              *policy.Store
	ReasoningEffort     string
	ServiceTier         string
	OnEvent             func(Event)
	OnMemoryCitation    func([]string)
	Permissions         *permission.Manager
	ApprovalCategories  map[tool.Category]bool
	ModelCatalog        *modelcatalog.Catalog
}

type HookEvent string

const (
	HookUserPromptSubmit HookEvent = "user_prompt_submit"
	HookPreToolUse       HookEvent = "pre_tool_use"
	HookPostToolUse      HookEvent = "post_tool_use"
	HookPermission       HookEvent = "permission_request"
	HookPreCompact       HookEvent = "pre_compact"
	HookPostCompact      HookEvent = "post_compact"
	HookSubagentStart    HookEvent = "subagent_start"
	HookSubagentStop     HookEvent = "subagent_stop"
)

type HookInput struct {
	Prompt       string             `json:"prompt,omitempty"`
	Call         *provider.ToolCall `json:"call,omitempty"`
	Risk         tool.Risk          `json:"risk,omitempty"`
	Category     tool.Category      `json:"category,omitempty"`
	Result       *tool.Result       `json:"result,omitempty"`
	BeforeTokens int                `json:"before_tokens,omitempty"`
	AfterTokens  int                `json:"after_tokens,omitempty"`
}

type HookOutput struct {
	Allow             *bool  `json:"allow,omitempty"`
	Message           string `json:"message,omitempty"`
	Prompt            string `json:"prompt,omitempty"`
	Arguments         string `json:"arguments,omitempty"`
	AdditionalContext string `json:"additional_context,omitempty"`
}

type Hook func(context.Context, HookEvent, HookInput) (HookOutput, error)

type Input struct {
	Prompt       string
	History      []provider.Message
	Instructions string
	Images       []provider.Image
}

type EventType string

const (
	EventTextDelta        EventType = "text_delta"
	EventToolStarted      EventType = "tool_started"
	EventToolOutputDelta  EventType = "tool_output_delta"
	EventApprovalRequired EventType = "approval_required"
	EventToolFinished     EventType = "tool_finished"
	EventCompleted        EventType = "completed"
	EventQueuedCommitted  EventType = "queued_committed"
	EventContextCompacted EventType = "context_compacted"
	EventError            EventType = "error"
)

type Event struct {
	Type         EventType
	Delta        string
	Call         *provider.ToolCall
	Risk         tool.Risk
	Category     tool.Category
	Summary      string
	Result       *tool.Result
	Approval     *ApprovalRequest
	Response     *provider.Response
	History      []provider.Message
	Err          error
	Queued       []string
	BeforeTokens int
	AfterTokens  int
	SessionID    int64
}

// ApprovalRequest is resolved exactly once by the UI or non-interactive host.
type ApprovalRequest struct {
	Call        provider.ToolCall
	Risk        tool.Risk
	Category    tool.Category
	Summary     string
	Permissions *permission.Request
	Prefix      string
	PolicyPath  string
	once        sync.Once
	answer      chan ApprovalDecision
}

func newApprovalRequest(call provider.ToolCall, risk tool.Risk, category tool.Category, summary string, permissions *permission.Request) *ApprovalRequest {
	return &ApprovalRequest{Call: call, Risk: risk, Category: category, Summary: summary, Permissions: permissions, Prefix: commandPrefix(call), answer: make(chan ApprovalDecision, 1)}
}

// Decide preserves the original boolean API for non-interactive hosts.
func (r *ApprovalRequest) Decide(approved bool) {
	decision := ApprovalDeny
	if approved {
		decision = ApprovalAllowOnce
	}
	r.DecideWithScope(decision)
}

func (r *ApprovalRequest) DecideWithScope(decision ApprovalDecision) {
	switch decision {
	case ApprovalAllowOnce, ApprovalAllowSession, ApprovalAllowPrefix, ApprovalAllowPersistentPrefix, ApprovalDeny:
	default:
		decision = ApprovalDeny
	}
	if (decision == ApprovalAllowPrefix || decision == ApprovalAllowPersistentPrefix) && r.Prefix == "" {
		decision = ApprovalAllowOnce
	}
	r.once.Do(func() { r.answer <- decision })
}

type Runner struct {
	provider               provider.Provider
	tools                  *tool.Registry
	options                Options
	eventsMu               sync.Mutex
	rulesMu                sync.RWMutex
	rules                  map[string]struct{}
	toolSlots              chan struct{}
	managesPermissionTurns bool
	callIDPrefix           string
	callIDSequence         atomic.Uint64
}

var (
	runnerIDSequence    atomic.Uint64
	globalParallelSlots = make(chan struct{}, parallelToolLimit)
)

// RunHandle permits user messages to steer an active turn at the next tool
// boundary while events continue streaming independently.
type RunHandle struct {
	Events <-chan Event
	steer  chan string
}

func (h *RunHandle) Queue(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		return false
	}
	select {
	case h.steer <- message:
		return true
	default:
		return false
	}
}

func New(modelProvider provider.Provider, registry *tool.Registry, options Options) (*Runner, error) {
	if modelProvider == nil {
		return nil, errors.New("agent provider is required")
	}
	if registry == nil {
		return nil, errors.New("tool registry is required")
	}
	if strings.TrimSpace(options.Model) == "" {
		return nil, provider.ErrEmptyModel
	}
	if options.MaxTurns < 0 {
		return nil, errors.New("max turns must not be negative")
	}
	if options.ModelCatalog != nil {
		if err := options.ModelCatalog.Validate(options.Model, options.ReasoningEffort, options.ServiceTier); err != nil {
			return nil, err
		}
		if options.ContextWindowTokens <= 0 {
			options.ContextWindowTokens, options.AutoCompactTokens, options.UsableContextTokens = options.ModelCatalog.Limits(options.Model, DefaultContextWindowTokens)
		}
	}
	if options.ContextWindowTokens <= 0 {
		options.ContextWindowTokens = DefaultContextWindowTokens
	}
	if options.UsableContextTokens <= 0 {
		options.UsableContextTokens = options.ContextWindowTokens * 95 / 100
	}
	if options.UsableContextTokens > options.ContextWindowTokens {
		options.UsableContextTokens = options.ContextWindowTokens
	}
	if options.AutoCompactTokens <= 0 {
		options.AutoCompactTokens = options.ContextWindowTokens * 90 / 100
	}
	if options.AutoCompactTokens >= options.UsableContextTokens {
		options.AutoCompactTokens = options.UsableContextTokens * 90 / 95
	}
	if options.ToolOutputTokens <= 0 {
		options.ToolOutputTokens = DefaultToolOutputTokens
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 10 * time.Minute
	}
	if options.Approval == "" {
		options.Approval = ApprovalOnRequest
	}
	if _, err := ParseApprovalMode(string(options.Approval)); err != nil {
		return nil, err
	}
	runnerID := runnerIDSequence.Add(1)
	return &Runner{
		provider: modelProvider, tools: registry, options: options, rules: make(map[string]struct{}),
		toolSlots: globalParallelSlots, managesPermissionTurns: true,
		callIDPrefix: fmt.Sprintf("%x_%x", time.Now().UnixNano(), runnerID),
	}, nil
}

func (r *Runner) ToolNames() []string { return r.tools.Names() }

// SetModel and SetApproval are used by interactive slash commands between
// turns. The TUI never mutates these settings while a run is active.
func (r *Runner) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return provider.ErrEmptyModel
	}
	if resolver, ok := r.provider.(provider.ModelResolver); ok {
		resolved, err := resolver.ResolveModel(model)
		if err != nil {
			return err
		}
		model = resolved.Selector
	}
	if r.options.ModelCatalog != nil {
		if err := r.options.ModelCatalog.Validate(model, r.options.ReasoningEffort, r.options.ServiceTier); err != nil {
			return err
		}
		r.options.ContextWindowTokens, r.options.AutoCompactTokens, r.options.UsableContextTokens = r.options.ModelCatalog.Limits(model, r.options.ContextWindowTokens)
	}
	r.options.Model = model
	return nil
}

func (r *Runner) Model() string { return r.options.Model }

func (r *Runner) ContextLimits() (int, int, int) {
	return r.options.ContextWindowTokens, r.options.AutoCompactTokens, r.options.UsableContextTokens
}

func (r *Runner) ModelCapabilities(model string) (modelcatalog.Capabilities, bool) {
	if r.options.ModelCatalog == nil {
		return modelcatalog.Capabilities{}, false
	}
	return r.options.ModelCatalog.Resolve(model)
}

func (r *Runner) SetApproval(mode ApprovalMode) error {
	if _, err := ParseApprovalMode(string(mode)); err != nil {
		return err
	}
	r.options.Approval = mode
	return nil
}

func (r *Runner) SetReasoningEffort(value string) {
	r.options.ReasoningEffort = strings.TrimSpace(value)
}

func (r *Runner) SetServiceTier(value string) {
	r.options.ServiceTier = strings.TrimSpace(value)
}

// Fork creates an isolated runner with the same provider, tools, context
// limits, and hooks. It lets sub-agents select another compatible model without
// mutating the parent runner.
func (r *Runner) Fork(model string, approval ApprovalMode) (*Runner, error) {
	return r.ForkConfigured(model, approval, "")
}

// ForkConfigured creates a child runner while applying model-specific
// reasoning configuration. Child runs share the parent's global read-tool
// budget, but do not expire grants owned by the parent user turn.
func (r *Runner) ForkConfigured(model string, approval ApprovalMode, reasoningEffort string) (*Runner, error) {
	options := r.options
	if strings.TrimSpace(model) != "" {
		options.Model = strings.TrimSpace(model)
	}
	if strings.TrimSpace(reasoningEffort) != "" {
		options.ReasoningEffort = strings.TrimSpace(reasoningEffort)
	}
	options.Approval = approval
	options.OnUsage = nil
	child, err := New(r.provider, r.tools, options)
	if err != nil {
		return nil, err
	}
	child.toolSlots = r.toolSlots
	child.managesPermissionTurns = false
	return child, nil
}

func (r *Runner) NotifyHook(ctx context.Context, event HookEvent, input HookInput) error {
	return r.runInformationalHook(ctx, event, input)
}

// Run returns an event stream and performs the agent loop in a cancellable
// goroutine. The caller must keep consuming events until the channel closes.
func (r *Runner) Run(ctx context.Context, input Input) <-chan Event {
	return r.Start(ctx, input).Events
}

func (r *Runner) Start(ctx context.Context, input Input) *RunHandle {
	events := make(chan Event, 16)
	steer := make(chan string, 32)
	go func() {
		defer close(events)
		if err := r.run(ctx, input, events, steer); err != nil {
			r.emit(ctx, events, Event{Type: EventError, Err: err})
		}
	}()
	return &RunHandle{Events: events, steer: steer}
}

func (r *Runner) run(ctx context.Context, input Input, events chan<- Event, steer <-chan string) error {
	if r.options.Permissions != nil && r.managesPermissionTurns {
		r.options.Permissions.BeginTurn()
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return provider.ErrEmptyPrompt
	}
	if r.options.Hook != nil {
		output, err := r.options.Hook(ctx, HookUserPromptSubmit, HookInput{Prompt: prompt})
		if err != nil {
			return err
		}
		if output.Allow != nil && !*output.Allow {
			return fmt.Errorf("prompt blocked by hook: %s", strings.TrimSpace(output.Message))
		}
		if strings.TrimSpace(output.Prompt) != "" {
			prompt = strings.TrimSpace(output.Prompt)
		}
		if strings.TrimSpace(output.AdditionalContext) != "" {
			input.Instructions = strings.TrimSpace(input.Instructions + "\n\n" + output.AdditionalContext)
		}
	}
	conversation := append([]provider.Message(nil), input.History...)
	instructions := strings.TrimSpace(strings.Join([]string{r.options.Instructions, input.Instructions}, "\n\n"))
	allDefinitions := r.tools.Definitions()
	capabilities, hasCapabilities := modelcatalog.Capabilities{}, false
	if r.options.ModelCatalog != nil {
		capabilities, hasCapabilities = r.options.ModelCatalog.Resolve(r.options.Model)
	}
	definitions := make([]provider.ToolDefinition, 0, len(allDefinitions))
	definitionByName := make(map[string]provider.ToolDefinition, len(allDefinitions))
	enabledDefinitions := make(map[string]bool, len(allDefinitions))
	hasSearch := false
	for _, definition := range allDefinitions {
		definitionByName[definition.Name] = definition
		if definition.Name == "tool_search" {
			hasSearch = true
		}
	}
	for _, definition := range allDefinitions {
		if hasSearch && strings.HasPrefix(definition.Name, "mcp__") {
			continue
		}
		definitions = append(definitions, definition)
		enabledDefinitions[definition.Name] = true
	}
	definitionTokens := estimateDefinitionTokens(definitions)
	modelDefinitionTokens := func() int {
		if hasCapabilities && capabilities.ToolCalling != nil && !*capabilities.ToolCalling {
			return 0
		}
		return definitionTokens
	}

	for turn := 0; r.options.MaxTurns == 0 || turn < r.options.MaxTurns; turn++ {
		beforeTokens := EstimateMessagesTokens(conversation) + EstimateTextTokens(prompt) + EstimateTextTokens(instructions) + modelDefinitionTokens()
		if beforeTokens >= r.options.AutoCompactTokens {
			if err := r.runInformationalHook(ctx, HookPreCompact, HookInput{BeforeTokens: beforeTokens}); err != nil {
				return err
			}
			compacted, changed := CompactHistory(conversation, r.options.AutoCompactTokens/2)
			if changed {
				conversation = compacted
				afterTokens := EstimateMessagesTokens(conversation)
				r.emit(ctx, events, Event{Type: EventContextCompacted, BeforeTokens: beforeTokens, AfterTokens: afterTokens})
				if err := r.runInformationalHook(ctx, HookPostCompact, HookInput{BeforeTokens: beforeTokens, AfterTokens: afterTokens}); err != nil {
					return err
				}
			}
		}
		requestTokens := EstimateMessagesTokens(conversation) + EstimateTextTokens(prompt) + EstimateTextTokens(instructions) + modelDefinitionTokens()
		if requestTokens >= r.options.UsableContextTokens {
			return fmt.Errorf("estimated request context is %d tokens, exceeding the configured %d-token usable limit within the nominal %d-token window; use /compact or raise usable_context_tokens", requestTokens, r.options.UsableContextTokens, r.options.ContextWindowTokens)
		}
		request := provider.Request{
			Model: r.options.Model, Instructions: instructions, Prompt: prompt,
			History: append([]provider.Message(nil), conversation...), Tools: definitions, Images: append([]provider.Image(nil), input.Images...),
			ReasoningEffort: r.options.ReasoningEffort, ServiceTier: r.options.ServiceTier,
			ParallelToolCalls: capabilities.ParallelToolCalls,
		}
		response, err := r.modelTurn(ctx, request, events)
		if err != nil {
			return err
		}
		if r.options.OnUsage != nil {
			r.options.OnUsage(response.Usage)
		}
		if prompt != "" {
			conversation = append(conversation, provider.Message{Role: provider.MessageRoleUser, Content: prompt, Images: append([]provider.Image(nil), input.Images...)})
			prompt = ""
			input.Images = nil
		}
		for index := range response.ToolCalls {
			if response.ToolCalls[index].ID == "" {
				response.ToolCalls[index].ID = fmt.Sprintf("call_%s_%d", r.callIDPrefix, r.callIDSequence.Add(1))
			}
		}
		conversation = append(conversation, provider.Message{
			Role: provider.MessageRoleAssistant, Content: response.Text,
			ToolCalls: append([]provider.ToolCall(nil), response.ToolCalls...),
		})

		if len(response.ToolCalls) == 0 {
			if queued := drainSteers(steer); len(queued) > 0 {
				for _, message := range queued {
					conversation = append(conversation, provider.Message{Role: provider.MessageRoleUser, Content: message})
				}
				r.emit(ctx, events, Event{Type: EventQueuedCommitted, Queued: queued})
				continue
			}
			if !r.emit(ctx, events, Event{
				Type: EventCompleted, Response: &response,
				History: append([]provider.Message(nil), conversation...),
			}) {
				return ctx.Err()
			}
			return nil
		}

		parallelBatch := len(response.ToolCalls) > 1
		for _, call := range response.ToolCalls {
			item, exists := r.tools.Lookup(call.Name)
			if !exists || !tool.CanRunInParallel(item, call.Arguments) {
				parallelBatch = false
				break
			}
		}
		var returnedImages []provider.Image
		var returnedImageBytes int64
		commitPrepared := func(current *preparedToolCall) error {
			if current.item != nil && !current.finished {
				return errors.New("tool call execution did not produce a result")
			}
			if current.item != nil && current.executed && r.options.Hook != nil {
				if _, hookErr := r.options.Hook(ctx, HookPostToolUse, HookInput{Call: &current.call, Risk: current.risk, Category: current.category, Result: &current.result}); hookErr != nil {
					return hookErr
				}
			}
			if current.call.Name == "tool_search" {
				if added := activateSearchedDefinitions(current.result.Content, definitionByName, enabledDefinitions, &definitions); added {
					definitionTokens = estimateDefinitionTokens(definitions)
				}
			}
			if len(current.result.Images) > 0 {
				imageBytes, imageErr := validateReturnedToolImages(current.result.Images, len(returnedImages), returnedImageBytes, maxReturnedToolImages, maxReturnedToolImageBytes)
				if imageErr != nil {
					current.result.Images = nil
					current.result.IsError = true
					current.result.Content = boundToolContent(strings.TrimSpace(current.result.Content+"\nError: "+imageErr.Error()), r.options.ToolOutputTokens)
				} else {
					returnedImageBytes += imageBytes
				}
			}
			conversation = append(conversation, toolMessage(current.call.ID, current.result))
			returnedImages = append(returnedImages, current.result.Images...)
			if !r.emit(ctx, events, Event{Type: EventToolFinished, Call: &current.call, Risk: current.risk, Category: current.category, Result: &current.result}) {
				return ctx.Err()
			}
			return nil
		}
		executeAndCommit := func(current *preparedToolCall) error {
			if !current.finished {
				single := []preparedToolCall{*current}
				r.executePreparedCalls(ctx, events, single)
				*current = single[0]
			}
			return commitPrepared(current)
		}

		prepared := make([]preparedToolCall, 0, len(response.ToolCalls))
		for _, call := range response.ToolCalls {
			current := preparedToolCall{call: call}
			item, exists := r.tools.Lookup(call.Name)
			if !exists {
				current.result = tool.Result{Content: fmt.Sprintf("Unknown tool %q.", call.Name), IsError: true}
				current.finished = true
				if !parallelBatch {
					if err := executeAndCommit(&current); err != nil {
						return err
					}
				}
				prepared = append(prepared, current)
				continue
			}

			risk, summary := item.Risk(call.Arguments), item.Summary(call.Arguments)
			category := tool.CategoryOf(item)
			current.item, current.risk, current.category = item, risk, category
			if r.options.Hook != nil {
				output, hookErr := r.options.Hook(ctx, HookPreToolUse, HookInput{Call: &call, Risk: risk, Category: category})
				if hookErr != nil {
					return hookErr
				}
				if output.Allow != nil && !*output.Allow {
					current.risk, current.category = risk, category
					current.result = tool.Result{Content: "Tool call blocked by pre_tool_use hook: " + strings.TrimSpace(output.Message), IsError: true}
					current.finished = true
					if !parallelBatch {
						if err := executeAndCommit(&current); err != nil {
							return err
						}
					}
					prepared = append(prepared, current)
					continue
				}
				if strings.TrimSpace(output.Arguments) != "" {
					if !json.Valid([]byte(output.Arguments)) {
						return errors.New("pre_tool_use hook returned invalid JSON arguments")
					}
					call.Arguments = output.Arguments
					risk, summary = item.Risk(call.Arguments), item.Summary(call.Arguments)
				}
			}
			current.call, current.risk, current.category = call, risk, category
			if !r.emit(ctx, events, Event{Type: EventToolStarted, Call: &call, Risk: risk, Category: category, Summary: summary}) {
				return ctx.Err()
			}
			decision, err := r.approve(ctx, events, item, call, risk, category, summary)
			if err != nil {
				return err
			}
			if decision == ApprovalDeny {
				current.result = tool.Result{Content: "Tool call denied by the user or approval policy.", IsError: true}
				current.finished = true
				if !parallelBatch {
					if err := executeAndCommit(&current); err != nil {
						return err
					}
				}
				prepared = append(prepared, current)
				continue
			}
			if requester, ok := item.(tool.PermissionRequester); ok {
				request, requestErr := requester.PermissionRequest(call.Arguments)
				if requestErr != nil {
					return requestErr
				}
				if !permission.Empty(request.Permissions) {
					scope := permission.ScopeTurn
					if decision == ApprovalAllowSession {
						scope = permission.ScopeSession
					}
					if r.options.Permissions == nil {
						return errors.New("permission manager is unavailable")
					}
					if _, grantErr := r.options.Permissions.Grant(request.Permissions, scope); grantErr != nil {
						return fmt.Errorf("grant permissions: %w", grantErr)
					}
				}
			}
			if !parallelBatch {
				if err := executeAndCommit(&current); err != nil {
					return err
				}
			}
			prepared = append(prepared, current)
		}

		if parallelBatch {
			r.executePreparedCalls(ctx, events, prepared)
			for index := range prepared {
				if err := commitPrepared(&prepared[index]); err != nil {
					return err
				}
			}
		}
		if len(returnedImages) > 0 {
			conversation = append(conversation, provider.Message{Role: provider.MessageRoleUser, Content: "Images returned by the completed tool calls.", Images: returnedImages})
		}
		// Chat Completions requires one tool result for every call in an
		// assistant tool-call batch before a new user message is inserted.
		r.commitSteers(ctx, events, steer, &conversation)
	}
	return fmt.Errorf("agent stopped after reaching the configured %d-turn limit; set max_turns to 0 for no turn limit", r.options.MaxTurns)
}

func validateReturnedToolImages(images []provider.Image, previousCount int, previousBytes int64, maxCount int, maxBytes int64) (int64, error) {
	if len(images) > maxCount-previousCount {
		return 0, fmt.Errorf("tool-returned images exceed the aggregate %d-image limit", maxCount)
	}
	var added int64
	for _, image := range images {
		if image.Data == "" {
			continue
		}
		decoded := base64DecodedLength(image.Data)
		if decoded > maxBytes-previousBytes-added {
			return 0, fmt.Errorf("tool-returned images exceed the aggregate %d MiB decoded-data limit", maxBytes/(1024*1024))
		}
		added += decoded
	}
	return added, nil
}

func base64DecodedLength(value string) int64 {
	decoded := int64(base64.StdEncoding.DecodedLen(len(value)))
	if strings.HasSuffix(value, "=") {
		decoded--
	}
	if strings.HasSuffix(value, "==") {
		decoded--
	}
	return max(0, decoded)
}

type preparedToolCall struct {
	call     provider.ToolCall
	item     tool.Tool
	risk     tool.Risk
	category tool.Category
	result   tool.Result
	executed bool
	finished bool
}

const parallelToolLimit = 8

func (r *Runner) executePreparedCalls(ctx context.Context, events chan<- Event, calls []preparedToolCall) {
	executable := make([]int, 0, len(calls))
	parallel := true
	for index := range calls {
		if calls[index].item == nil || calls[index].finished {
			continue
		}
		executable = append(executable, index)
		if !tool.CanRunInParallel(calls[index].item, calls[index].call.Arguments) {
			parallel = false
		}
	}
	execute := func(index int) {
		current := &calls[index]
		executeContext := tool.WithProgressReporter(ctx, func(progress tool.Progress) {
			callCopy := current.call
			r.emit(ctx, events, Event{Type: EventToolOutputDelta, Call: &callCopy, Delta: progress.Delta, SessionID: progress.SessionID})
		})
		result, executeErr := current.item.Execute(executeContext, current.call.Arguments)
		if executeErr != nil {
			result.IsError = true
			if result.Content == "" {
				result.Content = executeErr.Error()
			} else {
				result.Content += "\nError: " + executeErr.Error()
			}
		}
		result.Content = boundToolContent(result.Content, r.options.ToolOutputTokens)
		current.result, current.executed, current.finished = result, true, true
	}
	executeWithSlot := func(index int) {
		select {
		case r.toolSlots <- struct{}{}:
			defer func() { <-r.toolSlots }()
			execute(index)
		case <-ctx.Done():
			calls[index].result = tool.Result{Content: ctx.Err().Error(), IsError: true}
			calls[index].finished = true
		}
	}
	if !parallel || len(executable) < 2 {
		for _, index := range executable {
			if parallel {
				executeWithSlot(index)
			} else {
				execute(index)
			}
		}
		return
	}
	jobs := make(chan int)
	workers := min(len(executable), parallelToolLimit)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for index := range jobs {
				executeWithSlot(index)
			}
		}()
	}
	for _, index := range executable {
		jobs <- index
	}
	close(jobs)
	group.Wait()
}

func estimateDefinitionTokens(definitions []provider.ToolDefinition) int {
	total := 0
	for _, definition := range definitions {
		total += EstimateTextTokens(definition.Name) + EstimateTextTokens(definition.Description) + EstimateTextTokens(string(definition.Parameters))
	}
	return total
}

func activateSearchedDefinitions(content string, available map[string]provider.ToolDefinition, enabled map[string]bool, definitions *[]provider.ToolDefinition) bool {
	var response struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal([]byte(content), &response) != nil {
		return false
	}
	added := false
	for _, match := range response.Tools {
		definition, ok := available[match.Name]
		if !ok || enabled[match.Name] {
			continue
		}
		*definitions = append(*definitions, definition)
		enabled[match.Name] = true
		added = true
	}
	return added
}

func (r *Runner) runInformationalHook(ctx context.Context, event HookEvent, input HookInput) error {
	if r.options.Hook == nil {
		return nil
	}
	_, err := r.options.Hook(ctx, event, input)
	return err
}

func drainSteers(steer <-chan string) []string {
	var queued []string
	for {
		select {
		case message := <-steer:
			queued = append(queued, message)
		default:
			return queued
		}
	}
}

func (r *Runner) commitSteers(ctx context.Context, events chan<- Event, steer <-chan string, conversation *[]provider.Message) {
	queued := drainSteers(steer)
	for _, message := range queued {
		*conversation = append(*conversation, provider.Message{Role: provider.MessageRoleUser, Content: message})
	}
	if len(queued) > 0 {
		r.emit(ctx, events, Event{Type: EventQueuedCommitted, Queued: queued})
	}
}

func (r *Runner) approve(ctx context.Context, events chan<- Event, item tool.Tool, call provider.ToolCall, risk tool.Risk, category tool.Category, summary string) (ApprovalDecision, error) {
	if r.options.Approval == ApprovalGranular {
		if allowed, configured := r.options.ApprovalCategories[category]; configured && !allowed {
			return ApprovalDeny, nil
		}
	}
	// Workspace file tools already reject escaping paths and symlinks and apply
	// changes atomically. They are the equivalent of a workspace-write grant and
	// should not interrupt the user with an additional approval prompt.
	if risk == tool.RiskRead || risk == tool.RiskWrite || r.options.Approval == ApprovalAlways {
		return ApprovalAllowOnce, nil
	}
	if r.options.Approval == ApprovalNever {
		return ApprovalDeny, nil
	}
	if r.ruleAllowed(call) {
		return ApprovalAllowOnce, nil
	}
	if r.options.Hook != nil {
		output, err := r.options.Hook(ctx, HookPermission, HookInput{Call: &call, Risk: risk, Category: category})
		if err != nil {
			return ApprovalDeny, err
		}
		if output.Allow != nil && !*output.Allow {
			return ApprovalDeny, nil
		}
	}
	var permissions *permission.Request
	if requester, ok := item.(tool.PermissionRequester); ok {
		value, err := requester.PermissionRequest(call.Arguments)
		if err != nil {
			return ApprovalDeny, err
		}
		if !permission.Empty(value.Permissions) {
			permissions = &value
		}
	}
	request := newApprovalRequest(call, risk, category, summary, permissions)
	if r.options.Policy != nil {
		request.PolicyPath = r.options.Policy.Path()
	}
	if !r.emit(ctx, events, Event{Type: EventApprovalRequired, Call: &call, Risk: risk, Category: category, Summary: summary, Approval: request}) {
		return ApprovalDeny, ctx.Err()
	}
	select {
	case decision := <-request.answer:
		switch decision {
		case ApprovalAllowSession:
			if permissions == nil {
				r.rememberRule(sessionRule(call))
			}
			return decision, nil
		case ApprovalAllowPrefix:
			r.rememberRule(prefixRule(call))
			return decision, nil
		case ApprovalAllowPersistentPrefix:
			if r.options.Policy == nil {
				return ApprovalDeny, errors.New("persistent policy store is unavailable")
			}
			if _, err := r.options.Policy.AddCommandPrefix(request.Prefix); err != nil {
				return ApprovalDeny, fmt.Errorf("persist approval rule: %w", err)
			}
			return decision, nil
		case ApprovalAllowOnce:
			return decision, nil
		default:
			return ApprovalDeny, nil
		}
	case <-ctx.Done():
		return ApprovalDeny, ctx.Err()
	}
}

func (r *Runner) rememberRule(rule string) {
	if rule == "" {
		return
	}
	r.rulesMu.Lock()
	r.rules[rule] = struct{}{}
	r.rulesMu.Unlock()
}

func (r *Runner) ruleAllowed(call provider.ToolCall) bool {
	if r.options.Policy != nil {
		if _, ok := r.options.Policy.Allows(call); ok {
			return true
		}
	}
	r.rulesMu.RLock()
	defer r.rulesMu.RUnlock()
	if _, ok := r.rules[sessionRule(call)]; ok {
		return true
	}
	if rule := prefixRule(call); rule != "" {
		_, ok := r.rules[rule]
		return ok
	}
	return false
}

func sessionRule(call provider.ToolCall) string { return "tool:" + call.Name }

func prefixRule(call provider.ToolCall) string {
	if prefix := commandPrefix(call); prefix != "" {
		return "command-prefix:" + prefix
	}
	return ""
}

func commandPrefix(call provider.ToolCall) string {
	return policy.PrefixForCall(call)
}
