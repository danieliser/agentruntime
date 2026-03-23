package schema

import (
	"encoding/json"
	"time"
)

// HealthResponse is returned by GET /health.
type HealthResponse struct {
	Status  string `json:"status"`
	Runtime string `json:"runtime"`
}

// SessionRequest is the top-level dispatch shape for creating agent sessions.
// Three equal dispatch paths use this struct: HTTP JSON, Go SDK, CLI YAML file.
//
// Minimum valid request: Agent + Prompt + (WorkDir or a Mount).
type SessionRequest struct {
	// Task identity
	SessionID string            `json:"session_id,omitempty" yaml:"session_id,omitempty"` // caller-defined session ID (must be valid UUID if set)
	TaskID    string            `json:"task_id,omitempty" yaml:"task_id,omitempty"`
	Name   string            `json:"name,omitempty"    yaml:"name,omitempty"` // human label for observability
	Tags   map[string]string `json:"tags,omitempty"    yaml:"tags,omitempty"`

	// What to run
	Agent   string `json:"agent"              yaml:"agent"`
	Runtime string `json:"runtime,omitempty"  yaml:"runtime,omitempty"` // "local" | "docker" (default: server default)
	Model   string `json:"model,omitempty"    yaml:"model,omitempty"`   // cross-agent convenience (e.g. "claude-opus-4-5")
	Prompt  string `json:"prompt"             yaml:"prompt"`

	// Timing
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"` // duration string: "5m", "1h30m" (default: "5m")

	// Session behavior
	PTY            bool   `json:"pty,omitempty"            yaml:"pty,omitempty"`            // allocate PTY for interactive agents
	Interactive    bool   `json:"interactive,omitempty"    yaml:"interactive,omitempty"`    // keep stdin open and steer via WS stdin frames
	ResumeSession  string `json:"resume_session,omitempty" yaml:"resume_session,omitempty"` // session ID to resume
	PersistSession bool   `json:"persist_session,omitempty" yaml:"persist_session,omitempty"` // create named Docker volume for session persistence

	// Filesystem — explicit multi-mount with access modes.
	// WorkDir is shorthand: becomes {Host: val, Container: "/workspace", Mode: "rw"}.
	WorkDir string  `json:"work_dir,omitempty" yaml:"work_dir,omitempty"`
	Mounts  []Mount `json:"mounts,omitempty"   yaml:"mounts,omitempty"`

	// Agent-specific config — caller provides content/paths, agentruntime places files.
	// Only the block matching Agent is read; others are ignored.
	Claude *ClaudeConfig `json:"claude,omitempty" yaml:"claude,omitempty"`
	Codex  *CodexConfig  `json:"codex,omitempty"  yaml:"codex,omitempty"`

	// AutoDiscover controls configuration auto-discovery from the filesystem.
	// Supports three forms:
	//
	// 1. bool (shorthand):
	//    true  — enable ALL discovery categories
	//    false — disable ALL discovery (explicit config only)
	//    nil/unset — platform default (true for Docker, true for local)
	//
	// 2. map[string]bool (granular):
	//    {"claude_md": true, "settings": false, ...}
	//    All unspecified keys default to false (opt-in per category)
	//
	// Valid category keys:
	//   Claude:  "claude_md", "settings", "mcp", "rules", "agents"
	//   Codex:   "agents_md", "config_toml"
	AutoDiscover interface{} `json:"auto_discover,omitempty" yaml:"auto_discover,omitempty"`

	// MCP servers — materialized into agent's config at spawn time.
	MCPServers []MCPServer `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`

	// Clean-room env: only these vars enter the container. Never inherits host env.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// Container settings — image, resource limits, network, security options.
	// Renamed from "resources" because image isn't a resource — honest naming.
	Container *ContainerConfig `json:"container,omitempty" yaml:"container,omitempty"`

	// Team config — enables Claude Code's Agent Teams inbox protocol.
	// When set, the agent is spawned with --agent-id, --agent-name, --team-name
	// flags and CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 env var.
	// The orchestrator must scaffold the team directory before spawning.
	Team *TeamConfig `json:"team,omitempty" yaml:"team,omitempty"`
}

// Mount describes a bind-mount or named volume mount between host/volume and container.
type Mount struct {
	Host      string `json:"host"      yaml:"host"`      // host path (for bind-mount) or volume name (for "volume" type)
	Container string `json:"container" yaml:"container"`
	Mode      string `json:"mode"      yaml:"mode"` // "rw" | "ro"
	Type      string `json:"type"      yaml:"type"` // "bind" (default) | "volume"
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

	// MaxTurns limits the number of agentic turns (Claude --max-turns).
	MaxTurns int `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`

	// AllowedTools restricts which tools the agent can use (Claude --allowedTools).
	AllowedTools []string `json:"allowed_tools,omitempty" yaml:"allowed_tools,omitempty"`

	// OutputFormat is not user-configurable — the sidecar always uses
	// "stream-json" for structured event streaming. Retained in the schema
	// for backward compatibility with existing request payloads but ignored.
	OutputFormat string `json:"output_format,omitempty" yaml:"output_format,omitempty"`

	// Bare mode — skip hooks, plugins, LSP, automem, CLAUDE.md (clean room).
	Bare bool `json:"bare,omitempty" yaml:"bare,omitempty"`
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
	Type  string            `json:"type"            yaml:"type"`          // "http" | "stdio" | "websocket"
	URL   string            `json:"url,omitempty"   yaml:"url,omitempty"` // supports ${HOST_GATEWAY}
	Cmd   []string          `json:"cmd,omitempty"   yaml:"cmd,omitempty"` // for stdio type
	Env   map[string]string `json:"env,omitempty"   yaml:"env,omitempty"`
	Token string            `json:"token,omitempty" yaml:"token,omitempty"`
}

// TeamConfig enables Claude Code's Agent Teams inbox protocol for a session.
// The orchestrator scaffolds the team directory (~/.claude/teams/{name}/)
// before spawning; agentruntime validates it exists and passes the flags.
type TeamConfig struct {
	// Name is the team name. Must match an existing team directory.
	Name string `json:"name" yaml:"name"`

	// AgentName is this agent's identity within the team. Required.
	AgentName string `json:"agent_name" yaml:"agent_name"`

	// AgentID is the full agent identifier in "name@team" format.
	// Auto-generated as {AgentName}@{Name} if empty.
	AgentID string `json:"agent_id,omitempty" yaml:"agent_id,omitempty"`
}

// ContainerConfig holds container image, resource limits, and security options.
// If omitted entirely, sane defaults are applied:
// --cap-drop ALL --cap-add DAC_OVERRIDE --security-opt no-new-privileges:true
type ContainerConfig struct {
	Image       string   `json:"image,omitempty"        yaml:"image,omitempty"`   // default: "agentruntime-agent:latest"
	Memory      string   `json:"memory,omitempty"       yaml:"memory,omitempty"`  // e.g. "4g"
	CPUs        float64  `json:"cpus,omitempty"         yaml:"cpus,omitempty"`    // e.g. 2.0
	Network     string   `json:"network,omitempty"      yaml:"network,omitempty"` // default: "bridge"
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
	TeamName  string            `json:"team_name,omitempty"`
	TeamAgent string            `json:"team_agent,omitempty"`
}

