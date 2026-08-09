package nativeprotocol

import (
	"encoding/json"
)

type claudeAdapter struct{}

func (claudeAdapter) Provider() Provider { return ProviderClaude }

func (claudeAdapter) Encode(input Input) ([][]byte, error) {
	const op = "encode_claude_input"
	var messages []any
	switch input.Kind {
	case InputPrompt:
		if input.Text == "" {
			return nil, newError(CodeInvalidArgument, op, "prompt text is required", nil)
		}
		messages = append(messages, claudePrompt(input.Text))
	case InputInterrupt:
		messages = append(messages, claudeInterrupt())
	case InputSteer:
		if input.Text == "" {
			return nil, newError(CodeInvalidArgument, op, "steer text is required", nil)
		}
		messages = append(messages, claudeInterrupt(), claudePrompt(input.Text))
	case InputApproval:
		if len(input.RequestID) == 0 || !json.Valid(input.RequestID) {
			return nil, newError(CodeInvalidArgument, op, "approval request ID must be valid JSON", nil)
		}
		var requestID any
		if err := json.Unmarshal(input.RequestID, &requestID); err != nil {
			return nil, newError(CodeEncode, op, "decode approval request ID", err)
		}
		behavior := "deny"
		if input.ApprovalAllow {
			behavior = "allow"
		}
		messages = append(messages, map[string]any{"type": "control_response", "response": map[string]any{
			"request_id": requestID, "behavior": behavior,
		}})
	default:
		return nil, newError(CodeInvalidArgument, op, "unsupported input kind", nil)
	}
	return marshalMessages(op, messages)
}

func claudeInterrupt() map[string]any {
	return map[string]any{"type": "control_request", "request": map[string]any{"subtype": "interrupt"}}
}

func claudePrompt(text string) map[string]any {
	return map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	}
}

func (claudeAdapter) Decode(raw []byte) (DerivedEvent, error) {
	var envelope struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		SessionID string          `json:"session_id"`
		Event     json.RawMessage `json:"event"`
		Message   json.RawMessage `json:"message"`
		Request   json.RawMessage `json:"request"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return unparsedEvent(ProviderClaude, raw, err), nil
	}
	derived := DerivedEvent{Type: "provider.event", ProviderID: envelope.SessionID}
	payload := map[string]any{"provider": ProviderClaude, "native_type": envelope.Type}
	if envelope.Subtype != "" {
		payload["native_subtype"] = envelope.Subtype
	}
	switch envelope.Type {
	case "system":
		if envelope.Subtype == "init" {
			derived.Type = "lifecycle.provider.initialized"
		} else {
			derived.Type = "lifecycle.provider"
		}
	case "stream_event":
		var event struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(envelope.Event, &event)
		if event.Delta.Type == "text_delta" {
			derived.Type = "content.delta"
			payload["text"] = event.Delta.Text
		}
	case "assistant":
		var message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Text  string          `json:"text"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			Usage json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(envelope.Message, &message)
		derived.Type = "content.completed"
		var calls []map[string]any
		var text string
		for _, item := range message.Content {
			if item.Type == "tool_use" {
				calls = append(calls, map[string]any{"id": item.ID, "name": item.Name, "input": item.Input})
			}
			if item.Type == "text" {
				text += item.Text
			}
		}
		if len(calls) > 0 {
			derived.Type = "tool.call"
			payload["calls"] = calls
		}
		if text != "" {
			payload["text"] = text
		}
		if len(message.Usage) > 0 {
			payload["usage"] = message.Usage
		}
	case "user":
		derived.Type = "tool.result"
		payload["message"] = envelope.Message
	case "control_request":
		derived.Type = "control.approval.request"
		payload["request"] = envelope.Request
	case "result":
		if envelope.IsError || envelope.Subtype != "success" {
			derived.Type = "turn.failed"
		} else {
			derived.Type = "turn.completed"
		}
		var full map[string]any
		if err := json.Unmarshal(raw, &full); err == nil {
			payload["result"] = full
		}
	case "progress":
		derived.Type = "lifecycle.progress"
	}
	derived.Payload = mustPayload(payload)
	return derived, nil
}
