package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/daemon365/supercode/internal/config"
	"github.com/daemon365/supercode/internal/mcp"
)

func runMCPCommand(ctx context.Context, configPath, workspace string, configuration config.File, options options, output io.Writer) error {
	if configuration.MCPServers == nil {
		configuration.MCPServers = make(map[string]config.MCPServer)
	}
	switch options.mcpAction {
	case "list":
		names := make([]string, 0, len(configuration.MCPServers))
		for name := range configuration.MCPServers {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 0 {
			_, err := fmt.Fprintln(output, "No MCP servers configured.")
			return err
		}
		for _, name := range names {
			server := configuration.MCPServers[name]
			transport := firstNonEmpty(server.Transport, map[bool]string{true: "http", false: "stdio"}[server.URL != ""])
			target := server.Command
			if server.URL != "" {
				target = server.URL
			}
			status := "enabled"
			if server.Enabled != nil && !*server.Enabled {
				status = "disabled"
			}
			if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", name, transport, status, target); err != nil {
				return err
			}
		}
		return nil
	case "get":
		server, ok := configuration.MCPServers[options.mcpValues[0]]
		if !ok {
			return fmt.Errorf("MCP server %q was not found", options.mcpValues[0])
		}
		encoded, err := yaml.Marshal(map[string]config.MCPServer{options.mcpValues[0]: server})
		if err != nil {
			return err
		}
		_, err = output.Write(encoded)
		return err
	case "add":
		if len(options.mcpValues) == 0 {
			return errors.New("MCP server name is required")
		}
		server := options.mcpServer
		if server.URL == "" && server.Command == "" {
			return errors.New("MCP add requires --url or a stdio command")
		}
		if server.Transport == "" {
			server.Transport = map[bool]string{true: "http", false: "stdio"}[server.URL != ""]
		}
		configuration.MCPServers[options.mcpValues[0]] = server
		if err := config.Save(configPath, configuration); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "Saved MCP server "+options.mcpValues[0]+".")
		return err
	case "remove":
		name := options.mcpValues[0]
		if _, ok := configuration.MCPServers[name]; !ok {
			return fmt.Errorf("MCP server %q was not found", name)
		}
		delete(configuration.MCPServers, name)
		if err := config.Save(configPath, configuration); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "Removed MCP server "+name+".")
		return err
	case "login":
		name := options.mcpValues[0]
		server, ok := configuration.MCPServers[name]
		if !ok {
			return fmt.Errorf("MCP server %q was not found", name)
		}
		server.OAuthTokenCommand = append([]string(nil), options.mcpValues[1:]...)
		configuration.MCPServers[name] = server
		if err := config.Save(configPath, configuration); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "Configured OAuth token command for "+name+". SuperCode does not store the token itself.")
		return err
	case "logout":
		name := options.mcpValues[0]
		server, ok := configuration.MCPServers[name]
		if !ok {
			return fmt.Errorf("MCP server %q was not found", name)
		}
		server.OAuthTokenCommand = nil
		configuration.MCPServers[name] = server
		if err := config.Save(configPath, configuration); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "Removed OAuth token configuration for "+name+".")
		return err
	case "status", "reload":
		configurations := make(map[string]mcp.Config)
		for name, server := range configuration.MCPServers {
			if len(options.mcpValues) == 1 && name != options.mcpValues[0] {
				continue
			}
			if server.Enabled != nil && !*server.Enabled {
				continue
			}
			configurations[name] = mcp.Config{Transport: server.Transport, Command: server.Command, Args: server.Args, Env: server.Env, URL: server.URL, Headers: server.Headers, OAuthTokenCommand: server.OAuthTokenCommand}
		}
		if len(configurations) == 0 {
			return errors.New("no matching enabled MCP servers")
		}
		checkContext, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		manager, err := mcp.ConnectAll(checkContext, workspace, configurations)
		if err != nil {
			return err
		}
		defer manager.Close()
		if _, err = fmt.Fprintf(output, "Connected %d of %d server(s); discovered %d tool/resource/prompt entries.\n", len(manager.Names()), len(configurations), len(manager.Tools())); err != nil {
			return err
		}
		for _, failure := range manager.Failures() {
			if _, err = fmt.Fprintln(output, "Failed:", failure); err != nil {
				return err
			}
		}
		return errors.Join(manager.Failures()...)
	default:
		return fmt.Errorf("unknown MCP action %q", options.mcpAction)
	}
}

func keyValueMap(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, item, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("expected KEY=VALUE, got %q", value)
		}
		result[strings.TrimSpace(key)] = item
	}
	return result, nil
}
