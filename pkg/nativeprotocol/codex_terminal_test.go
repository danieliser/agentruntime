package nativeprotocol

import (
	"encoding/json"
	"testing"
)

func TestCodexDecodesProviderErrors(t *testing.T) {
	tests := []struct {
		name, params, message string
		retry                 *bool
	}{
		{"terminal nested error", `{"error":{"message":"fixture model unsupported","codexErrorInfo":"other"},"willRetry":false}`, "fixture model unsupported", boolPointer(false)},
		{"retryable nested error", `{"error":{"message":"temporary transport error"},"willRetry":true}`, "temporary transport error", boolPointer(true)},
		{"legacy message", `{"message":"legacy error"}`, "legacy error", nil},
		{"nested message takes precedence", `{"message":"outer","error":{"message":"nested"}}`, "nested", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := (codexAdapter{}).Decode([]byte(`{"method":"error","params":` + tt.params + `}`))
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if event.Type != "error.provider" || payload["message"] != tt.message {
				t.Errorf("event=%+v payload=%+v", event, payload)
			}
			got, present := payload["will_retry"]
			if tt.retry == nil {
				if present {
					t.Errorf("unexpected retry flag: %v", got)
				}
			} else if !present || got != *tt.retry {
				t.Errorf("retry=%v present=%v want=%v", got, present, *tt.retry)
			}
		})
	}
}

func TestCodexDecodesTerminalTurnStatus(t *testing.T) {
	tests := []struct{ name, turn, event, status, message string }{
		{"failed", `{"id":"turn-1","status":"failed","error":{"message":"fixture model unsupported"}}`, "turn.failed", "failed", "fixture model unsupported"},
		{"interrupted", `{"id":"turn-1","status":"interrupted","error":null}`, "turn.failed", "interrupted", ""},
		{"completed", `{"id":"turn-1","status":"completed","error":null}`, "turn.completed", "completed", ""},
		{"legacy missing turn", `null`, "turn.completed", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":` + tt.turn + `,"usage":{"input_tokens":7}}}`)
			original := string(raw)
			event, err := (codexAdapter{}).Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Status  string `json:"status"`
				Message string `json:"message"`
				Usage   struct {
					Input int `json:"input_tokens"`
				} `json:"usage"`
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if event.Type != tt.event || payload.Status != tt.status || payload.Message != tt.message {
				t.Errorf("type=%s status=%s message=%s", event.Type, payload.Status, payload.Message)
			}
			if event.ProviderID != "thread-1" || (tt.turn != "null" && event.TurnID != "turn-1") || payload.Usage.Input != 7 {
				t.Errorf("identity/usage lost: %+v %+v", event, payload)
			}
			if tt.name == "failed" && len(payload.Error) == 0 {
				t.Error("native error missing")
			}
			if string(raw) != original {
				t.Error("raw record mutated")
			}
		})
	}
}

func boolPointer(value bool) *bool { return &value }
