package nativeprotocol

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"
)

// PendingApproval is live provider authority, never reconstructed from history.
type PendingApproval struct {
	RequestID json.RawMessage `json:"request_id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	ExpiresAt time.Time       `json:"expires_at"`
	CanAccept bool            `json:"can_accept"`
}
type ApprovalTransport interface {
	PendingApprovals() []PendingApproval
	DecideApproval(context.Context, json.RawMessage, string) error
}

func approvalKey(id json.RawMessage) string {
	var text string
	if json.Unmarshal(id, &text) == nil && text != "" && len(text) <= 256 {
		return "s:" + text
	}
	n, err := strconv.ParseInt(string(id), 10, 64)
	if err == nil && n >= 0 {
		return "n:" + strconv.FormatInt(n, 10)
	}
	return ""
}
func ValidApprovalRequestID(id json.RawMessage) bool { return approvalKey(id) != "" }
func (tr *StreamTransport) observeApproval(raw []byte) {
	var env struct {
		ID        json.RawMessage `json:"id"`
		Method    string          `json:"method"`
		Params    json.RawMessage `json:"params"`
		Type      string          `json:"type"`
		RequestID json.RawMessage `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return
	}
	p := PendingApproval{RequestID: env.ID, Method: env.Method, Params: env.Params, ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	if tr.adapter.Provider() == ProviderClaude {
		if env.Type != "control_request" {
			return
		}
		var req struct {
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(env.Request, &req) != nil || req.Subtype != "can_use_tool" {
			return
		}
		p.RequestID = env.RequestID
		p.Method = "can_use_tool"
		p.Params = env.Request
		p.CanAccept = true
	} else {
		if env.Method != "item/commandExecution/requestApproval" && env.Method != "item/fileChange/requestApproval" && env.Method != "mcpServer/elicitation/request" && env.Method != "item/permissions/requestApproval" {
			return
		}
		p.CanAccept = env.Method == "item/commandExecution/requestApproval" || env.Method == "item/fileChange/requestApproval"
	}
	key := approvalKey(p.RequestID)
	if key == "" {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.approvals == nil {
		tr.approvals = make(map[string]PendingApproval)
	}
	if _, exists := tr.approvals[key]; !exists {
		tr.approvals[key] = p
	}
}
func (tr *StreamTransport) PendingApprovals() []PendingApproval {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]PendingApproval, 0, len(tr.approvals))
	if tr.closed {
		return out
	}
	for key, p := range tr.approvals {
		if time.Now().Before(p.ExpiresAt) {
			out = append(out, p)
		} else {
			delete(tr.approvals, key)
		}
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].RequestID) < string(out[j].RequestID) })
	return out
}
func (tr *StreamTransport) DecideApproval(ctx context.Context, id json.RawMessage, decision string) error {
	const op = "decide_native_approval"
	if decision != "accept" && decision != "decline" && decision != "cancel" {
		return newError(CodeInvalidArgument, op, "invalid approval decision", nil)
	}
	key := approvalKey(id)
	if key == "" {
		return newError(CodeInvalidArgument, op, "invalid approval request ID", nil)
	}
	tr.mu.Lock()
	p, ok := tr.approvals[key]
	if !ok || tr.closed || !time.Now().Before(p.ExpiresAt) {
		tr.mu.Unlock()
		return newError(CodeInvalidState, op, "approval is not pending", nil)
	}
	if decision == "accept" && !p.CanAccept {
		tr.mu.Unlock()
		return newError(CodeInvalidArgument, op, "approval input is unsupported", nil)
	}
	var message any
	if tr.adapter.Provider() == ProviderClaude {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(p.Params, &req) != nil {
			tr.mu.Unlock()
			return newError(CodeInvalidState, op, "invalid tool request", nil)
		}
		body := map[string]any{"behavior": "deny", "message": "Human declined the tool request"}
		if decision == "accept" {
			body = map[string]any{"behavior": "allow", "updatedInput": req.Input}
		}
		message = map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": p.RequestID, "response": body}}
	} else if p.Method == "mcpServer/elicitation/request" {
		message = map[string]any{"id": p.RequestID, "result": map[string]any{"action": decision}}
	} else if p.Method == "item/permissions/requestApproval" {
		message = map[string]any{"id": p.RequestID, "result": map[string]any{"permissions": map[string]any{}, "scope": "turn"}}
	} else {
		message = map[string]any{"id": p.RequestID, "result": map[string]any{"decision": decision}}
	}
	// Claim before writing. Unknown delivery is not replayed as fresh consent.
	delete(tr.approvals, key)
	tr.mu.Unlock()
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return tr.writeMessages(ctx, op, [][]byte{raw})
}
