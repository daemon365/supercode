// Package mcp connects trusted Model Context Protocol servers and exposes their
// tools through SuperCode's provider-neutral registry.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/daemon365/supercode/internal/provider"
	"github.com/daemon365/supercode/internal/tool"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Config struct {
	Transport         string
	Command           string
	Args              []string
	Env               map[string]string
	URL               string
	Headers           map[string]string
	OAuthTokenCommand []string
}

type Manager struct {
	sessions []*sdkmcp.ClientSession
	tools    []tool.Tool
}

func ConnectAll(ctx context.Context, workspace string, configurations map[string]Config) (*Manager, error) {
	manager := &Manager{}
	names := make([]string, 0, len(configurations))
	for name := range configurations {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		session, err := connect(ctx, workspace, configurations[name])
		if err != nil {
			_ = manager.Close()
			return nil, fmt.Errorf("connect MCP server %q: %w", name, err)
		}
		manager.sessions = append(manager.sessions, session)
		for remote, err := range session.Tools(ctx, nil) {
			if err != nil {
				_ = manager.Close()
				return nil, fmt.Errorf("list MCP tools from %q: %w", name, err)
			}
			manager.tools = append(manager.tools, &remoteTool{server: name, session: session, remote: remote})
		}
		initialized := session.InitializeResult()
		if initialized == nil || initialized.Capabilities == nil {
			continue
		}
		if initialized.Capabilities.Resources != nil {
			manager.tools = append(manager.tools, resourceTools(name, session)...)
		}
		if initialized.Capabilities.Prompts != nil {
			manager.tools = append(manager.tools, promptTools(name, session)...)
		}
	}
	return manager, nil
}

func (m *Manager) Tools() []tool.Tool {
	if m == nil {
		return nil
	}
	return append([]tool.Tool(nil), m.tools...)
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var failures []error
	for _, session := range m.sessions {
		if err := session.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func connect(ctx context.Context, workspace string, configuration Config) (*sdkmcp.ClientSession, error) {
	transportName := strings.ToLower(strings.TrimSpace(configuration.Transport))
	if transportName == "" {
		if strings.TrimSpace(configuration.URL) != "" {
			transportName = "http"
		} else {
			transportName = "stdio"
		}
	}
	var transport sdkmcp.Transport
	switch transportName {
	case "stdio":
		if strings.TrimSpace(configuration.Command) == "" {
			return nil, errors.New("stdio MCP server command is required")
		}
		command := exec.CommandContext(ctx, configuration.Command, configuration.Args...)
		command.Dir = workspace
		command.Env = os.Environ()
		keys := make([]string, 0, len(configuration.Env))
		for key := range configuration.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			command.Env = append(command.Env, key+"="+os.ExpandEnv(configuration.Env[key]))
		}
		command.Stderr = io.Discard
		transport = &sdkmcp.CommandTransport{Command: command}
	case "http", "streamable-http":
		if strings.TrimSpace(configuration.URL) == "" {
			return nil, errors.New("HTTP MCP server URL is required")
		}
		headers := make(map[string]string, len(configuration.Headers)+1)
		for key, value := range configuration.Headers {
			headers[key] = os.ExpandEnv(value)
		}
		if len(configuration.OAuthTokenCommand) > 0 {
			token, err := credentialCommand(ctx, configuration.OAuthTokenCommand)
			if err != nil {
				return nil, fmt.Errorf("MCP OAuth token command: %w", err)
			}
			if _, exists := headers["Authorization"]; !exists {
				headers["Authorization"] = "Bearer " + token
			}
		}
		transport = &sdkmcp.StreamableClientTransport{
			Endpoint:             configuration.URL,
			HTTPClient:           &http.Client{Transport: &headerTransport{base: http.DefaultTransport, headers: headers}},
			DisableStandaloneSSE: true,
		}
	default:
		return nil, fmt.Errorf("unsupported transport %q", transportName)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "supercode", Title: "SuperCode", Version: "0.1.0"}, nil)
	return client.Connect(ctx, transport, nil)
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for key, value := range t.headers {
		clone.Header.Set(key, value)
	}
	return t.base.RoundTrip(clone)
}

