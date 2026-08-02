package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/collaboration"
	"github.com/daemon365/supercode/internal/memory"
	"github.com/daemon365/supercode/internal/policy"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/session"
	"github.com/daemon365/supercode/internal/skill"
	"github.com/daemon365/supercode/internal/taskstate"
	terminalUI "github.com/daemon365/supercode/internal/tui"
)

const (
	maxPromptBytes       = 8 * 1024 * 1024
	maxOutputSchemaBytes = 1024 * 1024
)

type executionContext struct {
	environment   projectEnvironment
	stores        applicationStores
	runtime       agentRuntime
	policyStore   *policy.Store
	eventLogger   func(agent.Event)
	promptArgs    []string
	initialImages []provider.Image
	stdin         io.Reader
	stdout        io.Writer
	stderr        io.Writer
}

func executeInvocation(ctx context.Context, execution executionContext) error {
	options := execution.environment.options
	promptArgs := execution.promptArgs
	if options.review {
		focus := strings.TrimSpace(strings.Join(promptArgs, " "))
		promptArgs = []string{prompts.ReviewPrompt(focus)}
		options.chat = false
	}
	turnPrompt := prompts.TurnInput{
		Model: options.modelName, Mode: prompts.NormalizeMode(execution.runtime.session.Mode), Approval: string(options.approval),
		SandboxStatus: execution.runtime.sandboxStatus, Workspace: options.workspace,
		ContextWindowTokens: options.contextWindowTokens, AutoCompactTokens: options.autoCompactTokens,
		UsableContextTokens: options.usableContextTokens, MaxTurns: options.maxTurns,
		Plugins: append([]string(nil), execution.environment.plugins.Names...),
		Hooks:   summarizeHooks(execution.environment.fileConfig.Hooks), MCPServers: execution.runtime.mcpNames,
	}
	interactiveAllowed := !options.review && !options.jsonOutput && options.outputSchema == "" && options.outputLastMessage == "" && !options.ephemeral
	if execution.runtime.terminalInput && interactiveAllowed && (options.chat || len(promptArgs) == 0) {
		return runTUI(ctx, execution, options, promptArgs)
	}
	if options.chat {
		return runAgentChat(
			ctx, execution.runtime.runner, execution.runtime.provider, options, promptArgs, execution.runtime.session,
			execution.stores.sessions, execution.stores.skills, execution.stores.memory,
			execution.runtime.taskState, execution.runtime.collaboration, turnPrompt,
			execution.stdin, execution.stdout, execution.stderr, execution.runtime.terminalInput,
		)
	}
	return runSingleTurn(ctx, execution, options, promptArgs, turnPrompt)
}

func runTUI(ctx context.Context, execution executionContext, options options, promptArgs []string) error {
	fileConfig := execution.environment.fileConfig
	return terminalUI.Run(ctx, execution.runtime.provider, terminalUI.Options{
		Model:               options.modelName,
		Instructions:        execution.environment.agentInstructions,
		InitialPrompt:       strings.TrimSpace(strings.Join(promptArgs, " ")),
		InitialImages:       execution.initialImages,
		InitialImageLabels:  append([]string(nil), options.imagePaths...),
		Stream:              options.stream,
		Timeout:             options.timeout,
		MaxTurns:            options.maxTurns,
		ContextWindowTokens: options.contextWindowTokens,
		AutoCompactTokens:   options.autoCompactTokens,
		UsableContextTokens: options.usableContextTokens,
		ToolOutputTokens:    options.toolOutputTokens,
		Approval:            options.approval,
		Permissions:         execution.runtime.permissions,
		ApprovalCategories:  options.approvalCategories,
		ModelCatalog:        options.modelCatalog,
		Workspace:           options.workspace,
		Tools:               execution.runtime.tools,
		SessionStore:        execution.stores.sessions,
		Session:             execution.runtime.session,
		Skills:              execution.stores.skills,
		Memory:              execution.stores.memory,
		TaskState:           execution.runtime.taskState,
		Hook:                execution.stores.hooks.Run,
		Collaboration:       execution.runtime.collaboration,
		GoalAutoContinue:    options.goalAutoContinue,
		AlternateScreen:     options.alternateScreen,
		Models:              options.models,
		ModelInfos:          options.modelInfos,
		ReasoningEffort:     options.reasoningEffort,
		ServiceTier:         options.serviceTier,
		FallbackModels:      options.fallbackModels,
		ConfigSummary:       execution.environment.configSummary,
		SandboxStatus:       execution.runtime.sandboxStatus,
		Policy:              execution.policyStore,
		UserInput:           execution.runtime.userInput,
		Plugins:             append([]string(nil), execution.environment.plugins.Names...),
		HookSummary:         summarizeHooks(fileConfig.Hooks),
		Theme:               fileConfig.Theme,
		Keymap:              fileConfig.Keymap,
		Notification:        fileConfig.Notification,
		TerminalTitle:       fileConfig.TerminalTitle,
		OnEvent:             execution.eventLogger,
		StartupWarnings:     append([]string(nil), execution.runtime.mcpWarnings...),
	}, execution.stdin, execution.stdout)
}

