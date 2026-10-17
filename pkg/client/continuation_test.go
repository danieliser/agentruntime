package client

import (
	"encoding/json"
	"testing"
)

func TestDurableSessionPreservesContinuationView(t *testing.T) {
	var session DurableSession
	if err := json.Unmarshal([]byte(`{"session_id":"ready","continuable":true,"continuable_reason":"completed generation can resume"}`), &session); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view["continuable"] != true || view["continuable_reason"] != "completed generation can resume" {
		t.Fatalf("client discarded continuation fields: %s", raw)
	}
}
