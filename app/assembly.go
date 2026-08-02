package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/collaboration"
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/credential"
	"github.com/daemon365/supercode/internal/mcp"
	"github.com/daemon365/supercode/internal/permission"
	"github.com/daemon365/supercode/internal/policy"
	"github.com/daemon365/supercode/internal/provider"
	anthropicProvider "github.com/daemon365/supercode/internal/provider/anthropic"
	openaiProvider "github.com/daemon365/supercode/internal/provider/openai"
	openaiResponsesProvider "github.com/daemon365/supercode/internal/provider/openairesponses"
	openrouterProvider "github.com/daemon365/supercode/internal/provider/openrouter"
	"github.com/daemon365/supercode/internal/session"
	"github.com/daemon365/supercode/internal/taskstate"
	"github.com/daemon365/supercode/internal/tool"
	"github.com/daemon365/supercode/internal/userinput"
)

type agentRuntime struct {
	session       session.Session
	permissions   *permission.Manager
	sandboxStatus string
	mcpNames      []string
	provider      provider.Provider
	tools         *tool.Registry
	runner        *agent.Runner
	taskState     *taskstate.State
	collaboration *collaboration.Manager
	terminalInput bool
	userInput     *userinput.Manager
	mcp           *mcp.Manager
	mcpWarnings   []string
	builtins      io.Closer
}

func (r agentRuntime) close() {
	if r.collaboration != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = r.collaboration.Shutdown(ctx)
		cancel()
	}
	if r.builtins != nil {
		_ = r.builtins.Close()
	}
	if r.mcp != nil {
		_ = r.mcp.Close()
	}
}

func listSessions(store *session.Store, workspace string, output io.Writer) error {
	values, err := store.List(workspace, 50)
	if err != nil {
		return err
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\n", value.ID, value.UpdatedAt.Local().Format(time.RFC3339), value.Title); err != nil {
			return err
		}
	}
	return nil
}

func loadActiveSession(store *session.Store, options options) (session.Session, error) {
	if options.resume == "" {
		return store.New(options.workspace, options.modelName)
	}
	var (
		active session.Session
		err    error
	)
	if options.resume == "latest" {
		active, err = store.Latest(options.workspace)
	} else {
		active, err = store.Load(options.resume)
	}
	if err != nil {
		return session.Session{}, err
	}
	if active.Workspace != options.workspace {
		return session.Session{}, errors.New("the session belongs to a different workspace")
	}
	return active, nil
}

