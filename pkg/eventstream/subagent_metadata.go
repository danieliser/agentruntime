package eventstream

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/danieliser/agentruntime/pkg/durable"
)

type subagentMetadata struct {
	ID, Path, Model string
	ShellTask       bool
}

type subagentIndex struct{ entries map[string]subagentMetadata }

func loadSubagentIndex(ctx context.Context, store durable.Store, session string) (*subagentIndex, error) {
	index := &subagentIndex{entries: make(map[string]subagentMetadata)}
	var cursor int64
	for {
		page, err := store.ListEvents(ctx, durable.EventQuery{SessionID: session, AfterSequence: cursor, Limit: 1000})
		if err != nil {
			return nil, err
		}
		for _, ev := range page.Events {
			index.apply(ev.Type, ev.Payload)
			cursor = ev.Sequence
		}
		if !page.HasMore || len(page.Events) == 0 {
			return index, nil
		}
	}
}

// apply retains provider-exposed identity/model facts for subsequent records.
// Claude correlates child output by tool-use ID, while task notifications use
// task ID. Shell tasks are work performed by an agent, not additional agents.
func (index *subagentIndex) apply(kind string, payload json.RawMessage) (string, json.RawMessage) {
	var p struct {
		ID           string `json:"subagent_thread_id"`
		Path         string `json:"subagent_path"`
		Model        string `json:"model"`
		TaskID       string `json:"task_id"`
		ToolID       string `json:"task_tool_use_id"`
		TaskType     string `json:"task_type"`
		ParentTaskID string `json:"parent_task_id"`
		Subtype      string `json:"native_subtype"`
	}
	if json.Unmarshal(payload, &p) != nil || p.ID == "" && p.TaskID == "" {
		return kind, payload
	}
	key := p.ID
	if p.TaskID != "" {
		key = p.TaskID
	}
	meta, known := index.entries[key]
	if !known {
		meta = index.entries[p.ID]
	}
	if meta.ID == "" && !meta.ShellTask {
		meta.ID = p.ID
		meta.Path = p.Path
	}
	if p.Path != "" && p.Path != p.ID {
		meta.Path = p.Path
	}
	if p.Model != "" {
		meta.Model = p.Model
	}
	if p.TaskType == "local_bash" {
		meta = index.entries[p.ParentTaskID]
		meta.ShellTask = true
	}
	if meta.Path == "" {
		meta.Path = meta.ID
	}
	index.entries[key] = meta
	if meta.ID != "" && !meta.ShellTask {
		index.entries[meta.ID] = meta
	}
	if p.ToolID != "" {
		index.entries[p.ToolID] = meta
	}
	if meta.ShellTask {
		suffix := "progress"
		if p.Subtype == "task_started" {
			suffix = "call"
		}
		if p.Subtype == "task_notification" {
			suffix = "result"
		}
		kind = "lifecycle.task." + suffix
		if meta.ID != "" {
			kind = "subagent.tool." + suffix
		}
	}
	var fields map[string]any
	_ = json.Unmarshal(payload, &fields)
	if strings.HasPrefix(kind, "subagent.") {
		fields["subagent_thread_id"], fields["subagent_path"], fields["model"] = meta.ID, meta.Path, meta.Model
	} else {
		delete(fields, "subagent_thread_id")
		delete(fields, "subagent_path")
		delete(fields, "model")
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return kind, payload
	}
	return kind, normalized
}
