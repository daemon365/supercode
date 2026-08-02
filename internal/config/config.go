package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/daemon365/supercode/internal/modelcatalog"
	"gopkg.in/yaml.v3"
)

const (
	DefaultURL   = "https://api.openai.com/v1"
	DefaultModel = "gpt-5.6"
)

const defaultContents = `# SuperCode configuration
# Environment variables and command-line flags override these values.
config_version: 1
url: https://api.openai.com/v1
model: gpt-5.6
# models: [gpt-5.6]
# Multiple endpoints can be configured instead of url/token/models:
# providers:
#   - name: openai
#     provider: openai
#     url: https://api.openai.com/v1
#     token: ${OPENAI_API_KEY}
#     models: [gpt-5.6]
#   - name: anthropic
#     provider: anthropic
#     url: https://api.anthropic.com
#     token: ${ANTHROPIC_API_KEY}
#     models: [claude-sonnet-4-6]
# model_catalog:
#   gpt-5.6:
#     context_window_tokens: 272000
#     input_modalities: [text, image]
#     tool_calling: true
#     parallel_tool_calls: true
# reasoning_effort: ""
# service_tier: ""
# fallback_models: []
token: ""
# token_command: ["secret-tool", "lookup", "service", "supercode"]
stream: true
timeout: 10m
max_retries: 2
approval: on-request
# approval_categories: {shell: true, network: true, mcp: true, permission: true}
max_turns: 0 # unlimited; cancellation, context, timeout, and budgets still apply
context_window_tokens: 272000 # nominal model context window
auto_compact_tokens: 244800 # 90%; compact history before reaching the limit
usable_context_tokens: 258400 # 95%; reserve 5% for instructions, tools, and output
tool_output_tokens: 12000
goal_auto_continue: true
alternate_screen: true # isolated TUI page; mouse selection remains terminal-native
# instructions: ""
# project_doc_fallback_filenames: ["PROJECT.md"]
# project_doc_max_bytes: 65536
# read_roots: []
# write_roots: []
# deny_roots: []
# memory_max_tokens: 2500
# memory_auto_capture: false
# memory_generate: false # asynchronous Phase 1/2 model jobs; disabled by default to avoid surprise API cost
# memory_use: true
# memory_dedicated_tools: true
# memory_max_rollouts_per_startup: 2
# memory_max_rollout_age_days: 10
# memory_min_rollout_idle_hours: 6
# memory_max_raw_memories_for_consolidation: 256
# memory_max_unused_days: 30
# memory_extract_model: "" # empty uses the active model
# memory_consolidation_model: "" # empty uses the active model
# theme: violet
# keymap: standard
# notification: bell
# terminal_title: SuperCode
# debug_log: ""
# trusted_workspaces: ["/absolute/path/to/project"]
# mcp_servers:
#   example:
#     transport: stdio
#     command: example-mcp-server
# hooks:
#   pre_tool_use:
#     - command: ["./scripts/check-tool.sh"]
`

type MCPServer struct {
	Transport         string            `yaml:"transport,omitempty"`
	Command           string            `yaml:"command,omitempty"`
	Args              []string          `yaml:"args,omitempty"`
	Env               map[string]string `yaml:"env,omitempty"`
	URL               string            `yaml:"url,omitempty"`
	Headers           map[string]string `yaml:"headers,omitempty"`
	OAuthTokenCommand []string          `yaml:"oauth_token_command,omitempty"`
	Enabled           *bool             `yaml:"enabled,omitempty"`
}

type Hook struct {
	Command []string `yaml:"command"`
	Timeout string   `yaml:"timeout,omitempty"`
	Matcher string   `yaml:"matcher,omitempty"`
	Enabled *bool    `yaml:"enabled,omitempty"`
	SHA256  string   `yaml:"sha256,omitempty"`
}

// ProviderConfig describes one named model endpoint. Provider selects the wire
// protocol: openai, openai_responses, anthropic, or openrouter.
type ProviderConfig struct {
	Name         string            `yaml:"name"`
	Provider     string            `yaml:"provider"`
	URL          string            `yaml:"url,omitempty"`
	Token        string            `yaml:"token,omitempty"`
	TokenCommand []string          `yaml:"token_command,omitempty"`
	Models       []string          `yaml:"models"`
	MaxTokens    int64             `yaml:"maxTokens,omitempty"`
	Headers      map[string]string `yaml:"headers,omitempty"`
}

