package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMCPServer_Initialize(t *testing.T) {
	server := newStartedMCPServer(t)

	conn := mustDialMCP(t, server, http.Header{
		"x-claude-code-ide-authorization": []string{server.AuthToken()},
	})
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26"},
	}); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	msg := readJSONMessage(t, conn)
	result := mcpMapField(t, msg, "result")
	if result["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("protocolVersion = %#v, want %q", result["protocolVersion"], mcpProtocolVersion)
	}
}

func TestMCPServer_ToolsList(t *testing.T) {
	server := newStartedMCPServer(t)

	conn := mustDialMCP(t, server, http.Header{
		"x-claude-code-ide-authorization": []string{server.AuthToken()},
	})
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	}); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}

	msg := readJSONMessage(t, conn)
	result := mcpMapField(t, msg, "result")
	tools := mcpSliceField(t, result, "tools")
	if len(tools) != 12 {
		t.Fatalf("tool count = %d, want 12", len(tools))
	}
}

func TestMCPServer_ToolCall(t *testing.T) {
	server := newStartedMCPServer(t)

	conn := mustDialMCP(t, server, http.Header{
		"x-claude-code-ide-authorization": []string{server.AuthToken()},
	})
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "getWorkspaceFolders",
			"arguments": map[string]any{},
		},
	}); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}

	msg := readJSONMessage(t, conn)
	result := mcpMapField(t, msg, "result")
	content := mcpSliceField(t, result, "content")
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1", len(content))
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] type = %T, want map[string]any", content[0])
	}
	if item["type"] != "text" {
		t.Fatalf("content type = %#v, want text", item["type"])
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(item["text"].(string)), &payload); err != nil {
		t.Fatalf("decode tool payload: %v", err)
	}
	if payload["success"] != true {
		t.Fatalf("tool payload = %#v, want success true", payload)
	}
}

func TestMCPServer_LockFile(t *testing.T) {
	server := newStartedMCPServer(t)

	data, err := os.ReadFile(server.LockFile())
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode lock file: %v", err)
	}
	if payload["authToken"] != server.AuthToken() {
		t.Fatalf("authToken = %#v, want %q", payload["authToken"], server.AuthToken())
	}
	if payload["transport"] != "ws" {
		t.Fatalf("transport = %#v, want ws", payload["transport"])
	}
}

func TestMCPServer_Auth(t *testing.T) {
	server := newStartedMCPServer(t)

	wsURL := "ws://127.0.0.1:" + strconv.Itoa(server.Port()) + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, http.Header{
		"x-claude-code-ide-authorization": []string{"wrong-token"},
	})
	if err == nil {
		t.Fatal("expected unauthorized websocket dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

func newStartedMCPServer(t *testing.T) *MCPServer {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	server, err := NewMCPServer(MCPServerConfig{
		WorkspaceFolders: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatalf("NewMCPServer() error = %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	return server
}

func mustDialMCP(t *testing.T, server *MCPServer, header http.Header) *websocket.Conn {
	t.Helper()

	wsURL := "ws://127.0.0.1:" + strconv.Itoa(server.Port()) + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		t.Fatalf("dial mcp websocket: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func readJSONMessage(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read json message: %v", err)
	}
	return msg
}

func mcpMapField(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, m)
	}
	out, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("key %q type = %T, want map[string]any", key, raw)
	}
	return out
}

func mcpSliceField(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, m)
	}
	out, ok := raw.([]any)
	if !ok {
		t.Fatalf("key %q type = %T, want []any", key, raw)
	}
	return out
}
