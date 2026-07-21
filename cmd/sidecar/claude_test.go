package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaudeBackend_SpawnWithDualChannel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	proc := newFakeClaudeProcess()
	var gotSpec ClaudeSpawnSpec

	backend := NewClaudeBackend(ClaudeBackendConfig{
		Binary:           "claude",
		SessionID:        "sess-dual",
		WorkspaceFolders: []string{t.TempDir()},
		StartProcess: func(_ context.Context, spec ClaudeSpawnSpec) (ClaudeProcess, error) {
			gotSpec = spec
			return proc, nil
		},
	})
	t.Cleanup(func() { _ = backend.Stop() })

	if err := backend.Spawn(context.Background()); err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}

	if gotSpec.Command != "claude" {
		t.Fatalf("command = %q, want claude", gotSpec.Command)
	}
	wantArgs := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
		"--ide",
		"--session-id", "sess-dual",
	}
	if !claudeEqualStrings(gotSpec.Args, wantArgs) {
		t.Fatalf("args = %v, want %v", gotSpec.Args, wantArgs)
	}
	if !containsEnvVar(gotSpec.Env, "ENABLE_IDE_INTEGRATION=true") {
		t.Fatalf("expected ENABLE_IDE_INTEGRATION env, got %v", gotSpec.Env)
	}
	if !containsEnvPrefix(gotSpec.Env, "CLAUDE_CODE_SSE_PORT=") {
		t.Fatalf("expected CLAUDE_CODE_SSE_PORT env, got %v", gotSpec.Env)
	}
	if !containsEnvVar(gotSpec.Env, "CLAUDE_CODE_EXIT_AFTER_STOP_DELAY=0") {
		t.Fatalf("expected EXIT_AFTER_STOP_DELAY env, got %v", gotSpec.Env)
	}
	if backend.currentMCP() == nil {
		t.Fatal("expected MCP server to start before spawn")
	}
	if _, err := os.Stat(backend.currentMCP().LockFile()); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}

func TestClaudeBackend_SendPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	proc := newFakeClaudeProcess()
	backend := testClaudeBackend(t, proc)

	if err := backend.SendPrompt("fix the bug"); err != nil {
		t.Fatalf("SendPrompt() error = %v", err)
	}

	got := strings.TrimSpace(proc.stdin.String())
	// Verify the Anthropic API message format
	if !strings.Contains(got, `"type":"user"`) {
		t.Fatalf("expected type:user in stdin, got %q", got)
	}
	if !strings.Contains(got, `"role":"user"`) {
		t.Fatalf("expected role:user in stdin, got %q", got)
	}
	if !strings.Contains(got, `"text":"fix the bug"`) {
		t.Fatalf("expected prompt text in stdin, got %q", got)
	}
}

func TestClaudeBackend_EventMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	proc := newFakeClaudeProcess()
	backend := testClaudeBackend(t, proc)

	proc.sendStdoutJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Hello "},
				{"type": "tool_use", "id": "toolu_1", "name": "Edit", "input": map[string]any{"file_path": "/tmp/app.go"}},
				{"type": "text", "text": "world"},
			},
			"usage": map[string]any{
				"input_tokens":                11,
				"output_tokens":               7,
				"cache_read_input_tokens":     3,
				"cache_creation_input_tokens": 2,
			},
		},
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type":    "progress",
		"status":  "running",
		"message": "Reading files",
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": "sess-events",
	})
	proc.sendStdoutJSON(t, map[string]any{
		"type":        "result",
		"subtype":     "success",
		"cost_usd":    0.42,
		"duration_ms": 99,
		"session_id":  "sess-events",
		"num_turns":   2,
	})

	agentMessage := claudeExpectEventType(t, backend.Events(), "agent_message")
	agentData := claudeEventData(t, agentMessage)
	if agentData["text"] != "Hello world" {
		t.Fatalf("agent text = %#v, want Hello world", agentData["text"])
	}
	usage, ok := agentData["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage type = %T, want map[string]any", agentData["usage"])
	}
	if usage["input_tokens"] != float64(11) && usage["input_tokens"] != 11 {
		t.Fatalf("unexpected usage: %#v", usage)
	}

	toolUse := claudeExpectEventType(t, backend.Events(), "tool_use")
	toolData := claudeEventData(t, toolUse)
	if toolData["name"] != "Edit" {
		t.Fatalf("tool name = %#v, want Edit", toolData["name"])
	}

	progress := claudeExpectEventType(t, backend.Events(), "progress")
	progressData := claudeEventData(t, progress)
	if progressData["message"] != "Reading files" {
		t.Fatalf("progress message = %#v, want Reading files", progressData["message"])
	}

	system := claudeExpectEventType(t, backend.Events(), "system")
	systemData := claudeEventData(t, system)
	if systemData["subtype"] != "init" {
		t.Fatalf("system subtype = %#v, want init", systemData["subtype"])
	}

	result := claudeExpectEventType(t, backend.Events(), "result")
	resultData := claudeEventData(t, result)
	if resultData["session_id"] != "sess-events" {
		t.Fatalf("result session_id = %#v, want sess-events", resultData["session_id"])
	}
}

