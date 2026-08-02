package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/credential"
	projectinstructions "github.com/daemon365/supercode/internal/instructions"
	"github.com/daemon365/supercode/internal/prompts"
	"github.com/daemon365/supercode/internal/provider"
)

func executeRequest(
	ctx context.Context,
	modelProvider provider.Provider,
	options options,
	request provider.Request,
	stdout io.Writer,
) (provider.Response, error) {
	requestContext, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()

	if options.stream {
		return writeStream(requestContext, modelProvider, request, stdout)
	}

	response, err := modelProvider.Generate(requestContext, request)
	if err != nil {
		return provider.Response{}, err
	}
	_, err = fmt.Fprintln(stdout, response.Text)
	return response, err
}

func executeAgent(
	ctx context.Context,
	runner *agent.Runner,
	prompt string,
	history []provider.Message,
	instructions string,
	images []provider.Image,
	jsonOutput bool,
	stdout io.Writer,
	stderr io.Writer,
) ([]provider.Message, error) {
	events := runner.Run(ctx, agent.Input{Prompt: prompt, History: history, Instructions: instructions, Images: images})
	wroteText := false
	var partialAssistant strings.Builder
	var completedHistory []provider.Message
	partialHistory := func() []provider.Message {
		partial := append([]provider.Message(nil), history...)
		if strings.TrimSpace(prompt) != "" {
			partial = append(partial, provider.Message{Role: provider.MessageRoleUser, Content: prompt, Images: append([]provider.Image(nil), images...)})
		}
		if partialAssistant.Len() > 0 {
			partial = append(partial, provider.Message{Role: provider.MessageRoleAssistant, Content: partialAssistant.String()})
		}
		return partial
	}
	for event := range events {
		if jsonOutput {
			if err := writeAgentJSONEvent(stdout, event); err != nil {
				return nil, err
			}
		}
		switch event.Type {
		case agent.EventTextDelta:
			partialAssistant.WriteString(event.Delta)
			if jsonOutput {
				break
			}
			if _, err := io.WriteString(stdout, event.Delta); err != nil {
				return nil, err
			}
			wroteText = true
		case agent.EventToolStarted:
			if jsonOutput {
				break
			}
			if wroteText {
				_, _ = fmt.Fprintln(stdout)
				wroteText = false
			}
			_, _ = fmt.Fprintf(stderr, "[tool] %s\n", event.Summary)
		case agent.EventToolOutputDelta:
			if !jsonOutput {
				_, _ = io.WriteString(stderr, event.Delta)
			}
		case agent.EventApprovalRequired:
			// One-shot/piped operation has no safe interactive approval surface.
			event.Approval.Decide(false)
			if !jsonOutput {
				_, _ = fmt.Fprintf(stderr, "[denied] %s (use --approval always to allow non-interactively)\n", event.Summary)
			}
		case agent.EventToolFinished:
			if !jsonOutput && event.Result != nil && event.Result.IsError {
				_, _ = fmt.Fprintln(stderr, "[tool error] "+event.Result.Content)
			}
		case agent.EventCompleted:
			completedHistory = event.History
		case agent.EventError:
			if wroteText && !jsonOutput {
				_, _ = fmt.Fprintln(stdout)
			}
			return partialHistory(), event.Err
		}
	}
	if wroteText && !jsonOutput {
		_, _ = fmt.Fprintln(stdout)
	}
	if completedHistory == nil {
		if err := ctx.Err(); err != nil {
			return partialHistory(), err
		}
		return partialHistory(), errors.New("agent ended without a completed response")
	}
	return completedHistory, nil
}

func writeAgentJSONEvent(output io.Writer, event agent.Event) error {
	value := map[string]any{"type": event.Type}
	if event.Delta != "" {
		value["delta"] = event.Delta
	}
	if event.Call != nil {
		value["tool_call"] = event.Call
	}
	if event.Risk != "" {
		value["risk"] = event.Risk
	}
	if event.Category != "" {
		value["category"] = event.Category
	}
	if event.Summary != "" {
		value["summary"] = event.Summary
	}
	if event.Result != nil {
		value["result"] = event.Result
	}
	if event.Response != nil {
		value["response"] = event.Response
	}
	if event.Err != nil {
		value["error"] = event.Err.Error()
	}
	if len(event.Queued) > 0 {
		value["queued"] = event.Queued
	}
	if event.BeforeTokens > 0 || event.AfterTokens > 0 {
		value["before_tokens"], value["after_tokens"] = event.BeforeTokens, event.AfterTokens
	}
	if event.SessionID != 0 {
		value["session_id"] = event.SessionID
	}
	return json.NewEncoder(output).Encode(value)
}

func lastAssistantMessage(history []provider.Message) string {
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == provider.MessageRoleAssistant && strings.TrimSpace(history[index].Content) != "" {
			return history[index].Content
		}
	}
	return ""
}

