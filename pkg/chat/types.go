// Package chat implements named persistent chat sessions — a stable,
// human-readable conversation handle that survives agent session rotation.
package chat

import (
	"time"

	"github.com/danieliser/agentruntime/pkg/api/schema"
)

// ChatState represents the lifecycle state of a named chat.
type ChatState string

const (
	ChatStateCreated ChatState = "created"
	ChatStateRunning ChatState = "running"
	ChatStateIdle    ChatState = "idle"
	ChatStateDeleted ChatState = "deleted"
)

// String returns the string representation of the state.
func (s ChatState) String() string { return string(s) }

// IsTerminal reports whether the state is a terminal state.
// Only "deleted" is terminal.
func (s ChatState) IsTerminal() bool { return s == ChatStateDeleted }

const defaultIdleTimeout = 30 * time.Minute

// ChatConfig is the stored config applied to every spawned session.
// Specified at creation and reused on respawn. PATCH-able only when idle.
type ChatConfig struct {
	Agent        string             `json:"agent"`
	Runtime      string             `json:"runtime,omitempty"`
	Model        string             `json:"model,omitempty"`
	Effort       string             `json:"effort,omitempty"`
	MCPServers   []schema.MCPServer `json:"mcp_servers,omitempty"`
	AutoDiscover interface{}        `json:"auto_discover,omitempty"`
	WorkDir      string             `json:"work_dir,omitempty"`
	Mounts       []schema.Mount     `json:"mounts,omitempty"`
	Env          map[string]string  `json:"env,omitempty"`
	IdleTimeout  string             `json:"idle_timeout,omitempty"`
	MaxTurns     int                `json:"max_turns,omitempty"`
}

// EffectiveIdleTimeout parses IdleTimeout and returns the duration.
// Returns 30 minutes if the string is empty or unparseable.
func (c ChatConfig) EffectiveIdleTimeout() time.Duration {
	if c.IdleTimeout == "" {
		return defaultIdleTimeout
	}
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil {
		return defaultIdleTimeout
	}
	return d
}

// ChatRecord is the persisted representation of a named chat.
// Stored as JSON in {dataDir}/chats/{name}.json.
type ChatRecord struct {
	Name           string     `json:"name"`
	Config         ChatConfig `json:"config"`
	State          ChatState  `json:"state"`
	VolumeName     string     `json:"volume_name,omitempty"`
	CurrentSession string     `json:"current_session,omitempty"`
	SessionChain   []string   `json:"session_chain"`
	PendingMessage   string            `json:"pending_message,omitempty"`
	ClaudeSessionIDs map[string]string `json:"claude_session_ids,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	LastActiveAt     *time.Time        `json:"last_active_at,omitempty"`
}

// LastSessionID returns the last entry in SessionChain, or "" if empty.
func (r *ChatRecord) LastSessionID() string {
	if len(r.SessionChain) == 0 {
		return ""
	}
	return r.SessionChain[len(r.SessionChain)-1]
}
