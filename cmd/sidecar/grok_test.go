package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spawnGrokBackend spawns a grok backend against a fake process and returns
// the backend plus the captured spawn spec.
func spawnGrokBackend(t *testing.T, mutate func(*GrokBackendConfig)) (*GrokBackend, *fakeClaudeProcess, ClaudeSpawnSpec) {
	t.Helper()

	proc := newFakeClaudeProcess()
	var gotSpec ClaudeSpawnSpec
	cfg := GrokBackendConfig{
		Binary:           "grok",
		Prompt:           "build the game",
		WorkspaceFolders: []string{t.TempDir()},
		StartProcess: func(_ context.Context, spec ClaudeSpawnSpec) (ClaudeProcess, error) {
			gotSpec = spec
			return proc, nil
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}

	backend := NewGrokBackend(cfg)
	t.Cleanup(func() {
		proc.finish(nil)
		_ = backend.Close()
	})
	if err := backend.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return backend, proc, gotSpec
}

func TestGrokBackend_SpawnArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, _, spec := spawnGrokBackend(t, func(cfg *GrokBackendConfig) {
		cfg.Model = "grok-4.5"
		cfg.Effort = "high"
		cfg.MaxTurns = 30
		cfg.SystemPrompt = "You are a neutral agent."
	})

	if spec.Command != "grok" {
		t.Fatalf("command = %q, want grok", spec.Command)
	}
	if !hasArg(spec.Args, "--always-approve") {
		t.Fatalf("missing --always-approve, args = %v", spec.Args)
	}
	if got, _ := argValue(spec.Args, "--output-format"); got != "streaming-json" {
		t.Fatalf("--output-format = %q, want streaming-json", got)
	}
	if got, _ := argValue(spec.Args, "--model"); got != "grok-4.5" {
		t.Fatalf("--model = %q, want grok-4.5", got)
	}
	if got, _ := argValue(spec.Args, "--reasoning-effort"); got != "high" {
		t.Fatalf("--reasoning-effort = %q, want high", got)
	}
	if got, _ := argValue(spec.Args, "--max-turns"); got != "30" {
		t.Fatalf("--max-turns = %q, want 30", got)
	}
	if got, _ := argValue(spec.Args, "--system-prompt-override"); got != "You are a neutral agent." {
		t.Fatalf("--system-prompt-override = %q", got)
	}
	if got, _ := argValue(spec.Args, "-p"); got != "build the game" {
		t.Fatalf("-p = %q, want prompt", got)
	}
}

func TestGrokBackend_RequiresPrompt(t *testing.T) {
	backend := NewGrokBackend(GrokBackendConfig{
		StartProcess: func(_ context.Context, _ ClaudeSpawnSpec) (ClaudeProcess, error) {
			t.Fatal("process must not start without a prompt")
			return nil, nil
		},
	})
	t.Cleanup(func() { _ = backend.Close() })

	err := backend.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AGENT_PROMPT") {
		t.Fatalf("Start() error = %v, want AGENT_PROMPT requirement", err)
	}
}

