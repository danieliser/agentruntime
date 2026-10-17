package nativeprotocol

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSubagentActivityDecode(t *testing.T) {
	for _, tc := range []struct {
		provider      Provider
		raw, kind, id string
	}{
		{ProviderCodex, `{"method":"item/completed","params":{"threadId":"parent","item":{"type":"subAgentActivity","kind":"started","agentThreadId":"child","agentPath":"/root/check","model":"gpt-6-luna"}}}`, "subagent.started", "child"},
		{ProviderClaude, `{"type":"assistant","session_id":"parent","parent_tool_use_id":"task-1","message":{"model":"claude-haiku-4-5","content":[{"type":"text","text":"child summary"}]}}`, "subagent.content.completed", "task-1"},
		{ProviderClaude, `{"type":"system","subtype":"task_started","session_id":"parent","task_id":"task-1","tool_use_id":"tool-1","description":"child"}`, "subagent.started", "task-1"},
		{ProviderClaude, `{"type":"system","subtype":"task_notification","session_id":"parent","task_id":"task-1","status":"completed","summary":"done"}`, "subagent.completed", "task-1"},
	} {
		adapter, _ := NewAdapter(tc.provider)
		got, err := adapter.Decode([]byte(tc.raw))
		var p map[string]any
		_ = json.Unmarshal(got.Payload, &p)
		if err != nil || got.Type != tc.kind || p["subagent_thread_id"] != tc.id || p["subagent_path"] == "" {
			t.Errorf("decode=%+v payload=%s err=%v", got, got.Payload, err)
		}
	}
}

func TestCodexChildDoesNotRetargetParentControls(t *testing.T) {
	process := newFakeProcess()
	adapter, _ := NewAdapter(ProviderCodex)
	tr := NewStreamTransport(adapter, process.IO(), RecoveryMetadata{})
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.finish(Exit{}); <-tr.Wait() })
	for _, raw := range []string{
		`{"method":"turn/started","params":{"threadId":"parent","turn":{"id":"parent-turn"}}}`,
		`{"method":"turn/started","params":{"threadId":"child","turn":{"id":"child-turn"}}}`,
	} {
		process.writeStdout(t, []byte(raw))
		receiveRecord(t, tr.Records())
	}
	if err := tr.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := decodeInputObject(t, process.readInput(t))
	p := req["params"].(map[string]any)
	if p["threadId"] != "parent" || p["turnId"] != "parent-turn" {
		t.Fatalf("retargeted parent: %v", req)
	}
}

func TestCodexNestedThreadIdentityAndChildTags(t *testing.T) {
	adapter, _ := NewAdapter(ProviderCodex)
	for _, tc := range []struct{ raw, id, kind string }{
		{`{"method":"thread/started","params":{"thread":{"id":"parent","model":"primary"}}}`, "parent", "lifecycle.provider.session"},
		{`{"method":"thread/started","params":{"thread":{"id":"child","parentThreadId":"parent","model":"child-model"}}}`, "child", "subagent.lifecycle.provider.session"},
	} {
		event, err := adapter.Decode([]byte(tc.raw))
		if err != nil || event.ProviderID != tc.id || event.Type != tc.kind {
			t.Fatalf("event=%+v err=%v", event, err)
		}
	}
}
