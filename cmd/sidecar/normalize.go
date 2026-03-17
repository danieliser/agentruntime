package main

import "encoding/json"

// Normalized event data shapes — agent-agnostic.
// Both Claude and Codex backends emit raw events, then normalize()
// maps them to these standard shapes before broadcasting.

// NormalizedAgentMessage is the unified shape for agent text output.
type NormalizedAgentMessage struct {
	Text     string          `json:"text"`               // full text (or delta chunk)
	Delta    bool            `json:"delta"`               // true if this is a streaming chunk, false if final
	Model    string          `json:"model,omitempty"`     // model that generated this
	Usage    *NormalizedUsage `json:"usage,omitempty"`     // token counts (only on final messages)
	TurnID   string          `json:"turn_id,omitempty"`   // agent's internal turn identifier
	ItemID   string          `json:"item_id,omitempty"`   // agent's internal item identifier
}

// NormalizedUsage is the unified token count shape.
type NormalizedUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// NormalizedToolUse is the unified shape for tool call start.
type NormalizedToolUse struct {
	ID        string         `json:"id"`                  // tool call ID
	Name      string         `json:"name"`                // tool name (Edit, Bash, Read, etc.)
	Server    string         `json:"server,omitempty"`    // MCP server name if applicable
	Input     map[string]any `json:"input,omitempty"`     // tool arguments
}

// NormalizedToolResult is the unified shape for tool call completion.
type NormalizedToolResult struct {
	ID         string `json:"id"`                         // matches ToolUse.ID
	Name       string `json:"name,omitempty"`             // tool name
	Output     string `json:"output,omitempty"`           // text output
	IsError    bool   `json:"is_error,omitempty"`         // true if tool errored
	DurationMs int64  `json:"duration_ms,omitempty"`      // execution time
}

// NormalizedResult is the unified shape for turn/session completion.
type NormalizedResult struct {
	SessionID  string          `json:"session_id,omitempty"` // agent session/thread ID
	TurnID     string          `json:"turn_id,omitempty"`    // turn that completed
	Status     string          `json:"status"`               // "success", "error", "interrupted"
	CostUSD    float64         `json:"cost_usd,omitempty"`   // estimated cost
	DurationMs int64           `json:"duration_ms,omitempty"`// total turn duration
	NumTurns   int             `json:"num_turns,omitempty"`  // turns in this session
	Usage      *NormalizedUsage `json:"usage,omitempty"`      // aggregate token counts
}

// normalizeClaudeAgentMessage converts Claude's assistant message to standard shape.
func normalizeClaudeAgentMessage(raw map[string]any) map[string]any {
	msg := NormalizedAgentMessage{}
	// Respect delta flag — stream_event deltas set delta:true,
	// full assistant messages leave it false.
	if isDelta, ok := raw["delta"].(bool); ok {
		msg.Delta = isDelta
	}

	if text, ok := raw["text"].(string); ok {
		msg.Text = text
	}
	if model, ok := raw["model"].(string); ok {
		msg.Model = model
	}

	if usage, ok := raw["usage"].(map[string]any); ok {
		msg.Usage = &NormalizedUsage{
			InputTokens:              intField(usage, "input_tokens"),
			OutputTokens:             intField(usage, "output_tokens"),
			CacheReadInputTokens:     intField(usage, "cache_read_input_tokens"),
			CacheCreationInputTokens: intField(usage, "cache_creation_input_tokens"),
		}
	}

	return structToMap(msg)
}

// normalizeClaudeToolUse converts Claude's tool_use content block to standard shape.
func normalizeClaudeToolUse(raw map[string]any) map[string]any {
	tu := NormalizedToolUse{
		ID:    stringVal(raw, "id"),
		Name:  stringVal(raw, "name"),
		Input: mapVal(raw, "input"),
	}
	return structToMap(tu)
}

// normalizeClaudeResult converts Claude's result event to standard shape.
func normalizeClaudeResult(raw map[string]any) map[string]any {
	r := NormalizedResult{
		SessionID:  stringVal(raw, "session_id"),
		Status:     stringVal(raw, "subtype"),
		CostUSD:    floatField(raw, "cost_usd"),
		DurationMs: int64Field(raw, "duration_ms"),
		NumTurns:   intField(raw, "num_turns"),
	}
	return structToMap(r)
}

// normalizeCodexAgentMessage converts Codex's agent message to standard shape.
func normalizeCodexAgentMessage(raw map[string]any) map[string]any {
	msg := NormalizedAgentMessage{
		TurnID: stringVal(raw, "turnId"),
		ItemID: stringVal(raw, "itemId"),
	}

	if final, ok := raw["final"].(bool); ok && final {
		msg.Delta = false
		msg.Text = stringVal(raw, "text")
		// Extract usage from item if present
		if item, ok := raw["item"].(map[string]any); ok {
			msg.Text = stringVal(item, "text")
		}
	} else {
		msg.Delta = true
		msg.Text = stringVal(raw, "delta")
		if msg.Text == "" {
			msg.Text = stringVal(raw, "text")
		}
	}

	return structToMap(msg)
}

// normalizeCodexToolUse converts Codex's item/started to standard shape.
func normalizeCodexToolUse(raw map[string]any) map[string]any {
	item := mapVal(raw, "item")
	tu := NormalizedToolUse{
		ID:     stringVal(item, "id"),
		Name:   stringVal(item, "tool"),
		Server: stringVal(item, "server"),
		Input:  mapVal(item, "arguments"),
	}
	// Fallback for command execution
	if tu.Name == "" {
		itemType := stringVal(item, "type")
		if itemType == "commandExecution" || itemType == "command_execution" {
			tu.Name = "Bash"
			tu.Input = map[string]any{"command": stringVal(item, "command")}
		} else if itemType == "fileChange" || itemType == "file_change" {
			tu.Name = "Edit"
		}
	}
	return structToMap(tu)
}

// normalizeCodexToolResult converts Codex's item/completed to standard shape.
func normalizeCodexToolResult(raw map[string]any) map[string]any {
	item := mapVal(raw, "item")
	tr := NormalizedToolResult{
		ID:         stringVal(item, "id"),
		Name:       stringVal(item, "tool"),
		DurationMs: int64Field(item, "durationMs"),
		IsError:    item["error"] != nil,
	}
	// Extract output text from result.content
	if result, ok := item["result"].(map[string]any); ok {
		if content, ok := result["content"].([]any); ok && len(content) > 0 {
			if first, ok := content[0].(map[string]any); ok {
				tr.Output = stringVal(first, "text")
			}
		}
	}
	// Fallback for command execution
	if tr.Name == "" {
		itemType := stringVal(item, "type")
		if itemType == "commandExecution" || itemType == "command_execution" {
			tr.Name = "Bash"
			tr.Output = stringVal(item, "aggregatedOutput")
		} else if itemType == "fileChange" || itemType == "file_change" {
			tr.Name = "Edit"
		}
	}
	return structToMap(tr)
}

// normalizeCodexResult converts Codex's turn/completed to standard shape.
func normalizeCodexResult(raw map[string]any) map[string]any {
	r := NormalizedResult{
		TurnID: stringVal(raw, "threadId"),
		Status: "success",
	}
	if turn, ok := raw["turn"].(map[string]any); ok {
		r.TurnID = stringVal(turn, "id")
		r.Status = stringVal(turn, "status")
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		r.Usage = &NormalizedUsage{
			InputTokens:          intField(usage, "input_tokens"),
			OutputTokens:         intField(usage, "output_tokens"),
			CacheReadInputTokens: intField(usage, "cached_input_tokens"),
		}
	}
	return structToMap(r)
}

// --- helpers ---

func intField(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func int64Field(m map[string]any, key string) int64 {
	if v, ok := m[key].(float64); ok {
		return int64(v)
	}
	return 0
}

func floatField(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func stringVal(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func mapVal(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func structToMap(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}
