// Package agent defines the Agent interface and built-in agent definitions
// for AI coding assistants (Claude Code, Codex, OpenCode).
package agent

// Agent knows how to construct the CLI command for a specific AI tool.
// It does not manage processes — that's the runtime's job. This separation
// means any agent can run on any runtime without coupling.
type Agent interface {
	// BuildCmd returns the command and arguments to spawn this agent with
	// the given prompt and configuration. The returned slice is passed
	// directly to Runtime.Spawn as SpawnConfig.Cmd.
	BuildCmd(prompt string, cfg AgentConfig) ([]string, error)

	// Name returns the agent identifier ("claude", "codex", "opencode").
	Name() string

	// ParseOutput reads raw output bytes and extracts a structured result
	// if the agent emits one. Returns nil, false if no structured output found.
	ParseOutput(output []byte) (*AgentResult, bool)
}

// AgentConfig holds per-invocation configuration for an agent.
type AgentConfig struct {
	// Model overrides the default model for this invocation (e.g., "claude-sonnet-4-6").
	Model string

	// MaxTokens limits the response length.
	MaxTokens int

	// WorkDir is the working directory for the agent process.
	WorkDir string

	// SessionID identifies the agentruntime session for this invocation.
	SessionID string

	// ResumeSessionID resumes a prior agent-native session if supported.
	ResumeSessionID string

	// Interactive keeps stdin open and avoids argv prompt injection.
	Interactive bool

	// AllowedTools restricts which tools the agent can use (agent-specific).
	AllowedTools []string

	// Env provides additional environment variables.
	Env map[string]string
}

// AgentResult holds structured output extracted from agent process output.
type AgentResult struct {
	// Summary is a human-readable summary of what the agent did.
	Summary string

	// ExitCode is the agent's self-reported exit status (distinct from process exit code).
	ExitCode int

	// Metadata holds agent-specific structured data.
	Metadata map[string]any
}

// Registry maps agent names to their implementations.
type Registry struct {
	agents map[string]Agent
}

// NewRegistry creates an empty agent registry.
func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]Agent)}
}

// Register adds an agent to the registry.
func (r *Registry) Register(a Agent) {
	r.agents[a.Name()] = a
}

// Get returns the agent for the given name, or nil if not found.
func (r *Registry) Get(name string) Agent {
	return r.agents[name]
}

// DefaultRegistry returns a registry pre-loaded with built-in agents.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(&ClaudeAgent{})
	r.Register(&CodexAgent{})
	r.Register(&OpenCodeAgent{})
	return r
}