func runSingleTurn(
	ctx context.Context,
	execution executionContext,
	options options,
	promptArgs []string,
	turnPrompt prompts.TurnInput,
) error {
	prompt, err := readPrompt(promptArgs, execution.stdin)
	if err != nil {
		return err
	}
	_, _ = execution.stores.memory.AutoCapture(prompt)
	turnPrompt.Skills = execution.stores.skills.Instructions(prompt)
	turnPrompt.Memory = execution.stores.memory.Instructions()
	turnPrompt.Goal = goalPrompt(execution.runtime.taskState)
	turnInstructions := prompts.Turn(turnPrompt)
	resolvedSchema, turnInstructions, err := resolveOutputSchema(options.outputSchema, turnInstructions)
	if err != nil {
		return err
	}
	if !options.ephemeral {
		_ = execution.stores.sessions.Append(execution.runtime.session.ID, "user_prompt", map[string]string{"content": prompt})
	}
	history, err := executeAgent(
		ctx, execution.runtime.runner, prompt, execution.runtime.session.Messages,
		turnInstructions, execution.initialImages, options.jsonOutput, execution.stdout, execution.stderr,
	)
	if err != nil {
		shutdownErr := shutdownCollaboration(execution.runtime.collaboration)
		if !options.ephemeral && len(history) > 0 {
			interrupted := execution.runtime.session
			interrupted.Messages = history
			interrupted.Plan, interrupted.Goal = execution.runtime.taskState.Snapshot()
			interrupted.Agents = execution.runtime.collaboration.Snapshot()
			if interrupted.Title == "" {
				interrupted.Title = title(prompt)
			}
			commitErr := execution.stores.sessions.Commit(interrupted)
			shutdownErr = errors.Join(shutdownErr, commitErr)
			if commitErr == nil {
				shutdownErr = errors.Join(shutdownErr, runNonTUIMemoryShutdown(execution.stores.memory, execution.runtime.provider, execution.stores.sessions, interrupted.ID, options.modelName))
			}
		}
		return errors.Join(err, shutdownErr)
	}
	if err := shutdownCollaboration(execution.runtime.collaboration); err != nil {
		return fmt.Errorf("stop sub-agents before saving: %w", err)
	}
	lastMessage := lastAssistantMessage(history)
	if err := validateStructuredOutput(resolvedSchema, lastMessage); err != nil {
		return err
	}
	if options.outputLastMessage != "" {
		if err := os.WriteFile(options.outputLastMessage, []byte(lastMessage), 0o600); err != nil {
			return fmt.Errorf("write last message: %w", err)
		}
	}
	activeSession := execution.runtime.session
	activeSession.Messages = history
	activeSession.Plan, activeSession.Goal = execution.runtime.taskState.Snapshot()
	activeSession.Agents = execution.runtime.collaboration.Snapshot()
	if activeSession.Title == "" {
		activeSession.Title = title(prompt)
	}
	if options.ephemeral {
		return nil
	}
	if err := execution.stores.sessions.Commit(activeSession); err != nil {
		return err
	}
	return runNonTUIMemoryShutdown(execution.stores.memory, execution.runtime.provider, execution.stores.sessions, activeSession.ID, options.modelName)
}

func resolveOutputSchema(path, instructions string) (*jsonschema.Resolved, string, error) {
	if path == "" {
		return nil, instructions, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open output schema: %w", err)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maxOutputSchemaBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, "", fmt.Errorf("read output schema: %w", readErr)
	}
	if closeErr != nil {
		return nil, "", fmt.Errorf("close output schema: %w", closeErr)
	}
	if len(content) > maxOutputSchemaBytes {
		return nil, "", fmt.Errorf("output schema exceeds the %d-byte limit", maxOutputSchemaBytes)
	}
	var schemaValue jsonschema.Schema
	if err := json.Unmarshal(content, &schemaValue); err != nil {
		return nil, "", fmt.Errorf("parse output schema: %w", err)
	}
	resolved, err := schemaValue.Resolve(nil)
	if err != nil {
		return nil, "", fmt.Errorf("resolve output schema: %w", err)
	}
	instructions = strings.TrimSpace(instructions + "\n\nReturn the final assistant message as JSON only, without Markdown fences, conforming exactly to this JSON Schema:\n" + string(content))
	return resolved, instructions, nil
}