func codingInstructions(workspace, custom string) string {
	set, _ := projectinstructions.Discover(projectinstructions.Options{
		Root: projectinstructions.FindProjectRoot(workspace), CWD: workspace,
	})
	return codingInstructionsFromSet(workspace, custom, set)
}

func codingInstructionsFromSet(workspace, custom string, set projectinstructions.Set) string {
	projectSections := []string{strings.TrimSpace(set.Text())}
	for _, instructionPath := range []string{filepath.Join(workspace, ".supercode", "instructions.md")} {
		info, err := os.Lstat(instructionPath)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 64*1024 {
			continue
		}
		contents, err := os.ReadFile(instructionPath)
		if err == nil && len(contents) > 0 {
			projectSections = append(projectSections, fmt.Sprintf("Project instructions from %s:\n%s", instructionPath, contents))
		}
	}
	return prompts.Session(prompts.SessionInput{
		Workspace: workspace, ProjectInstructions: strings.TrimSpace(strings.Join(projectSections, "\n\n")),
		CustomInstructions: custom, ToolNames: []string{"apply_patch", "spawn_agent"},
	})
}

func title(prompt string) string {
	value := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(value)
	if len(runes) > 60 {
		return string(runes[:57]) + "..."
	}
	return value
}

func runChat(
	ctx context.Context,
	modelProvider provider.Provider,
	options options,
	initialPrompt []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	showPrompts bool,
) error {
	var history []provider.Message

	ask := func(prompt string) error {
		response, err := executeRequest(ctx, modelProvider, options, provider.Request{
			Model:        options.modelName,
			Instructions: options.instructions,
			Prompt:       prompt,
			History:      append([]provider.Message(nil), history...),
		}, stdout)
		if err != nil {
			return err
		}
		if response.Text != "" {
			history = append(history,
				provider.Message{Role: provider.MessageRoleUser, Content: prompt},
				provider.Message{Role: provider.MessageRoleAssistant, Content: response.Text},
			)
		}
		return nil
	}

	if showPrompts {
		if _, err := fmt.Fprintln(stdout, "Ask a question to start. Use /new to reset or /exit to quit."); err != nil {
			return err
		}
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
			history = nil
			if showPrompts {
				if _, err := fmt.Fprintln(stdout, "Started a new conversation."); err != nil {
					return err
				}
			}
			continue
		}

		if err := ask(prompt); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if showPrompts {
				_, _ = fmt.Fprintln(stdout)
			}
			if _, writeErr := fmt.Fprintf(stderr, "supercode: %v\n", err); writeErr != nil {
				return writeErr
			}
		}
	}
}

func writeStream(
	ctx context.Context,
	modelProvider provider.Provider,
	request provider.Request,
	stdout io.Writer,
) (response provider.Response, returnErr error) {
	stream, err := modelProvider.Stream(ctx, request)
	if err != nil {
		return provider.Response{}, err
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("close response stream: %w", closeErr)
		}
	}()

	wroteText := false
	var completed *provider.Response
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case provider.StreamEventTextDelta:
			if _, err := io.WriteString(stdout, event.Delta); err != nil {
				return provider.Response{}, fmt.Errorf("write streamed response: %w", err)
			}
			wroteText = true
		case provider.StreamEventCompleted:
			completed = event.Response
		}
	}
	if err := stream.Err(); err != nil {
		return provider.Response{}, err
	}

	if !wroteText && completed != nil {
		_, err = fmt.Fprintln(stdout, completed.Text)
		return *completed, err
	}
	if wroteText {
		_, err = fmt.Fprintln(stdout)
	}
	if completed != nil {
		return *completed, err
	}
	return provider.Response{}, err
}

func isTerminal(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func readPrompt(args []string, stdin io.Reader) (string, error) {
	if len(args) > 0 {
		size := len(args) - 1
		for _, argument := range args {
			size += len(argument)
			if size > maxPromptBytes {
				return "", fmt.Errorf("prompt exceeds the %d-byte limit", maxPromptBytes)
			}
		}
		prompt := strings.TrimSpace(strings.Join(args, " "))
		if prompt == "" {
			return "", provider.ErrEmptyPrompt
		}
		return prompt, nil
	}

	content, err := io.ReadAll(io.LimitReader(stdin, maxPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read prompt from stdin: %w", err)
	}
	if len(content) > maxPromptBytes {
		return "", fmt.Errorf("prompt exceeds the %d-byte limit", maxPromptBytes)
	}
	prompt := strings.TrimSpace(string(content))
	if prompt == "" {
		return "", errors.New("prompt is required: pass it as arguments or stdin")
	}
	return prompt, nil
}

func envOrDefault(lookupEnv func(string) (string, bool), name string, fallback string) string {
	if value, ok := lookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func resolveAPIKey(ctx context.Context, fileConfig config.File, lookupEnv func(string) (string, bool)) (string, error) {
	resolver := credential.Resolver{LookupEnv: lookupEnv}
	return resolver.Resolve(ctx, credential.Source{Token: fileConfig.Token, Command: fileConfig.TokenCommand})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
