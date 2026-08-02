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
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	closers      []func() error
	cancels      []context.CancelFunc
	tools        []tool.Tool
	names        []string
	failures     []error
	closeOnce    sync.Once
	closeErr     error
	closeTimeout time.Duration
}

const managerCloseTimeout = 5 * time.Second

const (
	maxMCPTextBytes = 256 * 1024
	maxMCPBlobBytes = 192 * 1024
	mcpTextMarker   = "\n[MCP text truncated]"
)

func ConnectAll(ctx context.Context, workspace string, configurations map[string]Config) (*Manager, error) {
	return connectAllWith(ctx, workspace, configurations, 15*time.Second, discoverServer)
}

type discoveredServer struct {
	name    string
	session *sdkmcp.ClientSession
	tools   []tool.Tool
	cancel  context.CancelFunc
	close   func() error
}

func (s discoveredServer) closer() func() error {
	if s.close != nil {
		return s.close
	}
	return func() error {
		if s.session == nil {
			return nil
		}
		return s.session.Close()
	}
}

type serverDiscoverer func(context.Context, string, string, Config) (discoveredServer, error)

func connectAllWith(ctx context.Context, workspace string, configurations map[string]Config, timeout time.Duration, discover serverDiscoverer) (*Manager, error) {
	manager := &Manager{}
	names := make([]string, 0, len(configurations))
	for name := range configurations {
		names = append(names, name)
	}
	sort.Strings(names)
	type outcome struct {
		server discoveredServer
		err    error
	}
	outcomes := make([]outcome, len(names))
	var group sync.WaitGroup
	for index, name := range names {
		group.Add(1)
		go func(index int, name string) {
			defer group.Done()
			// A stdio transport is tied to its context for the full process
			// lifetime, so a deadline context cannot be canceled after a
			// successful handshake. Race discovery against a timer instead and
			// cancel only failed/timed-out servers; the parent owns healthy ones.
			serverContext, cancel := context.WithCancel(ctx)
			result := make(chan outcome)
			go func() {
				server, err := discover(serverContext, workspace, name, configurations[name])
				if err != nil && (server.session != nil || server.close != nil) {
					_ = server.closer()()
					server = discoveredServer{}
				}
				value := outcome{server: server, err: err}
				select {
				case result <- value:
				case <-serverContext.Done():
					if server.session != nil || server.close != nil {
						_ = server.closer()()
					}
				}
			}()
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case outcomes[index] = <-result:
				if outcomes[index].err != nil {
					cancel()
				} else {
					outcomes[index].server.cancel = cancel
				}
			case <-timer.C:
				cancel()
				outcomes[index].err = fmt.Errorf("startup exceeded %s", timeout)
			case <-ctx.Done():
				cancel()
				outcomes[index].err = ctx.Err()
			}
		}(index, name)
	}
	group.Wait()
	if ctx.Err() != nil {
		for _, outcome := range outcomes {
			if outcome.server.session != nil || outcome.server.close != nil {
				_ = outcome.server.closer()()
			}
		}
		return nil, ctx.Err()
	}
	for index, outcome := range outcomes {
		if outcome.err != nil {
			manager.failures = append(manager.failures, fmt.Errorf("MCP server %q: %w", names[index], outcome.err))
			continue
		}
		manager.names = append(manager.names, outcome.server.name)
		manager.closers = append(manager.closers, outcome.server.closer())
		manager.cancels = append(manager.cancels, outcome.server.cancel)
		manager.tools = append(manager.tools, outcome.server.tools...)
	}
	return manager, nil
}

func discoverServer(ctx context.Context, workspace, name string, configuration Config) (discoveredServer, error) {
	session, err := connect(ctx, workspace, configuration)
	if err != nil {
		return discoveredServer{}, fmt.Errorf("connect: %w", err)
	}
	server := discoveredServer{name: name, session: session}
	for remote, listErr := range session.Tools(ctx, nil) {
		if listErr != nil {
			_ = session.Close()
			return discoveredServer{}, fmt.Errorf("list tools: %w", listErr)
		}
		server.tools = append(server.tools, &remoteTool{server: name, session: session, remote: remote})
	}
	initialized := session.InitializeResult()
	if initialized == nil || initialized.Capabilities == nil {
		return server, nil
	}
	if initialized.Capabilities.Resources != nil {
		server.tools = append(server.tools, resourceTools(name, session)...)
	}
	if initialized.Capabilities.Prompts != nil {
		server.tools = append(server.tools, promptTools(name, session)...)
	}
	return server, nil
}