// File is the user-editable YAML configuration stored in ~/.supercode.
// Stream is a pointer so an omitted value can be distinguished from false.
type File struct {
	ConfigVersion                int                                  `yaml:"config_version,omitempty"`
	URL                          string                               `yaml:"url"`
	Model                        string                               `yaml:"model"`
	Models                       []string                             `yaml:"models,omitempty"`
	Providers                    []ProviderConfig                     `yaml:"providers,omitempty"`
	ModelCatalog                 map[string]modelcatalog.Capabilities `yaml:"model_catalog,omitempty"`
	ReasoningEffort              string                               `yaml:"reasoning_effort,omitempty"`
	ServiceTier                  string                               `yaml:"service_tier,omitempty"`
	FallbackModels               []string                             `yaml:"fallback_models,omitempty"`
	Token                        string                               `yaml:"token"`
	TokenCommand                 []string                             `yaml:"token_command,omitempty"`
	Instructions                 string                               `yaml:"instructions,omitempty"`
	ProjectDocFallbacks          []string                             `yaml:"project_doc_fallback_filenames,omitempty"`
	ProjectDocMaxBytes           int                                  `yaml:"project_doc_max_bytes,omitempty"`
	ReadRoots                    []string                             `yaml:"read_roots,omitempty"`
	WriteRoots                   []string                             `yaml:"write_roots,omitempty"`
	DenyRoots                    []string                             `yaml:"deny_roots,omitempty"`
	MemoryMaxTokens              int                                  `yaml:"memory_max_tokens,omitempty"`
	MemoryAutoCapture            *bool                                `yaml:"memory_auto_capture,omitempty"`
	MemoryGenerate               *bool                                `yaml:"memory_generate,omitempty"`
	MemoryUse                    *bool                                `yaml:"memory_use,omitempty"`
	MemoryDedicatedTools         *bool                                `yaml:"memory_dedicated_tools,omitempty"`
	MemoryMaxRolloutsPerStartup  int                                  `yaml:"memory_max_rollouts_per_startup,omitempty"`
	MemoryMaxRolloutAgeDays      int                                  `yaml:"memory_max_rollout_age_days,omitempty"`
	MemoryMinRolloutIdleHours    int                                  `yaml:"memory_min_rollout_idle_hours,omitempty"`
	MemoryMaxRawForConsolidation int                                  `yaml:"memory_max_raw_memories_for_consolidation,omitempty"`
	MemoryMaxUnusedDays          int                                  `yaml:"memory_max_unused_days,omitempty"`
	MemoryExtractModel           string                               `yaml:"memory_extract_model,omitempty"`
	MemoryConsolidationModel     string                               `yaml:"memory_consolidation_model,omitempty"`
	Theme                        string                               `yaml:"theme,omitempty"`
	Keymap                       string                               `yaml:"keymap,omitempty"`
	Notification                 string                               `yaml:"notification,omitempty"`
	TerminalTitle                string                               `yaml:"terminal_title,omitempty"`
	DebugLog                     string                               `yaml:"debug_log,omitempty"`
	Stream                       *bool                                `yaml:"stream,omitempty"`
	Timeout                      string                               `yaml:"timeout,omitempty"`
	MaxRetries                   int                                  `yaml:"max_retries,omitempty"`
	Approval                     string                               `yaml:"approval,omitempty"`
	ApprovalCategories           map[string]bool                      `yaml:"approval_categories,omitempty"`
	MaxTurns                     int                                  `yaml:"max_turns,omitempty"`
	ContextWindowTokens          int                                  `yaml:"context_window_tokens,omitempty"`
	AutoCompactTokens            int                                  `yaml:"auto_compact_tokens,omitempty"`
	UsableContextTokens          int                                  `yaml:"usable_context_tokens,omitempty"`
	ToolOutputTokens             int                                  `yaml:"tool_output_tokens,omitempty"`
	GoalAutoContinue             *bool                                `yaml:"goal_auto_continue,omitempty"`
	AlternateScreen              *bool                                `yaml:"alternate_screen,omitempty"`
	TrustedWorkspaces            []string                             `yaml:"trusted_workspaces,omitempty"`
	MCPServers                   map[string]MCPServer                 `yaml:"mcp_servers,omitempty"`
	Hooks                        map[string][]Hook                    `yaml:"hooks,omitempty"`
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".supercode", "config.yaml"), nil
}

