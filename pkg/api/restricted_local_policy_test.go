package api

import (
	"encoding/json"
	"testing"

	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func TestRestrictedLocalCodexTurnGrantsOnlyHostWorkspace(t *testing.T) {
	req := SessionRequest{Agent: "codex", Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
	adapter, err := nativeprotocol.NewAdapter(nativeprotocol.ProviderCodex)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := adapter.Encode(nativeprotocol.Input{Kind: nativeprotocol.InputPrompt, Text: "inspect", ProviderID: "fixture", Policy: nativePolicy(req)})
	if err != nil {
		t.Fatal(err)
	}

	var body struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frames[0], &body); err != nil {
		t.Fatal(err)
	}
	if string(body.Params["permissions"]) != `"agentd-local-v1"` {
		t.Fatal("missing named profile")
	}
	if _, exists := body.Params["sandboxPolicy"]; exists {
		t.Fatal("legacy override discards read confinement")
	}
}
