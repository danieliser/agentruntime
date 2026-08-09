package observer

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
)

func TestEventFramePreservesDurableEnvelope(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 34, 56, 789, time.UTC)
	event := durable.Event{
		SchemaVersion: "1.0", EventID: "21c63ba3-4280-4678-b608-dc00e1080a9e",
		SessionID: "718258fe-2921-4f67-91c9-cb70720264b4", Generation: 2,
		Sequence: 42, Timestamp: now, Type: "tool.call", Stream: durable.StreamProviderStdout,
		Payload: json.RawMessage(`{"name":"shell"}`), Raw: []byte(`{"type":"tool_call"}`),
		RawSHA256: "f00d",
	}

	frame := NewEventFrame("delivery-42", event)
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var decoded EventFrame
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != MessageEvent || decoded.DeliveryID != "delivery-42" {
		t.Fatalf("unexpected frame identity: %+v", decoded)
	}
	if decoded.Event.EventID != event.EventID || decoded.Event.Sequence != event.Sequence || decoded.Event.Generation != event.Generation {
		t.Fatalf("durable identity changed: %+v", decoded.Event)
	}
	if string(decoded.Event.Raw) != string(event.Raw) || decoded.Event.RawSHA256 != event.RawSHA256 {
		t.Fatalf("raw provider record changed: raw=%q hash=%q", decoded.Event.Raw, decoded.Event.RawSHA256)
	}
}

func TestContextEventFrameCarriesDurableLinkageWithoutSecretValues(t *testing.T) {
	event := durable.Event{SchemaVersion: "1.0", EventID: "event-1", SessionID: "session-1", Generation: 1, Sequence: 1, Timestamp: time.Now().UTC()}
	context := EventContext{
		JobID: "trading-floor-job", Agent: "codex", Runtime: "docker",
		RequestManifest: json.RawMessage(`{"prompt":"inspect","env":{"SAFE":"yes"}}`),
		SecretGrants:    []string{"OPENAI_API_KEY"}, ProviderSessionID: "thread-1",
		ImageDigest: "sha256:image", SandboxProfile: "docker-native-v1",
	}
	frame := NewContextEventFrame("delivery-1", event, context)
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) || frame.Context.JobID != "trading-floor-job" || frame.Context.ProviderSessionID != "thread-1" {
		t.Fatalf("context frame = %s", encoded)
	}
	if len(encoded) == 0 || bytes.Contains(encoded, []byte("secret-value")) {
		t.Fatalf("context leaked a secret value: %s", encoded)
	}
}

func TestValidateHandshakeRejectsIncompatibleContracts(t *testing.T) {
	valid := HelloResponse{
		Type: MessageHello, RequestID: "hello-1", Plugin: PluginIdentity{Name: "opentraces", Version: "0.1.0"},
		PluginAPIVersion: APIVersion, EventSchemaVersions: []string{"1.0"},
		Capabilities: Capabilities{ImmutableEvents: true, IdempotentEvents: true, TraceLinkage: true},
	}
	if err := ValidateHandshake(valid, "hello-1", "1.0"); err != nil {
		t.Fatalf("valid handshake rejected: %v", err)
	}

	tests := map[string]HelloResponse{
		"request mismatch": func() HelloResponse { v := valid; v.RequestID = "wrong"; return v }(),
		"API mismatch":     func() HelloResponse { v := valid; v.PluginAPIVersion = "2.0"; return v }(),
		"schema mismatch":  func() HelloResponse { v := valid; v.EventSchemaVersions = []string{"0.9"}; return v }(),
		"mutable consumer": func() HelloResponse { v := valid; v.Capabilities.ImmutableEvents = false; return v }(),
		"not idempotent":   func() HelloResponse { v := valid; v.Capabilities.IdempotentEvents = false; return v }(),
		"no trace linkage": func() HelloResponse { v := valid; v.Capabilities.TraceLinkage = false; return v }(),
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateHandshake(response, "hello-1", "1.0"); err == nil {
				t.Fatal("expected incompatible handshake")
			}
		})
	}
}

func TestValidateAckRequiresExactImmutableIdentity(t *testing.T) {
	event := WireEvent{EventID: "event-1", SessionID: "session-1", Sequence: 7}
	valid := AckFrame{Type: MessageAck, DeliveryID: "delivery-7", Status: AckAccepted, EventID: event.EventID, SessionID: event.SessionID, Sequence: event.Sequence, TraceID: "851ad0da-3f90-4ea8-9094-9b644d1913f7"}
	if err := ValidateAck(valid, "delivery-7", event); err != nil {
		t.Fatalf("valid acknowledgement rejected: %v", err)
	}

	mutations := []func(*AckFrame){
		func(v *AckFrame) { v.DeliveryID = "wrong" },
		func(v *AckFrame) { v.EventID = "wrong" },
		func(v *AckFrame) { v.SessionID = "wrong" },
		func(v *AckFrame) { v.Sequence++ },
		func(v *AckFrame) { v.Status = AckRejected },
	}
	for i, mutate := range mutations {
		candidate := valid
		mutate(&candidate)
		if err := ValidateAck(candidate, "delivery-7", event); err == nil {
			t.Fatalf("mutation %d unexpectedly accepted: %+v", i, candidate)
		}
	}
}

func TestProtocolContainsNoExecutionControlMessages(t *testing.T) {
	for _, messageType := range SupportedMessageTypes() {
		switch messageType {
		case "start", "prompt", "steer", "interrupt", "cancel", "terminate", "resume", "authorize", "admit":
			t.Fatalf("observer protocol grants execution authority through %q", messageType)
		}
	}
}
