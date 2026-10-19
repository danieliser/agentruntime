package nativeprotocol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExpiredApprovalDeclinesBeforeRemoval(t *testing.T) {
	tr, w := approvalTransport(t, ProviderCodex)
	tr.observeApproval([]byte(`{"id":42,"method":"item/fileChange/requestApproval","params":{}}`))
	p := tr.approvals["n:42"]
	p.ExpiresAt = time.Now().Add(-time.Second)
	tr.approvals["n:42"] = p
	if len(tr.PendingApprovals()) != 0 {
		t.Fatal("expired authority remains live")
	}
	if !strings.Contains(w.String(), `"decision":"decline"`) {
		t.Fatal("provider was left waiting for expired approval")
	}
}

func TestPermissionRequestUsesExplicitRPCDecline(t *testing.T) {
	tr, w := approvalTransport(t, ProviderCodex)
	tr.observeApproval([]byte(`{"id":42,"method":"item/permissions/requestApproval","params":{}}`))
	if err := tr.DecideApproval(context.Background(), json.RawMessage(`42`), "decline"); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(w.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || len(response.Result) > 0 {
		t.Fatal("denial encoded as a successful grant")
	}
}

func TestNewCodexApprovalDecisionShapes(t *testing.T) {
	for _, method := range []string{"item/fileChange/requestApproval", "item/permissions/requestApproval", "mcpServer/elicitation/request"} {
		for _, decision := range []string{"accept", "decline", "cancel"} {
			t.Run(method+"/"+decision, func(t *testing.T) {
				tr, w := approvalTransport(t, ProviderCodex)
				tr.observeApproval([]byte(`{"id":42,"method":"` + method + `","params":{"mode":"form","_meta":{"codex_approval_kind":"mcp_tool_call"},"requestedSchema":{"type":"object","properties":{}}}}`))
				err := tr.DecideApproval(context.Background(), json.RawMessage(`42`), decision)
				if method == "item/permissions/requestApproval" && decision == "accept" {
					if err == nil || w.Len() != 0 {
						t.Fatal("unsupported permission expansion accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(w.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				if method == "item/permissions/requestApproval" {
					if len(wire["error"]) == 0 || len(wire["result"]) > 0 {
						t.Fatal("permission denial became grant")
					}
				} else {
					key := "decision"
					if method == "mcpServer/elicitation/request" {
						key = "action"
					}
					var result map[string]json.RawMessage
					if err := json.Unmarshal(wire["result"], &result); err != nil {
						t.Fatal(err)
					}
					if string(result[key]) != `"`+decision+`"` {
						t.Fatal("incorrect decision")
					}
				}
				if len(tr.PendingApprovals()) != 0 {
					t.Fatal("consent remained live")
				}
			})
		}
	}
}