func TestClaudeBackend_AutoApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	proc := newFakeClaudeProcess()
	testClaudeBackend(t, proc)

	proc.sendStdoutJSON(t, map[string]any{
		"type": "control_request",
		"request": map[string]any{
			"request_id": "req-123",
			"subtype":    "can_use_tool",
			"tool_name":  "Bash",
		},
	})

	waitFor(t, 2*time.Second, func() bool {
		data := proc.Bytes()
		return bytes.Contains(data, []byte(`"request_id":"req-123"`)) &&
			bytes.Contains(data, []byte(`"behavior":"allow"`))
	})
}

func TestClaudeBackend_ContextInjection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	proc := newFakeClaudeProcess()
	backend := testClaudeBackend(t, proc)

	conn := mustDialMCP(t, backend.currentMCP(), http.Header{
		"x-claude-code-ide-authorization": []string{backend.currentMCP().AuthToken()},
	})
	defer conn.Close()

	if err := backend.SendContext("selected text", "/tmp/app.go"); err != nil {
		t.Fatalf("SendContext() error = %v", err)
	}

	msg := readJSONMessage(t, conn)
	if msg["method"] != "selection_changed" {
		t.Fatalf("method = %v, want selection_changed", msg["method"])
	}
	params := mcpMapField(t, msg, "params")
	if params["text"] != "selected text" {
		t.Fatalf("text = %#v, want selected text", params["text"])
	}
	if params["filePath"] != "/tmp/app.go" {
		t.Fatalf("filePath = %#v, want /tmp/app.go", params["filePath"])
	}
}

// spawnClaudePromptMode spawns a prompt-mode backend with the given config
// overrides and returns the captured spawn spec.
func spawnClaudePromptMode(t *testing.T, mutate func(*ClaudeBackendConfig)) ClaudeSpawnSpec {
	t.Helper()

	proc := newFakeClaudeProcess()
	var gotSpec ClaudeSpawnSpec
	cfg := ClaudeBackendConfig{
		Binary:           "claude",
		SessionID:        "sess-prompt",
		Prompt:           "do the thing",
		WorkspaceFolders: []string{t.TempDir()},
		StartProcess: func(_ context.Context, spec ClaudeSpawnSpec) (ClaudeProcess, error) {
			gotSpec = spec
			return proc, nil
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}

	backend := NewClaudeBackend(cfg)
	t.Cleanup(func() {
		proc.finish(nil)
		_ = backend.Stop()
	})
	if err := backend.Spawn(context.Background()); err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	return gotSpec
}

func argValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasArg(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func TestClaudeBackend_EffortXHighPairsAlwaysThinking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, effort := range []string{"xhigh", "max"} {
		spec := spawnClaudePromptMode(t, func(cfg *ClaudeBackendConfig) {
			cfg.SessionID = "sess-effort-" + effort
			cfg.Effort = effort
		})

		if got, _ := argValue(spec.Args, "--effort"); got != effort {
			t.Fatalf("--effort = %q, want %q", got, effort)
		}
		settingsJSON, ok := argValue(spec.Args, "--settings")
		if !ok {
			t.Fatalf("effort %s: expected --settings flag, args = %v", effort, spec.Args)
		}
		var settings map[string]any
		if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
			t.Fatalf("settings JSON invalid: %v", err)
		}
		if settings["alwaysThinkingEnabled"] != true {
			t.Fatalf("effort %s: alwaysThinkingEnabled = %#v, want true", effort, settings["alwaysThinkingEnabled"])
		}
	}
}

