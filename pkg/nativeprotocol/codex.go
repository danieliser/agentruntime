package nativeprotocol

import (
	"encoding/json"
	"strings"
)

type codexAdapter struct{ runtimeName string }

// Runtime comes from AgentD, never provider-supplied JSON. Unknown runtimes get
// a generic warning without claiming that execution occurs on the host.
func (adapter codexAdapter) approvalWarning(method string) string {
	if method != "item/commandExecution/requestApproval" && method != "item/fileChange/requestApproval" {
		return ""
	}
	switch adapter.runtimeName {
	case "local":
		return "Approved escalation runs outside the sandbox on the host."
	case "docker":
		return "Approved escalation runs outside the sandbox in the container."
	default:
		return "Approved escalation runs outside the sandbox."
	}
}

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
				sandboxPolicy["writableRoots"] = []string{policyWorkDir(input.Policy)}
			}
		}
		params := map[string]any{
			"threadId":       input.ProviderID,
			"input":          []map[string]any{{"type": "text", "text": input.Text}},
			"approvalPolicy": approvalPolicy, "sandboxPolicy": sandboxPolicy,
		}
		if input.Policy.PermissionProfile != "" {
			delete(params, "sandboxPolicy")
			params["permissions"] = input.Policy.PermissionProfile
			params["approvalsReviewer"] = "user"
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
		message = codexRequest(requestID, "turn/interrupt", map[string]any{"threadId": input.ProviderID, "turnId": input.TurnID})
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

func (adapter codexAdapter) Decode(raw []byte) (DerivedEvent, error) {
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
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		Delta     string          `json:"delta"`
		Message   string          `json:"message"`
		Item      json.RawMessage `json:"item"`
		Usage     json.RawMessage `json:"usage"`
		Error     json.RawMessage `json:"error"`
		WillRetry *bool           `json:"willRetry"`
		Thread    struct {
			ID       string `json:"id"`
			ParentID string `json:"parentThreadId"`
			Model    string `json:"model"`
		} `json:"thread"`
		Turn struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(envelope.Params, &params)
	derived.ProviderID, derived.TurnID = params.ThreadID, params.TurnID
	if derived.ProviderID == "" {
		derived.ProviderID = params.Thread.ID
	}
	if derived.TurnID == "" {
		derived.TurnID = params.Turn.ID
	}
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
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "mcpServer/elicitation/request":
		derived.Type = "control.approval.request"
		payload["request_id"] = envelope.ID
		if warning := adapter.approvalWarning(envelope.Method); warning != "" {
			payload["warning"] = warning
		}
	case "error":
		derived.Type = "error.provider"
		payload["message"] = codexErrorMessage(params.Error, params.Message)
		if len(params.Error) > 0 && string(params.Error) != "null" {
			payload["error"] = params.Error
		}
		if params.WillRetry != nil {
			payload["will_retry"] = *params.WillRetry
		}
	case "turn/completed":
		derived.Type = "turn.completed"
		if params.Turn.Status != "" {
			payload["status"] = params.Turn.Status
		}
		if params.Turn.Status == "failed" || params.Turn.Status == "interrupted" {
			derived.Type = "turn.failed"
		}
		if len(params.Turn.Error) > 0 && string(params.Turn.Error) != "null" {
			payload["error"] = params.Turn.Error
			payload["message"] = codexErrorMessage(params.Turn.Error, "")
		}
		payload["usage"] = params.Usage
	}
	if strings.HasSuffix(strings.ToLower(envelope.Method), "delta") && derived.Type == "provider.event" {
		derived.Type = "provider.delta"
		payload["text"] = params.Delta
	}
	derived.Payload = mustPayload(payload)
	if params.Thread.ParentID != "" {
		derived = TagSubagent(derived, params.Thread.ID, "", params.Thread.Model)
	}
	return codexSubagent(derived, params.Item), nil
}

// Codex app-server errors carry the diagnostic under error.message; older
// notifications placed it directly on params. Keep both wire forms readable.
func codexErrorMessage(raw json.RawMessage, fallback string) string {
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &detail) == nil && detail.Message != "" {
		return detail.Message
	}
	return fallback
}
