package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daemon365/supercode/internal/tool"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHTTPServerDiscoveryAndToolCall(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		if envelope.Method == "notifications/initialized" {
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch envelope.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(envelope.Params, &params); err != nil {
				t.Fatal(err)
			}
			result = map[string]any{
				"protocolVersion": params.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "test-server", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "Echo text", "inputSchema": map[string]any{"type": "object"},
				"annotations": map[string]any{"readOnlyHint": true},
			}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello from MCP"}}}
		default:
			t.Fatalf("unexpected method %s", envelope.Method)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": result})
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()

	manager, err := ConnectAll(context.Background(), t.TempDir(), map[string]Config{
		"demo": {Transport: "http", URL: server.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(manager.Tools()) != 1 || manager.Tools()[0].Definition().Name != "mcp__demo__echo" {
		t.Fatalf("tools = %#v", manager.Tools())
	}
	result, err := manager.Tools()[0].Execute(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "hello from MCP" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestMCPHTTPHeadersNeverFollowCrossOriginRedirect(t *testing.T) {
	startServer := func(handler http.Handler) *httptest.Server {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Skipf("local sockets are unavailable: %v", err)
		}
		server := httptest.NewUnstartedServer(handler)
		server.Listener = listener
		server.Start()
		return server
	}
	var leaked atomic.Bool
	target := startServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-MCP-Secret") != "" {
			leaked.Store(true)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := startServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-MCP-Secret") != "secret" {
			t.Errorf("origin request headers = %#v", request.Header)
		}
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer source.Close()

	origin, err := url.Parse(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &headerTransport{base: http.DefaultTransport, origin: origin, headers: map[string]string{
			"Authorization": "Bearer secret", "X-MCP-Secret": "secret",
		}},
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			if !sameMCPOrigin(origin, request.URL) {
				return errors.New("cross-origin MCP redirect blocked")
			}
			return nil
		},
	}
	if _, err := client.Get(source.URL); err == nil || !strings.Contains(err.Error(), "cross-origin MCP redirect blocked") {
		t.Fatalf("redirect error = %v", err)
	}
	response, err := (&http.Client{Transport: client.Transport}).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if leaked.Load() {
		t.Fatal("MCP credentials leaked to the redirect target")
	}
}

func TestConnectAllRunsServersConcurrentlyAndDegradesFailures(t *testing.T) {
	configurations := map[string]Config{"good-a": {}, "bad": {}, "good-b": {}}
	started := time.Now()
	manager, err := connectAllWith(context.Background(), t.TempDir(), configurations, time.Second,
		func(ctx context.Context, _ string, name string, _ Config) (discoveredServer, error) {
			select {
			case <-time.After(40 * time.Millisecond):
			case <-ctx.Done():
				return discoveredServer{}, ctx.Err()
			}
			if name == "bad" {
				return discoveredServer{}, errors.New("offline")
			}
			return discoveredServer{name: name}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 90*time.Millisecond {
		t.Fatalf("connections ran serially: %s", elapsed)
	}
	if got := manager.Names(); len(got) != 2 || got[0] != "good-a" || got[1] != "good-b" {
		t.Fatalf("healthy servers = %v", got)
	}
	if failures := manager.Failures(); len(failures) != 1 || !strings.Contains(failures[0].Error(), "bad") {
		t.Fatalf("failures = %v", failures)
	}
}

func TestConnectAllClosesServerThatFinishesAfterTimeout(t *testing.T) {
	release := make(chan struct{})
	closed := make(chan struct{})
	manager, err := connectAllWith(context.Background(), t.TempDir(), map[string]Config{"slow": {}}, 20*time.Millisecond,
		func(context.Context, string, string, Config) (discoveredServer, error) {
			<-release
			return discoveredServer{name: "slow", close: func() error { close(closed); return nil }}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.Failures()) != 1 {
		t.Fatalf("failures = %v", manager.Failures())
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("late MCP session was not closed")
	}
}

func TestManagerCloseCancelsBeforeBoundedConcurrentClose(t *testing.T) {
	var canceled atomic.Bool
	var active, maximum atomic.Int64
	manager := &Manager{cancels: []context.CancelFunc{func() { canceled.Store(true) }}}
	for index := 0; index < 4; index++ {
		manager.closers = append(manager.closers, func() error {
			if !canceled.Load() {
				return errors.New("close started before cancellation")
			}
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			active.Add(-1)
			return nil
		})
	}
	started := time.Now()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if maximum.Load() < 2 {
		t.Fatalf("maximum close concurrency = %d", maximum.Load())
	}
	if elapsed := time.Since(started); elapsed >= 120*time.Millisecond {
		t.Fatalf("sessions closed serially: %s", elapsed)
	}
}

func TestCredentialOutputIsBoundedWhileWriting(t *testing.T) {
	var output boundedCredentialOutput
	output.limit = 64
	value := strings.Repeat("x", 4096)
	if count, err := output.Write([]byte(value)); err != nil || count != len(value) {
		t.Fatalf("Write() = %d, %v", count, err)
	}
	if output.Len() != 64 || !output.exceeded {
		t.Fatalf("bounded output len=%d exceeded=%t", output.Len(), output.exceeded)
	}
}

func TestRemoteReadOnlyHintDoesNotLowerRiskOrEnableParallelism(t *testing.T) {
	remote := &remoteTool{remote: &sdkmcp.Tool{Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true}}}
	if risk := remote.Risk(`{}`); risk != tool.RiskExecute {
		t.Fatalf("risk = %q, want execute", risk)
	}
	if tool.CanRunInParallel(remote, `{}`) {
		t.Fatal("remote ReadOnlyHint enabled parallel execution")
	}
}

func TestManagerCloseReturnsAfterDeadlineWhenCloserBlocks(t *testing.T) {
	release := make(chan struct{})
	manager := &Manager{
		closeTimeout: 20 * time.Millisecond,
		closers: []func() error{func() error {
			<-release
			return nil
		}},
	}
	started := time.Now()
	err := manager.Close()
	if err == nil || !strings.Contains(err.Error(), "timed out closing MCP sessions") {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("Close() ignored deadline: %s", elapsed)
	}
	close(release)
}

func TestCredentialCommandStopsImmediatelyWhenOutputIsTooLarge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command test is Unix-specific")
	}
	started := time.Now()
	_, err := credentialCommand(context.Background(), []string{"/bin/sh", "-c", "(while :; do printf '0123456789abcdef0123456789abcdef'; done) & wait"})
	if err == nil || !strings.Contains(err.Error(), "output is too large") {
		t.Fatalf("credentialCommand() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("oversized credential command was not stopped promptly: %s", elapsed)
	}
}

func TestCredentialCommandTimeoutKillsHelperTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command test is Unix-specific")
	}
	started := time.Now()
	_, err := credentialCommandWithTimeout(context.Background(), []string{"/bin/sh", "-c", "sleep 30 & wait"}, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("credential timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("credential helper tree was not stopped promptly: %s", elapsed)
	}
}

func TestCredentialCommandDoesNotWaitForeverForEscapedPipeHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command test is Unix-specific")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	started := time.Now()
	_, err := credentialCommandWithTimeout(context.Background(), []string{"/bin/sh", "-c", "setsid sh -c 'sleep 3' & exit 0"}, 30*time.Millisecond)
	if err == nil {
		t.Fatal("escaped pipe holder unexpectedly produced a credential")
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("credential helper waited for escaped pipe holder: %s", elapsed)
	}
}