func (m *Manager) Tools() []tool.Tool {
	if m == nil {
		return nil
	}
	return append([]tool.Tool(nil), m.tools...)
}

func (m *Manager) Names() []string {
	if m == nil {
		return nil
	}
	return append([]string(nil), m.names...)
}

func (m *Manager) Failures() []error {
	if m == nil {
		return nil
	}
	return append([]error(nil), m.failures...)
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		for _, cancel := range m.cancels {
			if cancel != nil {
				cancel()
			}
		}
		finished := make(chan error, 1)
		go func() {
			const closeWorkers = 4
			jobs := make(chan func() error)
			failures := make(chan error, len(m.closers))
			var workers sync.WaitGroup
			for index := 0; index < min(closeWorkers, len(m.closers)); index++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for closeSession := range jobs {
						if closeSession != nil {
							failures <- closeSession()
						}
					}
				}()
			}
			for _, closeSession := range m.closers {
				jobs <- closeSession
			}
			close(jobs)
			workers.Wait()
			close(failures)
			var closeFailures []error
			for err := range failures {
				if err != nil {
					closeFailures = append(closeFailures, err)
				}
			}
			finished <- errors.Join(closeFailures...)
		}()
		timeout := m.closeTimeout
		if timeout <= 0 {
			timeout = managerCloseTimeout
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case m.closeErr = <-finished:
		case <-timer.C:
			m.closeErr = fmt.Errorf("timed out closing MCP sessions after %s", timeout)
		}
	})
	return m.closeErr
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
		configureCommandTree(command)
		command.WaitDelay = time.Second
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
		endpoint, err := url.Parse(configuration.URL)
		if err != nil || endpoint.Host == "" || !strings.EqualFold(endpoint.Scheme, "http") && !strings.EqualFold(endpoint.Scheme, "https") {
			return nil, errors.New("HTTP MCP server URL must use http or https and include a host")
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
		httpClient := &http.Client{
			Transport: &headerTransport{base: http.DefaultTransport, headers: headers, origin: endpoint},
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("MCP HTTP redirect limit exceeded")
				}
				if !sameMCPOrigin(endpoint, request.URL) {
					return fmt.Errorf("MCP HTTP redirect changed origin from %s to %s", endpoint.Host, request.URL.Host)
				}
				return nil
			},
		}
		transport = &sdkmcp.StreamableClientTransport{
			Endpoint:             configuration.URL,
			HTTPClient:           httpClient,
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
	origin  *url.URL
}

func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for key := range t.headers {
		clone.Header.Del(key)
	}
	if sameMCPOrigin(t.origin, request.URL) {
		for key, value := range t.headers {
			clone.Header.Set(key, value)
		}
	}
	return t.base.RoundTrip(clone)
}

func sameMCPOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
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
	const (
		maxResultImages     = 8
		maxResultImageBytes = 16 * 1024 * 1024
	)
	imageCount := 0
	imageBytes := 0
	for _, content := range items {
		image, ok := content.(*sdkmcp.ImageContent)
		if !ok {
			continue
		}
		imageCount++
		if imageCount > maxResultImages {
			return tool.Result{}, fmt.Errorf("MCP result exceeds the %d-image limit", maxResultImages)
		}
		if len(image.Data) > maxResultImageBytes-imageBytes {
			return tool.Result{}, fmt.Errorf("MCP result images exceed the %d MiB limit", maxResultImageBytes/(1024*1024))
		}
		imageBytes += len(image.Data)
	}
	text := newBoundedMCPText(maxMCPTextBytes)
	images := make([]provider.Image, 0, imageCount)
	for _, content := range items {
		switch item := content.(type) {
		case *sdkmcp.TextContent:
			if !text.truncated {
				text.appendSection(item.Text)
			}
		case *sdkmcp.ImageContent:
			images = append(images, provider.Image{Data: base64.StdEncoding.EncodeToString(item.Data), MIMEType: item.MIMEType, Detail: "high"})
		case *sdkmcp.EmbeddedResource:
			if item.Resource != nil && !text.truncated {
				if err := appendResourceContent(text, item.Resource); err != nil {
					return tool.Result{}, err
				}
			}
		case *sdkmcp.ResourceLink:
			if !text.truncated {
				text.startSection()
				text.append(strings.TrimSpace(item.Name))
				text.append("\n")
				text.append(strings.TrimSpace(item.URI))
				text.append("\n")
				text.append(strings.TrimSpace(item.Description))
			}
		default:
			if !text.truncated {
				encoded, fits, err := boundedJSON(content, text.remaining())
				if err == nil {
					if !fits {
						text.markTruncated()
					} else {
						text.appendSection(string(encoded))
					}
				}
			}
		}
	}
	if structured != nil && text.sections == 0 {
		encoded, fits, err := boundedJSON(structured, text.remaining())
		if err != nil {
			return tool.Result{}, fmt.Errorf("encode MCP structured content: %w", err)
		}
		if !fits {
			text.markTruncated()
		} else {
			text.appendSection(string(encoded))
		}
	}
	return tool.Result{Content: text.String(), Images: images, IsError: isError}, nil
}

