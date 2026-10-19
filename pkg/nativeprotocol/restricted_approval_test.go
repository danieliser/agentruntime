package nativeprotocol

import (
	"fmt"
	"testing"
)

func TestCodexRestrictedApprovalEvents(t *testing.T) {
	adapter, err := NewAdapter(ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "mcpServer/elicitation/request"} {
		event, err := adapter.Decode([]byte(fmt.Sprintf(`{"id":42,"method":%q,"params":{"threadId":"fixture"}}`, method)))
		if err != nil || event.Type != "control.approval.request" {
			t.Errorf("%s: type=%s err=%v", method, event.Type, err)
		}
	}
}