type remoteTool struct {
	server  string
	session *sdkmcp.ClientSession
	remote  *sdkmcp.Tool
}

func (*remoteTool) Category() tool.Category { return tool.CategoryMCP }

func (t *remoteTool) Definition() provider.ToolDefinition {
	schema, err := json.Marshal(t.remote.InputSchema)
	if err != nil || len(schema) == 0 || bytes.Equal(schema, []byte("null")) {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return provider.ToolDefinition{
		Name:        mcpName(t.server, t.remote.Name),
		Description: "MCP " + t.server + ": " + strings.TrimSpace(t.remote.Description),
		Parameters:  schema,
	}
}

func (t *remoteTool) Risk(string) tool.Risk {
	if t.remote.Annotations != nil && t.remote.Annotations.ReadOnlyHint {
		return tool.RiskRead
	}
	return tool.RiskExecute
}

func (t *remoteTool) Summary(arguments string) string {
	return "call MCP " + t.server + "/" + t.remote.Name + " " + compact(arguments, 180)
}

func (t *remoteTool) Execute(ctx context.Context, arguments string) (tool.Result, error) {
	var values any = map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &values); err != nil {
			return tool.Result{}, fmt.Errorf("decode MCP arguments: %w", err)
		}
	}
	response, err := t.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: t.remote.Name, Arguments: values})
	if err != nil {
		return tool.Result{}, err
	}
	return mapContent(response.Content, response.StructuredContent, response.IsError)
}

func mapContent(items []sdkmcp.Content, structured any, isError bool) (tool.Result, error) {
	var text []string
	var images []provider.Image
	for _, content := range items {
		switch item := content.(type) {
		case *sdkmcp.TextContent:
			text = append(text, item.Text)
		case *sdkmcp.ImageContent:
			images = append(images, provider.Image{Data: base64.StdEncoding.EncodeToString(item.Data), MIMEType: item.MIMEType, Detail: "high"})
		case *sdkmcp.EmbeddedResource:
			if item.Resource != nil {
				text = append(text, resourceContentText(item.Resource))
			}
		case *sdkmcp.ResourceLink:
			text = append(text, strings.TrimSpace(item.Name+"\n"+item.URI+"\n"+item.Description))
		default:
			if encoded, err := content.MarshalJSON(); err == nil {
				text = append(text, string(encoded))
			}
		}
	}
	if structured != nil && len(text) == 0 {
		text = append(text, prettyJSON(structured))
	}
	return tool.Result{Content: strings.Join(text, "\n\n"), Images: images, IsError: isError}, nil
}

func resourceContentText(content *sdkmcp.ResourceContents) string {
	value := content.Text
	if value == "" && len(content.Blob) > 0 {
		value = base64.StdEncoding.EncodeToString(content.Blob)
	}
	return strings.TrimSpace(content.URI + "\n" + value)
}

type methodTool struct {
	definition provider.ToolDefinition
	method     string
	risk       tool.Risk
	execute    func(context.Context, string) (tool.Result, error)
}

func (*methodTool) Category() tool.Category { return tool.CategoryMCP }

func (t *methodTool) Definition() provider.ToolDefinition { return t.definition }
func (t *methodTool) Risk(string) tool.Risk               { return t.risk }
func (t *methodTool) Summary(arguments string) string {
	return t.method + " " + compact(arguments, 180)
}
func (t *methodTool) Execute(ctx context.Context, arguments string) (tool.Result, error) {
	return t.execute(ctx, arguments)
}