func appendResourceContent(output *boundedMCPText, content *sdkmcp.ResourceContents) error {
	if content == nil {
		return nil
	}
	text := strings.TrimSpace(content.Text)
	if text == "" && len(content.Blob) > maxMCPBlobBytes {
		return fmt.Errorf("MCP resource blob exceeds the %d KiB limit", maxMCPBlobBytes/1024)
	}
	uri := strings.TrimSpace(content.URI)
	output.startSection()
	output.append(uri)
	if text != "" {
		if uri != "" {
			output.append("\n")
		}
		output.append(text)
		return nil
	}
	if len(content.Blob) == 0 {
		return nil
	}
	encodedBytes := base64.StdEncoding.EncodedLen(len(content.Blob))
	separatorBytes := 0
	if uri != "" {
		separatorBytes = 1
	}
	if encodedBytes+separatorBytes > output.remaining() {
		output.markTruncated()
		return nil
	}
	if separatorBytes > 0 {
		output.append("\n")
	}
	encoder := base64.NewEncoder(base64.StdEncoding, output)
	_, err := encoder.Write(content.Blob)
	if closeErr := encoder.Close(); err == nil {
		err = closeErr
	}
	return err
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
func (t *methodTool) ParallelSafe(string) bool            { return t.risk == tool.RiskRead }
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
				values := newBoundedMCPText(maxMCPTextBytes)
				for _, content := range result.Contents {
					if values.truncated {
						break
					}
					if err := appendResourceContent(values, content); err != nil {
						return tool.Result{}, err
					}
				}
				return tool.Result{Content: values.String()}, nil
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

type boundedMCPText struct {
	buffer    bytes.Buffer
	limit     int
	sections  int
	truncated bool
}

func newBoundedMCPText(limit int) *boundedMCPText {
	return &boundedMCPText{limit: limit}
}

func (b *boundedMCPText) remaining() int {
	return max(0, b.limit-len(mcpTextMarker)-b.buffer.Len())
}

func (b *boundedMCPText) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.remaining()
	if remaining > 0 {
		_, _ = b.buffer.Write(value[:min(len(value), remaining)])
	}
	if original > remaining {
		b.truncated = true
	}
	return original, nil
}

func (b *boundedMCPText) append(value string) {
	_, _ = io.WriteString(b, value)
}

func (b *boundedMCPText) startSection() {
	if b.truncated {
		return
	}
	if b.sections > 0 {
		b.append("\n\n")
	}
	b.sections++
}

func (b *boundedMCPText) appendSection(value string) {
	b.startSection()
	b.append(value)
}

func (b *boundedMCPText) markTruncated() { b.truncated = true }

func (b *boundedMCPText) String() string {
	value := b.buffer.String()
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	if b.truncated {
		value += mcpTextMarker
	}
	return value
}

func prettyJSON(value any) string {
	output := newBoundedMCPText(maxMCPTextBytes)
	encoded, fits, err := boundedJSON(value, output.remaining())
	if err != nil {
		output.append("[unable to encode MCP response: ")
		output.append(err.Error())
		output.append("]")
	} else if !fits {
		output.markTruncated()
	} else {
		output.append(string(encoded))
	}
	return output.String()
}

func boundedJSON(value any, limit int) ([]byte, bool, error) {
	remaining := int64(limit)
	if !jsonValueFits(reflect.ValueOf(value), &remaining, 0) {
		return nil, false, nil
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, false, err
	}
	if len(encoded) > limit {
		return nil, false, nil
	}
	return encoded, true, nil
}

var rawMessageType = reflect.TypeOf(json.RawMessage{})

