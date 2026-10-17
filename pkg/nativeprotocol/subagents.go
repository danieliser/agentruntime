package nativeprotocol

import (
	"encoding/json"
	"strings"
)

// TagSubagent preserves the native payload while separating child boundaries
// from parent lifecycle events. Unknown model/path values remain explicit.
func TagSubagent(event DerivedEvent, id, path, model string) DerivedEvent {
	var payload map[string]any
	_ = json.Unmarshal(event.Payload, &payload)
	if payload == nil {
		payload = make(map[string]any)
	}
	if path == "" {
		path = id
	}
	payload["subagent_thread_id"], payload["subagent_path"], payload["model"] = id, path, model
	if !strings.HasPrefix(event.Type, "subagent.") {
		event.Type = "subagent." + event.Type
	}
	event.Payload = mustPayload(payload)
	return event
}

func codexSubagent(event DerivedEvent, item json.RawMessage) DerivedEvent {
	var activity struct {
		Type     string `json:"type"`
		Kind     string `json:"kind"`
		ThreadID string `json:"agentThreadId"`
		Path     string `json:"agentPath"`
		Model    string `json:"model"`
	}
	if json.Unmarshal(item, &activity) != nil || activity.Type != "subAgentActivity" {
		return event
	}
	event.Type = "subagent." + activity.Kind
	return TagSubagent(event, activity.ThreadID, activity.Path, activity.Model)
}

func claudeSubagent(event DerivedEvent, raw []byte) DerivedEvent {
	var envelope struct {
		Type         string `json:"type"`
		Subtype      string `json:"subtype"`
		Parent       string `json:"parent_tool_use_id"`
		TaskID       string `json:"task_id"`
		TaskType     string `json:"task_type"`
		ParentTaskID string `json:"parent_task_id"`
		ToolID       string `json:"tool_use_id"`
		Status       string `json:"status"`
		Summary      string `json:"summary"`
		Description  string `json:"description"`
		Message      struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	_ = json.Unmarshal(raw, &envelope)
	id := envelope.Parent
	if envelope.Type == "system" && strings.HasPrefix(envelope.Subtype, "task_") {
		id = envelope.TaskID
		if id == "" {
			id = envelope.ToolID
		}
		switch envelope.Subtype {
		case "task_started":
			event.Type = "subagent.started"
		case "task_notification":
			event.Type = "subagent.completed"
			if envelope.Status == "failed" {
				event.Type = "subagent.failed"
			}
		default:
			event.Type = "subagent.progress"
		}
		var p map[string]any
		_ = json.Unmarshal(event.Payload, &p)
		p["text"] = envelope.Summary
		if envelope.Summary == "" {
			p["text"] = envelope.Description
		}
		p["task_tool_use_id"] = envelope.ToolID
		p["task_id"], p["task_type"], p["parent_task_id"] = envelope.TaskID, envelope.TaskType, envelope.ParentTaskID
		event.Payload = mustPayload(p)
	}
	if id != "" {
		// Forwarded records can expose a child session ID. Parent generation
		// identity comes from the primary init, never from a child record.
		event.ProviderID = ""
		if envelope.Type == "stream_event" {
			event.Type = "content.delta"
		}
		return TagSubagent(event, id, id, envelope.Message.Model)
	}
	return event
}