func resourceTools(server string, session *sdkmcp.ClientSession) []tool.Tool {
	return []tool.Tool{
		&methodTool{
			method: "resources/list", risk: tool.RiskRead,
			definition: provider.ToolDefinition{Name: mcpName(server, "resources_list"), Description: "List resources from MCP server " + server, Parameters: json.RawMessage(`{"type":"object","properties":{"cursor":{"type":"string"}},"additionalProperties":false}`)},
			execute: func(ctx context.Context, arguments string) (tool.Result, error) {
				var input struct {
					Cursor string `json:"cursor"`
				}
				if err := decodeArguments(arguments, &input); err != nil {
					return tool.Result{}, err
				}
				result, err := session.ListResources(ctx, &sdkmcp.ListResourcesParams{Cursor: input.Cursor})
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{Content: prettyJSON(result)}, nil
			},
		},
		&methodTool{
			method: "resources/read", risk: tool.RiskRead,
			definition: provider.ToolDefinition{Name: mcpName(server, "resources_read"), Description: "Read a resource from MCP server " + server, Parameters: json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string"}},"required":["uri"],"additionalProperties":false}`)},
			execute: func(ctx context.Context, arguments string) (tool.Result, error) {
				var input struct {
					URI string `json:"uri"`
				}
				if err := decodeArguments(arguments, &input); err != nil {
					return tool.Result{}, err
				}
				result, err := session.ReadResource(ctx, &sdkmcp.ReadResourceParams{URI: input.URI})
				if err != nil {
					return tool.Result{}, err
				}
				values := make([]string, 0, len(result.Contents))
				for _, content := range result.Contents {
					values = append(values, resourceContentText(content))
				}
				return tool.Result{Content: strings.Join(values, "\n\n")}, nil
			},
		},
	}
}

func promptTools(server string, session *sdkmcp.ClientSession) []tool.Tool {
	return []tool.Tool{
		&methodTool{
			method: "prompts/list", risk: tool.RiskRead,
			definition: provider.ToolDefinition{Name: mcpName(server, "prompts_list"), Description: "List prompts from MCP server " + server, Parameters: json.RawMessage(`{"type":"object","properties":{"cursor":{"type":"string"}},"additionalProperties":false}`)},
			execute: func(ctx context.Context, arguments string) (tool.Result, error) {
				var input struct {
					Cursor string `json:"cursor"`
				}
				if err := decodeArguments(arguments, &input); err != nil {
					return tool.Result{}, err
				}
				result, err := session.ListPrompts(ctx, &sdkmcp.ListPromptsParams{Cursor: input.Cursor})
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{Content: prettyJSON(result)}, nil
			},
		},
		&methodTool{
			method: "prompts/get", risk: tool.RiskRead,
			definition: provider.ToolDefinition{Name: mcpName(server, "prompts_get"), Description: "Get a prompt from MCP server " + server, Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"object","additionalProperties":{"type":"string"}}},"required":["name"],"additionalProperties":false}`)},
			execute: func(ctx context.Context, arguments string) (tool.Result, error) {
				var input struct {
					Name      string            `json:"name"`
					Arguments map[string]string `json:"arguments"`
				}
				if err := decodeArguments(arguments, &input); err != nil {
					return tool.Result{}, err
				}
				result, err := session.GetPrompt(ctx, &sdkmcp.GetPromptParams{Name: input.Name, Arguments: input.Arguments})
				if err != nil {
					return tool.Result{}, err
				}
				var content []sdkmcp.Content
				for _, message := range result.Messages {
					content = append(content, &sdkmcp.TextContent{Text: string(message.Role) + ":"}, message.Content)
				}
				return mapContent(content, nil, false)
			},
		},
	}
}

func decodeArguments(arguments string, target any) error {
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode MCP arguments: %w", err)
	}
	return nil
}

func prettyJSON(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

var invalidName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func mcpName(server, name string) string {
	value := "mcp__" + invalidName.ReplaceAllString(server, "_") + "__" + invalidName.ReplaceAllString(name, "_")
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > limit {
		return value[:limit] + "…"
	}
	return value
}

func credentialCommand(ctx context.Context, command []string) (string, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return "", errors.New("credential command is empty")
	}
	commandContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	process := exec.CommandContext(commandContext, command[0], command[1:]...)
	var stdout bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = io.Discard
	if err := process.Run(); err != nil {
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return "", errors.New("credential command timed out")
		}
		return "", fmt.Errorf("credential command failed: %w", err)
	}
	if stdout.Len() > 64*1024 {
		return "", errors.New("credential command output is too large")
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" {
		return "", errors.New("credential command returned an empty token")
	}
	return token, nil
}
