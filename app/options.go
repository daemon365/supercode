package app

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/daemon365/supercode/internal/agent"
	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/modelcatalog"
	"github.com/daemon365/supercode/internal/tool"
)

const defaultModel = config.DefaultModel

type options struct {
	modelName           string
	reasoningEffort     string
	serviceTier         string
	instructions        string
	baseURL             string
	imagePaths          []string
	jsonOutput          bool
	outputSchema        string
	outputLastMessage   string
	ephemeral           bool
	review              bool
	chat                bool
	stream              bool
	timeout             time.Duration
	initConfig          bool
	approval            agent.ApprovalMode
	approvalCategories  map[tool.Category]bool
	maxTurns            int
	contextWindowTokens int
	autoCompactTokens   int
	usableContextTokens int
	toolOutputTokens    int
	maxRetries          int
	workspace           string
	resume              string
	listSessions        bool
	trustProject        bool
	goalAutoContinue    bool
	alternateScreen     bool
	models              []string
	modelCatalog        *modelcatalog.Catalog
	fallbackModels      []string
	configDiagnostics   bool
	policyAction        string
	policyValues        []string
	mcpAction           string
	mcpValues           []string
	mcpServer           config.MCPServer
	skillAction         string
	skillValues         []string
	pluginAction        string
	pluginValues        []string
	hookAction          string
	hookValues          []string
	doctor              bool
	debugLog            string
	diagnosticExport    string
	helpShown           bool
}

func parseOptions(
	args []string,
	stderr io.Writer,
	lookupEnv func(string) (string, bool),
) (options, []string, error) {
	return parseOptionsWithConfig(args, stderr, lookupEnv, config.File{})
}