// Path returns SUPERCODE_CONFIG when set, otherwise ~/.supercode/config.yaml.
func Path(lookupEnv func(string) (string, bool)) (string, error) {
	if value, ok := lookupEnv("SUPERCODE_CONFIG"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value), nil
	}
	return DefaultPath()
}

// Ensure creates a secure configuration directory and starter file. Existing
// configuration content is never overwritten. It returns true when a new file
// was created.
func Ensure(path string) (created bool, returnErr error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("config path is required")
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return false, fmt.Errorf("secure config directory: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false, fmt.Errorf("create config file: %w", err)
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return false, fmt.Errorf("inspect config file: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("config file must not be a symbolic link")
		}
		if !info.Mode().IsRegular() {
			return false, errors.New("config path is not a regular file")
		}
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return false, fmt.Errorf("secure config file: %w", chmodErr)
		}
		return false, nil
	}

	removeOnError := true
	defer func() {
		closeErr := file.Close()
		if returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close config file: %w", closeErr)
		}
		if returnErr == nil {
			removeOnError = false
		}
		if removeOnError {
			_ = os.Remove(path)
		}
	}()

	if _, err := io.WriteString(file, defaultContents); err != nil {
		return false, fmt.Errorf("write config file: %w", err)
	}
	return true, nil
}

func Load(path string) (File, error) {
	file, err := os.Open(path)
	if err != nil {
		return File{}, fmt.Errorf("open config file: %w", err)
	}
	defer file.Close()

	var result File
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&result); err != nil {
		return File{}, fmt.Errorf("decode config file: %w", err)
	}
	if result.ConfigVersion < 0 || result.ConfigVersion > 1 {
		return File{}, fmt.Errorf("unsupported config_version %d (maximum supported is 1)", result.ConfigVersion)
	}
	for name, capabilities := range result.ModelCatalog {
		if err := modelcatalog.ValidateCapabilities(capabilities); err != nil {
			return File{}, fmt.Errorf("model_catalog.%s: %w", name, err)
		}
	}
	if err := validateProviders(result.Providers); err != nil {
		return File{}, err
	}
	for category := range result.ApprovalCategories {
		switch strings.ToLower(strings.TrimSpace(category)) {
		case "tool", "file", "shell", "network", "mcp", "skill", "permission":
		default:
			return File{}, fmt.Errorf("approval_categories: unsupported category %q", category)
		}
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return File{}, errors.New("decode config file: multiple YAML documents are not supported")
		}
		return File{}, fmt.Errorf("decode config file: %w", err)
	}
	return result, nil
}

func ProjectPath(workspace string) string {
	return filepath.Join(workspace, ".supercode", "config.yaml")
}

// IsWorkspaceTrusted compares canonical paths so symlink aliases cannot grant
// trust to a different directory.
func IsWorkspaceTrusted(configuration File, workspace string) bool {
	wanted := canonicalPath(workspace)
	for _, candidate := range configuration.TrustedWorkspaces {
		if canonicalPath(candidate) == wanted {
			return true
		}
	}
	return false
}

func canonicalPath(value string) string {
	absolute, err := filepath.Abs(value)
	if err != nil {
		absolute = value
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute)
}

