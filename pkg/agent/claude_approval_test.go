package agent

import "testing"

func TestClaudeNativeStreamRoutesPermissionPromptsToHost(t *testing.T) {
	for _, cfg := range []AgentConfig{
		{NativeStream: true},
		{NativeStream: true, PermissionMode: "bypassPermissions"},
		{NativeStream: true, EnforcePolicy: true, PermissionMode: "dontAsk", AllowedTools: []string{"Read"}},
	} {
		cmd, err := (&ClaudeAgent{}).BuildCmd("", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !containsSequence(cmd, "--permission-prompt-tool", "stdio") {
			t.Fatalf("native permission requests have no host handler: %v", cmd)
		}
	}
}

func TestClaudeNonNativeStreamDoesNotAdvertiseHostApproval(t *testing.T) {
	for _, cfg := range []AgentConfig{{}, {Interactive: true}} {
		cmd, err := (&ClaudeAgent{}).BuildCmd("test", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if contains(cmd, "--permission-prompt-tool") {
			t.Fatalf("command without native approval transport advertises host approval: %v", cmd)
		}
	}
}
