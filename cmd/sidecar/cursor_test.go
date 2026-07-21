package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// spawnCursorBackend spawns a cursor backend against a fake process and
// returns the backend plus the captured spawn spec.
func spawnCursorBackend(t *testing.T, mutate func(*CursorBackendConfig)) (*CursorBackend, *fakeClaudeProcess, ClaudeSpawnSpec) {
	t.Helper()

	proc := newFakeClaudeProcess()
	var gotSpec ClaudeSpawnSpec
	cfg := CursorBackendConfig{
		Binary:           "cursor-agent",
		Prompt:           "build the feature",
		WorkspaceFolders: []string{t.TempDir()},
		StartProcess: func(_ context.Context, spec ClaudeSpawnSpec) (ClaudeProcess, error) {
			gotSpec = spec
			return proc, nil
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}

	backend := NewCursorBackend(cfg)
	t.Cleanup(func() {
		proc.finish(nil)
		_ = backend.Close()
	})
	if err := backend.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return backend, proc, gotSpec
}

func TestCursorBackend_SpawnArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, _, spec := spawnCursorBackend(t, func(cfg *CursorBackendConfig) {
		cfg.Model = "grok-4.5"
	})

	if spec.Command != "cursor-agent" {
		t.Fatalf("command = %q, want cursor-agent", spec.Command)
	}
	for _, flag := range []string{"--print", "--force", "--trust"} {
		if !hasArg(spec.Args, flag) {
			t.Fatalf("missing %s, args = %v", flag, spec.Args)
		}
	}
	if got, _ := argValue(spec.Args, "--output-format"); got != "stream-json" {
		t.Fatalf("--output-format = %q, want stream-json", got)
	}
	if got, _ := argValue(spec.Args, "--model"); got != "grok-4.5" {
		t.Fatalf("--model = %q, want grok-4.5", got)
	}
	if spec.Args[len(spec.Args)-1] != "build the feature" {
		t.Fatalf("prompt must be the final arg, args = %v", spec.Args)
	}
}