func parseOptionsWithConfig(
	args []string,
	stderr io.Writer,
	lookupEnv func(string) (string, bool),
	fileConfig config.File,
) (options, []string, error) {
	modelDefault := firstNonEmpty(fileConfig.Model, defaultModel)
	modelDefault = envOrDefault(lookupEnv, "OPENAI_MODEL", modelDefault)
	catalog := modelcatalog.New(append(append([]string{modelDefault}, fileConfig.Models...), fileConfig.FallbackModels...), fileConfig.ModelCatalog)
	baseURLDefault := firstNonEmpty(fileConfig.URL, config.DefaultURL)
	baseURLDefault = envOrDefault(lookupEnv, "OPENAI_BASE_URL", baseURLDefault)
	instructionsDefault := envOrDefault(lookupEnv, "SUPERCODE_INSTRUCTIONS", fileConfig.Instructions)
	approvalDefault := firstNonEmpty(fileConfig.Approval, string(agent.ApprovalOnRequest))
	maxTurnsDefault := fileConfig.MaxTurns
	catalogContext, catalogCompact, catalogUsable := catalog.Limits(modelDefault, agent.DefaultContextWindowTokens)
	contextWindowDefault := fileConfig.ContextWindowTokens
	if contextWindowDefault <= 0 {
		contextWindowDefault = catalogContext
	}
	autoCompactDefault := fileConfig.AutoCompactTokens
	if autoCompactDefault <= 0 {
		if fileConfig.ContextWindowTokens <= 0 {
			autoCompactDefault = catalogCompact
		} else {
			autoCompactDefault = contextWindowDefault * 90 / 100
		}
	}
	usableContextDefault := fileConfig.UsableContextTokens
	if usableContextDefault <= 0 {
		if fileConfig.ContextWindowTokens <= 0 {
			usableContextDefault = catalogUsable
		} else {
			usableContextDefault = contextWindowDefault * 95 / 100
		}
	}
	toolOutputDefault := fileConfig.ToolOutputTokens
	if toolOutputDefault <= 0 {
		toolOutputDefault = agent.DefaultToolOutputTokens
	}
	maxRetriesDefault := fileConfig.MaxRetries
	if maxRetriesDefault <= 0 {
		maxRetriesDefault = 2
	}
	goalAutoContinueDefault := true
	if fileConfig.GoalAutoContinue != nil {
		goalAutoContinueDefault = *fileConfig.GoalAutoContinue
	}
	alternateScreenDefault := true
	if fileConfig.AlternateScreen != nil {
		alternateScreenDefault = *fileConfig.AlternateScreen
	}

	streamDefault := true
	if fileConfig.Stream != nil {
		streamDefault = *fileConfig.Stream
	}
	if value, ok := lookupEnv("SUPERCODE_STREAM"); ok && strings.TrimSpace(value) != "" {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return options{}, nil, fmt.Errorf("parse SUPERCODE_STREAM: %w", err)
		}
		streamDefault = parsed
	}

	timeoutDefault := 10 * time.Minute
	if strings.TrimSpace(fileConfig.Timeout) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(fileConfig.Timeout))
		if err != nil {
			return options{}, nil, fmt.Errorf("parse config timeout: %w", err)
		}
		timeoutDefault = parsed
	}
	if value, ok := lookupEnv("SUPERCODE_TIMEOUT"); ok && strings.TrimSpace(value) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return options{}, nil, fmt.Errorf("parse SUPERCODE_TIMEOUT: %w", err)
		}
		timeoutDefault = parsed
	}

	var promptArgs []string
	executed := false
	policyAction := ""
	var policyValues []string
	reviewRequested := false
	diagnosticExport := ""
	mcpAction := ""
	var mcpValues []string
	skillAction, pluginAction, hookAction := "", "", ""
	var skillValues, pluginValues, hookValues []string
	command := &cobra.Command{
		Use:           "supercode [prompt...]",
		Short:         "An extensible coding agent for your terminal",
		Long:          "SuperCode is an extensible terminal coding agent with streaming, tools, sessions, skills, MCP, and multi-agent collaboration.",
		Example:       "  supercode\n  supercode chat\n  supercode \"Explain this repository\"\n  supercode sessions\n  supercode config diagnostics",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, values []string) error {
			executed = true
			promptArgs = append([]string(nil), values...)
			return nil
		},
	}
	command.SetOut(stderr)
	command.SetErr(stderr)
	flags := command.PersistentFlags()

	modelName := flags.StringP("model", "m", modelDefault, "OpenAI model ID")
	reasoningEffort := flags.String("reasoning-effort", fileConfig.ReasoningEffort, "reasoning effort: low, medium, high, or xhigh")
	serviceTier := flags.String("service-tier", fileConfig.ServiceTier, "service tier: auto, default, flex, scale, priority, or fast")
	instructions := flags.String("instructions", instructionsDefault, "system/developer instructions")
	baseURL := flags.String("base-url", baseURLDefault, "OpenAI API base URL")
	imagePaths := flags.StringSliceP("image", "i", nil, "attach an image path (repeatable)")
	jsonOutput := flags.Bool("json", false, "emit non-interactive events as JSONL")
	outputSchema := flags.String("output-schema", "", "validate the final response against a JSON Schema file")
	outputLastMessage := flags.String("output-last-message", "", "write the final assistant message to a file")
	ephemeral := flags.Bool("ephemeral", false, "do not save the non-interactive session")
	chat := flags.Bool("chat", false, "start an interactive multi-turn chat")
	stream := flags.Bool("stream", streamDefault, "stream response output")
	timeout := flags.Duration("timeout", timeoutDefault, "request timeout")
	initConfig := flags.Bool("init-config", false, "create the config file if missing and print its path")
	approval := flags.String("approval", approvalDefault, "tool approval policy: on-request, granular, always, or never")
	maxTurns := flags.Int("max-turns", maxTurnsDefault, "maximum model turns per task (0 means unlimited)")
	contextWindowTokens := flags.Int("context-window-tokens", contextWindowDefault, "estimated model context window")
	autoCompactTokens := flags.Int("auto-compact-tokens", autoCompactDefault, "estimated token threshold for history compaction")
	usableContextTokens := flags.Int("usable-context-tokens", usableContextDefault, "hard request-context limit after reserving output headroom")
	toolOutputTokens := flags.Int("tool-output-tokens", toolOutputDefault, "maximum estimated tokens retained from one tool result")
	maxRetries := flags.Int("max-retries", maxRetriesDefault, "maximum retries for transient model API failures")
	workspace := flags.StringP("workspace", "w", "", "workspace root (default: current directory)")
	resume := flags.String("resume", "", "resume a session ID, or latest")
	listSessions := flags.Bool("sessions", false, "list saved sessions for the workspace")
	trustProject := flags.Bool("trust-project", false, "trust and enable .supercode/config.yaml in this workspace")
	goalAutoContinue := flags.Bool("goal-auto-continue", goalAutoContinueDefault, "automatically continue an active explicit goal between turns")
	alternateScreen := flags.Bool("alt-screen", alternateScreenDefault, "use a full-screen terminal buffer")
	noAlternateScreen := flags.Bool("no-alt-screen", false, "preserve terminal scrollback instead of using a full-screen buffer")
	configDiagnostics := flags.Bool("config-diagnostics", false, "print configuration sources and trust status")
	doctor := flags.Bool("doctor", false, "check local client dependencies and security boundaries")
	debugLog := flags.String("debug-log", fileConfig.DebugLog, "write redacted local JSONL diagnostics to a file")

	command.AddCommand(&cobra.Command{
		Use:   "chat [prompt...]",
		Short: "Start an interactive multi-turn chat",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, values []string) error {
			executed = true
			*chat = true
			promptArgs = append([]string(nil), values...)
			return nil
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "review [focus...]",
		Short: "Run a non-interactive review of current Git changes",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, values []string) error {
			executed = true
			reviewRequested = true
			promptArgs = append([]string(nil), values...)
			return nil
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "sessions",
		Short: "List saved sessions for the workspace",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			executed = true
			*listSessions = true
			return nil
		},
	})
	configCommand := &cobra.Command{
		Use:   "config",
		Short: "Manage and inspect SuperCode configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	configCommand.AddCommand(
		&cobra.Command{
			Use:   "init",
			Short: "Create the user configuration and print its path",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				executed = true
				*initConfig = true
				return nil
			},
		},
		&cobra.Command{
			Use:   "diagnostics",
			Short: "Show configuration sources, precedence, and trust",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				executed = true
				*configDiagnostics = true
				return nil
			},
		},
	)
	command.AddCommand(configCommand)
	command.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Check terminal, auth, sandbox, clipboard, and local state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			executed = true
			*doctor = true
			return nil
		},
	})
	diagnosticsCommand := &cobra.Command{Use: "diagnostics", Short: "Export redacted local diagnostics", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	diagnosticsCommand.AddCommand(&cobra.Command{Use: "export <zip-path>", Short: "Write a redacted diagnostic archive", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, values []string) error {
		executed = true
		diagnosticExport = values[0]
		return nil
	}})
	command.AddCommand(diagnosticsCommand)
	policyCommand := &cobra.Command{
		Use:   "policy",
		Short: "Inspect persistent execution policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	policyCommand.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List persistent approval rules",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				executed = true
				policyAction = "list"
				return nil
			},
		},
		&cobra.Command{
			Use:   "check <command...>",
			Short: "Check whether a shell command matches persistent policy",
			Args:  cobra.MinimumNArgs(1),
			RunE: func(_ *cobra.Command, values []string) error {
				executed = true
				policyAction = "check"
				policyValues = append([]string(nil), values...)
				return nil
			},
		},
		&cobra.Command{
			Use:   "remove <rule-id>",
			Short: "Remove a persistent approval rule",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, values []string) error {
				executed = true
				policyAction = "remove"
				policyValues = append([]string(nil), values...)
				return nil
			},
		},
	)
	command.AddCommand(policyCommand)
	mcpCommand := &cobra.Command{Use: "mcp", Short: "Manage Model Context Protocol servers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	mcpSimple := func(use, short, action string, args cobra.PositionalArgs) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(_ *cobra.Command, values []string) error {
			executed = true
			mcpAction = action
			mcpValues = append([]string(nil), values...)
			return nil
		}}
	}
	mcpAdd := mcpSimple("add <name> [command-args...]", "Add or replace an MCP server", "add", cobra.MinimumNArgs(1))
	mcpURL := mcpAdd.Flags().String("url", "", "Streamable HTTP endpoint")
	mcpTransport := mcpAdd.Flags().String("transport", "", "transport: stdio or http")
	mcpCommandName := mcpAdd.Flags().String("command", "", "stdio executable (defaults to the first command argument)")
	mcpEnv := mcpAdd.Flags().StringSlice("env", nil, "environment KEY=VALUE (repeatable)")
	mcpHeaders := mcpAdd.Flags().StringSlice("header", nil, "HTTP header KEY=VALUE (repeatable)")
	mcpCommand.AddCommand(
		mcpSimple("list", "List configured MCP servers", "list", cobra.NoArgs),
		mcpSimple("get <name>", "Show one MCP server", "get", cobra.ExactArgs(1)),
		mcpAdd,
		mcpSimple("remove <name>", "Remove an MCP server", "remove", cobra.ExactArgs(1)),
		mcpSimple("status [name]", "Connect and report MCP status", "status", cobra.MaximumNArgs(1)),
		mcpSimple("reload", "Validate all enabled MCP servers", "reload", cobra.NoArgs),
		mcpSimple("login <name> <token-command...>", "Configure an OAuth token command", "login", cobra.MinimumNArgs(2)),
		mcpSimple("logout <name>", "Remove OAuth token configuration", "logout", cobra.ExactArgs(1)),
	)
	command.AddCommand(mcpCommand)
	managementCommand := func(name, short string, specs ...*cobra.Command) *cobra.Command {
		root := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
		root.AddCommand(specs...)
		return root
	}
	managementAction := func(use, short, name string, action *string, values *[]string, args cobra.PositionalArgs) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(_ *cobra.Command, input []string) error {
			executed = true
			*action = name
			*values = append([]string(nil), input...)
			return nil
		}}
	}
	command.AddCommand(managementCommand("skills", "Manage local skills",
		managementAction("list", "List installed skills", "list", &skillAction, &skillValues, cobra.NoArgs),
		managementAction("check", "Check skill dependencies", "check", &skillAction, &skillValues, cobra.NoArgs),
		managementAction("install <directory>", "Install a skill from a local directory", "install", &skillAction, &skillValues, cobra.ExactArgs(1)),
		managementAction("remove <name>", "Remove an installed user skill", "remove", &skillAction, &skillValues, cobra.ExactArgs(1)),
	))
	command.AddCommand(managementCommand("plugins", "Manage local plugins",
		managementAction("list", "List installed plugins", "list", &pluginAction, &pluginValues, cobra.NoArgs),
		managementAction("install <directory>", "Install a plugin from a local directory", "install", &pluginAction, &pluginValues, cobra.ExactArgs(1)),
		managementAction("enable <directory-name>", "Enable an installed plugin", "enable", &pluginAction, &pluginValues, cobra.ExactArgs(1)),
		managementAction("disable <directory-name>", "Disable an installed plugin", "disable", &pluginAction, &pluginValues, cobra.ExactArgs(1)),
		managementAction("remove <directory-name>", "Remove an installed plugin", "remove", &pluginAction, &pluginValues, cobra.ExactArgs(1)),
	))
	command.AddCommand(managementCommand("hooks", "Inspect and trust lifecycle hooks",
		managementAction("list", "List configured hooks", "list", &hookAction, &hookValues, cobra.NoArgs),
		managementAction("trust <event> <index>", "Record the hook executable SHA-256", "trust", &hookAction, &hookValues, cobra.ExactArgs(2)),
		managementAction("enable <event> <index>", "Enable a hook", "enable", &hookAction, &hookValues, cobra.ExactArgs(2)),
		managementAction("disable <event> <index>", "Disable a hook", "disable", &hookAction, &hookValues, cobra.ExactArgs(2)),
	))
	command.SetArgs(normalizeLegacyCLIArgs(args))
	if err := command.Execute(); err != nil {
		return options{}, nil, err
	}
	if !executed {
		return options{helpShown: true}, nil, nil
	}
	if flags.Changed("model") {
		modelContext, modelCompact, modelUsable := catalog.Limits(*modelName, agent.DefaultContextWindowTokens)
		if !flags.Changed("context-window-tokens") && fileConfig.ContextWindowTokens <= 0 {
			*contextWindowTokens = modelContext
		}
		if !flags.Changed("auto-compact-tokens") && fileConfig.AutoCompactTokens <= 0 {
			*autoCompactTokens = modelCompact
		}
		if !flags.Changed("usable-context-tokens") && fileConfig.UsableContextTokens <= 0 {
			*usableContextTokens = modelUsable
		}
	}
	if *timeout <= 0 {
		return options{}, nil, errors.New("timeout must be greater than zero")
	}
	approvalMode, err := agent.ParseApprovalMode(*approval)
	if err != nil {
		return options{}, nil, err
	}
	if *maxTurns < 0 {
		return options{}, nil, errors.New("max-turns must not be negative")
	}
	if *contextWindowTokens <= 0 || *autoCompactTokens <= 0 || *usableContextTokens <= 0 || *toolOutputTokens <= 0 {
		return options{}, nil, errors.New("context and tool token limits must be greater than zero")
	}
	if *usableContextTokens > *contextWindowTokens {
		return options{}, nil, errors.New("usable-context-tokens must not exceed context-window-tokens")
	}
	if *autoCompactTokens >= *usableContextTokens {
		return options{}, nil, errors.New("auto-compact-tokens must be smaller than usable-context-tokens")
	}
	if *maxRetries < 0 {
		return options{}, nil, errors.New("max-retries must not be negative")
	}
	if value := strings.TrimSpace(*reasoningEffort); value != "" && value != "low" && value != "medium" && value != "high" && value != "xhigh" {
		return options{}, nil, errors.New("reasoning-effort must be low, medium, high, or xhigh")
	}
	if value := strings.TrimSpace(*serviceTier); value != "" && value != "auto" && value != "default" && value != "flex" && value != "scale" && value != "priority" && value != "fast" {
		return options{}, nil, errors.New("service-tier must be auto, default, flex, scale, priority, or fast")
	}
	if err := catalog.Validate(*modelName, *reasoningEffort, *serviceTier); err != nil {
		return options{}, nil, err
	}
	parsedOptions := options{
		modelName:           *modelName,
		reasoningEffort:     strings.TrimSpace(*reasoningEffort),
		serviceTier:         strings.TrimSpace(*serviceTier),
		instructions:        *instructions,
		baseURL:             *baseURL,
		imagePaths:          append([]string(nil), (*imagePaths)...),
		jsonOutput:          *jsonOutput,
		outputSchema:        strings.TrimSpace(*outputSchema),
		outputLastMessage:   strings.TrimSpace(*outputLastMessage),
		ephemeral:           *ephemeral,
		review:              reviewRequested,
		chat:                *chat,
		stream:              *stream,
		timeout:             *timeout,
		initConfig:          *initConfig,
		approval:            approvalMode,
		approvalCategories:  approvalCategoryMap(fileConfig.ApprovalCategories),
		maxTurns:            *maxTurns,
		contextWindowTokens: *contextWindowTokens,
		autoCompactTokens:   *autoCompactTokens,
		usableContextTokens: *usableContextTokens,
		toolOutputTokens:    *toolOutputTokens,
		maxRetries:          *maxRetries,
		workspace:           strings.TrimSpace(*workspace),
		resume:              strings.TrimSpace(*resume),
		listSessions:        *listSessions,
		trustProject:        *trustProject,
		goalAutoContinue:    *goalAutoContinue,
		alternateScreen:     *alternateScreen && !*noAlternateScreen,
		models:              append([]string(nil), fileConfig.Models...),
		modelCatalog:        catalog,
		fallbackModels:      append([]string(nil), fileConfig.FallbackModels...),
		configDiagnostics:   *configDiagnostics,
		policyAction:        policyAction,
		policyValues:        policyValues,
		mcpAction:           mcpAction,
		mcpValues:           mcpValues,
		skillAction:         skillAction,
		skillValues:         skillValues,
		pluginAction:        pluginAction,
		pluginValues:        pluginValues,
		hookAction:          hookAction,
		hookValues:          hookValues,
		doctor:              *doctor,
		debugLog:            strings.TrimSpace(*debugLog),
		diagnosticExport:    diagnosticExport,
	}
	if mcpAction == "add" {
		server := config.MCPServer{Transport: strings.TrimSpace(*mcpTransport), URL: strings.TrimSpace(*mcpURL), Command: strings.TrimSpace(*mcpCommandName)}
		arguments := append([]string(nil), mcpValues[1:]...)
		if server.Command == "" && server.URL == "" && len(arguments) > 0 {
			server.Command, arguments = arguments[0], arguments[1:]
		}
		server.Args = arguments
		server.Env, err = keyValueMap(*mcpEnv)
		if err != nil {
			return options{}, nil, fmt.Errorf("MCP env: %w", err)
		}
		server.Headers, err = keyValueMap(*mcpHeaders)
		if err != nil {
			return options{}, nil, fmt.Errorf("MCP header: %w", err)
		}
		parsedOptions.mcpServer = server
	}
	return parsedOptions, promptArgs, nil
}

