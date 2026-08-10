package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestRestoreRecoveredNativeSessionDeduplicatesRetainedPrefix(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	createdAt := time.Unix(1_000, 0).UTC()
	created, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: "recovered-session", IdempotencyKey: "recovered-job", RequestHash: "sha256:recovered",
		RequestManifest: []byte(`{"agent":"claude","runtime":"docker","timeout":"1000000h"}`), Agent: "claude", Runtime: "docker", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: createdAt.Add(time.Second)}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: created.Session.ID, Runtime: "docker", ContainerID: "container-recovered",
		ProviderID: "provider-recovered", CreatedAt: createdAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{
		SessionID: created.Session.ID, Generation: 1, From: durable.GenerationStarting, To: durable.GenerationRunning, At: createdAt.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("run generation: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: created.Session.ID, From: durable.StateStarting, To: durable.StateRunning, At: createdAt.Add(4 * time.Second)}); err != nil {
		t.Fatalf("run session: %v", err)
	}
	broker := eventstream.New(store)
	firstRaw := []byte(`{"type":"system","subtype":"init","session_id":"provider-recovered"}`)
	firstTimestamp := createdAt.Add(5 * time.Second)
	if _, err := broker.Ingest(ctx, eventstream.IngestParams{SessionID: created.Session.ID, Generation: 1, Record: nativeRecord("claude", 1, firstTimestamp, firstRaw)}); err != nil {
		t.Fatalf("seed first event: %v", err)
	}

	handle := newRecoveredNativeTestHandle(strings.Join([]string{
		string(firstRaw),
		`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"after restart"}}}`,
	}, "\n") + "\n")
	manager := session.NewManager()
	orphaned := manager.Recover([]runtime.ProcessHandle{handle}, "docker")
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: broker,
	})
	server.RestoreRecoveredSessions(orphaned)
	waitForDurableTerminal(t, store, created.Session.ID)

	page, err := store.ListEvents(ctx, durable.EventQuery{SessionID: created.Session.ID, Limit: 10})
	if err != nil {
		t.Fatalf("list recovered events: %v", err)
	}
	if len(page.Events) != 3 || page.Events[0].Sequence != 1 || page.Events[1].Sequence != 2 || page.Events[2].Stream != durable.StreamTerminal {
		t.Fatalf("recovered event ledger = %+v", page.Events)
	}
	if !page.Events[0].Timestamp.Equal(firstTimestamp) {
		t.Fatalf("replayed prefix timestamp changed from %s to %s", firstTimestamp, page.Events[0].Timestamp)
	}
}

func TestRestoreRecoveredNativeSessionHonorsOriginalTimeoutDeadline(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	createdAt := time.Now().UTC().Add(-time.Hour)
	created, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: "overdue-session", IdempotencyKey: "overdue-job", RequestHash: "sha256:overdue",
		RequestManifest: []byte(`{"agent":"claude","runtime":"docker","timeout":"1s","interactive":true}`),
		Agent:           "claude", Runtime: "docker", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
		SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: createdAt.Add(time.Millisecond),
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: created.Session.ID, Runtime: "docker", ContainerID: "container-overdue",
		ProviderID: "provider-overdue", CreatedAt: createdAt.Add(2 * time.Millisecond),
	}); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{
		SessionID: created.Session.ID, Generation: 1, From: durable.GenerationStarting,
		To: durable.GenerationRunning, At: createdAt.Add(3 * time.Millisecond),
	}); err != nil {
		t.Fatalf("run generation: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
		SessionID: created.Session.ID, From: durable.StateStarting, To: durable.StateRunning, At: createdAt.Add(4 * time.Millisecond),
	}); err != nil {
		t.Fatalf("run session: %v", err)
	}

	handle := newBlockingRecoveredNativeTestHandle(created.Session.ID, "container-overdue")
	manager := session.NewManager()
	orphaned := manager.Recover([]runtime.ProcessHandle{handle}, "docker")
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	server.RestoreRecoveredSessions(orphaned)
	waitForDurableTerminal(t, store, created.Session.ID)
	receipt, err := store.GetTerminalReceipt(ctx, created.Session.ID)
	if err != nil || receipt.State != durable.StateTimedOut {
		t.Fatalf("overdue recovered receipt = %+v err=%v", receipt, err)
	}
}