func TestCursorBackend_RequiresPrompt(t *testing.T) {
	backend := NewCursorBackend(CursorBackendConfig{
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

func TestCursorBackend_EventMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	backend, proc, _ := spawnCursorBackend(t, nil)

	proc.sendStdoutJSON(t, map[string]any{
		"type": "system", "subtype": "init",
		"session_id": "cursor-sess-1", "model": "Cursor Grok 4.5 High",
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type": "user", "session_id": "cursor-sess-1",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "prompt echo"}}},
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type": "thinking", "subtype": "delta", "text": "pondering", "session_id": "cursor-sess-1",
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type": "thinking", "subtype": "completed", "session_id": "cursor-sess-1",
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type": "assistant", "session_id": "cursor-sess-1",
		"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "Creating file."}}},
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type": "tool_call", "subtype": "started", "call_id": "call-1", "session_id": "cursor-sess-1",
		"tool_call": map[string]any{
			"editToolCall": map[string]any{
				"args": map[string]any{"path": "/tmp/probe.txt", "streamContent": "hello\n"},
			},
		},
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type": "tool_call", "subtype": "completed", "call_id": "call-1", "session_id": "cursor-sess-1",
		"tool_call": map[string]any{
			"editToolCall": map[string]any{
				"args":   map[string]any{"path": "/tmp/probe.txt"},
				"result": map[string]any{"success": map[string]any{"linesAdded": 1}},
			},
		},
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"duration_ms": 6641, "result": "Creating file.DONE", "session_id": "cursor-sess-1",
		"usage": map[string]any{"inputTokens": 17996, "outputTokens": 100},
	})

	system := claudeExpectEventType(t, backend.Events(), "system")
	if claudeEventData(t, system)["subtype"] != "init" {
		t.Fatalf("system subtype = %#v", claudeEventData(t, system)["subtype"])
	}

	// user echo is skipped; next is the thinking delta.
	thought := claudeExpectEventType(t, backend.Events(), "agent_message")
	thoughtData := claudeEventData(t, thought)
	if thoughtData["thought"] != true || thoughtData["text"] != "pondering" {
		t.Fatalf("thinking event data = %#v", thoughtData)
	}

	// thinking/completed emits nothing; next is the assistant message.
	message := claudeExpectEventType(t, backend.Events(), "agent_message")
	messageData := claudeEventData(t, message)
	if messageData["text"] != "Creating file." {
		t.Fatalf("assistant text = %#v", messageData["text"])
	}

	toolUse := claudeExpectEventType(t, backend.Events(), "tool_use")
	toolUseData := claudeEventData(t, toolUse)
	if toolUseData["name"] != "edit" || toolUseData["id"] != "call-1" {
		t.Fatalf("tool_use data = %#v", toolUseData)
	}
	input, _ := toolUseData["input"].(map[string]any)
	if input["path"] != "/tmp/probe.txt" {
		t.Fatalf("tool_use input = %#v", input)
	}

	toolResult := claudeExpectEventType(t, backend.Events(), "tool_result")
	toolResultData := claudeEventData(t, toolResult)
	if toolResultData["name"] != "edit" || toolResultData["output"] == nil {
		t.Fatalf("tool_result data = %#v", toolResultData)
	}

	result := claudeExpectEventType(t, backend.Events(), "result")
	resultData := claudeEventData(t, result)
	if resultData["status"] != "success" || resultData["is_error"] != false {
		t.Fatalf("result data = %#v", resultData)
	}
	if backend.SessionID() != "cursor-sess-1" {
		t.Fatalf("SessionID() = %q, want cursor-sess-1", backend.SessionID())
	}
}

func TestCursorBackend_Contamination(t *testing.T) {
	backend := NewCursorBackend(CursorBackendConfig{})
	got := backend.Contamination()
	if len(got) != 1 || got[0] != "cursor-account-rules" {
		t.Fatalf("Contamination() = %v, want [cursor-account-rules]", got)
	}
}

func TestCursorBackend_CleanContextFakeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Host config that must NOT reach the fake home.
	if err := os.MkdirAll(home+"/.cursor", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileOrFail(t, home+"/.cursor/AGENTS.md", "host cursor rules")
	if runtime.GOOS == "darwin" {
		if err := os.MkdirAll(home+"/Library/Keychains", 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Injection vectors that must not survive clean context.
	t.Setenv("NODE_OPTIONS", "--require /tmp/evil.js")
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	backend, _, spec := spawnCursorBackend(t, func(cfg *CursorBackendConfig) {
		cfg.Context = "clean"
	})

	for _, key := range []string{"NODE_OPTIONS", "XDG_CONFIG_HOME"} {
		if envHasKey(spec.Env, key) {
			t.Fatalf("clean context env must strip %s: %v", key, spec.Env)
		}
	}

	fakeHome := ""
	for _, entry := range spec.Env {
		if strings.HasPrefix(entry, "HOME=") && strings.TrimPrefix(entry, "HOME=") != home {
			fakeHome = strings.TrimPrefix(entry, "HOME=")
		}
	}
	if fakeHome == "" {
		t.Fatalf("expected fake HOME override in env, got %v", spec.Env)
	}

	if _, err := os.Stat(filepath.Join(fakeHome, ".cursor")); !os.IsNotExist(err) {
		t.Fatal("fake home must not contain .cursor")
	}
	if runtime.GOOS == "darwin" {
		link, err := os.Readlink(filepath.Join(fakeHome, "Library", "Keychains"))
		if err != nil {
			t.Fatalf("keychain symlink missing: %v", err)
		}
		if link != filepath.Join(home, "Library", "Keychains") {
			t.Fatalf("keychain symlink = %q, want real keychain dir", link)
		}
	}

	_ = backend.Close()
	if _, err := os.Stat(fakeHome); !os.IsNotExist(err) {
		t.Fatalf("fake home not removed on Close: %v", err)
	}
}

// TestCursorBackend_CloseRacesNaturalExit reproduces the double-close panic:
// API deletion (Close) racing natural process exit (waitForExit) both closed
// the done channel behind a non-atomic select-then-close. Run with -race.
func TestCursorBackend_CloseRacesNaturalExit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for i := 0; i < 50; i++ {
		backend, proc, _ := spawnCursorBackend(t, nil)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			proc.finish(nil)
		}()
		go func() {
			defer wg.Done()
			_ = backend.Close()
		}()
		wg.Wait()
	}
}

func TestCursorBackend_ToolNameExtraction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	backend, proc, _ := spawnCursorBackend(t, nil)

	raw, err := json.Marshal(map[string]any{
		"type": "tool_call", "subtype": "started", "call_id": "call-9",
		"tool_call": map[string]any{
			"shellToolCall": map[string]any{"args": map[string]any{"command": "ls"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend.handleStdoutLine(raw)
	_ = proc // spawn helper keeps the process alive

	event := claudeExpectEventType(t, backend.Events(), "tool_use")
	data := claudeEventData(t, event)
	if data["name"] != "shell" {
		t.Fatalf("tool name = %#v, want shell", data["name"])
	}
}