func TestMapContentRejectsTooManyOrOversizedImagesBeforeEncoding(t *testing.T) {
	tooMany := make([]sdkmcp.Content, 9)
	for index := range tooMany {
		tooMany[index] = &sdkmcp.ImageContent{Data: []byte{byte(index)}, MIMEType: "image/png"}
	}
	if _, err := mapContent(tooMany, nil, false); err == nil || !strings.Contains(err.Error(), "image limit") {
		t.Fatalf("too many images error = %v", err)
	}
	tooLarge := []sdkmcp.Content{&sdkmcp.ImageContent{Data: make([]byte, 16*1024*1024+1), MIMEType: "image/png"}}
	if _, err := mapContent(tooLarge, nil, false); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("oversized images error = %v", err)
	}
}

func TestMapContentBoundsLargeTextAndStructuredContent(t *testing.T) {
	large := strings.Repeat("x", maxMCPTextBytes*2)
	result, err := mapContent([]sdkmcp.Content{&sdkmcp.TextContent{Text: large}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) > maxMCPTextBytes || !strings.Contains(result.Content, "[MCP text truncated]") {
		t.Fatalf("large text result len=%d suffix=%q", len(result.Content), result.Content[len(result.Content)-min(64, len(result.Content)):])
	}

	result, err = mapContent(nil, map[string]any{"payload": large}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) > maxMCPTextBytes || !strings.Contains(result.Content, "[MCP text truncated]") {
		t.Fatalf("large structured result len=%d content=%q", len(result.Content), result.Content)
	}
}

func TestMapContentRejectsOversizedResourceBlobBeforeBase64(t *testing.T) {
	content := []sdkmcp.Content{&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{
		URI: "memory://large", Blob: make([]byte, maxMCPBlobBytes+1),
	}}}
	if _, err := mapContent(content, nil, false); err == nil || !strings.Contains(err.Error(), "resource blob") {
		t.Fatalf("oversized resource blob error = %v", err)
	}
}

func TestPrettyJSONBoundsLargeMCPLists(t *testing.T) {
	value := map[string]any{"items": []string{strings.Repeat("y", maxMCPTextBytes*2)}}
	encoded := prettyJSON(value)
	if len(encoded) > maxMCPTextBytes || !strings.Contains(encoded, "[MCP text truncated]") {
		t.Fatalf("prettyJSON len=%d content=%q", len(encoded), encoded)
	}
}