func jsonValueFits(value reflect.Value, remaining *int64, depth int) bool {
	if depth > 100 || *remaining < 0 {
		return false
	}
	if !value.IsValid() {
		return consumeJSONBytes(remaining, 4)
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return consumeJSONBytes(remaining, 4)
		}
		return jsonValueFits(value.Elem(), remaining, depth+1)
	}
	if value.Type() == rawMessageType {
		return consumeJSONBytes(remaining, int64(value.Len()))
	}
	switch value.Kind() {
	case reflect.Bool:
		return consumeJSONBytes(remaining, 5)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return consumeJSONBytes(remaining, 32)
	case reflect.String:
		return consumeJSONString(remaining, value.String())
	case reflect.Slice:
		if value.IsNil() {
			return consumeJSONBytes(remaining, 4)
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return consumeJSONBytes(remaining, int64(base64.StdEncoding.EncodedLen(value.Len())+2))
		}
		fallthrough
	case reflect.Array:
		if !consumeJSONBytes(remaining, 2) {
			return false
		}
		for index := 0; index < value.Len(); index++ {
			if !consumeJSONBytes(remaining, 2) || !jsonValueFits(value.Index(index), remaining, depth+1) {
				return false
			}
		}
		return true
	case reflect.Map:
		if value.IsNil() {
			return consumeJSONBytes(remaining, 4)
		}
		if !consumeJSONBytes(remaining, 2) {
			return false
		}
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key()
			if !consumeJSONBytes(remaining, 2) {
				return false
			}
			if key.Kind() == reflect.String {
				if !consumeJSONString(remaining, key.String()) {
					return false
				}
			} else if !consumeJSONBytes(remaining, 64) {
				return false
			}
			if !jsonValueFits(iterator.Value(), remaining, depth+1) {
				return false
			}
		}
		return true
	case reflect.Struct:
		if !consumeJSONBytes(remaining, 2) {
			return false
		}
		typeInfo := value.Type()
		for index := 0; index < value.NumField(); index++ {
			fieldInfo := typeInfo.Field(index)
			if fieldInfo.PkgPath != "" {
				continue
			}
			name := strings.Split(fieldInfo.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = fieldInfo.Name
			}
			if !consumeJSONBytes(remaining, 2) || !consumeJSONString(remaining, name) || !jsonValueFits(value.Field(index), remaining, depth+1) {
				return false
			}
		}
		return true
	default:
		return consumeJSONBytes(remaining, 64)
	}
}

func consumeJSONString(remaining *int64, value string) bool {
	if !consumeJSONBytes(remaining, 2) {
		return false
	}
	for _, character := range value {
		size := utf8.RuneLen(character)
		if character < 0x20 || character == '\\' || character == '"' || character == '<' || character == '>' || character == '&' || character == '\u2028' || character == '\u2029' {
			size = 6
		}
		if character == utf8.RuneError {
			size = 6
		}
		if !consumeJSONBytes(remaining, int64(size)) {
			return false
		}
	}
	return true
}

func consumeJSONBytes(remaining *int64, amount int64) bool {
	if amount < 0 || amount > *remaining {
		*remaining = -1
		return false
	}
	*remaining -= amount
	return true
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
	return credentialCommandWithTimeout(ctx, command, 5*time.Second)
}

func credentialCommandWithTimeout(ctx context.Context, command []string, timeout time.Duration) (string, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return "", errors.New("credential command is empty")
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	process := exec.CommandContext(commandContext, command[0], command[1:]...)
	configureCommandTree(process)
	process.WaitDelay = time.Second
	stdout := boundedCredentialOutput{limit: 64 * 1024}
	stdout.onExceeded = func() {
		cancel()
		_ = terminateCommandTree(process)
	}
	process.Stdout = &stdout
	process.Stderr = io.Discard
	runErr := process.Run()
	cleanupCommandTree(process)
	if stdout.exceeded {
		return "", errors.New("credential command output is too large")
	}
	if runErr != nil {
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return "", errors.New("credential command timed out")
		}
		return "", fmt.Errorf("credential command failed: %w", runErr)
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" {
		return "", errors.New("credential command returned an empty token")
	}
	return token, nil
}

type boundedCredentialOutput struct {
	buffer     bytes.Buffer
	limit      int
	exceeded   bool
	onExceeded func()
	exceedOnce sync.Once
}

func (b *boundedCredentialOutput) Len() int       { return b.buffer.Len() }
func (b *boundedCredentialOutput) String() string { return b.buffer.String() }

func (b *boundedCredentialOutput) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		_, _ = b.buffer.Write(value[:min(len(value), remaining)])
	}
	if original > remaining {
		b.exceeded = true
		if b.onExceeded != nil {
			b.exceedOnce.Do(b.onExceeded)
		}
	}
	return original, nil
}
