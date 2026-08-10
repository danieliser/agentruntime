package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestResolveNativeExecutionAppliesClaudeControls(t *testing.T) {
	request := SessionRequest{
		Agent: "claude", Model: "claude-opus-5", Effort: "max", Fast: true,
		Prompt: "research", Interactive: false,
		Env:    map[string]string{"VISIBLE": "yes"},
		Claude: &ClaudeConfig{MaxTurns: 1, AllowedTools: []string{"WebSearch"}},
	}
	resolved, err := resolveNativeExecution(request, agent.DefaultRegistry().Get("claude"), "/workspace", "provider-claude")
	if err != nil {
		t.Fatalf("resolve Claude execution: %v", err)
	}
	if resolved.Config.Model != request.Model || resolved.Config.Effort != request.Effort || !resolved.Config.Fast {
		t.Fatalf("resolved Claude model controls = %+v", resolved.Config)
	}
	if resolved.Config.MaxTokens != 1 || !slices.Equal(resolved.Config.AllowedTools, []string{"WebSearch"}) {
		t.Fatalf("resolved Claude turn/tool controls = %+v", resolved.Config)
	}
	for _, argument := range []string{"--model", "claude-opus-5", "--effort", "max", "--max-turns", "1", "--allowedTools", "WebSearch"} {
		if !slices.Contains(resolved.Command, argument) {
			t.Errorf("resolved Claude command missing %q: %v", argument, resolved.Command)
		}
	}
	if resolved.Config.ResumeSessionID != "provider-claude" || !resolved.Config.NativeStream {
		t.Fatalf("resolved Claude native identity = %+v", resolved.Config)
	}
}

func TestResolveNativeExecutionAppliesCodexAppServerControls(t *testing.T) {
	request := SessionRequest{
		Agent: "codex", Model: "gpt-5.6-sol", Effort: "ultra", Fast: true,
		Prompt: "research", Codex: &CodexConfig{},
	}
	resolved, err := resolveNativeExecution(request, agent.DefaultRegistry().Get("codex"), "/workspace", "provider-codex")
	if err != nil {
		t.Fatalf("resolve Codex execution: %v", err)
	}
	want := []string{
		"codex", "app-server", "--listen", "stdio://", "--strict-config",
		"-c", `model="gpt-5.6-sol"`,
		"-c", `model_reasoning_effort="ultra"`,
		"-c", `service_tier="priority"`,
	}
	if !slices.Equal(resolved.Command, want) {
		t.Fatalf("resolved Codex command = %v, want %v", resolved.Command, want)
	}
	if resolved.Config.Model != request.Model || resolved.Config.Effort != request.Effort || !resolved.Config.Fast || resolved.Config.ResumeSessionID != "provider-codex" {
		t.Fatalf("resolved Codex controls = %+v", resolved.Config)
	}
}

func TestResolveNativeExecutionRejectsMissingAgent(t *testing.T) {
	_, err := resolveNativeExecution(SessionRequest{Agent: "codex"}, nil, "", "")
	if err == nil {
		t.Fatal("missing agent resolver unexpectedly succeeded")
	}
}

func TestV1HTTPCreateUsesCanonicalNativeResolver(t *testing.T) {
	store := memory.New()
	registry := agent.NewRegistry()
	capture := &resolverCaptureAgent{configs: make(chan agent.AgentConfig, 1)}
	registry.Register(capture)
	manager := session.NewManager()
	server := NewServer(manager, newFakeRuntime(t), registry, ServerConfig{
		DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"),
		DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(func() {
		httpServer.Close()
		manager.ShutdownAll()
		_ = store.Close()
	})
	response := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "act-1001-http", "agent": "claude", "runtime": "test",
		"prompt": "research", "model": "claude-opus-5", "effort": "max", "fast": true,
		"timeout": "47s", "claude": map[string]any{"max_turns": 1, "allowed_tools": []string{"WebSearch"}},
	})
	defer response.Body.Close()
	if response.StatusCode != 201 {
		var failure any
		_ = json.NewDecoder(response.Body).Decode(&failure)
		t.Fatalf("HTTP create status=%d body=%v", response.StatusCode, failure)
	}
	var envelope struct {
		Data v1SessionData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode HTTP create: %v", err)
	}
	select {
	case config := <-capture.configs:
		if config.Model != "claude-opus-5" || config.Effort != "max" || !config.Fast || config.MaxTokens != 1 || !slices.Equal(config.AllowedTools, []string{"WebSearch"}) {
			t.Fatalf("HTTP create resolved config = %+v", config)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP create did not resolve the provider command")
	}
	stored, err := store.GetSession(context.Background(), envelope.Data.SessionID)
	if err != nil || !strings.Contains(string(stored.RequestManifest), `"model":"claude-opus-5"`) || !strings.Contains(string(stored.RequestManifest), `"allowed_tools":["WebSearch"]`) {
		t.Fatalf("durable resolved request manifest = %s err=%v", stored.RequestManifest, err)
	}
}

type resolverCaptureAgent struct {
	configs chan agent.AgentConfig
}

func (*resolverCaptureAgent) Name() string { return "claude" }
func (capture *resolverCaptureAgent) BuildCmd(_ string, config agent.AgentConfig) ([]string, error) {
	capture.configs <- config
	return []string{"/bin/sh", "-c", `IFS= read -r prompt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"resolver-provider"}' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`}, nil
}
func (*resolverCaptureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }
