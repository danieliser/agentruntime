package main

import "testing"

func TestMapDurableEventPreservesTUIRenderingContract(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		wantType  string
	}{
		{name: "content", eventType: "content.delta", wantType: "agent_message"},
		{name: "tool call", eventType: "tool.call", wantType: "tool_use"},
		{name: "tool result", eventType: "tool.result", wantType: "tool_result"},
		{name: "stderr", eventType: "runtime.stderr", wantType: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped, ok := mapDurableEvent(durableEvent{
				Type: test.eventType, Sequence: 7, Payload: map[string]interface{}{"text": "hello"},
			})
			if !ok || mapped.Type != test.wantType || mapped.Offset != 7 {
				t.Fatalf("mapped event = %+v ok=%v", mapped, ok)
			}
		})
	}
	if _, ok := mapDurableEvent(durableEvent{Type: "lifecycle.turn.started"}); ok {
		t.Fatal("non-rendered lifecycle event should be ignored")
	}
}
