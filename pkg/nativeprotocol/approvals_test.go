package nativeprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

type approvalBuffer struct{ bytes.Buffer }

func (*approvalBuffer) Close() error { return nil }
func approvalTransport(t *testing.T, provider Provider) (*StreamTransport, *approvalBuffer) {
	t.Helper()
	adapter, err := NewAdapter(provider)
	if err != nil {
		t.Fatal(err)
	}
	writer := &approvalBuffer{}
	tr := NewStreamTransport(adapter, ProcessIO{Stdin: writer}, RecoveryMetadata{})
	tr.started = true
	return tr, writer
}
func TestApprovalExactIDAndOneResponse(t *testing.T) {
	tr, w := approvalTransport(t, ProviderCodex)
	tr.observeApproval([]byte(`{"id":42,"method":"item/commandExecution/requestApproval","params":{"command":"echo ok"}}`))
	if len(tr.PendingApprovals()) != 1 {
		t.Fatal("missing pending approval")
	}
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`43`), "accept"); err == nil {
		t.Fatal("wrong ID allowed")
	}
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`42`), "whatever"); err == nil {
		t.Fatal("invalid decision allowed")
	}
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`42`), "accept"); err != nil {
		t.Fatal(err)
	}
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`42`), "accept"); err == nil {
		t.Fatal("duplicate decision replayed")
	}
	if w.String() != "{\"id\":42,\"result\":{\"decision\":\"accept\"}}\n" {
		t.Fatal(w.String())
	}
}
func TestClaudeApprovalCarriesOriginalInput(t *testing.T) {
	tr, w := approvalTransport(t, ProviderClaude)
	tr.observeApproval([]byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"echo ok"}}}`))
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`"r1"`), "accept"); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(w.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	r := d["response"].(map[string]any)
	if r["subtype"] != "success" {
		t.Fatal(d)
	}
	body := r["response"].(map[string]any)
	if body["updatedInput"].(map[string]any)["command"] != "echo ok" {
		t.Fatal(d)
	}
}
func TestApprovalDoesNotResurrectOnFreshTransport(t *testing.T) {
	old, _ := approvalTransport(t, ProviderCodex)
	old.observeApproval([]byte(`{"id":7,"method":"item/fileChange/requestApproval","params":{}}`))
	fresh, _ := approvalTransport(t, ProviderCodex)
	if len(fresh.PendingApprovals()) != 0 {
		t.Fatal("consent resurrected")
	}
	if err := fresh.DecideApproval(context.Background(), json.RawMessage(`7`), "accept"); err == nil {
		t.Fatal("old ID accepted")
	}
}
func TestNonApprovalAndInvalidIDsIgnored(t *testing.T) {
	tr, _ := approvalTransport(t, ProviderCodex)
	for _, raw := range []string{`{"id":{},"method":"item/commandExecution/requestApproval"}`, `{"id":null,"method":"item/commandExecution/requestApproval"}`, `{"id":1,"method":"turn/started"}`} {
		tr.observeApproval([]byte(raw))
	}
	if len(tr.PendingApprovals()) != 0 {
		t.Fatal("invalid requests admitted")
	}
}

func TestCodexEmptyMCPToolConsentUsesAcceptContent(t *testing.T) {
	tr, w := approvalTransport(t, ProviderCodex)
	tr.observeApproval([]byte(`{"id":"memory-consent","method":"mcpServer/elicitation/request","params":{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":{},"required":[]}}}`))
	pending := tr.PendingApprovals()
	if len(pending) != 1 || !pending[0].CanAccept {
		t.Fatal("empty MCP tool consent cannot be accepted")
	}
	if w.Len() != 0 {
		t.Fatal("tool consent was approved without a human decision")
	}
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`"memory-consent"`), "accept"); err != nil {
		t.Fatal(err)
	}
	if w.String() != "{\"id\":\"memory-consent\",\"result\":{\"action\":\"accept\",\"content\":{}}}\n" {
		t.Fatal(w.String())
	}
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`"memory-consent"`), "accept"); err == nil {
		t.Fatal("consent replay accepted")
	}
}

func TestCodexMCPDataAndURLFormsRemainUnsupported(t *testing.T) {
	for _, params := range []string{
		`{"mode":"url","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":{}}}`,
		`{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":{"secret":{"type":"string"}}}}`,
		`{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":{},"required":["secret"]}}`,
		`{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":{},"required":null}}`,
		`{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":[]}}`,
		`{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":null}}`,
		`{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object"}}`,
		`{"mode":"form","requestedSchema":{"type":"object","properties":{}}}`,
	} {
		tr, w := approvalTransport(t, ProviderCodex)
		tr.observeApproval([]byte(`{"id":8,"method":"mcpServer/elicitation/request","params":` + params + `}`))
		pending := tr.PendingApprovals()
		if len(pending) != 1 || pending[0].CanAccept {
			t.Fatalf("data/URL form accepted: %s", params)
		}
		if err := tr.DecideApproval(context.Background(), json.RawMessage(`8`), "accept"); err == nil {
			t.Fatalf("data/URL form decision accepted: %s", params)
		}
		if w.Len() != 0 {
			t.Fatal("unsupported accept wrote provider input")
		}
		if err := tr.DecideApproval(context.Background(), json.RawMessage(`8`), "decline"); err != nil {
			t.Fatal(err)
		}
		if w.String() != "{\"id\":8,\"result\":{\"action\":\"decline\"}}\n" {
			t.Fatal(w.String())
		}
	}
}

func TestCodexEmptyToolConsentAllowsAbsentRequired(t *testing.T) {
	if !isEmptyCodexToolConsent(json.RawMessage(`{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":{}}}`)) {
		t.Fatal("pinned empty form without required was rejected")
	}
}
