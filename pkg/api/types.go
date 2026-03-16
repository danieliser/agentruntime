package api

import "time"

// SessionRequest is the top-level dispatch shape for creating agent sessions.
// Three equal dispatch paths use this struct: HTTP JSON, Go SDK, CLI YAML file.
//
// Minimum valid request: Agent + Prompt + (WorkDir or a Mount).
type SessionRequest struct {
	// Task identity
	TaskID    string            `json:"task_id,omitempty"    yaml:"task_id,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"       yaml:"tags,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"` // default: 300000

	// What to run
	Agent   string `json:"agent"              yaml:"agent"`
	Runtime string `json:"runtime,omitempty"  yaml:"runtime,omitempty"` // "local" | "docker" (default: server default)
	Prompt  string `json:"prompt"             yaml:"prompt"`

	// Filesystem — explicit multi-mount with access modes.
	// WorkDir is shorthand: becomes {Host: val, Container: "/workspace", Mode: "rw"}.
	WorkDir string  `json:"work_dir,omitempty" yaml:"work_dir,omitempty"`
	Mounts  []Mount `json:"mounts,omitempty"   yaml:"mounts,omitempty"`

	// Agent-specific config — caller provides content/paths, agentruntime places files.
	// Only the block matching Agent is read; others are ignored.
	Claude *ClaudeConfig `json:"claude,omitempty" yaml:"claude,omitempty"`
	Codex  *CodexConfig  `json:"codex,omitempty"  yaml:"codex,omitempty"`

	// MCP servers — materialized into agent's config at spawn time.
	MCPServers []MCPServer `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`

	// Clean-room env: only these vars enter the container. Never inherits host env.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// Container resources — fully optional, sane defaults applied if omitted.
	Resources *Resources `json:"resources,omitempty" yaml:"resources,omitempty"`
}

// Mount describes a bind-mount between host and container.
type Mount struct {
	Host      string `json:"host"      yaml:"host"`
	Container string `json:"container" yaml:"container"`
	Mode      string `json:"mode"      yaml:"mode"` // "rw" | "ro"
}

// ClaudeConfig holds pre-materialized content/paths for Claude Code.
// agentruntime writes these to the correct paths inside the container.
// Content fields take priority over path fields if both are set.
type ClaudeConfig struct {
	// Inline content — written to files inside the container.
	SettingsJSON map[string]any `json:"settings_json,omitempty" yaml:"settings_json,omitempty"` // → ~/.claude/settings.json
	ClaudeMD     string         `json:"claude_md,omitempty"     yaml:"claude_md,omitempty"`     // → ~/.claude/CLAUDE.md
	McpJSON      map[string]any `json:"mcp_json,omitempty"      yaml:"mcp_json,omitempty"`      // → ~/.claude/.mcp.json (merged with MCPServers)

	// Host paths — bind-mounted read-only into the container.
	CredentialsPath string `json:"credentials_path,omitempty" yaml:"credentials_path,omitempty"` // → ~/.claude/credentials.json
	MemoryPath      string `json:"memory_path,omitempty"      yaml:"memory_path,omitempty"`      // → ~/.claude/projects/{hash}/

	// CLI flags
	OutputFormat string `json:"output_format,omitempty" yaml:"output_format,omitempty"` // default: "stream-json"
}

// CodexConfig holds pre-materialized content/paths for Codex.
type CodexConfig struct {
	ConfigTOML   map[string]any `json:"config_toml,omitempty"   yaml:"config_toml,omitempty"`   // → ~/.codex/config.toml
	Instructions string         `json:"instructions,omitempty"  yaml:"instructions,omitempty"`  // → ~/.codex/instructions.md
	ApprovalMode string         `json:"approval_mode,omitempty" yaml:"approval_mode,omitempty"` // "auto-edit" | "suggest" | "full-auto"
}

// MCPServer describes an MCP server to inject into the agent's config.
type MCPServer struct {
	Name  string            `json:"name"            yaml:"name"`
	Type  string            `json:"type"            yaml:"type"`           // "http" | "stdio" | "websocket"
	URL   string            `json:"url,omitempty"   yaml:"url,omitempty"`  // supports ${HOST_GATEWAY}
	Cmd   []string          `json:"cmd,omitempty"   yaml:"cmd,omitempty"`  // for stdio type
	Env   map[string]string `json:"env,omitempty"   yaml:"env,omitempty"`
	Token string            `json:"token,omitempty" yaml:"token,omitempty"`
}

// Resources holds optional container resource constraints.
// If omitted entirely, sane defaults are applied:
// --cap-drop ALL --cap-add DAC_OVERRIDE --security-opt no-new-privileges:true
type Resources struct {
	Image       string   `json:"image,omitempty"        yaml:"image,omitempty"`        // default: "ubuntu:22.04"
	Memory      string   `json:"memory,omitempty"       yaml:"memory,omitempty"`       // e.g. "4g"
	CPUs        float64  `json:"cpus,omitempty"         yaml:"cpus,omitempty"`         // e.g. 2.0
	Network     string   `json:"network,omitempty"      yaml:"network,omitempty"`      // default: "bridge"
	PTY         bool     `json:"pty,omitempty"          yaml:"pty,omitempty"`
	SecurityOpt []string `json:"security_opt,omitempty" yaml:"security_opt,omitempty"`
}

// SessionResponse is returned by POST /sessions.
type SessionResponse struct {
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id,omitempty"`
	Agent     string `json:"agent"`
	Runtime   string `json:"runtime"`
	Status    string `json:"status"`
	WSURL     string `json:"ws_url"`
	LogURL    string `json:"log_url"`
}

// SessionSummary is returned by GET /sessions.
type SessionSummary struct {
	SessionID string            `json:"session_id"`
	TaskID    string            `json:"task_id,omitempty"`
	Agent     string            `json:"agent"`
	Runtime   string            `json:"runtime"`
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// EffectiveMounts resolves WorkDir shorthand into the Mounts list.
// Returns a new slice — does not modify the original request.
func (r *SessionRequest) EffectiveMounts() []Mount {
	mounts := make([]Mount, 0, len(r.Mounts)+1)
	if r.WorkDir != "" {
		mounts = append(mounts, Mount{
			Host:      r.WorkDir,
			Container: "/workspace",
			Mode:      "rw",
		})
	}
	mounts = append(mounts, r.Mounts...)
	return mounts
}

// EffectiveTimeout returns the timeout in milliseconds, defaulting to 300000 (5min).
func (r *SessionRequest) EffectiveTimeout() int {
	if r.TimeoutMs > 0 {
		return r.TimeoutMs
	}
	return 300000
}
