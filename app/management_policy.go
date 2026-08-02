package app

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/policy"
	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
)

func runPolicyCommand(store *policy.Store, action string, values []string, output io.Writer) error {
	switch action {
	case "list":
		rules := store.List()
		if len(rules) == 0 {
			_, err := fmt.Fprintf(output, "No persistent policy rules.\nPolicy file: %s\n", store.Path())
			return err
		}
		for _, rule := range rules {
			detail := rule.Tool
			if len(rule.Argv) > 0 {
				detail += " " + strings.Join(rule.Argv, " ")
			}
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\n", rule.ID, rule.Kind, detail); err != nil {
				return err
			}
		}
		return nil
	case "check":
		command := strings.Join(values, " ")
		arguments, _ := json.Marshal(map[string]string{"cmd": command})
		rule, allowed := store.Allows(provider.ToolCall{Name: "exec_command", Arguments: string(arguments)})
		if !allowed {
			_, err := fmt.Fprintln(output, "approval required")
			return err
		}
		_, err := fmt.Fprintf(output, "allowed by %s\n", rule.ID)
		return err
	case "remove":
		if len(values) != 1 {
			return errors.New("policy remove requires one rule ID")
		}
		removed, err := store.Remove(values[0])
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("policy rule %q was not found", values[0])
		}
		_, err = fmt.Fprintln(output, "Removed "+values[0]+".")
		return err
	default:
		return fmt.Errorf("unknown policy action %q", action)
	}
}

func runDoctor(configPath, workspace string, fileConfig config.File, policyStore *policy.Store, output io.Writer, lookupEnv func(string) (string, bool)) error {
	type check struct{ name, status, detail string }
	checks := []check{{name: "Config", status: "OK", detail: configPath}, {name: "Workspace", status: "OK", detail: workspace}}
	if info, err := os.Stat(configPath); err != nil {
		checks[0] = check{name: "Config", status: "FAIL", detail: err.Error()}
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		checks[0] = check{name: "Config", status: "WARN", detail: fmt.Sprintf("permissions are %04o; expected 0600", info.Mode().Perm())}
	}
	if len(fileConfig.Providers) == 0 {
		authConfigured := strings.TrimSpace(fileConfig.Token) != "" || len(fileConfig.TokenCommand) > 0
		if token, ok := lookupEnv("OPENAI_API_KEY"); ok && strings.TrimSpace(token) != "" {
			authConfigured = true
		}
		if authConfigured {
			checks = append(checks, check{name: "Auth", status: "OK", detail: "API token source configured"})
		} else {
			checks = append(checks, check{name: "Auth", status: "WARN", detail: "OPENAI_API_KEY, token, or token_command is required before a model call"})
		}
	} else {
		for _, providerConfig := range fileConfig.Providers {
			environmentName := providerAPIKeyEnvironment(providerConfig)
			hasEnvironment := false
			if value, ok := lookupEnv(environmentName); ok && strings.TrimSpace(value) != "" {
				hasEnvironment = true
			}
			if explicit := providerTokenReference(providerConfig.Token); explicit != "" {
				environmentName = explicit
				value, ok := lookupEnv(explicit)
				hasEnvironment = ok && strings.TrimSpace(value) != ""
			}
			configured := hasEnvironment || strings.TrimSpace(providerConfig.Token) != "" && providerTokenReference(providerConfig.Token) == "" || len(providerConfig.TokenCommand) > 0
			status, source := "WARN", "no API key source configured"
			if configured {
				status, source = "OK", "API key source configured"
			} else if environmentName != "" {
				source = environmentName + " or provider token/token_command is required"
			}
			checks = append(checks, check{name: "Provider", status: status, detail: fmt.Sprintf("%s (%s, %d model(s)): %s", providerConfig.Name, providerConfig.Provider, len(providerConfig.Models), source)})
		}
	}
	sandboxOptions := tool.SandboxOptions{ReadRoots: fileConfig.ReadRoots, WriteRoots: fileConfig.WriteRoots, DenyRoots: fileConfig.DenyRoots}
	sandboxStatus := tool.SandboxStatusWithOptions(workspace, sandboxOptions)
	sandboxLevel := "OK"
	if strings.Contains(sandboxStatus, "approval-only") || sandboxStatus == "unavailable" {
		sandboxLevel = "WARN"
	}
	checks = append(checks, check{name: "Sandbox", status: sandboxLevel, detail: sandboxStatus})
	for _, executable := range []string{"git", "rg"} {
		path, err := exec.LookPath(executable)
		if err != nil {
			checks = append(checks, check{name: executable, status: "WARN", detail: "not found on PATH"})
		} else {
			checks = append(checks, check{name: executable, status: "OK", detail: path})
		}
	}
	clipboardDetail := "OSC 52 terminal fallback"
	for _, name := range []string{"wl-copy", "xclip", "xsel", "pbcopy", "clip.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			clipboardDetail = path
			break
		}
	}
	checks = append(checks, check{name: "Clipboard", status: "OK", detail: clipboardDetail})
	mcpCount, hookCount := 0, 0
	for _, server := range fileConfig.MCPServers {
		if server.Enabled == nil || *server.Enabled {
			mcpCount++
		}
	}
	for _, hooks := range fileConfig.Hooks {
		hookCount += len(hooks)
	}
	checks = append(checks,
		check{name: "MCP", status: "OK", detail: fmt.Sprintf("%d enabled server(s)", mcpCount)},
		check{name: "Hooks", status: "OK", detail: fmt.Sprintf("%d configured hook(s)", hookCount)},
		check{name: "Policy", status: "OK", detail: fmt.Sprintf("%d rule(s) in %s", len(policyStore.List()), policyStore.Path())},
	)
	for _, item := range checks {
		if _, err := fmt.Fprintf(output, "%-10s %-4s %s\n", item.name, item.status, item.detail); err != nil {
			return err
		}
	}
	return nil
}