func TestGrokBackend_EventMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	backend, proc, _ := spawnGrokBackend(t, nil)

	proc.sendStdoutJSON(t, map[string]any{"type": "thought", "data": "thinking..."})
	proc.sendStdoutJSON(t, map[string]any{"type": "text", "data": "OK"})
	proc.sendStdoutJSON(t, map[string]any{
		"type":           "end",
		"stopReason":     "EndTurn",
		"sessionId":      "grok-sess-1",
		"num_turns":      2,
		"total_cost_usd": 0.0047,
		"usage":          map[string]any{"input_tokens": 656, "output_tokens": 28},
	})

	thought := claudeExpectEventType(t, backend.Events(), "agent_message")
	thoughtData := claudeEventData(t, thought)
	if thoughtData["thought"] != true || thoughtData["delta"] != true {
		t.Fatalf("thought event data = %#v", thoughtData)
	}
	if thoughtData["text"] != "thinking..." {
		t.Fatalf("thought text = %#v", thoughtData["text"])
	}

	text := claudeExpectEventType(t, backend.Events(), "agent_message")
	textData := claudeEventData(t, text)
	if textData["text"] != "OK" || textData["delta"] != true {
		t.Fatalf("text event data = %#v", textData)
	}
	if _, hasThought := textData["thought"]; hasThought {
		t.Fatalf("text event should not carry thought flag: %#v", textData)
	}

	result := claudeExpectEventType(t, backend.Events(), "result")
	resultData := claudeEventData(t, result)
	if resultData["session_id"] != "grok-sess-1" {
		t.Fatalf("result session_id = %#v", resultData["session_id"])
	}
	if resultData["status"] != "EndTurn" {
		t.Fatalf("result status = %#v", resultData["status"])
	}
	if resultData["cost_usd"] != 0.0047 {
		t.Fatalf("result cost_usd = %#v", resultData["cost_usd"])
	}

	// The end event's sessionId becomes the backend session identity.
	if backend.SessionID() != "grok-sess-1" {
		t.Fatalf("SessionID() = %q, want grok-sess-1", backend.SessionID())
	}
}

func TestGrokBackend_CleanContextFakeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Real grok config + contaminants that must NOT reach the fake home.
	if err := os.MkdirAll(home+"/.grok", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileOrFail(t, home+"/.grok/auth.json", `{"token":"xai-secret"}`)
	writeFileOrFail(t, home+"/.grok/agent_id", "agent-123")
	writeFileOrFail(t, home+"/.grok/models_cache.json", `{"models":[]}`)
	writeFileOrFail(t, home+"/.grok/config.toml", "[mcp_servers.leaky]\ncommand = \"evil\"\n")
	if err := os.MkdirAll(home+"/.claude", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileOrFail(t, home+"/.claude/CLAUDE.md", "host instructions")

	backend, _, spec := spawnGrokBackend(t, func(cfg *GrokBackendConfig) {
		cfg.Context = "clean"
	})

	fakeHome := ""
	for _, entry := range spec.Env {
		if strings.HasPrefix(entry, "HOME=") && strings.TrimPrefix(entry, "HOME=") != home {
			fakeHome = strings.TrimPrefix(entry, "HOME=")
		}
	}
	if fakeHome == "" {
		t.Fatalf("expected fake HOME override in env, got %v", spec.Env)
	}

	auth, err := os.ReadFile(filepath.Join(fakeHome, ".grok", "auth.json"))
	if err != nil {
		t.Fatalf("auth.json not copied: %v", err)
	}
	if string(auth) != `{"token":"xai-secret"}` {
		t.Fatalf("auth.json content = %q", auth)
	}
	if _, err := os.Stat(filepath.Join(fakeHome, ".grok", "agent_id")); err != nil {
		t.Fatalf("agent_id not copied: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(fakeHome, ".grok", "config.toml"))
	if err != nil {
		t.Fatalf("config.toml not written: %v", err)
	}
	if strings.Contains(string(config), "mcp_servers") {
		t.Fatalf("host config.toml leaked into fake home: %q", config)
	}
	if _, err := os.Stat(filepath.Join(fakeHome, ".claude")); !os.IsNotExist(err) {
		t.Fatal("fake home must not contain .claude")
	}

	// Teardown removes the fake home.
	_ = backend.Close()
	if _, err := os.Stat(fakeHome); !os.IsNotExist(err) {
		t.Fatalf("fake home not removed on Close: %v", err)
	}
}

func TestGrokBackend_CleanContextRequiresAuth(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no .grok/auth.json

	backend := NewGrokBackend(GrokBackendConfig{
		Prompt:  "task",
		Context: "clean",
		StartProcess: func(_ context.Context, _ ClaudeSpawnSpec) (ClaudeProcess, error) {
			t.Fatal("process must not start without grok auth")
			return nil, nil
		},
	})
	t.Cleanup(func() { _ = backend.Close() })

	err := backend.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auth.json") {
		t.Fatalf("Start() error = %v, want auth.json requirement", err)
	}
}

func writeFileOrFail(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
