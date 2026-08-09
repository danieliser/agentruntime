package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestV1CreateSessionIsConcurrentAndRestartIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentd.sqlite")
	store, err := durablesqlite.Open(path)
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	counter := &atomic.Int32{}
	ts := newV1SessionTestServer(t, store, counter)
	body := map[string]any{
		"idempotency_key": "job-v1-idempotent",
		"agent":           "sleep-test", "runtime": "test", "prompt": "run once",
	}

	const requests = 16
	type result struct {
		status int
		id     string
		err    error
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal concurrent request: %v", err)
	}
	results := make(chan result, requests)
	var wait sync.WaitGroup
	for range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			resp, err := http.Post(ts.URL+"/api/v1/sessions", "application/json", bytes.NewReader(encodedBody))
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			var envelope struct {
				Data struct {
					SessionID string `json:"session_id"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				results <- result{err: err}
				return
			}
			results <- result{status: resp.StatusCode, id: envelope.Data.SessionID}
		}()
	}
	wait.Wait()
	close(results)
	created := 0
	var sessionID string
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent create: %v", got.err)
		}
		if got.status == http.StatusCreated {
			created++
		} else if got.status != http.StatusOK {
			t.Fatalf("duplicate status = %d, want 200/201", got.status)
		}
		if sessionID == "" {
			sessionID = got.id
		}
		if got.id == "" || got.id != sessionID {
			t.Fatalf("duplicate session ID = %q, want %q", got.id, sessionID)
		}
	}
	if created != 1 || counter.Load() != 1 {
		t.Fatalf("created responses=%d spawn count=%d, want 1/1", created, counter.Load())
	}
	stored, err := store.GetSession(context.Background(), sessionID)
	if err != nil || stored.State != durable.StateRunning || stored.ActiveGeneration != 1 {
		t.Fatalf("active durable session = %+v err=%v", stored, err)
	}
	generation, err := store.GetGeneration(context.Background(), sessionID, 1)
	if err != nil || generation.Runtime != "test" || generation.ContainerID == "" || generation.State != durable.GenerationRunning {
		t.Fatalf("active durable generation = %+v err=%v", generation, err)
	}

	ts.Close()
	ts.manager.ShutdownAll()
	waitForDurableTerminal(t, store, sessionID)
	if err := store.Close(); err != nil {
		t.Fatalf("close durable store: %v", err)
	}
	reopened, err := durablesqlite.Open(path)
	if err != nil {
		t.Fatalf("reopen durable store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	ts = newV1SessionTestServer(t, reopened, counter)
	resp := postV1Session(t, ts.URL, body)
	defer resp.Body.Close()
	var replay struct {
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	decodeJSON(t, resp.Body, &replay)
	if resp.StatusCode != http.StatusOK || replay.Data.SessionID != sessionID || counter.Load() != 1 {
		t.Fatalf("restart duplicate: status=%d id=%q spawn=%d", resp.StatusCode, replay.Data.SessionID, counter.Load())
	}
	inspect := get(t, ts.Server, "/api/v1/sessions/"+sessionID)
	defer inspect.Body.Close()
	var inspected struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, inspect.Body, &inspected)
	if inspect.StatusCode != http.StatusOK || inspected.Data.SessionID != sessionID || !inspected.Data.State.Terminal() || inspected.Data.Generation != 1 {
		t.Fatalf("durable inspect: status=%d data=%+v", inspect.StatusCode, inspected.Data)
	}
}

func TestV1DockerNativeSpawnKeepsProviderArguments(t *testing.T) {
	command := []string{"claude", "-p", "hello", "--output-format", "stream-json"}
	got := runtimeSpawnCommand(command, "docker", true, "claude")
	if len(got) != len(command) {
		t.Fatalf("durable native Docker command = %v, want %v", got, command)
	}
	legacy := runtimeSpawnCommand(command, "docker", false, "claude")
	if len(legacy) != 1 || legacy[0] != "claude" {
		t.Fatalf("legacy sidecar Docker command = %v", legacy)
	}
}

func TestV1CreateSessionRejectsChangedRequestAndScrubsGrantedSecrets(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	counter := &atomic.Int32{}
	ts := newV1SessionTestServer(t, store, counter)
	body := map[string]any{
		"idempotency_key": "job-v1-secret",
		"secret_grants":   []string{"PROVIDER_API_TOKEN"},
		"agent":           "sleep-test", "runtime": "test", "prompt": "original",
		"env":         map[string]string{"NORMAL_SETTING": "visible", "PROVIDER_API_TOKEN": "must-not-persist"},
		"mcp_servers": []map[string]any{{"name": "private-mcp", "type": "http", "url": "https://mcp.invalid", "token": "nested-must-not-persist"}},
	}
	first := postV1Session(t, ts.URL, body)
	defer first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		contents, _ := io.ReadAll(first.Body)
		t.Fatalf("first status=%d body=%s", first.StatusCode, contents)
	}
	stored, err := store.GetSessionByIdempotencyKey(context.Background(), "job-v1-secret")
	if err != nil {
		t.Fatalf("get durable session: %v", err)
	}
	if bytes.Contains(stored.RequestManifest, []byte("must-not-persist")) || !bytes.Contains(stored.RequestManifest, []byte("visible")) {
		t.Fatalf("stored manifest leaked or lost values: %s", stored.RequestManifest)
	}
	if len(stored.SecretGrants) != 2 || stored.SecretGrants[0] != "PROVIDER_API_TOKEN" || stored.SecretGrants[1] != "request:mcp_servers[0].token" {
		t.Fatalf("stored secret grants = %v", stored.SecretGrants)
	}

	changed := cloneJSONMap(t, body)
	changed["prompt"] = "different paid request"
	conflict := postV1Session(t, ts.URL, changed)
	defer conflict.Body.Close()
	var failure struct {
		Error apiErrorEnvelope `json:"error"`
	}
	decodeJSON(t, conflict.Body, &failure)
	if conflict.StatusCode != http.StatusConflict || failure.Error.Code != durable.CodeIdempotencyConflict || counter.Load() != 1 {
		t.Fatalf("changed request: status=%d error=%+v spawn=%d", conflict.StatusCode, failure.Error, counter.Load())
	}
}

func TestV1CreateSessionRejectsUndeclaredSecretEnvironment(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	counter := &atomic.Int32{}
	ts := newV1SessionTestServer(t, store, counter)
	resp := postV1Session(t, ts.URL, map[string]any{
		"idempotency_key": "job-v1-undeclared-secret",
		"agent":           "sleep-test", "runtime": "test", "prompt": "must reject",
		"env": map[string]string{"ANTHROPIC_API_KEY": "must-not-persist"},
	})
	defer resp.Body.Close()
	var failure struct {
		Error apiErrorEnvelope `json:"error"`
	}
	decodeJSON(t, resp.Body, &failure)
	if resp.StatusCode != http.StatusBadRequest || failure.Error.Code != durable.CodeInvalidArgument || counter.Load() != 0 {
		t.Fatalf("undeclared secret: status=%d error=%+v spawn=%d", resp.StatusCode, failure.Error, counter.Load())
	}
	if _, err := store.GetSessionByIdempotencyKey(context.Background(), "job-v1-undeclared-secret"); !durable.IsCode(err, durable.CodeNotFound) {
		t.Fatalf("undeclared secret created durable session: %v", err)
	}
}

func TestV1ClaudeOutputUsesDurableNativeLedger(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	counter := &atomic.Int32{}
	ts := newV1SessionTestServer(t, store, counter)
	resp := postV1Session(t, ts.URL, map[string]any{
		"idempotency_key": "job-v1-native-claude",
		"agent":           "claude", "runtime": "test", "prompt": "fixture",
	})
	defer resp.Body.Close()
	var created struct {
		Data  v1SessionData    `json:"data"`
		Error apiErrorEnvelope `json:"error"`
	}
	decodeJSON(t, resp.Body, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("native create status=%d data=%+v error=%+v", resp.StatusCode, created.Data, created.Error)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)

	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 10})
	if err != nil {
		t.Fatalf("list native events: %v", err)
	}
	if len(page.Events) != 4 || page.Events[0].Type != "lifecycle.provider.initialized" || page.Events[1].Type != "content.delta" || page.Events[2].Type != "turn.completed" || page.Events[3].Stream != durable.StreamTerminal {
		t.Fatalf("native event ledger = %+v", page.Events)
	}
	if string(page.Events[0].Raw) != `{"type":"system","subtype":"init","session_id":"claude-fixture-session"}` {
		t.Fatalf("first native raw = %q", page.Events[0].Raw)
	}
	generation, err := store.GetGeneration(context.Background(), created.Data.SessionID, 1)
	if err != nil || generation.ProviderID != "claude-fixture-session" || generation.SandboxProfile != "test-native-v1" {
		t.Fatalf("native provider identity = %+v err=%v", generation, err)
	}
	receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
	if err != nil || receipt.LastSequence != 4 {
		t.Fatalf("native terminal receipt = %+v err=%v", receipt, err)
	}
}

func TestV1CodexAppServerBootstrapsAndPersistsNativeLedger(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	counter := &atomic.Int32{}
	ts := newV1SessionTestServer(t, store, counter)
	resp := postV1Session(t, ts.URL, map[string]any{
		"idempotency_key": "job-v1-native-codex",
		"agent":           "codex", "runtime": "test", "prompt": "fixture prompt",
	})
	defer resp.Body.Close()
	var created struct {
		Data  v1SessionData    `json:"data"`
		Error apiErrorEnvelope `json:"error"`
	}
	decodeJSON(t, resp.Body, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("native create status=%d data=%+v error=%+v", resp.StatusCode, created.Data, created.Error)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 20})
	if err != nil {
		t.Fatalf("list Codex native events: %v", err)
	}
	if len(page.Events) != 8 || page.Events[2].Type != "lifecycle.provider.session" || page.Events[5].Type != "content.delta" || page.Events[6].Type != "turn.completed" || page.Events[7].Stream != durable.StreamTerminal {
		t.Fatalf("Codex native event ledger = %+v", page.Events)
	}
	generation, err := store.GetGeneration(context.Background(), created.Data.SessionID, 1)
	if err != nil || generation.ProviderID != "codex-fixture-thread" {
		t.Fatalf("Codex provider identity = %+v err=%v", generation, err)
	}
}

func TestV1NativeInputAndInterruptUseActiveProviderTransport(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := agent.NewRegistry()
	registry.Register(&interactiveCodexFixtureAgent{})
	manager := session.NewManager()
	server := NewServer(manager, runtime.NewLocalRuntime(), registry, ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	createdResponse := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "job-native-controls", "agent": "codex", "runtime": "local",
		"prompt": "first", "interactive": true,
	})
	defer createdResponse.Body.Close()
	var created struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, createdResponse.Body, &created)
	if createdResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create interactive native status=%d", createdResponse.StatusCode)
	}
	waitForEventType(t, store, created.Data.SessionID, "turn.completed", 1)

	inputResponse := postV1Control(t, httpServer.URL, created.Data.SessionID, "input", map[string]any{"kind": "prompt", "text": "second"})
	defer inputResponse.Body.Close()
	if inputResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("native input status=%d", inputResponse.StatusCode)
	}
	waitForEventType(t, store, created.Data.SessionID, "content.delta", 1)
	interruptResponse := postV1Control(t, httpServer.URL, created.Data.SessionID, "interrupt", map[string]any{})
	defer interruptResponse.Body.Close()
	if interruptResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("native interrupt status=%d", interruptResponse.StatusCode)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)
}

func postV1Control(t *testing.T, baseURL, sessionID, operation string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal control: %v", err)
	}
	response, err := http.Post(baseURL+"/api/v1/sessions/"+sessionID+"/"+operation, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("post control: %v", err)
	}
	return response
}

func waitForEventType(t *testing.T, store durable.Store, sessionID, eventType string, minimum int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: sessionID, Limit: 100})
		if err == nil {
			count := 0
			for _, event := range page.Events {
				if event.Type == eventType {
					count++
				}
			}
			if count >= minimum {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s did not persist %d %s events", sessionID, minimum, eventType)
}

type v1SessionTestServer struct {
	*httptest.Server
	manager *session.Manager
}

func newV1SessionTestServer(t *testing.T, store durable.Store, counter *atomic.Int32) *v1SessionTestServer {
	t.Helper()
	rt := &countingRuntime{Runtime: runtime.NewLocalRuntime(), count: counter}
	reg := agent.NewRegistry()
	reg.Register(&sleepAgent{})
	reg.Register(&nativeClaudeFixtureAgent{})
	reg.Register(&nativeCodexFixtureAgent{})
	mgr := session.NewManager()
	server := NewServer(mgr, rt, reg, ServerConfig{
		DataDir: t.TempDir(), LogDir: filepath.Join(t.TempDir(), "logs"),
		DurableStore: store, EventBroker: eventstream.New(store),
	})
	t.Cleanup(mgr.ShutdownAll)
	ts := httptest.NewServer(server.router)
	t.Cleanup(ts.Close)
	return &v1SessionTestServer{Server: ts, manager: mgr}
}

type nativeClaudeFixtureAgent struct{}

func (*nativeClaudeFixtureAgent) Name() string { return "claude" }

func (*nativeClaudeFixtureAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	return []string{"/bin/sh", "-c", `IFS= read -r prompt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-fixture-session"}' '{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"hello"}}}' '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`}, nil
}

func (*nativeClaudeFixtureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }

type nativeCodexFixtureAgent struct{}

func (*nativeCodexFixtureAgent) Name() string { return "codex" }

func (*nativeCodexFixtureAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	return []string{"/bin/sh", "-c", `
IFS= read -r initialize
printf '%s\n' '{"id":0,"result":{"userAgent":"codex-fixture"}}'
IFS= read -r initialized
IFS= read -r thread_start
printf '%s\n' '{"id":1,"result":{"threadId":"codex-fixture-thread"}}'
printf '%s\n' '{"method":"thread/started","params":{"threadId":"codex-fixture-thread"}}'
IFS= read -r turn_start
printf '%s\n' '{"id":2,"result":{}}'
printf '%s\n' '{"method":"turn/started","params":{"threadId":"codex-fixture-thread","turnId":"codex-fixture-turn"}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"codex-fixture-thread","turnId":"codex-fixture-turn","delta":"hello"}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"codex-fixture-thread","turnId":"codex-fixture-turn","status":"completed","usage":{"inputTokens":2,"outputTokens":1}}}'
`}, nil
}

func (*nativeCodexFixtureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }

type interactiveCodexFixtureAgent struct{}

func (*interactiveCodexFixtureAgent) Name() string { return "codex" }
func (*interactiveCodexFixtureAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	return []string{"/bin/sh", "-c", `
IFS= read -r initialize
printf '%s\n' '{"id":0,"result":{"userAgent":"codex-controls"}}'
IFS= read -r initialized
IFS= read -r thread_start
printf '%s\n' '{"id":1,"result":{"threadId":"codex-controls-thread"}}' '{"method":"thread/started","params":{"threadId":"codex-controls-thread"}}'
IFS= read -r first_turn
printf '%s\n' '{"id":2,"result":{}}' '{"method":"turn/started","params":{"threadId":"codex-controls-thread","turnId":"turn-1"}}' '{"method":"turn/completed","params":{"threadId":"codex-controls-thread","turnId":"turn-1","status":"completed"}}'
IFS= read -r second_turn
printf '%s\n' '{"id":3,"result":{}}' '{"method":"turn/started","params":{"threadId":"codex-controls-thread","turnId":"turn-2"}}' '{"method":"item/agentMessage/delta","params":{"threadId":"codex-controls-thread","turnId":"turn-2","delta":"second answer"}}' '{"method":"turn/completed","params":{"threadId":"codex-controls-thread","turnId":"turn-2","status":"completed"}}'
IFS= read -r interrupt
printf '%s\n' '{"id":4,"result":{}}'
`}, nil
}
func (*interactiveCodexFixtureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) {
	return nil, false
}

type countingRuntime struct {
	Runtime runtime.Runtime
	count   *atomic.Int32
}

func (counting *countingRuntime) Name() string { return "test" }

func (counting *countingRuntime) Spawn(ctx context.Context, config runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	counting.count.Add(1)
	return counting.Runtime.Spawn(ctx, config)
}

func (counting *countingRuntime) Recover(ctx context.Context) ([]runtime.ProcessHandle, error) {
	return counting.Runtime.Recover(ctx)
}

func (counting *countingRuntime) Cleanup(ctx context.Context) error {
	return counting.Runtime.Cleanup(ctx)
}

func postV1Session(t *testing.T, baseURL string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal v1 request: %v", err)
	}
	resp, err := http.Post(baseURL+"/api/v1/sessions", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("post v1 session: %v", err)
	}
	return resp
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal clone: %v", err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatalf("unmarshal clone: %v", err)
	}
	return cloned
}

func waitForDurableTerminal(t *testing.T, store durable.Store, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stored, err := store.GetSession(context.Background(), sessionID)
		if err == nil && stored.State.Terminal() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s did not become durably terminal", sessionID)
}