func validateStructuredOutput(schema *jsonschema.Resolved, value string) error {
	if schema == nil {
		return nil
	}
	var instance any
	if err := json.Unmarshal([]byte(value), &instance); err != nil {
		return fmt.Errorf("final message is not valid JSON: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("final message does not match output schema: %w", err)
	}
	return nil
}

func runAgentChat(
	ctx context.Context,
	runner *agent.Runner,
	modelProvider provider.Provider,
	options options,
	initialPrompt []string,
	activeSession session.Session,
	store *session.Store,
	skills *skill.Catalog,
	memoryStore *memory.Store,
	taskState *taskstate.State,
	collaborationManager *collaboration.Manager,
	turnPrompt prompts.TurnInput,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	showPrompts bool,
) (returnErr error) {
	history := append([]provider.Message(nil), activeSession.Messages...)
	defer func() {
		shutdownErr := shutdownCollaboration(collaborationManager)
		if !options.ephemeral && activeSession.ID != "" {
			activeSession.Messages = append([]provider.Message(nil), history...)
			activeSession.Plan, activeSession.Goal = taskState.Snapshot()
			activeSession.Agents = collaborationManager.Snapshot()
			commitErr := store.Commit(activeSession)
			shutdownErr = errors.Join(shutdownErr, commitErr)
			if commitErr == nil {
				shutdownErr = errors.Join(shutdownErr, runNonTUIMemoryShutdown(memoryStore, modelProvider, store, activeSession.ID, options.modelName))
			}
		}
		if shutdownErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("finalize chat session: %w", shutdownErr))
		}
	}()
	ask := func(prompt string) error {
		_, _ = memoryStore.AutoCapture(prompt)
		turnPrompt.Model = options.modelName
		turnPrompt.Mode = prompts.NormalizeMode(activeSession.Mode)
		turnPrompt.Approval = string(options.approval)
		turnPrompt.Skills = skills.Instructions(prompt)
		turnPrompt.Memory = memoryStore.Instructions()
		turnPrompt.Goal = goalPrompt(taskState)
		instructions := prompts.Turn(turnPrompt)
		if !options.ephemeral {
			if err := store.Append(activeSession.ID, "user_prompt", map[string]string{"content": prompt}); err != nil {
				return err
			}
		}
		updated, err := executeAgent(ctx, runner, prompt, history, instructions, nil, false, stdout, stderr)
		if len(updated) > 0 {
			history, activeSession.Messages = updated, updated
		}
		if err != nil {
			return err
		}
		activeSession.Plan, activeSession.Goal = taskState.Snapshot()
		activeSession.Agents = collaborationManager.Snapshot()
		if activeSession.Title == "" {
			activeSession.Title = title(prompt)
		}
		if options.ephemeral {
			return nil
		}
		return store.Commit(activeSession)
	}
	if len(initialPrompt) > 0 {
		if err := ask(strings.TrimSpace(strings.Join(initialPrompt, " "))); err != nil {
			return err
		}
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for {
		if showPrompts {
			if _, err := fmt.Fprint(stdout, "> "); err != nil {
				return err
			}
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read chat input: %w", err)
			}
			if showPrompts {
				_, _ = fmt.Fprintln(stdout)
			}
			return nil
		}
		prompt := strings.TrimSpace(scanner.Text())
		switch prompt {
		case "":
			continue
		case "/exit", "/quit":
			return nil
		case "/new":
			if err := quiesceCollaboration(collaborationManager); err != nil {
				return fmt.Errorf("stop sub-agents before starting a new session: %w", err)
			}
			if !options.ephemeral {
				activeSession.Messages = append([]provider.Message(nil), history...)
				activeSession.Plan, activeSession.Goal = taskState.Snapshot()
				activeSession.Agents = collaborationManager.Snapshot()
				if err := store.Commit(activeSession); err != nil {
					return fmt.Errorf("save session before /new: %w", err)
				}
			}
			if err := collaborationManager.Restore(json.RawMessage(`[]`)); err != nil {
				return fmt.Errorf("clear sub-agents for /new: %w", err)
			}
			history = nil
			taskState.Reset()
			if !options.ephemeral {
				var err error
				activeSession, err = store.New(options.workspace, options.modelName)
				if err != nil {
					return err
				}
			} else {
				activeSession = session.Session{Workspace: options.workspace, Model: options.modelName}
			}
			continue
		}
		if err := ask(prompt); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if _, writeErr := fmt.Fprintf(stderr, "supercode: %v\n", err); writeErr != nil {
				return writeErr
			}
		}
	}
}

func quiesceCollaboration(manager *collaboration.Manager) error {
	if manager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return manager.Quiesce(ctx)
}

func shutdownCollaboration(manager *collaboration.Manager) error {
	if manager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return manager.Shutdown(ctx)
}

func runNonTUIMemoryShutdown(memoryStore *memory.Store, modelProvider provider.Provider, sessions *session.Store, sessionID, modelName string) error {
	if memoryStore == nil || modelProvider == nil || sessions == nil || sessionID == "" || !memoryStore.Configuration().Generate {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err := memoryStore.RunShutdown(ctx, modelProvider, sessions, sessionID, modelName)
	return err
}

func goalPrompt(state *taskstate.State) string {
	if state == nil {
		return ""
	}
	_, goal := state.Snapshot()
	if goal == nil {
		return ""
	}
	budget := "unlimited"
	if goal.TokenBudget > 0 {
		budget = fmt.Sprintf("%d tokens (%d remaining)", goal.TokenBudget, max(int64(0), int64(goal.TokenBudget)-goal.TotalTokens))
	}
	return fmt.Sprintf("Objective: %s\nStatus: %s\nUsage: %d tokens across %d turns\nBudget: %s", goal.Objective, goal.Status, goal.TotalTokens, goal.Turns, budget)
}
