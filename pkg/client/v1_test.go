package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danieliser/agentruntime/pkg/api"
)

func TestClientDurableDispatchDerivesStableIdempotencyAndReadsEvents(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/sessions":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			key, _ := body["idempotency_key"].(string)
			keys = append(keys, key)
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": map[string]any{
				"session_id": "durable-session", "idempotency_key": key, "agent": "claude", "runtime": "docker",
				"state": "running", "generation": 1, "last_sequence": 0,
				"events_url":       serverURL(request) + "/api/v1/sessions/durable-session/events",
				"event_stream_url": "ws://example/api/v1/ws/sessions/durable-session/events",
			}})
		case "/api/v1/sessions/durable-session/events":
			if request.URL.Query().Get("after_sequence") != "0" {
				t.Errorf("after_sequence = %q", request.URL.Query().Get("after_sequence"))
			}
			raw := []byte(`{"type":"stream_event"}`)
			_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": map[string]any{
				"events": []map[string]any{{
					"schema_version": "1.0", "event_id": "evt-1", "session_id": "durable-session",
					"generation": 1, "sequence": 1, "type": "content.delta", "stream": "provider_stdout",
					"payload": map[string]any{"text": "hello"}, "raw_base64": base64.StdEncoding.EncodeToString(raw), "raw_sha256": "sha256:raw",
				}}, "earliest_sequence": 1, "last_sequence": 1, "has_more": false,
			}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := New(server.URL)
	request := durableTestRequest()
	first, err := client.DispatchDurable(context.Background(), request)
	if err != nil {
		t.Fatalf("dispatch durable: %v", err)
	}
	second, err := client.DispatchDurable(context.Background(), request)
	if err != nil {
		t.Fatalf("repeat durable dispatch: %v", err)
	}
	if first.SessionID != "durable-session" || first.IdempotencyKey == "" || first.IdempotencyKey != second.IdempotencyKey || len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("durable dispatches first=%+v second=%+v keys=%v", first, second, keys)
	}
	page, err := client.GetEvents(context.Background(), first.SessionID, 0, 100)
	if err != nil || len(page.Events) != 1 || string(page.Events[0].Raw) != `{"type":"stream_event"}` || page.Events[0].Sequence != 1 {
		t.Fatalf("durable events = %+v err=%v", page, err)
	}
}

func TestClientStreamEventRawStopsAtDurableTerminal(t *testing.T) {
	providerRaw := []byte(`{"type":"stream_event","delta":"hello"}`)
	terminalRaw := []byte(`{"reason":"completed","exit_code":0}`)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/sessions/stream-session/events" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": map[string]any{
			"events": []map[string]any{
				{"schema_version": "1.0", "event_id": "evt-1", "session_id": "stream-session", "generation": 1, "sequence": 1, "type": "content.delta", "stream": "provider_stdout", "raw_base64": base64.StdEncoding.EncodeToString(providerRaw)},
				{"schema_version": "1.0", "event_id": "evt-2", "session_id": "stream-session", "generation": 1, "sequence": 2, "type": "session.completed", "stream": "terminal", "raw_base64": base64.StdEncoding.EncodeToString(terminalRaw)},
			}, "earliest_sequence": 1, "last_sequence": 2, "has_more": false,
		}})
	}))
	defer server.Close()
	stream, err := New(server.URL).StreamEventRaw(context.Background(), "stream-session", 0)
	if err != nil {
		t.Fatalf("stream raw events: %v", err)
	}
	defer stream.Close()
	raw, err := io.ReadAll(stream)
	if err != nil || string(raw) != string(providerRaw)+"\n" {
		t.Fatalf("raw durable stream = %q err=%v", raw, err)
	}
}

func TestClientListsDurableSessions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/sessions" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": []map[string]any{
			{"session_id": "active", "state": "running", "generation": 1, "last_sequence": 4},
			{"session_id": "history", "state": "completed", "generation": 1, "last_sequence": 9},
		}})
	}))
	defer server.Close()
	sessions, err := New(server.URL).ListDurableSessions(context.Background())
	if err != nil || len(sessions) != 2 || sessions[0].SessionID != "active" || sessions[1].State != "completed" {
		t.Fatalf("durable session list = %+v err=%v", sessions, err)
	}
}

func TestClientReadsObserverHealthAndTraceLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/plugins":
			_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": []map[string]any{{
				"name": "opentraces", "version": "0.9.0", "policy": "best_effort", "state": "degraded", "unacknowledged_events": 3,
			}}})
		case "/api/v1/sessions/native/traces":
			_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": []map[string]any{{
				"plugin": "opentraces", "session_id": "native", "trace_id": "851ad0da-3f90-4ea8-9094-9b644d1913f7", "acknowledged_sequence": 12,
			}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := New(server.URL)
	plugins, err := client.ListPlugins(context.Background())
	if err != nil || len(plugins) != 1 || plugins[0].Unacknowledged != 3 {
		t.Fatalf("plugins = %+v err=%v", plugins, err)
	}
	links, err := client.GetTraceLinks(context.Background(), "native")
	if err != nil || len(links) != 1 || links[0].AcknowledgedSequence != 12 {
		t.Fatalf("trace links = %+v err=%v", links, err)
	}
}

func TestClientUsesV1LifecycleControlsAndReceipt(t *testing.T) {
	type observedRequest struct {
		Method string
		Path   string
		Body   map[string]any
	}
	var observed []observedRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := map[string]any{}
		if request.Body != nil {
			_ = json.NewDecoder(request.Body).Decode(&body)
		}
		observed = append(observed, observedRequest{Method: request.Method, Path: request.URL.Path, Body: body})
		switch request.URL.Path {
		case "/api/v1/sessions/native/input", "/api/v1/sessions/native/interrupt", "/api/v1/sessions/native/cancel", "/api/v1/sessions/native/terminate":
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": map[string]any{"accepted": true}})
		case "/api/v1/sessions/native/resume":
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": map[string]any{"session_id": "native", "state": "running", "generation": 2}})
		case "/api/v1/sessions/native/receipt":
			_ = json.NewEncoder(writer).Encode(map[string]any{"api_version": "v1", "data": map[string]any{"session_id": "native", "generation": 2, "state": "cancelled", "reason": "terminated", "last_sequence": 8}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := New(server.URL)
	ctx := context.Background()
	if err := client.SendInput(ctx, "native", "prompt-key", "prompt", "continue"); err != nil {
		t.Fatalf("send input: %v", err)
	}
	if err := client.Interrupt(ctx, "native", "interrupt-key"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := client.Cancel(ctx, "native", "cancel-key"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := client.Terminate(ctx, "native", "terminate-key"); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	resumed, err := client.Resume(ctx, "native", "recover", map[string]string{"TOKEN": "secret"})
	if err != nil || resumed.Generation != 2 {
		t.Fatalf("resume = %+v err=%v", resumed, err)
	}
	receipt, err := client.GetTerminalReceipt(ctx, "native")
	if err != nil || receipt.Reason != "terminated" || receipt.LastSequence != 8 {
		t.Fatalf("receipt = %+v err=%v", receipt, err)
	}
	if len(observed) != 6 || observed[0].Path != "/api/v1/sessions/native/input" || observed[0].Body["kind"] != "prompt" ||
		observed[0].Body["idempotency_key"] != "prompt-key" || observed[3].Path != "/api/v1/sessions/native/terminate" ||
		observed[4].Body["prompt"] != "recover" || observed[5].Method != http.MethodGet {
		t.Fatalf("observed requests = %+v", observed)
	}
}

func durableTestRequest() api.SessionRequest {
	return api.SessionRequest{Agent: "claude", Runtime: "docker", Prompt: "hello"}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
