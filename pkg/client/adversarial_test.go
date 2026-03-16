package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/api"
)

func TestClient_Dispatch_InvalidJSON(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions" {
			t.Fatalf("expected /sessions, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session_id":`))
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.Dispatch(context.Background(), api.SessionRequest{
		Agent:  "claude",
		Prompt: "break me",
	})
	if err == nil {
		t.Fatal("expected decode error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("expected decode response error, got %v", err)
	}
}

func TestClient_Dispatch_HTTP500JSONErrorBody(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"session exploded"}`))
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.Dispatch(context.Background(), api.SessionRequest{
		Agent:  "claude",
		Prompt: "break me",
	})
	if err == nil {
		t.Fatal("expected server error")
	}
	if !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Fatalf("expected status in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "session exploded") {
		t.Fatalf("expected error body in error, got %v", err)
	}
}

func TestClient_Dispatch_ConnectionClosedMidResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}

		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack response: %v", err)
		}
		defer conn.Close()

		_, _ = buf.WriteString("HTTP/1.1 201 Created\r\n")
		_, _ = buf.WriteString("Content-Type: application/json\r\n")
		_, _ = buf.WriteString("Content-Length: 64\r\n")
		_, _ = buf.WriteString("\r\n")
		_, _ = buf.WriteString(`{"session_id":"sess-broken"`)
		_ = buf.Flush()
	}))
	defer server.Close()

	client := New(server.URL)
	result := make(chan error, 1)
	go func() {
		_, err := client.Dispatch(context.Background(), api.SessionRequest{
			Agent:  "claude",
			Prompt: "break me",
		})
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected error for truncated response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch hung after server closed connection mid-response")
	}
}

func TestClient_Dispatch_EmptyBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.Dispatch(context.Background(), api.SessionRequest{
		Agent:  "claude",
		Prompt: "break me",
	})
	if err == nil {
		t.Fatal("expected decode error for empty body")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("expected decode response error, got %v", err)
	}
}

func TestClient_Dispatch_EmptySessionRequestStillSent(t *testing.T) {
	t.Parallel()

	type capturedRequest struct {
		body        map[string]any
		contentType string
	}
	gotRequest := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if len(raw) == 0 {
			t.Fatal("expected JSON body for empty request")
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal request body: %v", err)
		}
		gotRequest <- capturedRequest{
			body:        body,
			contentType: r.Header.Get("Content-Type"),
		}

		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(api.SessionResponse{
			SessionID: "sess-empty",
			Agent:     "",
			Runtime:   "local",
			Status:    "running",
			WSURL:     "ws://example.test/ws/sessions/sess-empty",
			LogURL:    "http://example.test/sessions/sess-empty/logs",
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL)
	resp, err := client.Dispatch(context.Background(), api.SessionRequest{})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	got := <-gotRequest

	if got.contentType != "application/json" {
		t.Fatalf("expected application/json content type, got %q", got.contentType)
	}
	agent, ok := got.body["agent"]
	if !ok || agent != "" {
		t.Fatalf("expected empty agent field in request body, got %#v", got.body["agent"])
	}
	prompt, ok := got.body["prompt"]
	if !ok || prompt != "" {
		t.Fatalf("expected empty prompt field in request body, got %#v", got.body["prompt"])
	}
	if resp.SessionID != "sess-empty" {
		t.Fatalf("expected sess-empty response, got %q", resp.SessionID)
	}
}

func TestClient_GetLogs_MissingCursorHeaderDefaultsToZero(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/sess-log/logs" {
			t.Fatalf("expected log path, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("chunk"))
	}))
	defer server.Close()

	client := New(server.URL)
	data, nextCursor, err := client.GetLogs(context.Background(), "sess-log", 0)
	if err != nil {
		t.Fatalf("GetLogs returned error: %v", err)
	}
	if string(data) != "chunk" {
		t.Fatalf("expected chunk body, got %q", string(data))
	}
	if nextCursor != 0 {
		t.Fatalf("expected default cursor 0, got %d", nextCursor)
	}
}

func TestClient_GetLogs_NonNumericCursorHeader(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Agentruntime-Log-Cursor", "not-a-number")
		_, _ = w.Write([]byte("chunk"))
	}))
	defer server.Close()

	client := New(server.URL)
	_, _, err := client.GetLogs(context.Background(), "sess-log", 0)
	if err == nil {
		t.Fatal("expected cursor parse error")
	}
	if !strings.Contains(err.Error(), "parse Agentruntime-Log-Cursor") {
		t.Fatalf("expected cursor parse error, got %v", err)
	}
}

func TestClient_Kill_Conflict(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already stopped"}`))
	}))
	defer server.Close()

	client := New(server.URL)
	err := client.Kill(context.Background(), "sess-409")
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "409 Conflict") {
		t.Fatalf("expected conflict status in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "already stopped") {
		t.Fatalf("expected response body in error, got %v", err)
	}
}

func TestClient_StreamLogs_ContextCancellationStopsIt(t *testing.T) {
	t.Parallel()

	logCalls := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sessions/sess-stream/logs":
			select {
			case logCalls <- struct{}{}:
			default:
			}
			w.Header().Set("Agentruntime-Log-Cursor", "1")
			_, _ = w.Write([]byte("log line\n"))
		case "/sessions/sess-stream":
			if err := json.NewEncoder(w).Encode(api.SessionSummary{
				SessionID: "sess-stream",
				Agent:     "claude",
				Runtime:   "local",
				Status:    "running",
			}); err != nil {
				t.Fatalf("encode session: %v", err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := New(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.StreamLogs(ctx, "sess-stream")
	if err != nil {
		t.Fatalf("StreamLogs returned error: %v", err)
	}
	defer stream.Close()

	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(stream)
		readDone <- err
	}()

	select {
	case <-logCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLogs never issued a log request")
	}

	cancel()

	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("expected context cancellation error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLogs did not stop after context cancellation")
	}
}

func TestClient_GetSession_EncodesURLUnsafeID(t *testing.T) {
	t.Parallel()

	id := "sess with / and #"
	escaped := url.PathEscape(id)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "/sessions/"+escaped {
			t.Fatalf("expected encoded request URI, got %q", r.RequestURI)
		}
		if err := json.NewEncoder(w).Encode(api.SessionSummary{
			SessionID: id,
			Agent:     "claude",
			Runtime:   "local",
			Status:    "running",
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL)
	resp, err := client.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession returned error: %v", err)
	}
	if resp.SessionID != id {
		t.Fatalf("expected round-tripped session id %q, got %q", id, resp.SessionID)
	}
}

func TestClient_Dispatch_ConcurrentCalls(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}

		var req api.SessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		n := callCount.Add(1)
		if err := json.NewEncoder(w).Encode(api.SessionResponse{
			SessionID: fmt.Sprintf("sess-%d", n),
			Agent:     req.Agent,
			Runtime:   "local",
			Status:    "running",
			WSURL:     "ws://example.test/ws/sessions/test",
			LogURL:    "http://example.test/sessions/test/logs",
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL)

	const workers = 20
	var wg sync.WaitGroup
	results := make(chan string, workers)
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			resp, err := client.Dispatch(context.Background(), api.SessionRequest{
				Agent:  "claude",
				Prompt: fmt.Sprintf("prompt-%d", i),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- resp.SessionID
		}(i)
	}

	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	seen := make(map[string]struct{}, workers)
	for sessionID := range results {
		if sessionID == "" {
			t.Fatal("expected non-empty session id")
		}
		if _, exists := seen[sessionID]; exists {
			t.Fatalf("duplicate session id %q", sessionID)
		}
		seen[sessionID] = struct{}{}
	}

	if got := callCount.Load(); got != workers {
		t.Fatalf("expected %d requests, got %d", workers, got)
	}
}

func TestClient_Dispatch_UnreachableBaseURLTimeout(t *testing.T) {
	t.Parallel()

	client := &Client{
		BaseURL: "http://unreachable.invalid",
		HTTPClient: &http.Client{
			Timeout: 50 * time.Millisecond,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				},
			},
		},
	}

	_, err := client.Dispatch(context.Background(), api.SessionRequest{
		Agent:  "claude",
		Prompt: "break me",
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Fatalf("expected timeout-style error, got %v", err)
	}
}