// Merge overlays explicitly configured project values. TrustedWorkspaces stays
// user-owned and is never inherited from a repository.
func Merge(base, overlay File) File {
	result := base
	if overlay.URL != "" {
		result.URL = overlay.URL
	}
	if overlay.Model != "" {
		result.Model = overlay.Model
	}
	if len(overlay.Models) > 0 {
		result.Models = append([]string(nil), overlay.Models...)
	}
	if len(overlay.Providers) > 0 {
		result.Providers = cloneProviders(overlay.Providers)
	}
	if len(overlay.ModelCatalog) > 0 {
		result.ModelCatalog = make(map[string]modelcatalog.Capabilities, len(base.ModelCatalog)+len(overlay.ModelCatalog))
		for name, capabilities := range base.ModelCatalog {
			result.ModelCatalog[name] = capabilities
		}
		for name, capabilities := range overlay.ModelCatalog {
			result.ModelCatalog[name] = capabilities
		}
	}
	if overlay.ReasoningEffort != "" {
		result.ReasoningEffort = overlay.ReasoningEffort
	}
	if overlay.ServiceTier != "" {
		result.ServiceTier = overlay.ServiceTier
	}
	if len(overlay.FallbackModels) > 0 {
		result.FallbackModels = append([]string(nil), overlay.FallbackModels...)
	}
	if overlay.Token != "" {
		result.Token = overlay.Token
	}
	if len(overlay.TokenCommand) > 0 {
		result.TokenCommand = append([]string(nil), overlay.TokenCommand...)
	}
	if overlay.Instructions != "" {
		result.Instructions = strings.TrimSpace(strings.Join([]string{base.Instructions, overlay.Instructions}, "\n\n"))
	}
	if len(overlay.ProjectDocFallbacks) > 0 {
		result.ProjectDocFallbacks = append([]string(nil), overlay.ProjectDocFallbacks...)
	}
	if overlay.ProjectDocMaxBytes != 0 {
		result.ProjectDocMaxBytes = overlay.ProjectDocMaxBytes
	}
	if len(overlay.ReadRoots) > 0 {
		result.ReadRoots = append([]string(nil), overlay.ReadRoots...)
	}
	if len(overlay.WriteRoots) > 0 {
		result.WriteRoots = append([]string(nil), overlay.WriteRoots...)
	}
	if len(overlay.DenyRoots) > 0 {
		result.DenyRoots = append([]string(nil), overlay.DenyRoots...)
	}
	if overlay.MemoryMaxTokens != 0 {
		result.MemoryMaxTokens = overlay.MemoryMaxTokens
	}
	if overlay.MemoryAutoCapture != nil {
		value := *overlay.MemoryAutoCapture
		result.MemoryAutoCapture = &value
	}
	if overlay.MemoryGenerate != nil {
		value := *overlay.MemoryGenerate
		result.MemoryGenerate = &value
	}
	if overlay.MemoryUse != nil {
		value := *overlay.MemoryUse
		result.MemoryUse = &value
	}
	if overlay.MemoryDedicatedTools != nil {
		value := *overlay.MemoryDedicatedTools
		result.MemoryDedicatedTools = &value
	}
	if overlay.MemoryMaxRolloutsPerStartup != 0 {
		result.MemoryMaxRolloutsPerStartup = overlay.MemoryMaxRolloutsPerStartup
	}
	if overlay.MemoryMaxRolloutAgeDays != 0 {
		result.MemoryMaxRolloutAgeDays = overlay.MemoryMaxRolloutAgeDays
	}
	if overlay.MemoryMinRolloutIdleHours != 0 {
		result.MemoryMinRolloutIdleHours = overlay.MemoryMinRolloutIdleHours
	}
	if overlay.MemoryMaxRawForConsolidation != 0 {
		result.MemoryMaxRawForConsolidation = overlay.MemoryMaxRawForConsolidation
	}
	if overlay.MemoryMaxUnusedDays != 0 {
		result.MemoryMaxUnusedDays = overlay.MemoryMaxUnusedDays
	}
	if overlay.MemoryExtractModel != "" {
		result.MemoryExtractModel = overlay.MemoryExtractModel
	}
	if overlay.MemoryConsolidationModel != "" {
		result.MemoryConsolidationModel = overlay.MemoryConsolidationModel
	}
	if overlay.Theme != "" {
		result.Theme = overlay.Theme
	}
	if overlay.Keymap != "" {
		result.Keymap = overlay.Keymap
	}
	if overlay.Notification != "" {
		result.Notification = overlay.Notification
	}
	if overlay.TerminalTitle != "" {
		result.TerminalTitle = overlay.TerminalTitle
	}
	if overlay.DebugLog != "" {
		result.DebugLog = overlay.DebugLog
	}
	if overlay.Stream != nil {
		value := *overlay.Stream
		result.Stream = &value
	}
	if overlay.Timeout != "" {
		result.Timeout = overlay.Timeout
	}
	if overlay.MaxRetries != 0 {
		result.MaxRetries = overlay.MaxRetries
	}
	if overlay.Approval != "" {
		result.Approval = overlay.Approval
	}
	if len(overlay.ApprovalCategories) > 0 {
		result.ApprovalCategories = make(map[string]bool, len(base.ApprovalCategories)+len(overlay.ApprovalCategories))
		for category, allowed := range base.ApprovalCategories {
			result.ApprovalCategories[category] = allowed
		}
		for category, allowed := range overlay.ApprovalCategories {
			result.ApprovalCategories[category] = allowed
		}
	}
	if overlay.MaxTurns != 0 {
		result.MaxTurns = overlay.MaxTurns
	}
	if overlay.ContextWindowTokens != 0 {
		result.ContextWindowTokens = overlay.ContextWindowTokens
	}
	if overlay.AutoCompactTokens != 0 {
		result.AutoCompactTokens = overlay.AutoCompactTokens
	}
	if overlay.UsableContextTokens != 0 {
		result.UsableContextTokens = overlay.UsableContextTokens
	}
	if overlay.ToolOutputTokens != 0 {
		result.ToolOutputTokens = overlay.ToolOutputTokens
	}
	if overlay.GoalAutoContinue != nil {
		value := *overlay.GoalAutoContinue
		result.GoalAutoContinue = &value
	}
	if overlay.AlternateScreen != nil {
		value := *overlay.AlternateScreen
		result.AlternateScreen = &value
	}
	if len(overlay.MCPServers) > 0 {
		result.MCPServers = make(map[string]MCPServer, len(base.MCPServers)+len(overlay.MCPServers))
		for name, server := range base.MCPServers {
			result.MCPServers[name] = server
		}
		for name, server := range overlay.MCPServers {
			result.MCPServers[name] = server
		}
	}
	if len(overlay.Hooks) > 0 {
		result.Hooks = make(map[string][]Hook, len(base.Hooks)+len(overlay.Hooks))
		for event, hooks := range base.Hooks {
			result.Hooks[event] = append([]Hook(nil), hooks...)
		}
		for event, hooks := range overlay.Hooks {
			result.Hooks[event] = append(result.Hooks[event], hooks...)
		}
	}
	return result
}