func exportDiagnostics(path, configPath, workspace string, configuration config.File, policyStore *policy.Store, lookupEnv func(string) (string, bool), output io.Writer) error {
	var doctor bytes.Buffer
	if err := runDoctor(configPath, workspace, configuration, policyStore, &doctor, lookupEnv); err != nil {
		return err
	}
	if configuration.Token != "" {
		configuration.Token = "[redacted]"
	}
	configuration.TokenCommand = nil
	redactedProviders := make([]config.ProviderConfig, len(configuration.Providers))
	for index, providerConfig := range configuration.Providers {
		redactedProviders[index] = providerConfig
		if providerConfig.Token != "" {
			redactedProviders[index].Token = "[redacted]"
		}
		redactedProviders[index].TokenCommand = nil
		redactedProviders[index].URL = redactEndpoint(providerConfig.URL)
		redactedProviders[index].Headers = redactValues(providerConfig.Headers)
		redactedProviders[index].Models = append([]string(nil), providerConfig.Models...)
	}
	configuration.Providers = redactedProviders
	redactedServers := make(map[string]config.MCPServer, len(configuration.MCPServers))
	for name, server := range configuration.MCPServers {
		server.Env = redactValues(server.Env)
		server.Headers = redactValues(server.Headers)
		server.URL = redactEndpoint(server.URL)
		server.OAuthTokenCommand = nil
		redactedServers[name] = server
	}
	configuration.MCPServers = redactedServers
	redactedConfig, err := yaml.Marshal(configuration)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(file)
	add := func(name string, content []byte) error {
		entry, err := archive.Create(name)
		if err != nil {
			return err
		}
		_, err = entry.Write(content)
		return err
	}
	if err := add("doctor.txt", doctor.Bytes()); err != nil {
		archive.Close()
		file.Close()
		return err
	}
	if err := add("config.redacted.yaml", redactedConfig); err != nil {
		archive.Close()
		file.Close()
		return err
	}
	metadata := fmt.Sprintf("generated_at: %s\nworkspace: %s\ngo: %s\nos: %s/%s\n", time.Now().UTC().Format(time.RFC3339), workspace, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if err := add("metadata.txt", []byte(metadata)); err != nil {
		archive.Close()
		file.Close()
		return err
	}
	if err := archive.Close(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "Wrote redacted diagnostics to "+path+".")
	return err
}

func providerAPIKeyEnvironment(configuration config.ProviderConfig) string {
	switch strings.ToLower(strings.TrimSpace(configuration.Provider)) {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}

func providerTokenReference(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") && strings.Count(value, "${") == 1 {
		return strings.TrimSpace(value[2 : len(value)-1])
	}
	return ""
}

func redactValues(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	redacted := make(map[string]string, len(values))
	for key := range values {
		redacted[key] = "[redacted]"
	}
	return redacted
}

func redactEndpoint(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		if strings.TrimSpace(value) == "" {
			return ""
		}
		return "[redacted-url]"
	}
	// Endpoint paths can themselves contain bearer tokens or tenant secrets.
	// Diagnostics only need the transport and host to identify the server.
	return parsed.Scheme + "://" + parsed.Host + "/[redacted]"
}
