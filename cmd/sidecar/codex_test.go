package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

func TestCodexBackend_InitializeHandshake(t *testing.T) {
	proc := newFakeCodexProcess(t)
	runner := &fakeCodexSpawner{proc: proc}
	backend := newCodexBackendWithSpawner(log.New(io.Discard, "", 0), runner.spawn)
	t.Cleanup(func() { _ = backend.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.Spawn(context.Background())
	}()

	msg := proc.nextWrite(t)
	if got, want := runner.cmd, []string{"codex", "app-server", "--listen", "stdio://"}; !equalStrings(got, want) {
		t.Fatalf("spawned command = %v, want %v", got, want)
	}
	if msg["method"] != "initialize" {
		t.Fatalf("first method = %v, want initialize", msg["method"])
	}
	if idAsInt(t, msg["id"]) != 0 {
		t.Fatalf("initialize id = %v, want 0", msg["id"])
	}
	params := mapField(t, msg, "params")
	clientInfo := mapField(t, params, "clientInfo")
	if clientInfo["name"] != "agentruntime" || clientInfo["version"] != "0.3.0" {
		t.Fatalf("unexpected clientInfo: %#v", clientInfo)
	}
	capabilities := mapField(t, params, "capabilities")
	if capabilities["experimentalApi"] != true {
		t.Fatalf("expected experimentalApi true, got %#v", capabilities)
	}

	proc.send(t, map[string]any{
		"id": 0,
		"result": map[string]any{
			"userAgent": "codex-cli/0.114.0",
		},
	})

	msg = proc.nextWrite(t)
	if msg["method"] != "initialized" {
		t.Fatalf("second method = %v, want initialized", msg["method"])
	}
	if _, ok := msg["id"]; ok {
		t.Fatalf("initialized should be a notification, got id %v", msg["id"])
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Spawn() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Spawn to finish")
	}
}

func TestCodexBackend_SendPrompt_CreatesThread(t *testing.T) {
	proc := newFakeCodexProcess(t)
	backend := startTestCodexBackend(t, proc)

	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.SendPrompt("fix the bug")
	}()

	threadStart := proc.nextWrite(t)
	if threadStart["method"] != "thread/start" {
		t.Fatalf("first prompt method = %v, want thread/start", threadStart["method"])
	}
	proc.send(t, map[string]any{
		"id": idAsInt(t, threadStart["id"]),
		"result": map[string]any{
			"threadId": "thread-123",
		},
	})

	turnStart := proc.nextWrite(t)
	if turnStart["method"] != "turn/start" {
		t.Fatalf("second prompt method = %v, want turn/start", turnStart["method"])
	}
	params := mapField(t, turnStart, "params")
	if params["threadId"] != "thread-123" {
		t.Fatalf("turn/start threadId = %v, want thread-123", params["threadId"])
	}
	if params["approvalPolicy"] != "never" {
		t.Fatalf("approvalPolicy = %v, want never", params["approvalPolicy"])
	}
	sandbox := mapField(t, params, "sandboxPolicy")
	if sandbox["type"] != "dangerFullAccess" {
		t.Fatalf("sandbox type = %v, want dangerFullAccess", sandbox["type"])
	}
	input := sliceField(t, params, "input")
	if len(input) != 1 {
		t.Fatalf("input len = %d, want 1", len(input))
	}
	input0, _ := input[0].(map[string]any)
	if input0["type"] != "text" || input0["text"] != "fix the bug" {
		t.Fatalf("unexpected input payload: %#v", input0)
	}

	proc.send(t, map[string]any{
		"id":     idAsInt(t, turnStart["id"]),
		"result": map[string]any{},
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("SendPrompt() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SendPrompt")
	}
}

func TestCodexBackend_SendSteer(t *testing.T) {
	proc := newFakeCodexProcess(t)
	backend := startTestCodexBackend(t, proc)

	backend.mu.Lock()
	backend.threadID = "thread-123"
	backend.activeTurnID = "turn-456"
	backend.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.SendSteer("focus on the database")
	}()

	msg := proc.nextWrite(t)
	if msg["method"] != "turn/steer" {
		t.Fatalf("method = %v, want turn/steer", msg["method"])
	}
	params := mapField(t, msg, "params")
	if params["threadId"] != "thread-123" {
		t.Fatalf("threadId = %v, want thread-123", params["threadId"])
	}
	if params["expectedTurnId"] != "turn-456" {
		t.Fatalf("expectedTurnId = %v, want turn-456", params["expectedTurnId"])
	}

	proc.send(t, map[string]any{
		"id":     idAsInt(t, msg["id"]),
		"result": map[string]any{},
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("SendSteer() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SendSteer")
	}
}

func TestCodexBackend_EventMapping(t *testing.T) {
	proc := newFakeCodexProcess(t)
	backend := startTestCodexBackend(t, proc)

	proc.send(t, map[string]any{
		"method": "item/agentMessage/delta",
		"params": map[string]any{
			"delta": "hello",
		},
	})
	expectEventTypeText(t, backend.Events(), "agent_message", "hello")

	proc.send(t, map[string]any{
		"method": "item/started",
		"params": map[string]any{
			"item": map[string]any{
				"type": "command_execution",
				"id":   "cmd-1",
			},
		},
	})
	expectEventType(t, backend.Events(), "tool_use")

	proc.send(t, map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"item": map[string]any{
				"type": "command_execution",
				"id":   "cmd-1",
			},
		},
	})
	expectEventType(t, backend.Events(), "tool_result")

	proc.send(t, map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"item": map[string]any{
				"type": "agent_message",
				"text": "final answer",
			},
		},
	})
	expectEventTypeText(t, backend.Events(), "agent_message", "final answer")

	proc.send(t, map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"usage": map[string]any{
				"inputTokens": 12,
			},
		},
	})
	event := expectEventType(t, backend.Events(), "result")
	eventData := eventMap(t, event)
	if _, ok := eventData["usage"]; !ok {
		t.Fatalf("result event missing usage: %#v", eventData)
	}

	proc.send(t, map[string]any{
		"method": "thread/started",
		"params": map[string]any{
			"threadId": "thread-123",
		},
	})
	event = expectEventType(t, backend.Events(), "system")
	eventData = eventMap(t, event)
	if eventData["threadId"] != "thread-123" {
		t.Fatalf("system event threadId = %v, want thread-123", eventData["threadId"])
	}

	proc.send(t, map[string]any{
		"method": "error",
		"params": map[string]any{
			"message": "boom",
		},
	})
	expectEventTypeText(t, backend.Events(), "error", "boom")
}

func TestCodexBackend_AutoApproval(t *testing.T) {
	proc := newFakeCodexProcess(t)
	_ = startTestCodexBackend(t, proc)

	proc.send(t, map[string]any{
		"id":     99,
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{
			"itemId": "cmd-1",
		},
	})

	msg := proc.nextWrite(t)
	if idAsInt(t, msg["id"]) != 99 {
		t.Fatalf("approval response id = %v, want 99", msg["id"])
	}
	result := mapField(t, msg, "result")
	if result["decision"] != "accept" {
		t.Fatalf("approval decision = %v, want accept", result["decision"])
	}
}

type fakeCodexSpawner struct {
	mu   sync.Mutex
	cmd  []string
	proc *fakeCodexProcess
}

func (s *fakeCodexSpawner) spawn(_ context.Context, cmd []string, _ []string) (*codexTransport, error) {
	s.mu.Lock()
	s.cmd = append([]string(nil), cmd...)
	s.mu.Unlock()
	return &codexTransport{
		stdin:   s.proc.stdinW,
		stdout:  s.proc.stdoutR,
		wait:    s.proc.waitCh,
		closeFn: s.proc.close,
	}, nil
}

type fakeCodexProcess struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	waitCh  chan error
	writes  chan map[string]any
}

func newFakeCodexProcess(t *testing.T) *fakeCodexProcess {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	proc := &fakeCodexProcess{
		stdinR:  stdinR,
		stdinW:  stdinW,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		waitCh:  make(chan error, 1),
		writes:  make(chan map[string]any, 16),
	}

	go func() {
		decoder := json.NewDecoder(stdinR)
		for {
			var msg map[string]any
			if err := decoder.Decode(&msg); err != nil {
				return
			}
			proc.writes <- msg
		}
	}()

	t.Cleanup(func() {
		_ = proc.close()
	})
	return proc
}

func (p *fakeCodexProcess) send(t *testing.T, msg map[string]any) {
	t.Helper()
	if err := json.NewEncoder(p.stdoutW).Encode(msg); err != nil {
		t.Fatalf("send server message: %v", err)
	}
}

func (p *fakeCodexProcess) nextWrite(t *testing.T) map[string]any {
	t.Helper()
	select {
	case msg := <-p.writes:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client message")
		return nil
	}
}

func (p *fakeCodexProcess) close() error {
	_ = p.stdinW.Close()
	_ = p.stdinR.Close()
	_ = p.stdoutW.Close()
	_ = p.stdoutR.Close()
	select {
	case p.waitCh <- nil:
	default:
	}
	return nil
}

func startTestCodexBackend(t *testing.T, proc *fakeCodexProcess) *codexBackend {
	t.Helper()

	runner := &fakeCodexSpawner{proc: proc}
	backend := newCodexBackendWithSpawner(log.New(io.Discard, "", 0), runner.spawn)
	t.Cleanup(func() { _ = backend.Close() })

	errCh := make(chan error, 1)
	go func() {
		errCh <- backend.Spawn(context.Background())
	}()

	initMsg := proc.nextWrite(t)
	if initMsg["method"] != "initialize" {
		t.Fatalf("first method = %v, want initialize", initMsg["method"])
	}
	proc.send(t, map[string]any{
		"id": 0,
		"result": map[string]any{
			"userAgent": "codex-cli/0.114.0",
		},
	})
	if msg := proc.nextWrite(t); msg["method"] != "initialized" {
		t.Fatalf("second method = %v, want initialized", msg["method"])
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Spawn() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Spawn")
	}

	return backend
}

func expectEventType(t *testing.T, ch <-chan Event, want string) Event {
	t.Helper()
	select {
	case event := <-ch:
		if event.Type != want {
			t.Fatalf("event type = %q, want %q (%#v)", event.Type, want, event.Data)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q event", want)
		return Event{}
	}
}

func expectEventTypeText(t *testing.T, ch <-chan Event, wantType, wantText string) {
	t.Helper()
	data := eventMap(t, expectEventType(t, ch, wantType))
	if data["text"] != wantText && data["message"] != wantText {
		t.Fatalf("event payload = %#v, want text/message %q", data, wantText)
	}
}

func eventMap(t *testing.T, event Event) map[string]any {
	t.Helper()
	data, ok := event.Data.(map[string]any)
	if !ok {
		t.Fatalf("event data type = %T, want map[string]any", event.Data)
	}
	return data
}

func mapField(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, m)
	}
	out, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("key %q type = %T, want map[string]any", key, raw)
	}
	return out
}

func sliceField(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, m)
	}
	out, ok := raw.([]any)
	if !ok {
		t.Fatalf("key %q type = %T, want []any", key, raw)
	}
	return out
}

func idAsInt(t *testing.T, raw any) int {
	t.Helper()
	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		t.Fatalf("id type = %T, want numeric", raw)
		return 0
	}
}

func equalStrings(a, b []string) bool {
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
