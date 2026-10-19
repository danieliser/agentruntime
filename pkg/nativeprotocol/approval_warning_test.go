package nativeprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexApprovalWarningIsRuntimeAccurate(t *testing.T) {
	for _, runtimeName := range []string{"local", "docker", ""} {
		for _, method := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"} {
			t.Run(runtimeName+"/"+method, func(t *testing.T) {
				adapter, err := NewAdapter(ProviderCodex, runtimeName)
				if err != nil {
					t.Fatal(err)
				}
				tr := NewStreamTransport(adapter, ProcessIO{}, RecoveryMetadata{})
				raw := []byte(`{"id":42,"method":"` + method + `","params":{"runtime":"local"}}`)
				tr.observeApproval(raw)
				pending := tr.PendingApprovals()
				if len(pending) != 1 {
					t.Fatal("approval absent")
				}
				event, err := adapter.Decode(raw)
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					Warning string `json:"warning"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				for _, warning := range []string{pending[0].Warning, payload.Warning} {
					if !strings.Contains(warning, "runs outside the sandbox") {
						t.Error("missing unsandboxed warning")
					}
					if runtimeName == "local" && !strings.Contains(warning, "runs outside the sandbox on the host") {
						t.Error("missing local host warning")
					}
					if runtimeName != "local" && strings.Contains(warning, "host") {
						t.Error("nonlocal warning claims host execution")
					}
				}
			})
		}
	}
}
