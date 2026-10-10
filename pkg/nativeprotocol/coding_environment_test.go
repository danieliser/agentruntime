package nativeprotocol

import (
	"context"
	"testing"
)

func TestCodexRestrictedCodingKeepsLocalEnvironmentAndExactPolicy(t *testing.T) {
	adapter, _ := NewAdapter(ProviderCodex)
	process := newFakeProcess()
	transport := NewStreamTransport(adapter, process.IO(), RecoveryMetadata{SessionID: "coding-env", Generation: 1})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	policy := InputPolicy{Enforced: true, AllowLocalEnvironment: true, ApprovalPolicy: "on-request", Filesystem: "workspace_write", NetworkAccess: true}
	done := make(chan error, 1)
	go func() { done <- transport.Bootstrap(context.Background(), BootstrapRequest{Policy: policy}) }()
	_ = process.readInput(t)
	process.writeStdout(t, []byte(`{"id":0,"result":{"userAgent":"codex"}}`))
	_ = process.readInput(t)
	thread := decodeInputObject(t, process.readInput(t))["params"].(map[string]any)
	if _, present := thread["environments"]; present {
		t.Errorf("coding bootstrap disabled local environment: %+v", thread)
	}
	if thread["sandbox"] != "workspace-write" || thread["approvalPolicy"] != "on-request" {
		t.Errorf("coding policy widened: %+v", thread)
	}
	process.writeStdout(t, []byte(`{"id":1,"result":{"threadId":"coding-thread"}}`))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"initial", "follow-up"} {
		if err := transport.Send(context.Background(), Input{Kind: InputPrompt, Text: prompt}); err != nil {
			t.Fatal(err)
		}
		params := decodeInputObject(t, process.readInput(t))["params"].(map[string]any)
		sandbox := params["sandboxPolicy"].(map[string]any)
		if sandbox["type"] != "workspaceWrite" || params["approvalPolicy"] != "on-request" {
			t.Errorf("turn policy widened: %+v", params)
		}
		roots := sandbox["writableRoots"].([]any)
		if len(roots) != 1 || roots[0] != "/workspace" {
			t.Errorf("scope widened: %+v", roots)
		}
	}
	process.finish(Exit{Code: 0})
	<-transport.Wait()
}