func TestClaudeBackend_EffortHighNoSettingsOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	spec := spawnClaudePromptMode(t, func(cfg *ClaudeBackendConfig) {
		cfg.Effort = "high"
	})

	if hasArg(spec.Args, "--settings") {
		t.Fatalf("effort high should not add --settings, args = %v", spec.Args)
	}
}

func TestClaudeBackend_CleanContextIsolationFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A host-materialized .mcp.json must NOT be passed in clean mode.
	if err := os.MkdirAll(home+"/.claude", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.claude/.mcp.json", []byte(`{"mcpServers":{"leaky":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Injection vectors that must not survive clean context.
	t.Setenv("NODE_OPTIONS", "--require /tmp/evil.js")
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	spec := spawnClaudePromptMode(t, func(cfg *ClaudeBackendConfig) {
		cfg.CleanContext = true
	})

	if !hasArg(spec.Args, "--safe-mode") {
		t.Fatalf("expected --safe-mode, args = %v", spec.Args)
	}
	for _, key := range []string{"NODE_OPTIONS", "XDG_CONFIG_HOME"} {
		if envHasKey(spec.Env, key) {
			t.Fatalf("clean context env must strip %s: %v", key, spec.Env)
		}
	}
	mcpPath, ok := argValue(spec.Args, "--mcp-config")
	if !ok {
		t.Fatalf("expected --mcp-config, args = %v", spec.Args)
	}
	if strings.Contains(mcpPath, home) {
		t.Fatalf("clean context used host mcp config %q", mcpPath)
	}
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read empty mcp config: %v", err)
	}
	if string(data) != `{"mcpServers":{}}` {
		t.Fatalf("empty mcp config content = %q", data)
	}
	if !hasArg(spec.Args, "--strict-mcp-config") {
		t.Fatalf("expected --strict-mcp-config, args = %v", spec.Args)
	}
	if !hasArg(spec.Args, "--no-chrome") {
		t.Fatalf("expected --no-chrome, args = %v", spec.Args)
	}
	if hasArg(spec.Args, "--system-prompt") {
		t.Fatalf("clean context must not force a system prompt (--safe-mode covers persona), args = %v", spec.Args)
	}

	settingsJSON, ok := argValue(spec.Args, "--settings")
	if !ok {
		t.Fatalf("expected --settings, args = %v", spec.Args)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		t.Fatalf("settings JSON invalid: %v", err)
	}
	if hooks, isMap := settings["hooks"].(map[string]any); !isMap || len(hooks) != 0 {
		t.Fatalf("settings hooks = %#v, want {}", settings["hooks"])
	}
	if v, present := settings["statusLine"]; !present || v != nil {
		t.Fatalf("settings statusLine = %#v, want null", v)
	}
	if plugins, isMap := settings["enabledPlugins"].(map[string]any); !isMap || len(plugins) != 0 {
		t.Fatalf("settings enabledPlugins = %#v, want {}", settings["enabledPlugins"])
	}
}

func TestClaudeBackend_CleanContextCustomSystemPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	spec := spawnClaudePromptMode(t, func(cfg *ClaudeBackendConfig) {
		cfg.CleanContext = true
		cfg.SystemPrompt = "You are a game dev bot."
	})

	if got, _ := argValue(spec.Args, "--system-prompt"); got != "You are a game dev bot." {
		t.Fatalf("--system-prompt = %q, want custom prompt", got)
	}
}

