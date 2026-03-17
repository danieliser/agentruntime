package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMain_DetectsClaudeAgent(t *testing.T) {
	setAgentCommand(t, []string{"/usr/local/bin/claude", "--print"})

	server, port, err := newSidecarFromEnv()
	if err != nil {
		t.Fatalf("newSidecarFromEnv() error = %v", err)
	}
	defer func() { _ = server.Close() }()

	if port != defaultPort {
		t.Fatalf("port = %q, want %q", port, defaultPort)
	}
	if server.AgentType() != "claude" {
		t.Fatalf("AgentType() = %q, want claude", server.AgentType())
	}

	wsServer, ok := server.(*ExternalWSServer)
	if !ok {
		t.Fatalf("server type = %T, want *ExternalWSServer", server)
	}
	if _, ok := wsServer.backend.(*ClaudeBackend); !ok {
		t.Fatalf("backend type = %T, want *ClaudeBackend", wsServer.backend)
	}
}

func TestMain_DetectsCodexAgent(t *testing.T) {
	setAgentCommand(t, []string{"/opt/tools/codex"})

	server, _, err := newSidecarFromEnv()
	if err != nil {
		t.Fatalf("newSidecarFromEnv() error = %v", err)
	}
	defer func() { _ = server.Close() }()

	if server.AgentType() != "codex" {
		t.Fatalf("AgentType() = %q, want codex", server.AgentType())
	}

	wsServer, ok := server.(*ExternalWSServer)
	if !ok {
		t.Fatalf("server type = %T, want *ExternalWSServer", server)
	}

	backend, ok := wsServer.backend.(*codexBackend)
	if !ok {
		t.Fatalf("backend type = %T, want *codexBackend", wsServer.backend)
	}
	if backend.binary != "/opt/tools/codex" {
		t.Fatalf("codex binary = %q, want /opt/tools/codex", backend.binary)
	}
}

func TestMain_HealthEndpoint(t *testing.T) {
	setAgentCommand(t, []string{"claude"})
	t.Setenv("SIDECAR_PORT", "9191")

	server, port, err := newSidecarFromEnv()
	if err != nil {
		t.Fatalf("newSidecarFromEnv() error = %v", err)
	}
	defer func() { _ = server.Close() }()

	if port != "9191" {
		t.Fatalf("port = %q, want 9191", port)
	}

	ts := httptest.NewServer(server.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var payload healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if payload.Status != "ok" {
		t.Fatalf("status = %q, want ok", payload.Status)
	}
	if payload.AgentType != "claude" {
		t.Fatalf("agent_type = %q, want claude", payload.AgentType)
	}
	if payload.AgentRunning {
		t.Fatal("expected agent_running false before websocket connect")
	}
}

func setAgentCommand(t *testing.T, cmd []string) {
	t.Helper()

	raw, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal AGENT_CMD: %v", err)
	}
	t.Setenv("AGENT_CMD", string(raw))
}