func validateProviders(values []ProviderConfig) error {
	seen := make(map[string]bool, len(values))
	for index, value := range values {
		name := strings.TrimSpace(value.Name)
		if name == "" {
			return fmt.Errorf("providers[%d].name is required", index)
		}
		if strings.Contains(name, "/") {
			return fmt.Errorf("providers[%d].name must not contain '/'", index)
		}
		if seen[name] {
			return fmt.Errorf("providers: duplicate name %q", name)
		}
		seen[name] = true
		switch strings.ToLower(strings.TrimSpace(value.Provider)) {
		case "openai", "openai_responses", "anthropic", "openrouter":
		default:
			return fmt.Errorf("providers[%d].provider %q is not supported", index, value.Provider)
		}
		if len(value.Models) == 0 {
			return fmt.Errorf("providers[%d].models must not be empty", index)
		}
		if value.MaxTokens < 0 {
			return fmt.Errorf("providers[%d].maxTokens must not be negative", index)
		}
		models := make(map[string]bool, len(value.Models))
		for _, model := range value.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				return fmt.Errorf("providers[%d].models contains an empty model", index)
			}
			if models[model] {
				return fmt.Errorf("providers[%d].models contains duplicate %q", index, model)
			}
			models[model] = true
		}
	}
	return nil
}

func cloneProviders(values []ProviderConfig) []ProviderConfig {
	result := make([]ProviderConfig, len(values))
	for index, value := range values {
		result[index] = value
		result[index].Models = append([]string(nil), value.Models...)
		result[index].TokenCommand = append([]string(nil), value.TokenCommand...)
		if value.Headers != nil {
			result[index].Headers = make(map[string]string, len(value.Headers))
			for name, header := range value.Headers {
				result[index].Headers[name] = header
			}
		}
	}
	return result
}

// TrustWorkspace atomically records an explicit trust decision in the user
// configuration. This intentionally happens only through --trust-project.
func TrustWorkspace(path, workspace string) error {
	configuration, err := Load(path)
	if err != nil {
		return err
	}
	absolute := canonicalPath(workspace)
	if IsWorkspaceTrusted(configuration, absolute) {
		return nil
	}
	configuration.TrustedWorkspaces = append(configuration.TrustedWorkspaces, absolute)
	return Save(path, configuration)
}

// Save atomically replaces a user-owned configuration file.
func Save(path string, configuration File) error {
	encoded, err := yaml.Marshal(configuration)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
