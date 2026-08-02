package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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
