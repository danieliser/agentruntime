package runtime

import "encoding/json"

// sidecarAgentConfig mirrors the AgentConfig struct in cmd/sidecar.
// It is serialized as JSON into the AGENT_CONFIG env var so the sidecar
// can parse it at startup without knowing about the full SessionRequest.
type sidecarAgentConfig struct {
	Model         string            `json:"model,omitempty"`
	ResumeSession string            `json:"resume_session,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	ApprovalMode  string            `json:"approval_mode,omitempty"`
	MaxTurns      int               `json:"max_turns,omitempty"`
	AllowedTools  []string          `json:"allowed_tools,omitempty"`

	// Team fields — enable Claude Code Agent Teams inbox protocol.
	TeamName      string `json:"team_name,omitempty"`
	TeamAgentName string `json:"team_agent_name,omitempty"`
	TeamAgentID   string `json:"team_agent_id,omitempty"`

	// Bare mode — skip hooks, plugins, LSP, automem, CLAUDE.md (clean room).
	Bare bool `json:"bare,omitempty"`
}

// buildAgentConfigJSON builds the AGENT_CONFIG JSON string from SpawnConfig.
// Returns empty string if there is nothing to pass through.
func buildAgentConfigJSON(cfg SpawnConfig) string {
	ac := sidecarAgentConfig{}
	hasContent := false

	if cfg.Request != nil {
		if cfg.Request.Model != "" {
			ac.Model = cfg.Request.Model
			hasContent = true
		}
		if cfg.Request.ResumeSession != "" {
			ac.ResumeSession = cfg.Request.ResumeSession
			hasContent = true
		}
		if cfg.Request.Claude != nil {
			if cfg.Request.Claude.MaxTurns > 0 {
				ac.MaxTurns = cfg.Request.Claude.MaxTurns
				hasContent = true
			}
			if len(cfg.Request.Claude.AllowedTools) > 0 {
				ac.AllowedTools = cfg.Request.Claude.AllowedTools
				hasContent = true
			}
		}
		if cfg.Request.Codex != nil && cfg.Request.Codex.ApprovalMode != "" {
			ac.ApprovalMode = cfg.Request.Codex.ApprovalMode
			hasContent = true
		}
		if cfg.Request.Team != nil && cfg.Request.Team.Name != "" {
			ac.TeamName = cfg.Request.Team.Name
			ac.TeamAgentName = cfg.Request.Team.AgentName
			ac.TeamAgentID = cfg.Request.Team.AgentID
			hasContent = true
		}
		if cfg.Request.Claude != nil && cfg.Request.Claude.Bare {
			ac.Bare = true
			hasContent = true
		}
	}

	// Top-level model from SpawnConfig overrides Request.Model.
	if cfg.Model != "" {
		ac.Model = cfg.Model
		hasContent = true
	}

	// Merge request env into agent config env (these are distinct from the
	// container-level env — they're forwarded into the agent process itself).
	if cfg.Request != nil && len(cfg.Request.Env) > 0 {
		ac.Env = cfg.Request.Env
		hasContent = true
	}

	if !hasContent {
		return ""
	}

	data, err := json.Marshal(ac)
	if err != nil {
		return ""
	}
	return string(data)
}