func assembleAgentRuntime(
	ctx context.Context,
	environment projectEnvironment,
	stores applicationStores,
	policyStore *policy.Store,
	eventLogger func(agent.Event),
	lookupEnv func(string) (string, bool),
	stdin io.Reader,
) (result agentRuntime, returnErr error) {
	options, fileConfig := environment.options, environment.fileConfig
	activeSession, err := loadActiveSession(stores.sessions, options)
	if err != nil {
		return agentRuntime{}, err
	}
	readRoots := append([]string(nil), fileConfig.ReadRoots...)
	readRoots = append(readRoots, stores.memory.Root())
	for _, root := range stores.skillRoots {
		if info, statErr := os.Stat(root); statErr == nil && info.IsDir() {
			readRoots = append(readRoots, root)
		}
	}
	permissionManager, err := permission.NewManager(options.workspace)
	if err != nil {
		return agentRuntime{}, err
	}
	sandboxOptions := tool.SandboxOptions{
		ReadRoots: readRoots, WriteRoots: fileConfig.WriteRoots,
		DenyRoots: fileConfig.DenyRoots, Permissions: permissionManager,
	}
	builtins, builtinLifecycle, err := tool.BuiltinsWithLifecycle(options.workspace, sandboxOptions)
	if err != nil {
		return agentRuntime{}, err
	}
	defer func() {
		if returnErr != nil {
			_ = builtinLifecycle.Close()
		}
	}()
	mcpConfigurations, _ := mcpConfigs(fileConfig)
	mcpManager, err := mcp.ConnectAll(ctx, options.workspace, mcpConfigurations)
	if err != nil {
		return agentRuntime{}, err
	}
	defer func() {
		if returnErr != nil {
			_ = mcpManager.Close()
		}
	}()
	baseTools := append([]tool.Tool(nil), builtins...)
	baseTools = append(baseTools, stores.memory.Tools()...)
	baseTools = append(baseTools, mcpManager.Tools()...)
	mcpWarnings := make([]string, 0, len(mcpManager.Failures()))
	for _, failure := range mcpManager.Failures() {
		mcpWarnings = append(mcpWarnings, failure.Error())
	}
	modelProvider, err := buildModelProvider(ctx, fileConfig, options, lookupEnv)
	if err != nil {
		return agentRuntime{}, err
	}
	stores.memory.StartStartup(ctx, modelProvider, stores.sessions, activeSession.ID, options.modelName)
	baseRegistry, err := tool.NewRegistry(baseTools...)
	if err != nil {
		return agentRuntime{}, err
	}
	subOptions := agentOptions(environment, stores, policyStore, eventLogger, permissionManager)
	subOptions.Approval = agent.ApprovalNever
	subRunner, err := agent.New(modelProvider, baseRegistry, subOptions)
	if err != nil {
		return agentRuntime{}, err
	}
	taskState := taskstate.New(activeSession.Plan, activeSession.Goal)
	collaborationManager := collaboration.New(ctx, subRunner)
	if err := collaborationManager.Restore(activeSession.Agents); err != nil {
		return agentRuntime{}, err
	}
	subTools := append(taskState.Tools(), collaborationManager.Tools()...)
	searchableSubTools := append(append([]tool.Tool(nil), baseTools...), subTools...)
	subTools = append(subTools, tool.SearchTool(searchableSubTools))
	if err := baseRegistry.Add(subTools...); err != nil {
		return agentRuntime{}, err
	}
	terminalInput := isTerminal(stdin)
	userInputManager := userinput.New()
	allTools := append([]tool.Tool(nil), baseTools...)
	allTools = append(allTools, taskState.Tools()...)
	allTools = append(allTools, collaborationManager.Tools()...)
	if terminalInput {
		allTools = append(allTools, userInputManager.Tool())
	}
	allTools = append(allTools, tool.SearchTool(allTools))
	registry, err := tool.NewRegistry(allTools...)
	if err != nil {
		return agentRuntime{}, err
	}
	mainOptions := agentOptions(environment, stores, policyStore, eventLogger, permissionManager)
	mainOptions.Approval = options.approval
	mainOptions.OnUsage = taskState.RecordUsage
	runner, err := agent.New(modelProvider, registry, mainOptions)
	if err != nil {
		return agentRuntime{}, err
	}
	return agentRuntime{
		session: activeSession, permissions: permissionManager,
		sandboxStatus: tool.SandboxStatusWithOptions(options.workspace, sandboxOptions), mcpNames: mcpManager.Names(),
		provider: modelProvider, tools: registry, runner: runner, taskState: taskState,
		collaboration: collaborationManager, terminalInput: terminalInput, userInput: userInputManager, mcp: mcpManager, mcpWarnings: mcpWarnings, builtins: builtinLifecycle,
	}, nil
}

