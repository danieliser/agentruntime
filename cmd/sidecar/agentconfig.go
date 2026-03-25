package main

import (
	"encoding/json"
	"os"
)

// AgentConfig is the third config channel between the daemon and sidecar.
// It carries agent-tuning fields that don't belong in AGENT_CMD (the argv)
// or AGENT_PROMPT (the initial user prompt). The daemon serializes this as
// JSON into the AGENT_CONFIG env var; the sidecar parses it at startup and
// threads the fields into the appropriate backend.
type AgentConfig struct {
	// Model overrides the agent's default model (e.g. "claude-opus-4-5", "o3").
	Model string `json:"model,omitempty"`

	// ResumeSession is the session ID to resume instead of starting fresh.
	// For Claude this maps to --resume --session-id; for Codex it's a thread ID.
	ResumeSession string `json:"resume_session,omitempty"`

	// Env is additional environment variables merged into the agent process env.
	// These are layered on top of the sidecar's own clean env.
	Env map[string]string `json:"env,omitempty"`

	// ApprovalMode controls the agent's tool-approval policy.
	// Claude: ignored (always dangerously-skip-permissions in sidecar).
	// Codex: "full-auto" | "auto-edit" | "suggest" (default: "full-auto").
	ApprovalMode string `json:"approval_mode,omitempty"`

	// MaxTurns limits the number of agentic turns (Claude --max-turns).
	MaxTurns int `json:"max_turns,omitempty"`

	// AllowedTools restricts which tools the agent can use (Claude --allowedTools).
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// Effort controls the agent's effort level (Claude --effort).
	Effort string `json:"effort,omitempty"`

	// StallWarningTimeout is seconds of event-stream silence before emitting
	// an advisory stall_warning system event. Default: 600 (10 min). 0 = use default. -1 = disabled.
	StallWarningTimeout int `json:"stall_warning_timeout,omitempty"`

	// StallKillTimeout is seconds of event-stream silence before force-killing
	// the agent process. Default: 3000 (50 min). 0 = use default. -1 = disabled.
	StallKillTimeout int `json:"stall_kill_timeout,omitempty"`

	// ResultGracePeriod is seconds to wait after a result event for the process
	// to exit before force-killing. Default: 10. 0 = use default. -1 = disabled.
	ResultGracePeriod int `json:"result_grace_period,omitempty"`

	// Team fields — enable Claude Code Agent Teams inbox protocol.
	// When set, the Claude backend appends --agent-id, --agent-name, --team-name
	// flags and sets CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 env var.
	TeamName      string `json:"team_name,omitempty"`
	TeamAgentName string `json:"team_agent_name,omitempty"`
	TeamAgentID   string `json:"team_agent_id,omitempty"`

	// Bare mode — skip hooks, plugins, LSP, automem, CLAUDE.md (clean room).
	Bare bool `json:"bare,omitempty"`

	// Lifecycle hooks — scripts executed at defined points in the session lifecycle.
	Lifecycle *LifecycleConfig `json:"lifecycle,omitempty"`
}

// LifecycleConfig specifies scripts to run at defined points in the session lifecycle.
type LifecycleConfig struct {
	// PreInit runs BEFORE the agent binary starts. Blocking.
	PreInit string `json:"pre_init,omitempty"`

	// PostInit runs AFTER the agent is alive but BEFORE the first prompt. Blocking.
	PostInit string `json:"post_init,omitempty"`

	// Sidecar is spawned as a background process alongside the agent.
	Sidecar string `json:"sidecar,omitempty"`

	// PostRun runs AFTER the agent exits. Blocking.
	PostRun string `json:"post_run,omitempty"`

	// HookTimeout is the timeout in seconds for blocking hooks. Default: 30.
	HookTimeout int `json:"hook_timeout,omitempty"`
}

// HasHooks reports whether any lifecycle hooks are configured.
func (c *LifecycleConfig) HasHooks() bool {
	return c != nil && (c.PreInit != "" || c.PostInit != "" || c.Sidecar != "" || c.PostRun != "")
}

// BlockingTimeout returns the timeout for blocking hooks (pre_init, post_init).
func (c *LifecycleConfig) BlockingTimeout() int {
	if c != nil && c.HookTimeout > 0 {
		return c.HookTimeout
	}
	return 30
}

// PostRunTimeout returns the timeout for the post_run hook (2x blocking timeout).
func (c *LifecycleConfig) PostRunTimeout() int {
	return c.BlockingTimeout() * 2
}

// parseAgentConfig reads and parses the AGENT_CONFIG env var.
// Returns a zero-value AgentConfig if the env var is unset or empty.
func parseAgentConfig() (AgentConfig, error) {
	raw := os.Getenv("AGENT_CONFIG")
	if raw == "" {
		return AgentConfig{}, nil
	}
	var cfg AgentConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return AgentConfig{}, err
	}
	return cfg, nil
}
