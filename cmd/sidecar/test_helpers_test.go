package main

import (
	"encoding/json"
	"testing"
	"time"
)

// Shared test helpers — extracted to avoid redeclaration across test files.

func sharedExpectEventType(t *testing.T, events <-chan Event, wantType string) Event {
	t.Helper()
	select {
	case e := <-events:
		if e.Type != wantType {
			t.Fatalf("expected event type %q, got %q (data=%v)", wantType, e.Type, e.Data)
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for event type %q", wantType)
		return Event{}
	}
}

func sharedEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sharedMapField(t *testing.T, obj map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := obj[key]
	if !ok {
		t.Fatalf("missing key %q in %v", key, obj)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("key %q is %T, not map", key, v)
	}
	return m
}

func sharedSliceField(t *testing.T, obj map[string]any, key string) []any {
	t.Helper()
	v, ok := obj[key]
	if !ok {
		t.Fatalf("missing key %q in %v", key, obj)
	}
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("key %q is %T, not slice", key, v)
	}
	return s
}

func sharedReadJSONMessage(t *testing.T, conn interface{ ReadMessage() (int, []byte, error) }) map[string]any {
	t.Helper()
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	return msg
}
