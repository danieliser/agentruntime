package nativeprotocol

import "encoding/json"

type codexAdapter struct{}

func (codexAdapter) Provider() Provider { return ProviderCodex }

func (codexAdapter) Encode(input Input) ([][]byte, error) {
	const op = "encode_codex_input"
	if input.ProviderID == "" && input.Kind != InputApproval {
		return nil, newError(CodeInvalidArgument, op, "Codex thread ID is required", nil)
	}
	requestID := int64(1)
	if len(input.RequestID) > 0 {
		if !json.Valid(input.RequestID) {
			return nil, newError(CodeInvalidArgument, op, "request ID must be valid JSON", nil)
		}
		if err := json.Unmarshal(input.RequestID, &requestID); err != nil {
			return nil, newError(CodeInvalidArgument, op, "request ID must be an integer", err)
		}
	}
	var message any
	switch input.Kind {
	case InputPrompt:
		if input.Text == "" {
			return nil, newError(CodeInvalidArgument, op, "prompt text is required", nil)
		}
		approvalPolicy := "never"
		sandboxPolicy := map[string]any{"type": "dangerFullAccess"}
		if input.Policy.Enforced {
			approvalPolicy = input.Policy.ApprovalPolicy
			sandboxType := "readOnly"
			if input.Policy.Filesystem == "workspace_write" {
				sandboxType = "workspaceWrite"
			}
			sandboxPolicy = map[string]any{"type": sandboxType, "networkAccess": input.Policy.NetworkAccess}
			if sandboxType == "workspaceWrite" {
				sandboxPolicy["writableRoots"] = []string{"/workspace"}
			}
		}
		params := map[string]any{
			"threadId":       input.ProviderID,
			"input":          []map[string]any{{"type": "text", "text": input.Text}},
			"approvalPolicy": approvalPolicy, "sandboxPolicy": sandboxPolicy,
		}
		if len(input.OutputSchema) > 0 {
			params["outputSchema"] = json.RawMessage(input.OutputSchema)
		}
		message = codexRequest(requestID, "turn/start", params)
	case InputSteer:
		if input.Text == "" || input.TurnID == "" {
			return nil, newError(CodeInvalidArgument, op, "steer text and turn ID are required", nil)
		}
		message = codexRequest(requestID, "turn/steer", map[string]any{
			"threadId": input.ProviderID, "expectedTurnId": input.TurnID,
			"input": []map[string]any{{"type": "text", "text": input.Text}},
		})
	case InputInterrupt:
		message = codexRequest(requestID, "turn/interrupt", map[string]any{"threadId": input.ProviderID, "reason": "user"})
	case InputApproval:
		decision := "decline"
		if input.ApprovalAllow {
			decision = "accept"
		}
		message = map[string]any{"id": requestID, "result": map[string]any{"decision": decision}}
	default:
		return nil, newError(CodeInvalidArgument, op, "unsupported input kind", nil)
	}
	return marshalMessages(op, []any{message})
}

func codexRequest(id int64, method string, params any) map[string]any {
	return map[string]any{"id": id, "method": method, "params": params}
}

func (codexAdapter) Decode(raw []byte) (DerivedEvent, error) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return unparsedEvent(ProviderCodex, raw, err), nil
	}
	derived := DerivedEvent{Type: "provider.event"}
	payload := map[string]any{"provider": ProviderCodex}
	if envelope.Method == "" && len(envelope.ID) > 0 {
		derived.Type = "control.response"
		payload["id"] = envelope.ID
		if len(envelope.Result) > 0 {
			payload["result"] = envelope.Result
		}
		if len(envelope.Error) > 0 {
			payload["error"] = envelope.Error
		}
		derived.Payload = mustPayload(payload)
		return derived, nil
	}
	payload["native_method"] = envelope.Method
	var params struct {
		ThreadID string          `json:"threadId"`
		TurnID   string          `json:"turnId"`
		Delta    string          `json:"delta"`
		Message  string          `json:"message"`
		Item     json.RawMessage `json:"item"`
		Usage    json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(envelope.Params, &params)
	derived.ProviderID, derived.TurnID = params.ThreadID, params.TurnID
	switch envelope.Method {
	case "thread/started":
		derived.Type = "lifecycle.provider.session"
	case "turn/started":
		derived.Type = "lifecycle.turn.started"
	case "item/agentMessage/delta":
		derived.Type = "content.delta"
		payload["text"] = params.Delta
	case "item/started":
		derived.Type = "tool.call"
		payload["item"] = params.Item
	case "item/completed":
		derived.Type = "tool.result"
		payload["item"] = params.Item
	case "item/commandExecution/requestApproval":
		derived.Type = "control.approval.request"
		payload["request_id"] = envelope.ID
	case "error":
		derived.Type = "error.provider"
		payload["message"] = params.Message
	case "turn/completed":
		derived.Type = "turn.completed"
		payload["usage"] = params.Usage
	}
	derived.Payload = mustPayload(payload)
	return derived, nil
}
