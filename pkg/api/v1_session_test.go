package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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

func TestV1ListSessionsReturnsDurableActiveAndTerminalHistory(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for index, state := range []durable.SessionState{durable.StateCreated, durable.StateCompleted} {
		id := fmt.Sprintf("listed-%d", index)
		created, err := store.CreateSession(ctx, durable.CreateSessionParams{
			SessionID: id, IdempotencyKey: "job-" + id, RequestHash: "sha256:" + id,
			RequestManifest: json.RawMessage(`{"agent":"claude","runtime":"docker"}`),
			Agent:           "claude", Runtime: "docker", CreatedAt: time.Unix(int64(100+index), 0).UTC(),
		})
		if err != nil {
			t.Fatalf("create listed session: %v", err)
		}
		if state == durable.StateCompleted {
			if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
				SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: time.Unix(102, 0).UTC(),
			}); err != nil {
				t.Fatalf("start listed session: %v", err)
			}
			if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
				SessionID: created.Session.ID, Runtime: "docker", ContainerID: "container-listed", CreatedAt: time.Unix(103, 0).UTC(),
			}); err != nil {
				t.Fatalf("create listed generation: %v", err)
			}
			if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{
				SessionID: created.Session.ID, Generation: 1, From: durable.GenerationStarting, To: durable.GenerationRunning, At: time.Unix(104, 0).UTC(),
			}); err != nil {
				t.Fatalf("run listed generation: %v", err)
			}
			if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
				SessionID: created.Session.ID, From: durable.StateStarting, To: durable.StateRunning, At: time.Unix(104, 0).UTC(),
			}); err != nil {
				t.Fatalf("run listed session: %v", err)
			}
			exitCode := 0
			if _, err := store.FinalizeSession(ctx, durable.FinalizeSessionParams{
				From: durable.StateRunning, GenerationFrom: durable.GenerationRunning, GenerationTo: durable.GenerationExited,
				Receipt: durable.TerminalReceipt{SessionID: created.Session.ID, Generation: 1, State: durable.StateCompleted,
					ExitCode: &exitCode, StartedAt: time.Unix(103, 0).UTC(), EndedAt: time.Unix(105, 0).UTC(), OutputHash: "sha256:listed"},
			}); err != nil {
				t.Fatalf("finalize listed session: %v", err)
			}
		}
	}
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	response := get(t, httpServer, "/api/v1/sessions")
	defer response.Body.Close()
	var envelope struct {
		Data []v1SessionData `json:"data"`
	}
	decodeJSON(t, response.Body, &envelope)
	if response.StatusCode != http.StatusOK || len(envelope.Data) != 2 {
		t.Fatalf("list status=%d data=%+v", response.StatusCode, envelope.Data)
	}
	states := map[durable.SessionState]bool{}
	for _, listed := range envelope.Data {
		states[listed.State] = true
		if listed.CreatedAt.IsZero() || listed.UpdatedAt.IsZero() || listed.EventsURL == "" || listed.EventStreamURL == "" {
			t.Fatalf("incomplete listed session: %+v", listed)
		}
	}
	if !states[durable.StateCreated] || !states[durable.StateCompleted] {
		t.Fatalf("listed states = %v", states)
	}
}

func TestSpawnSessionUsesDurableNativePipelineForChat(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := agent.NewRegistry()
	internalAgent := &resumeClaudeAgent{configs: make(chan agent.AgentConfig, 1)}
	registry.Register(internalAgent)
	manager := session.NewManager()
	server := NewServer(manager, runtime.NewLocalRuntime(), registry, ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	sess, err := server.SpawnSession(context.Background(), SessionRequest{
		Agent: "claude", Runtime: "local", Prompt: "chat follow-up", Interactive: true,
		Model: "claude-opus-5", Effort: "max", Fast: true,
		Claude: &ClaudeConfig{MaxTurns: 1, AllowedTools: []string{"WebSearch"}},
		Tags:   map[string]string{"chat_name": "durable-chat"},
	})
	if err != nil {
		t.Fatalf("spawn chat session: %v", err)
	}
	select {
	case config := <-internalAgent.configs:
		if config.Model != "claude-opus-5" || config.Effort != "max" || !config.Fast ||
			config.MaxTokens != 1 || !slices.Equal(config.AllowedTools, []string{"WebSearch"}) {
			t.Fatalf("internal spawn resolved config = %+v", config)
		}
	case <-time.After(time.Second):
		t.Fatal("internal spawn did not resolve the provider command")
	}
	stored, err := store.GetSession(context.Background(), sess.ID)
	if err != nil || stored.IdempotencyKey == "" || stored.State != durable.StateRunning || stored.ActiveGeneration != 1 {
		t.Fatalf("durable chat session = %+v err=%v", stored, err)
	}
	waitForDurableTerminal(t, store, sess.ID)
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: sess.ID, Limit: 100})
	if err != nil || len(page.Events) < 2 || page.Events[len(page.Events)-1].Stream != durable.StreamTerminal {
		t.Fatalf("durable chat event ledger = %+v err=%v", page, err)
	}
}

func TestV1DockerNativeSpawnKeepsProviderArguments(t *testing.T) {
	command := []string{"claude", "-p", "hello", "--output-format", "stream-json"}
	got := runtimeSpawnCommand(command, "docker", "claude")
	if len(got) != len(command) {
		t.Fatalf("durable native Docker command = %v, want %v", got, command)
	}
	legacy := runtimeSpawnCommand(command, "docker", "claude")
	if len(legacy) != len(command) {
		t.Fatalf("direct legacy Docker command = %v, want %v", legacy, command)
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
	if err != nil || generation.ProviderID != "claude-fixture-session" || generation.ImageDigest != "sha256:test-image" || generation.SandboxProfile != "test-native-v1" {
		t.Fatalf("native provider identity = %+v err=%v", generation, err)
	}
	receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
	if err != nil || receipt.LastSequence != 4 {
		t.Fatalf("native terminal receipt = %+v err=%v", receipt, err)
	}
}

func TestV1StructuredOutputIsValidatedCommittedAndReceiptLinked(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ts := newV1SessionTestServer(t, store, &atomic.Int32{})
	response := postV1Session(t, ts.URL, map[string]any{
		"idempotency_key": "job-v1-structured-claude",
		"agent":           "claude", "runtime": "test", "prompt": "fixture",
		"structured_output": map[string]any{
			"json_schema": map[string]any{
				"type": "object", "required": []string{"url"},
				"properties":           map[string]any{"url": map[string]any{"type": "string"}},
				"additionalProperties": false,
			},
			"max_bytes": 128,
		},
	})
	defer response.Body.Close()
	var created struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, response.Body, &created)
	if response.StatusCode != http.StatusCreated || created.Data.OutputSchemaHash == "" {
		t.Fatalf("structured create status=%d data=%+v", response.StatusCode, created.Data)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)

	receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
	if err != nil || receipt.State != durable.StateCompleted || receipt.ArtifactHash == "" {
		t.Fatalf("structured receipt = %+v err=%v", receipt, err)
	}
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 5 || page.Events[3].Type != "output.final" || page.Events[4].Type != "session.completed" || page.Events[3].RawSHA256 != receipt.ArtifactHash {
		t.Fatalf("structured event ledger=%+v receipt=%+v", page.Events, receipt)
	}
	if got := string(page.Events[3].Raw); got != `{"url":"https://example.com"}` {
		t.Fatalf("exact final output = %q", got)
	}

	result := get(t, ts.Server, "/api/v1/sessions/"+created.Data.SessionID+"/result")
	defer result.Body.Close()
	bytes, err := io.ReadAll(result.Body)
	if err != nil || string(bytes) != `{"url":"https://example.com"}` || result.Header.Get("X-Content-SHA256") != receipt.ArtifactHash {
		t.Fatalf("result bytes=%q hash=%q err=%v", bytes, result.Header.Get("X-Content-SHA256"), err)
	}
}

func TestV1StructuredOutputFailureIsTypedAndHasNoArtifact(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ts := newV1SessionTestServer(t, store, &atomic.Int32{})
	response := postV1Session(t, ts.URL, map[string]any{
		"idempotency_key": "job-v1-invalid-structured-claude",
		"agent":           "claude", "runtime": "test", "prompt": "fixture",
		"structured_output": map[string]any{
			"json_schema": map[string]any{"type": "object", "required": []string{"count"}, "properties": map[string]any{"count": map[string]any{"type": "integer"}}},
			"max_bytes":   128,
		},
	})
	defer response.Body.Close()
	var created struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, response.Body, &created)
	waitForDurableTerminal(t, store, created.Data.SessionID)
	receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
	if err != nil || receipt.State != durable.StateFailed || receipt.Reason != string(durable.StateFailed) || receipt.ArtifactHash != "" {
		t.Fatalf("invalid structured receipt = %+v err=%v", receipt, err)
	}
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 10})
	if err != nil || len(page.Events) == 0 || page.Events[len(page.Events)-1].Type != "session.structured_output_invalid" {
		t.Fatalf("invalid structured terminal event = %+v err=%v", page.Events, err)
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

	inputBody := map[string]any{"idempotency_key": "second-turn", "kind": "prompt", "text": "second"}
	inputResponse := postV1Control(t, httpServer.URL, created.Data.SessionID, "input", inputBody)
	defer inputResponse.Body.Close()
	if inputResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("native input status=%d", inputResponse.StatusCode)
	}
	repeatInput := postV1Control(t, httpServer.URL, created.Data.SessionID, "input", inputBody)
	defer repeatInput.Body.Close()
	if repeatInput.StatusCode != http.StatusOK {
		t.Fatalf("idempotent native input status=%d", repeatInput.StatusCode)
	}
	waitForEventType(t, store, created.Data.SessionID, "content.delta", 1)
	interruptResponse := postV1Control(t, httpServer.URL, created.Data.SessionID, "interrupt", map[string]any{"idempotency_key": "interrupt-turn"})
	defer interruptResponse.Body.Close()
	if interruptResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("native interrupt status=%d", interruptResponse.StatusCode)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)
}

func TestV1CancelCommitsCancelledTerminalReceiptIdempotently(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := agent.NewRegistry()
	registry.Register(&cancelCodexFixtureAgent{})
	manager := session.NewManager()
	server := NewServer(manager, runtime.NewLocalRuntime(), registry, ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	createdResponse := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "job-native-cancel", "agent": "codex", "runtime": "local",
		"prompt": "start", "interactive": true,
	})
	defer createdResponse.Body.Close()
	var created struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, createdResponse.Body, &created)
	waitForEventType(t, store, created.Data.SessionID, "turn.completed", 1)
	waitForEventType(t, store, created.Data.SessionID, "tool.call", 1)
	cancelBody := map[string]any{"idempotency_key": "cancel-once"}
	cancelResponse := postV1Control(t, httpServer.URL, created.Data.SessionID, "cancel", cancelBody)
	defer cancelResponse.Body.Close()
	if cancelResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status=%d", cancelResponse.StatusCode)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)
	receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
	if err != nil || receipt.State != durable.StateCancelled {
		t.Fatalf("cancel receipt = %+v err=%v", receipt, err)
	}
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 100})
	if err != nil || len(page.Events) == 0 || page.Events[len(page.Events)-1].Type != "session.cancelled" {
		t.Fatalf("cancel terminal ledger = %+v err=%v", page, err)
	}
	for _, event := range page.Events {
		if event.Type == "tool.result" {
			t.Fatalf("cancelled in-flight tool unexpectedly completed: %+v", page.Events)
		}
	}
	repeat := postV1Control(t, httpServer.URL, created.Data.SessionID, "cancel", cancelBody)
	defer repeat.Body.Close()
	if repeat.StatusCode != http.StatusOK {
		t.Fatalf("idempotent cancel status=%d", repeat.StatusCode)
	}
}

func TestV1TerminateIsDistinctFromCallerCancellation(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := agent.NewRegistry()
	registry.Register(&cancelCodexFixtureAgent{})
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), registry, ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	createdResponse := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "job-native-terminate", "agent": "codex", "runtime": "local",
		"prompt": "start", "interactive": true,
	})
	defer createdResponse.Body.Close()
	var created struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, createdResponse.Body, &created)
	waitForEventType(t, store, created.Data.SessionID, "turn.completed", 1)
	terminated := postV1Control(t, httpServer.URL, created.Data.SessionID, "terminate", map[string]any{"idempotency_key": "terminate-once"})
	defer terminated.Body.Close()
	if terminated.StatusCode != http.StatusAccepted {
		t.Fatalf("terminate status=%d", terminated.StatusCode)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)
	receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
	if err != nil || receipt.State != durable.StateCancelled || receipt.Reason != "terminated" {
		t.Fatalf("terminate receipt = %+v err=%v", receipt, err)
	}
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 100})
	if err != nil || page.Events[len(page.Events)-1].Type != "session.terminated" {
		t.Fatalf("terminate ledger = %+v err=%v", page, err)
	}
}

func TestV1NativeSessionTimeoutCommitsDurableTerminalProof(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := agent.NewRegistry()
	registry.Register(&cancelCodexFixtureAgent{})
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), registry, ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()

	createdResponse := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "job-native-timeout", "agent": "codex", "runtime": "local",
		"prompt": "start", "interactive": true, "timeout": "250ms",
	})
	defer createdResponse.Body.Close()
	var created struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, createdResponse.Body, &created)
	if createdResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create timeout session status=%d", createdResponse.StatusCode)
	}
	waitForDurableTerminal(t, store, created.Data.SessionID)

	receipt, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID)
	if err != nil || receipt.State != durable.StateTimedOut {
		t.Fatalf("timeout receipt = %+v err=%v", receipt, err)
	}
	page, err := store.ListEvents(context.Background(), durable.EventQuery{SessionID: created.Data.SessionID, Limit: 100})
	if err != nil {
		t.Fatalf("list timeout events: %v", err)
	}
	types := make([]string, 0, len(page.Events))
	for _, event := range page.Events {
		types = append(types, event.Type)
	}
	wantSuffix := []string{"control.timeout.requested", "control.timeout.dispatched", "session.timed_out"}
	if len(types) < len(wantSuffix) || !slices.Equal(types[len(types)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("timeout event suffix = %v, want %v", types, wantSuffix)
	}
}

func TestActiveNativeSessionDoesNotReclassifyClaimedTerminalAsCancelled(t *testing.T) {
	active := newActiveNativeSession(nil)
	if reason := active.terminalReason(); reason != "" {
		t.Fatalf("natural terminal reason = %q, want empty", reason)
	}
	if active.beginCancel() {
		t.Fatal("cancel claimed a process whose natural terminal path already won")
	}
}

func TestRuntimeTerminalStateDistinguishesFailureSignalAndOOM(t *testing.T) {
	for _, test := range []struct {
		name   string
		result runtime.ExitResult
		want   durable.SessionState
	}{
		{name: "success", result: runtime.ExitResult{Code: 0}, want: durable.StateCompleted},
		{name: "failure", result: runtime.ExitResult{Code: 7}, want: durable.StateFailed},
		{name: "unproven signal shaped exit", result: runtime.ExitResult{Code: 143}, want: durable.StateIndeterminate},
		{name: "signal", result: runtime.ExitResult{Code: 143, Signal: "SIGTERM"}, want: durable.StateCrashed},
		{name: "oom", result: runtime.ExitResult{Code: 137, Signal: "SIGKILL", OOMKilled: true}, want: durable.StateCrashed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := runtimeTerminalState(test.result); got != test.want {
				t.Fatalf("terminal state = %s, want %s", got, test.want)
			}
		})
	}
}

func TestFinalizeV1SessionDoesNotGuessUnprovenSignal(t *testing.T) {
	store, sessionID := runningRecoveryStore(t)
	server := NewServer(session.NewManager(), &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	server.finalizeV1Session(sessionID, runtime.ExitResult{Code: 143})
	receipt, err := store.GetTerminalReceipt(context.Background(), sessionID)
	if err != nil || receipt.State != durable.StateIndeterminate || receipt.Signal != "" {
		t.Fatalf("ambiguous signal receipt = %+v err=%v", receipt, err)
	}
	generation, err := store.GetGeneration(context.Background(), sessionID, 1)
	if err != nil || generation.State != durable.GenerationIndeterminate {
		t.Fatalf("ambiguous signal generation = %+v err=%v", generation, err)
	}
}

func TestFinalizeV1SessionPersistsOOMReceiptProof(t *testing.T) {
	store, sessionID := runningRecoveryStore(t)
	server := NewServer(session.NewManager(), &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	startedAt := time.Unix(2_001, 0).UTC()
	endedAt := time.Unix(2_010, 0).UTC()
	server.finalizeV1Session(sessionID, runtime.ExitResult{
		Code: 137, Signal: "SIGKILL", OOMKilled: true, StartedAt: startedAt, EndedAt: endedAt,
	})
	receipt, err := store.GetTerminalReceipt(context.Background(), sessionID)
	if err != nil || receipt.State != durable.StateCrashed || receipt.Signal != "SIGKILL" || receipt.ExitCode == nil || *receipt.ExitCode != 137 {
		t.Fatalf("OOM receipt = %+v err=%v", receipt, err)
	}
	if !receipt.StartedAt.Equal(startedAt) || !receipt.EndedAt.Equal(endedAt) {
		t.Fatalf("OOM receipt timestamps = %s .. %s", receipt.StartedAt, receipt.EndedAt)
	}
}

func TestActiveNativeSessionWaitsForDurableCancelOutcome(t *testing.T) {
	active := newActiveNativeSession(nil)
	if !active.beginCancel() {
		t.Fatal("cancel did not claim active process")
	}
	reason := make(chan string, 1)
	go func() { reason <- active.terminalReason() }()
	select {
	case got := <-reason:
		t.Fatalf("terminal reason returned before cancel settled: %q", got)
	case <-time.After(10 * time.Millisecond):
	}
	active.settleCancel(durable.StateCancelled)
	select {
	case got := <-reason:
		if got != "cancelled" {
			t.Fatalf("settled terminal reason = %q, want cancelled", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal reason did not unblock after cancel settled")
	}
}

func TestActiveNativeSessionTimeoutWinsTerminalBoundary(t *testing.T) {
	active := newActiveNativeSession(nil)
	if !active.beginTimeout() {
		t.Fatal("timeout did not claim active process")
	}
	if active.beginCancel() {
		t.Fatal("cancel claimed process after timeout won")
	}
	reason := make(chan string, 1)
	go func() { reason <- active.terminalReason() }()
	select {
	case got := <-reason:
		t.Fatalf("terminal reason returned before timeout settled: %q", got)
	case <-time.After(10 * time.Millisecond):
	}
	active.settleTimeout(durable.StateTimedOut)
	select {
	case got := <-reason:
		if got != "timed_out" {
			t.Fatalf("settled terminal reason = %q, want timed_out", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal reason did not unblock after timeout settled")
	}
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

func (*nativeClaudeFixtureAgent) BuildCmd(_ string, config agent.AgentConfig) ([]string, error) {
	result := "done"
	delta := "hello"
	if len(config.JSONSchema) > 0 {
		result = `{"url":"https://example.com"}`
		delta = result
	}
	deltaJSON, _ := json.Marshal(delta)
	resultJSON, _ := json.Marshal(result)
	return []string{"/bin/sh", "-c", `IFS= read -r prompt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-fixture-session"}' "$1" "$2"`, "--",
		`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":` + string(deltaJSON) + `}}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":` + string(resultJSON) + `}`}, nil
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

type cancelCodexFixtureAgent struct{}

func (*cancelCodexFixtureAgent) Name() string { return "codex" }
func (*cancelCodexFixtureAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	return []string{"/bin/sh", "-c", `
IFS= read -r initialize
printf '%s\n' '{"id":0,"result":{"userAgent":"codex-cancel"}}'
IFS= read -r initialized
IFS= read -r thread_start
printf '%s\n' '{"id":1,"result":{"threadId":"codex-cancel-thread"}}' '{"method":"thread/started","params":{"threadId":"codex-cancel-thread"}}'
IFS= read -r first_turn
printf '%s\n' '{"id":2,"result":{}}' '{"method":"turn/started","params":{"threadId":"codex-cancel-thread","turnId":"turn-cancel"}}' '{"method":"turn/completed","params":{"threadId":"codex-cancel-thread","turnId":"turn-cancel","status":"completed"}}'
printf '%s\n' '{"method":"turn/started","params":{"threadId":"codex-cancel-thread","turnId":"turn-tool"}}' '{"method":"item/started","params":{"threadId":"codex-cancel-thread","turnId":"turn-tool","item":{"id":"tool-cancel","type":"command_execution","command":"sleep 60"}}}'
IFS= read -r wait_for_cancel
`}, nil
}
func (*cancelCodexFixtureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) {
	return nil, false
}

type countingRuntime struct {
	Runtime runtime.Runtime
	count   *atomic.Int32
}

func (counting *countingRuntime) Name() string { return "test" }

func (counting *countingRuntime) Spawn(ctx context.Context, config runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	counting.count.Add(1)
	handle, err := counting.Runtime.Spawn(ctx, config)
	if err != nil {
		return nil, err
	}
	return &imageIdentifiedTestHandle{ProcessHandle: handle}, nil
}

type imageIdentifiedTestHandle struct{ runtime.ProcessHandle }

func (*imageIdentifiedTestHandle) RuntimeImageDigest() string { return "sha256:test-image" }
func (*imageIdentifiedTestHandle) NativeStdio() bool          { return true }

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