func TestRestoreRecoveredNativeSessionAdoptsCrashBeforeGenerationCommit(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	createdAt := time.Unix(1_500, 0).UTC()
	created, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: "orphaned-start", IdempotencyKey: "orphaned-job", RequestHash: "sha256:orphaned",
		RequestManifest: []byte(`{"agent":"claude","runtime":"docker","timeout":"1000000h"}`), Agent: "claude", Runtime: "docker", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
		SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: createdAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	handle := newRecoveredNativeTestHandle(strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"adopted-provider"}`,
		`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"recovered"}}}`,
	}, "\n") + "\n")
	handle.runtimeID = "container-orphaned"
	handle.recovery = runtime.RecoveryInfo{
		SessionID: created.Session.ID, AgentName: "claude", Generation: 1,
		IdempotencyKey: "orphaned-job", RequestHash: "sha256:orphaned",
		ImageReference: "agentd:test", ImageDigest: "sha256:verified", SandboxProfile: "docker-native-v1",
	}
	manager := session.NewManager()
	orphaned := manager.Recover([]runtime.ProcessHandle{handle}, "docker")
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	server.RestoreRecoveredSessions(orphaned, "docker")
	waitForDurableTerminal(t, store, created.Session.ID)

	generation, err := store.GetGeneration(ctx, created.Session.ID, 1)
	if err != nil {
		t.Fatalf("get adopted generation: %v", err)
	}
	if generation.ContainerID != "container-orphaned" || generation.ImageReference != "agentd:test" ||
		generation.ImageDigest != "sha256:verified" || generation.SandboxProfile != "docker-native-v1" {
		t.Fatalf("adopted generation = %+v", generation)
	}
	stored, err := store.GetSession(ctx, created.Session.ID)
	if err != nil || stored.State != durable.StateCompleted {
		t.Fatalf("adopted session = %+v err=%v", stored, err)
	}
	if generation.State != durable.GenerationExited {
		t.Fatalf("adopted generation state = %s", generation.State)
	}
}

func TestRestoreRecoveredNativeSessionSettlesUnverifiableStartingContainer(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	createdAt := time.Unix(1_600, 0).UTC()
	created, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: "unverifiable-start", IdempotencyKey: "expected-job", RequestHash: "sha256:expected",
		RequestManifest: []byte(`{"agent":"claude","runtime":"docker","timeout":"1000000h"}`), Agent: "claude", Runtime: "docker", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{
		SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: createdAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	handle := newRecoveredNativeTestHandle("")
	handle.runtimeID = "container-unverifiable"
	handle.recovery = runtime.RecoveryInfo{
		SessionID: created.Session.ID, AgentName: "claude", Generation: 1,
		IdempotencyKey: "wrong-job", RequestHash: "sha256:wrong",
		ImageReference: "agentd:test", ImageDigest: "sha256:unverified", SandboxProfile: "docker-native-v1",
	}
	manager := session.NewManager()
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	server.RestoreRecoveredSessions(manager.Recover([]runtime.ProcessHandle{handle}, "docker"), "docker")

	stored, err := store.GetSession(ctx, created.Session.ID)
	if err != nil || stored.State != durable.StateIndeterminate {
		t.Fatalf("unverifiable session = %+v err=%v", stored, err)
	}
	receipt, err := store.GetTerminalReceipt(ctx, created.Session.ID)
	if err != nil || receipt.State != durable.StateIndeterminate || receipt.Generation != 1 || receipt.LastSequence != 1 {
		t.Fatalf("unverifiable receipt = %+v err=%v", receipt, err)
	}
}

func TestRestoreRecoveredNativeSessionAdoptsResumedGenerationBeforeCommit(t *testing.T) {
	store, sessionID := runningRecoveryStore(t)
	if _, err := store.TransitionGeneration(context.Background(), durable.TransitionGenerationParams{
		SessionID: sessionID, Generation: 1, From: durable.GenerationRunning,
		To: durable.GenerationLost, At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("mark previous generation lost: %v", err)
	}
	handle := newRecoveredNativeTestHandle(strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"resumed-provider"}`,
		`{"type":"stream_event","event":{"delta":{"type":"text_delta","text":"resumed"}}}`,
	}, "\n") + "\n")
	handle.runtimeID = "container-resumed-orphan"
	handle.recovery = runtime.RecoveryInfo{
		SessionID: sessionID, AgentName: "claude", Generation: 2,
		IdempotencyKey: "missing-job", RequestHash: "sha256:missing",
		ImageReference: "agentd:test", ImageDigest: "sha256:resumed", SandboxProfile: "docker-native-v1",
	}
	manager := session.NewManager()
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	server.RestoreRecoveredSessions(manager.Recover([]runtime.ProcessHandle{handle}, "docker"), "docker")
	waitForDurableTerminal(t, store, sessionID)

	generations, err := store.ListGenerations(context.Background(), sessionID)
	if err != nil || len(generations) != 2 || generations[1].ContainerID != "container-resumed-orphan" ||
		generations[1].ImageDigest != "sha256:resumed" || generations[1].State != durable.GenerationExited {
		t.Fatalf("reconstructed resume generations = %+v err=%v", generations, err)
	}
}