func approvalCategoryMap(values map[string]bool) map[tool.Category]bool {
	if len(values) == 0 {
		return nil
	}
	result := make(map[tool.Category]bool, len(values))
	for name, allowed := range values {
		result[tool.Category(strings.ToLower(strings.TrimSpace(name)))] = allowed
	}
	return result
}

func normalizeLegacyCLIArgs(args []string) []string {
	legacyLongFlags := map[string]struct{}{
		"approval": {}, "auto-compact-tokens": {}, "base-url": {}, "chat": {},
		"alt-screen": {}, "config-diagnostics": {}, "context-window-tokens": {}, "debug-log": {}, "doctor": {}, "goal-auto-continue": {},
		"ephemeral": {}, "image": {}, "init-config": {}, "instructions": {}, "json": {}, "max-retries": {}, "max-turns": {},
		"model": {}, "no-alt-screen": {}, "output-last-message": {}, "output-schema": {}, "reasoning-effort": {}, "resume": {}, "service-tier": {}, "sessions": {}, "stream": {}, "timeout": {},
		"tool-output-tokens": {}, "trust-project": {}, "usable-context-tokens": {}, "workspace": {},
	}
	normalized := append([]string(nil), args...)
	for index, value := range normalized {
		if !strings.HasPrefix(value, "-") || strings.HasPrefix(value, "--") || value == "-h" {
			continue
		}
		name := strings.TrimPrefix(value, "-")
		if separator := strings.IndexByte(name, '='); separator >= 0 {
			name = name[:separator]
		}
		if _, ok := legacyLongFlags[name]; ok {
			normalized[index] = "-" + value
		}
	}
	return normalized
}