func TestClaudeBackend_BareRequiresAPIKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")

	backend := NewClaudeBackend(ClaudeBackendConfig{
		Binary: "claude",
		Prompt: "task",
		Bare:   true,
		StartProcess: func(_ context.Context, _ ClaudeSpawnSpec) (ClaudeProcess, error) {
			t.Fatal("process should not start without API key in bare mode")
			return nil, nil
		},
	})
	t.Cleanup(func() { _ = backend.Stop() })

	err := backend.Spawn(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("Spawn() error = %v, want ANTHROPIC_API_KEY requirement", err)
	}
}

func TestClaudeBackend_BareAllowedWithAPIKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")

	spec := spawnClaudePromptMode(t, func(cfg *ClaudeBackendConfig) {
		cfg.Bare = true
		cfg.ExtraEnv = map[string]string{"ANTHROPIC_API_KEY": "sk-test"}
	})

	if !hasArg(spec.Args, "--bare") {
		t.Fatalf("expected --bare, args = %v", spec.Args)
	}
}

type fakeClaudeProcess struct {
	stdinMu sync.Mutex
	stdin   bytes.Buffer

	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter

	waitOnce sync.Once
	waitCh   chan error
}

func newFakeClaudeProcess() *fakeClaudeProcess {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &fakeClaudeProcess{
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		stderrR: stderrR,
		stderrW: stderrW,
		waitCh:  make(chan error, 1),
	}
}

func (p *fakeClaudeProcess) Stdin() io.WriteCloser { return fakeWriteCloser{Writer: p} }
func (p *fakeClaudeProcess) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeClaudeProcess) Stderr() io.ReadCloser { return p.stderrR }
func (p *fakeClaudeProcess) Wait() error           { return <-p.waitCh }
func (p *fakeClaudeProcess) Kill() error           { p.finish(nil); return nil }

func (p *fakeClaudeProcess) Write(data []byte) (int, error) {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	return p.stdin.Write(data)
}

func (p *fakeClaudeProcess) Bytes() []byte {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	return append([]byte(nil), p.stdin.Bytes()...)
}

func (p *fakeClaudeProcess) String() string {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	return p.stdin.String()
}

func (p *fakeClaudeProcess) sendStdoutJSON(t *testing.T, payload map[string]any) {
	t.Helper()
	if err := json.NewEncoder(p.stdoutW).Encode(payload); err != nil {
		t.Fatalf("send stdout json: %v", err)
	}
}

func (p *fakeClaudeProcess) finish(err error) {
	p.waitOnce.Do(func() {
		_ = p.stdoutW.Close()
		_ = p.stderrW.Close()
		p.waitCh <- err
		close(p.waitCh)
	})
}

type fakeWriteCloser struct {
	io.Writer
}

func (fakeWriteCloser) Close() error { return nil }

func testClaudeBackend(t *testing.T, proc *fakeClaudeProcess) *ClaudeBackend {
	t.Helper()

	backend := NewClaudeBackend(ClaudeBackendConfig{
		Binary:           "claude",
		SessionID:        "sess-events",
		WorkspaceFolders: []string{t.TempDir()},
		StartProcess: func(_ context.Context, _ ClaudeSpawnSpec) (ClaudeProcess, error) {
			return proc, nil
		},
	})
	t.Cleanup(func() {
		proc.finish(nil)
		_ = backend.Stop()
	})

	if err := backend.Spawn(context.Background()); err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	return backend
}

func claudeExpectEventType(t *testing.T, ch <-chan Event, want string) Event {
	t.Helper()
	select {
	case event := <-ch:
		if event.Type != want {
			t.Fatalf("event type = %q, want %q (%#v)", event.Type, want, event.Data)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q event", want)
		return Event{}
	}
}

func claudeEventData(t *testing.T, event Event) map[string]any {
	t.Helper()
	data, ok := event.Data.(map[string]any)
	if !ok {
		t.Fatalf("event data type = %T, want map[string]any", event.Data)
	}
	return data
}

func claudeEqualStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func containsEnvVar(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func containsEnvPrefix(env []string, prefix string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
