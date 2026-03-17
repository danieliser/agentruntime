package agent

import (
	"strings"
	"testing"
)

// TestRegistry_DefaultAgents verifies all built-in agents are registered and
// accessible by name — the contract any caller depends on.
func TestRegistry_DefaultAgents(t *testing.T) {
	r := DefaultRegistry()
	for _, name := range []string{"claude", "codex"} {
		if r.Get(name) == nil {
			t.Errorf("expected agent %q to be registered in default registry", name)
		}
	}
}

func TestRegistry_UnknownAgent(t *testing.T) {
	r := DefaultRegistry()
	if r.Get("nonexistent") != nil {
		t.Fatal("expected nil for unknown agent name")
	}
}

func TestRegistry_RegisterOverwrite(t *testing.T) {
	r := NewRegistry()
	r.Register(&ClaudeAgent{})
	r.Register(&ClaudeAgent{}) // second registration — should overwrite, not panic
	if r.Get("claude") == nil {
		t.Fatal("expected claude to be registered after double register")
	}
}

// --- ClaudeAgent ---

func TestClaudeAgent_Name(t *testing.T) {
	a := &ClaudeAgent{}
	if a.Name() != "claude" {
		t.Fatalf("expected name 'claude', got %q", a.Name())
	}
}

func TestClaudeAgent_BuildCmd_Basic(t *testing.T) {
	a := &ClaudeAgent{}
	cmd, err := a.BuildCmd("fix the bug", AgentConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must start with the binary name.
	if cmd[0] != "claude" {
		t.Fatalf("expected cmd[0]='claude', got %q", cmd[0])
	}
	// Must contain the prompt flag.
	if !containsSequence(cmd, "-p", "fix the bug") {
		t.Fatalf("expected -p 'fix the bug' in cmd, got %v", cmd)
	}
	// Must request structured output so callers can parse it.
	if !containsSequence(cmd, "--output-format", "stream-json") {
		t.Fatalf("expected --output-format stream-json in cmd, got %v", cmd)
	}
}

func TestClaudeAgent_BuildCmd_EmptyPrompt(t *testing.T) {
	a := &ClaudeAgent{}
	_, err := a.BuildCmd("", AgentConfig{})
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestClaudeAgent_BuildCmd_WithModel(t *testing.T) {
	a := &ClaudeAgent{}
	cmd, err := a.BuildCmd("hello", AgentConfig{Model: "claude-opus-4-6"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsSequence(cmd, "--model", "claude-opus-4-6") {
		t.Fatalf("expected --model flag in cmd, got %v", cmd)
	}
}

func TestClaudeAgent_BuildCmd_WithSession(t *testing.T) {
	a := &ClaudeAgent{}
	cmd, err := a.BuildCmd("continue", AgentConfig{SessionID: "abc-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must include resume flags when a session is provided.
	if !containsSequence(cmd, "--session-id", "abc-123") {
		t.Fatalf("expected --session-id in cmd, got %v", cmd)
	}
	if !contains(cmd, "--resume") {
		t.Fatalf("expected --resume in cmd, got %v", cmd)
	}
}

func TestClaudeAgent_BuildCmd_WithAllowedTools(t *testing.T) {
	a := &ClaudeAgent{}
	cmd, err := a.BuildCmd("do it", AgentConfig{AllowedTools: []string{"Read", "Write"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsSequence(cmd, "--allowedTools", "Read") {
		t.Fatalf("expected --allowedTools Read, got %v", cmd)
	}
	if !containsSequence(cmd, "--allowedTools", "Write") {
		t.Fatalf("expected --allowedTools Write, got %v", cmd)
	}
}

func TestClaudeAgent_BuildCmd_Interactive(t *testing.T) {
	a := &ClaudeAgent{}
	cmd, err := a.BuildCmd("", AgentConfig{Interactive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contains(cmd, "-p") {
		t.Fatalf("did not expect -p in interactive cmd, got %v", cmd)
	}
}

func TestClaudeAgent_BuildCmd_NoInjection(t *testing.T) {
	// A prompt with shell metacharacters must be passed as a single argument,
	// not interpreted by the shell. Since we use exec (not sh -c), this is
	// guaranteed by the []string representation — verify the raw value survives.
	a := &ClaudeAgent{}
	malicious := "$(rm -rf /); echo pwned"
	cmd, err := a.BuildCmd(malicious, AgentConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The prompt must appear verbatim as a single element, never split.
	if !contains(cmd, malicious) {
		t.Fatalf("expected prompt to appear verbatim in cmd, got %v", cmd)
	}
	// None of the metacharacters should leak into flag names.
	for _, arg := range cmd {
		if arg == "rm" || arg == "echo" {
			t.Fatalf("shell injection detected in cmd: %v", cmd)
		}
	}
}

func TestClaudeAgent_ParseOutput_NoStructuredOutput(t *testing.T) {
	a := &ClaudeAgent{}
	result, ok := a.ParseOutput([]byte("just some text output"))
	if ok {
		t.Fatal("expected ok=false for unstructured output")
	}
	if result != nil {
		t.Fatal("expected nil result for unstructured output")
	}
}

// --- CodexAgent ---

func TestCodexAgent_Name(t *testing.T) {
	a := &CodexAgent{}
	if a.Name() != "codex" {
		t.Fatalf("expected name 'codex', got %q", a.Name())
	}
}

func TestCodexAgent_BuildCmd_Basic(t *testing.T) {
	a := &CodexAgent{}
	cmd, err := a.BuildCmd("write a function", AgentConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd[0] != "codex" {
		t.Fatalf("expected cmd[0]='codex', got %q", cmd[0])
	}
	// Codex uses exec subcommand with --json for structured output.
	if !contains(cmd, "exec") {
		t.Fatalf("expected 'exec' subcommand in codex cmd, got %v", cmd)
	}
	if !contains(cmd, "--json") {
		t.Fatalf("expected --json flag in codex cmd, got %v", cmd)
	}
	if !contains(cmd, "write a function") {
		t.Fatalf("expected prompt in cmd, got %v", cmd)
	}
}

func TestCodexAgent_BuildCmd_EmptyPrompt(t *testing.T) {
	a := &CodexAgent{}
	_, err := a.BuildCmd("", AgentConfig{})
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestCodexAgent_BuildCmd_WithModel(t *testing.T) {
	a := &CodexAgent{}
	cmd, err := a.BuildCmd("task", AgentConfig{Model: "o4-mini"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsSequence(cmd, "--model", "o4-mini") {
		t.Fatalf("expected --model o4-mini in cmd, got %v", cmd)
	}
}

func TestCodexAgent_BuildCmd_Interactive(t *testing.T) {
	a := &CodexAgent{}
	cmd, err := a.BuildCmd("", AgentConfig{Interactive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contains(cmd, "exec") {
		t.Fatalf("did not expect exec in interactive cmd, got %v", cmd)
	}
}

// --- AgentConfig zero value ---

func TestAgentConfig_ZeroValue_AllAgents(t *testing.T) {
	// All agents must handle a zero-value AgentConfig without panicking.
	agents := []Agent{&ClaudeAgent{}, &CodexAgent{}}
	for _, a := range agents {
		t.Run(a.Name(), func(t *testing.T) {
			cmd, err := a.BuildCmd("test prompt", AgentConfig{})
			if err != nil {
				t.Fatalf("unexpected error with zero config: %v", err)
			}
			if len(cmd) == 0 {
				t.Fatal("expected non-empty cmd")
			}
		})
	}
}

// --- Command structure invariants ---

func TestAllAgents_CmdNeverEmpty(t *testing.T) {
	// Every agent must produce at least [binary, prompt] — a single-element
	// cmd cannot be a valid invocation.
	agents := []Agent{&ClaudeAgent{}, &CodexAgent{}}
	for _, a := range agents {
		t.Run(a.Name(), func(t *testing.T) {
			cmd, err := a.BuildCmd("do something", AgentConfig{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cmd) < 2 {
				t.Fatalf("expected at least 2 elements in cmd, got %v", cmd)
			}
		})
	}
}

func TestAllAgents_BinaryMatchesName(t *testing.T) {
	// The first element of cmd must be the agent binary name. This is the
	// contract the runtime's Spawn() depends on.
	agents := []Agent{&ClaudeAgent{}, &CodexAgent{}}
	for _, a := range agents {
		t.Run(a.Name(), func(t *testing.T) {
			cmd, err := a.BuildCmd("test", AgentConfig{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Binary must contain the agent name (opencode has "opencode" as binary).
			if !strings.Contains(cmd[0], a.Name()) {
				t.Fatalf("expected cmd[0] to contain agent name %q, got %q", a.Name(), cmd[0])
			}
		})
	}
}

// --- helpers ---

func contains(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}

// containsSequence checks that target appears immediately after key in slice.
func containsSequence(slice []string, key, target string) bool {
	for i := 0; i < len(slice)-1; i++ {
		if slice[i] == key && slice[i+1] == target {
			return true
		}
	}
	return false
}