// SessionInfo is returned by GET /sessions/:id/info.
type SessionInfo struct {
	SessionID     string      `json:"session_id"`
	TaskID        string      `json:"task_id,omitempty"`
	Agent         string      `json:"agent"`
	Runtime       string      `json:"runtime"`
	Status        string      `json:"status"`
	CreatedAt     time.Time   `json:"created_at"`
	EndedAt       *time.Time  `json:"ended_at,omitempty"`
	ExitCode      *int        `json:"exit_code,omitempty"`
	SessionDir    string      `json:"session_dir,omitempty"`
	VolumeName    string      `json:"volume_name,omitempty"`
	LogFile       string      `json:"log_file,omitempty"`
	WSURL         string      `json:"ws_url"`
	LogURL        string      `json:"log_url"`
	Uptime        string      `json:"uptime,omitempty"`
	LastActivity  *time.Time  `json:"last_activity,omitempty"`
	InputTokens   int         `json:"input_tokens,omitempty"`
	OutputTokens  int         `json:"output_tokens,omitempty"`
	CostUSD       float64     `json:"cost_usd,omitempty"`
	ToolCallCount int         `json:"tool_call_count,omitempty"`
	Team          *TeamConfig `json:"team,omitempty"`
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

// EffectiveTimeout parses the Timeout duration string and returns it.
// Defaults to 5 minutes if empty or unparseable.
func (r *SessionRequest) EffectiveTimeout() time.Duration {
	if r.Timeout != "" {
		if d, err := time.ParseDuration(r.Timeout); err == nil {
			return d
		}
	}
	return 5 * time.Minute
}

// Resources is an alias for backward compatibility during migration.
// Deprecated: use ContainerConfig instead.
type Resources = ContainerConfig

// --- Chat API types ---

// CreateChatRequest is the body for POST /chats.
type CreateChatRequest struct {
	Name   string          `json:"name"`
	Config ChatAPIConfig   `json:"config"`
}

// ChatAPIConfig mirrors chat.ChatConfig for API consumers.
type ChatAPIConfig struct {
	Agent        string            `json:"agent"`
	Runtime      string            `json:"runtime,omitempty"`
	Model        string            `json:"model,omitempty"`
	Effort       string            `json:"effort,omitempty"`
	MCPServers   []MCPServer       `json:"mcp_servers,omitempty"`
	AutoDiscover interface{}       `json:"auto_discover,omitempty"`
	WorkDir      string            `json:"work_dir,omitempty"`
	Mounts       []Mount           `json:"mounts,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	IdleTimeout  string            `json:"idle_timeout,omitempty"`
	MaxTurns     int               `json:"max_turns,omitempty"`
	Claude       *ClaudeConfig     `json:"claude,omitempty"`
	Codex        *CodexConfig      `json:"codex,omitempty"`
}

// ChatResponse is returned by GET /chats/:name and POST /chats.
type ChatResponse struct {
	Name             string            `json:"name"`
	Config           ChatAPIConfig     `json:"config"`
	State            string            `json:"state"`
	VolumeName       string            `json:"volume_name,omitempty"`
	CurrentSession   string            `json:"current_session,omitempty"`
	SessionChain     []string          `json:"session_chain"`
	ClaudeSessionIDs map[string]string `json:"claude_session_ids,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	LastActiveAt     *time.Time        `json:"last_active_at,omitempty"`
	WSURL            string            `json:"ws_url,omitempty"`
}

// ChatSummary is returned by GET /chats.
type ChatSummary struct {
	Name         string     `json:"name"`
	State        string     `json:"state"`
	Agent        string     `json:"agent"`
	Runtime      string     `json:"runtime,omitempty"`
	SessionCount int        `json:"session_count"`
	CreatedAt    time.Time  `json:"created_at"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
}

// SendMessageRequest is the body for POST /chats/:name/messages.
type SendMessageRequest struct {
	Message string `json:"message"`
}

// SendMessageResponse is returned by POST /chats/:name/messages.
type SendMessageResponse struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Queued    bool   `json:"queued,omitempty"`
	Spawned   bool   `json:"spawned,omitempty"`
	WSURL     string `json:"ws_url"`
}

// ChatMessageEntry is one entry in GET /chats/:name/messages.
type ChatMessageEntry struct {
	SessionID string          `json:"session_id"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	Offset    int64           `json:"offset"`
	Timestamp time.Time       `json:"timestamp"`
}

// ChatMessagesResponse is returned by GET /chats/:name/messages.
type ChatMessagesResponse struct {
	Messages []ChatMessageEntry `json:"messages"`
	Total    int                `json:"total"`
	HasMore  bool               `json:"has_more"`
	Before   int64              `json:"before,omitempty"`
}

// UpdateChatConfigRequest is the body for PATCH /chats/:name/config.
type UpdateChatConfigRequest struct {
	Config ChatAPIConfig `json:"config"`
}
