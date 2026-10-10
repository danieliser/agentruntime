package agent

import "testing"

func TestClaudeNativeLogicalSessionUUID(t *testing.T) {
	agent := &ClaudeAgent{}
	logical := "782f76c7eb0fd66ed1df3b999ea78725"
	cfg := AgentConfig{NativeStream: true, SessionID: logical}
	first, err := agent.BuildCmd("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Independent RFC UUIDv5 vector: URL namespace, UTF-8 name prefix plus logical ID.
	expected := "0de9e235-c8d0-5d00-b505-fc0b2ddb2bae"
	if !containsSequence(first, "--session-id", expected) {
		t.Fatalf("logical ID was not mapped to stable provider UUID: %v", first)
	}
	second, err := agent.BuildCmd("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSequence(second, "--session-id", expected) {
		t.Fatalf("provider UUID changed across rebuild: %v", second)
	}
	if cfg.SessionID != logical {
		t.Fatalf("public logical ID mutated: %s", cfg.SessionID)
	}
	other, err := agent.BuildCmd("", AgentConfig{NativeStream: true, SessionID: "782f76c7eb0fd66ed1df3b999ea78726"})
	if err != nil {
		t.Fatal(err)
	}
	if containsSequence(other, "--session-id", expected) {
		t.Fatalf("different logical IDs share provider UUID: %v", other)
	}
}

func TestClaudeNativeCanonicalUUIDPreserved(t *testing.T) {
	id := "3f3a8d90-2f7b-44f9-83ce-178e9d410013"
	cmd, err := (&ClaudeAgent{}).BuildCmd("", AgentConfig{NativeStream: true, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !containsSequence(cmd, "--session-id", id) {
		t.Fatalf("canonical provider UUID changed: %v", cmd)
	}
}

func TestClaudeNativeResumeIdentityNotDerived(t *testing.T) {
	providerID := "observed-provider-session"
	cmd, err := (&ClaudeAgent{}).BuildCmd("", AgentConfig{NativeStream: true, SessionID: "logical-id", ResumeSessionID: providerID})
	if err != nil {
		t.Fatal(err)
	}
	if !containsSequence(cmd, "--resume", providerID) || contains(cmd, "--session-id") {
		t.Fatalf("resume identity changed: %v", cmd)
	}
}
