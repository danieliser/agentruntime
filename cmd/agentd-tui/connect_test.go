package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDurableTUIControlUsesV1HTTP(t *testing.T) {
	requests := make(chan map[string]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/sessions/session-1/input" {
			t.Errorf("control request = %s %s", request.Method, request.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(request.Body).Decode(&body)
		requests <- body
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	if err := sendSessionInput(port, "session-1", "steer", "look here"); err != nil {
		t.Fatalf("send durable input: %v", err)
	}
	body := <-requests
	if body["kind"] != "steer" || body["text"] != "look here" || body["idempotency_key"] == "" {
		t.Fatalf("durable input body = %v", body)
	}
}

func TestLoadChatHistoryUsesDurableEventsAndReturnsCurrentCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/sessions/current/events" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{"api_version": "v1", "data": map[string]interface{}{
			"events": []map[string]interface{}{
				{"session_id": "current", "sequence": 1, "type": "content.delta", "stream": "provider_stdout", "payload": map[string]string{"text": "hello "}},
				{"session_id": "current", "sequence": 2, "type": "content.delta", "stream": "provider_stdout", "payload": map[string]string{"text": "world"}},
				{"session_id": "current", "sequence": 3, "type": "tool.call", "stream": "provider_stdout", "payload": map[string]string{"name": "Read"}},
			},
			"has_more": false,
		}})
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	messages, cursor := loadChatHistory(port, []string{"current"}, "current")
	if cursor != 3 || len(messages) != 2 || messages[0].Type != "agent_message" || messages[0].Data["text"] != "hello world" || messages[1].Type != "tool_use" {
		t.Fatalf("durable history = %+v cursor=%d", messages, cursor)
	}
}