func TestRestoreRecoveredSessionsMarksMissingGenerationLost(t *testing.T) {
	store, sessionID := runningRecoveryStore(t)
	manager := session.NewManager()
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	server.RestoreRecoveredSessions(nil, "docker")
	generation, err := store.GetGeneration(context.Background(), sessionID, 1)
	if err != nil {
		t.Fatalf("get reconciled generation: %v", err)
	}
	if generation.State != durable.GenerationLost {
		t.Fatalf("missing generation state = %s, want %s", generation.State, durable.GenerationLost)
	}
	stored, err := store.GetSession(context.Background(), sessionID)
	if err != nil || stored.State != durable.StateRunning {
		t.Fatalf("resumable logical session = %+v err=%v", stored, err)
	}
}

func TestRestoreRecoveredSessionsMarksDuplicateGenerationIndeterminate(t *testing.T) {
	store, sessionID := runningRecoveryStore(t)
	first := newRecoveredNativeTestHandle("")
	first.runtimeID = "duplicate-a"
	first.recovery = runtime.RecoveryInfo{SessionID: sessionID, AgentName: "claude", Generation: 1}
	second := newRecoveredNativeTestHandle("")
	second.runtimeID = "duplicate-b"
	second.recovery = first.recovery
	manager := session.NewManager()
	orphaned := manager.Recover([]runtime.ProcessHandle{first, second}, "docker")
	server := NewServer(manager, &recoveryTestRuntime{}, agent.DefaultRegistry(), ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	server.RestoreRecoveredSessions(orphaned, "docker")
	waitForDurableTerminal(t, store, sessionID)
	stored, err := store.GetSession(context.Background(), sessionID)
	if err != nil || stored.State != durable.StateIndeterminate {
		t.Fatalf("duplicate logical session = %+v err=%v", stored, err)
	}
	receipt, err := store.GetTerminalReceipt(context.Background(), sessionID)
	if err != nil || receipt.State != durable.StateIndeterminate || receipt.LastSequence != 1 {
		t.Fatalf("duplicate terminal receipt = %+v err=%v", receipt, err)
	}
}

func TestV1ResumeLostSessionCreatesOneNextGeneration(t *testing.T) {
	store, sessionID := runningRecoveryStore(t)
	if _, err := store.TransitionGeneration(context.Background(), durable.TransitionGenerationParams{
		SessionID: sessionID, Generation: 1, From: durable.GenerationRunning, To: durable.GenerationLost, At: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("mark first generation lost: %v", err)
	}
	var spawns atomic.Int32
	runtime := &resumeTestRuntime{count: &spawns}
	resumeAgent := &resumeClaudeAgent{configs: make(chan agent.AgentConfig, 1)}
	registry := agent.NewRegistry()
	registry.Register(resumeAgent)
	manager := session.NewManager()
	server := NewServer(manager, runtime, registry, ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	body := []byte(`{"prompt":"continue after loss"}`)
	response, err := http.Post(httpServer.URL+"/api/v1/sessions/"+sessionID+"/resume", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post resume: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		var failure any
		_ = json.NewDecoder(response.Body).Decode(&failure)
		t.Fatalf("resume status=%d body=%v", response.StatusCode, failure)
	}
	select {
	case config := <-resumeAgent.configs:
		if config.ResumeSessionID != "provider-missing" || config.Model != "claude-opus-5" ||
			config.Effort != "max" || !config.Fast || config.MaxTokens != 1 ||
			!slices.Equal(config.AllowedTools, []string{"WebSearch"}) {
			t.Fatalf("resolved provider resume config = %+v", config)
		}
	case <-time.After(time.Second):
		t.Fatal("resume agent was not built")
	}
	waitForDurableTerminal(t, store, sessionID)
	receiptResponse, err := http.Get(httpServer.URL + "/api/v1/sessions/" + sessionID + "/receipt")
	if err != nil {
		t.Fatalf("get terminal receipt: %v", err)
	}
	defer receiptResponse.Body.Close()
	var receiptEnvelope struct {
		Data v1TerminalReceiptData `json:"data"`
	}
	if err := json.NewDecoder(receiptResponse.Body).Decode(&receiptEnvelope); err != nil {
		t.Fatalf("decode terminal receipt: %v", err)
	}
	if receiptResponse.StatusCode != http.StatusOK || receiptEnvelope.Data.SessionID != sessionID || receiptEnvelope.Data.Generation != 2 || receiptEnvelope.Data.LastSequence < 1 {
		t.Fatalf("terminal receipt response status=%d data=%+v", receiptResponse.StatusCode, receiptEnvelope.Data)
	}
	generations, err := store.ListGenerations(context.Background(), sessionID)
	if err != nil || len(generations) != 2 || generations[1].ProviderID != "provider-missing" {
		t.Fatalf("resumed generations = %+v err=%v", generations, err)
	}
	response = postResume(t, httpServer.URL, sessionID, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || spawns.Load() != 1 {
		t.Fatalf("idempotent resume status=%d spawns=%d", response.StatusCode, spawns.Load())
	}
}

func postResume(t *testing.T, baseURL, sessionID string, body []byte) *http.Response {
	t.Helper()
	response, err := http.Post(baseURL+"/api/v1/sessions/"+sessionID+"/resume", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post resume: %v", err)
	}
	return response
}

func runningRecoveryStore(t *testing.T) (durable.Store, string) {
	t.Helper()
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	createdAt := time.Unix(2_000, 0).UTC()
	created, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: "missing-session", IdempotencyKey: "missing-job", RequestHash: "sha256:missing",
		RequestManifest: []byte(`{"agent":"claude","runtime":"docker","model":"claude-opus-5","effort":"max","fast":true,"timeout":"1000000h","claude":{"max_turns":1,"allowed_tools":["WebSearch"]}}`), Agent: "claude", Runtime: "docker", CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: created.Session.ID, From: durable.StateCreated, To: durable.StateStarting, At: createdAt.Add(time.Second)}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if _, err := store.CreateGeneration(ctx, durable.CreateGenerationParams{SessionID: created.Session.ID, Runtime: "docker", ContainerID: "missing-container", ProviderID: "provider-missing", CreatedAt: createdAt.Add(2 * time.Second)}); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	if _, err := store.TransitionGeneration(ctx, durable.TransitionGenerationParams{SessionID: created.Session.ID, Generation: 1, From: durable.GenerationStarting, To: durable.GenerationRunning, At: createdAt.Add(3 * time.Second)}); err != nil {
		t.Fatalf("run generation: %v", err)
	}
	if _, err := store.TransitionSession(ctx, durable.TransitionSessionParams{SessionID: created.Session.ID, From: durable.StateStarting, To: durable.StateRunning, At: createdAt.Add(4 * time.Second)}); err != nil {
		t.Fatalf("run session: %v", err)
	}
	return store, created.Session.ID
}

func nativeRecord(provider string, ordinal int64, timestamp time.Time, raw []byte) nativeprotocol.Record {
	return nativeprotocol.Record{
		Provider: nativeprotocol.Provider(provider), Stream: nativeprotocol.StreamProviderStdout,
		Ordinal: ordinal, Timestamp: timestamp, Raw: raw,
	}
}

type recoveredNativeTestHandle struct {
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	stdin     *testWriteCloser
	wait      chan runtime.ExitResult
	kill      func() error
	runtimeID string
	recovery  runtime.RecoveryInfo
}

func newBlockingRecoveredNativeTestHandle(sessionID, runtimeID string) *recoveredNativeTestHandle {
	stdout, stdoutWriter := io.Pipe()
	wait := make(chan runtime.ExitResult, 1)
	handle := &recoveredNativeTestHandle{
		stdout: stdout, stderr: io.NopCloser(strings.NewReader("")), stdin: &testWriteCloser{},
		wait: wait, runtimeID: runtimeID,
		recovery: runtime.RecoveryInfo{SessionID: sessionID, AgentName: "claude", Generation: 1},
	}
	handle.kill = func() error {
		_ = stdoutWriter.Close()
		wait <- runtime.ExitResult{Code: -1}
		close(wait)
		return nil
	}
	return handle
}

func newRecoveredNativeTestHandle(stdout string) *recoveredNativeTestHandle {
	wait := make(chan runtime.ExitResult, 1)
	wait <- runtime.ExitResult{Code: 0}
	return &recoveredNativeTestHandle{
		stdout: io.NopCloser(strings.NewReader(stdout)), stderr: io.NopCloser(strings.NewReader("")),
		stdin: &testWriteCloser{}, wait: wait, runtimeID: "container-recovered",
		recovery: runtime.RecoveryInfo{SessionID: "recovered-session", AgentName: "claude", Generation: 1},
	}
}

func (handle *recoveredNativeTestHandle) Stdin() io.WriteCloser           { return handle.stdin }
func (handle *recoveredNativeTestHandle) Stdout() io.ReadCloser           { return handle.stdout }
func (handle *recoveredNativeTestHandle) Stderr() io.ReadCloser           { return handle.stderr }
func (handle *recoveredNativeTestHandle) Wait() <-chan runtime.ExitResult { return handle.wait }
func (handle *recoveredNativeTestHandle) Kill() error {
	if handle.kill != nil {
		return handle.kill()
	}
	return nil
}
func (handle *recoveredNativeTestHandle) PID() int          { return 0 }
func (handle *recoveredNativeTestHandle) RuntimeID() string { return handle.runtimeID }
func (handle *recoveredNativeTestHandle) NativeStdio() bool { return true }
func (handle *recoveredNativeTestHandle) RecoveryInfo() *runtime.RecoveryInfo {
	copy := handle.recovery
	return &copy
}

type testWriteCloser struct{}

func (*testWriteCloser) Write(value []byte) (int, error) { return len(value), nil }
func (*testWriteCloser) Close() error                    { return nil }

type recoveryTestRuntime struct{}

func (*recoveryTestRuntime) Name() string { return "docker" }
func (*recoveryTestRuntime) Spawn(context.Context, runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	return nil, nil
}
func (*recoveryTestRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) {
	return nil, nil
}
func (*recoveryTestRuntime) Cleanup(context.Context) error { return nil }

type resumeTestRuntime struct {
	count *atomic.Int32
}

func (*resumeTestRuntime) Name() string { return "docker" }
func (rt *resumeTestRuntime) Spawn(ctx context.Context, config runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	rt.count.Add(1)
	return runtime.NewLocalRuntime().Spawn(ctx, config)
}
func (*resumeTestRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) { return nil, nil }
func (*resumeTestRuntime) Cleanup(context.Context) error                            { return nil }

type resumeClaudeAgent struct {
	configs chan agent.AgentConfig
}

func (*resumeClaudeAgent) Name() string { return "claude" }
func (agentImpl *resumeClaudeAgent) BuildCmd(_ string, config agent.AgentConfig) ([]string, error) {
	agentImpl.configs <- config
	return []string{"/bin/sh", "-c", `IFS= read -r prompt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"provider-missing"}' '{"type":"result","subtype":"success","is_error":false,"result":"resumed"}'`}, nil
}
func (*resumeClaudeAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }
