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