func buildModelProvider(ctx context.Context, fileConfig config.File, options options, lookupEnv func(string) (string, bool)) (provider.Provider, error) {
	if len(fileConfig.Providers) == 0 {
		apiKey, err := resolveAPIKey(ctx, fileConfig, lookupEnv)
		if err != nil {
			return nil, err
		}
		return openaiProvider.New(openaiProvider.Config{APIKey: apiKey, BaseURL: options.baseURL, MaxRetries: options.maxRetries})
	}

	routes := make([]provider.Route, 0, len(fileConfig.Providers))
	for _, configuration := range fileConfig.Providers {
		apiKey, err := resolveProviderAPIKey(ctx, configuration, lookupEnv)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", configuration.Name, err)
		}
		kind := strings.ToLower(strings.TrimSpace(configuration.Provider))
		var implementation provider.Provider
		switch kind {
		case "openai":
			implementation, err = openaiProvider.New(openaiProvider.Config{
				APIKey: apiKey, BaseURL: firstNonEmpty(configuration.URL, config.DefaultURL),
				MaxRetries: options.maxRetries, Headers: configuration.Headers,
			})
		case "openai_responses":
			implementation, err = openaiResponsesProvider.New(openaiResponsesProvider.Config{
				APIKey: apiKey, BaseURL: firstNonEmpty(configuration.URL, config.DefaultURL),
				MaxRetries: options.maxRetries, Headers: configuration.Headers,
			})
		case "anthropic":
			implementation, err = anthropicProvider.New(anthropicProvider.Config{
				APIKey: apiKey, BaseURL: configuration.URL, MaxRetries: options.maxRetries,
				MaxTokens: configuration.MaxTokens, Headers: configuration.Headers,
			})
		case "openrouter":
			implementation, err = openrouterProvider.New(openrouterProvider.Config{
				APIKey: apiKey, BaseURL: configuration.URL, Headers: configuration.Headers,
			})
		default:
			err = fmt.Errorf("unsupported provider type %q", configuration.Provider)
		}
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", configuration.Name, err)
		}
		routes = append(routes, provider.Route{Name: configuration.Name, Models: configuration.Models, Provider: implementation})
	}
	return provider.NewRouter(routes)
}

func resolveProviderAPIKey(ctx context.Context, configuration config.ProviderConfig, lookupEnv func(string) (string, bool)) (string, error) {
	configured := strings.TrimSpace(configuration.Token)
	if strings.HasPrefix(configured, "${") && strings.HasSuffix(configured, "}") && strings.Count(configured, "${") == 1 {
		name := strings.TrimSpace(configured[2 : len(configured)-1])
		if name == "" {
			return "", errors.New("token environment variable name is empty")
		}
		if value, ok := lookupEnv(name); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
		return "", fmt.Errorf("environment variable %s is required", name)
	}
	environmentName := "OPENAI_API_KEY"
	switch strings.ToLower(strings.TrimSpace(configuration.Provider)) {
	case "anthropic":
		environmentName = "ANTHROPIC_API_KEY"
	case "openrouter":
		environmentName = "OPENROUTER_API_KEY"
	}
	if value, ok := lookupEnv(environmentName); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value), nil
	}
	command := configuration.TokenCommand
	resolver := credential.Resolver{LookupEnv: func(string) (string, bool) { return "", false }}
	return resolver.Resolve(ctx, credential.Source{Token: configured, Command: command})
}

func agentOptions(
	environment projectEnvironment,
	stores applicationStores,
	policyStore *policy.Store,
	eventLogger func(agent.Event),
	permissions *permission.Manager,
) agent.Options {
	options := environment.options
	return agent.Options{
		Model: options.modelName, Instructions: environment.agentInstructions,
		Stream: options.stream, MaxTurns: options.maxTurns,
		ContextWindowTokens: options.contextWindowTokens, AutoCompactTokens: options.autoCompactTokens,
		UsableContextTokens: options.usableContextTokens, ToolOutputTokens: options.toolOutputTokens,
		Hook: stores.hooks.Run, FallbackModels: options.fallbackModels, RequestTimeout: options.timeout,
		Policy: policyStore, ReasoningEffort: options.reasoningEffort, ServiceTier: options.serviceTier,
		OnEvent: eventLogger, OnMemoryCitation: stores.memory.RecordUsage, Permissions: permissions,
		ApprovalCategories: options.approvalCategories, ModelCatalog: options.modelCatalog,
	}
}

func mcpConfigs(fileConfig config.File) (map[string]mcp.Config, []string) {
	configurations := make(map[string]mcp.Config)
	names := make([]string, 0, len(fileConfig.MCPServers))
	for name, server := range fileConfig.MCPServers {
		if server.Enabled != nil && !*server.Enabled {
			continue
		}
		configurations[name] = mcp.Config{
			Transport: server.Transport, Command: server.Command, Args: server.Args,
			Env: server.Env, URL: server.URL, Headers: server.Headers, OAuthTokenCommand: server.OAuthTokenCommand,
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return configurations, names
}
