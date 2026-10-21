package nativeprotocol

import "testing"

func TestClaudeAdapterRejectsDirectApprovalEncoding(t *testing.T) {
	_, err := (claudeAdapter{}).Encode(Input{Kind: InputApproval, RequestID: []byte(`"fixture"`), ApprovalAllow: true})
	if err == nil {
		t.Fatal("approvals must use the pending-approval transport")
	}
}
