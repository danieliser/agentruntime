package observer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	durablememory "github.com/danieliser/agentruntime/pkg/durable/memory"
)

func TestManagerReplaysLedgerAndRestartsFromAcknowledgedCheckpoint(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	recordPath := filepath.Join(dataDir, "trace-effects.txt")
	store := durablememory.New()
	sessionID := seedObserverSession(t, store, 2)
	config := Config{Version: "1", Plugins: []PluginConfig{helperPluginConfig(t, "healthy", recordPath)}}

	manager, err := NewManager(dataDir, store, config, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recordedEffects(t, recordPath); len(got) != 2 {
		t.Fatalf("effects after first sync = %v, want two", got)
	}
	link, ok := manager.TraceLink("opentraces", sessionID)
	if !ok || link.TraceID != "851ad0da-3f90-4ea8-9094-9b644d1913f7" || link.AcknowledgedSequence != 2 {
		t.Fatalf("trace link = %+v, %v", link, ok)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewManager(dataDir, store, config, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(ctx)
	if err := restarted.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if link, ok := restarted.TraceLink("opentraces", sessionID); !ok || link.AcknowledgedSequence != 2 {
		t.Fatalf("restarted trace link = %+v, %v", link, ok)
	}
	if got := recordedEffects(t, recordPath); len(got) != 2 {
		t.Fatalf("restart duplicated trace effects: %v", got)
	}
	appendObserverEvent(t, store, sessionID, 3)
	if err := restarted.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recordedEffects(t, recordPath); len(got) != 3 || got[2] != "event-3" {
		t.Fatalf("incremental replay effects = %v", got)
	}
}

func TestManagerRefusesFutureOrMismatchedCheckpoint(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store := durablememory.New()
	sessionID := seedObserverSession(t, store, 1)
	checkpoints, err := NewCheckpointStore(dataDir, "opentraces")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoints.Advance(Checkpoint{SessionID: sessionID, Sequence: 99, EventID: "future"}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(dataDir, store, Config{Version: "1", Plugins: []PluginConfig{helperPluginConfig(t, "healthy", "")}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(ctx)
	if err := manager.Sync(ctx); err == nil {
		t.Fatal("expected future checkpoint rejection")
	}
	status := manager.Status()[0]
	if status.State != HealthDegraded || !strings.Contains(status.LastError, "checkpoint") {
		t.Fatalf("status = %+v", status)
	}
}

func TestManagerPluginFailureDoesNotBlockEventCommit(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store := durablememory.New()
	sessionID := seedObserverSession(t, store, 0)
	config := helperPluginConfig(t, "slow", "")
	config.Timeout = 20 * time.Millisecond
	manager, err := NewManager(dataDir, store, Config{Version: "1", Plugins: []PluginConfig{config}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(ctx)

	appendObserverEvent(t, store, sessionID, 1)
	started := time.Now()
	err = manager.Sync(ctx)
	if err == nil {
		t.Fatal("expected slow plugin delivery failure")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("slow observer exceeded bounded timeout: %s", elapsed)
	}
	appendObserverEvent(t, store, sessionID, 2)
	page, err := store.ListEvents(ctx, durable.EventQuery{SessionID: sessionID, AfterSequence: 0, Limit: 10})
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("durable ingestion affected by plugin failure: events=%d err=%v", len(page.Events), err)
	}
	if status := manager.Status()[0]; status.State != HealthDown || status.Unacknowledged < 1 {
		t.Fatalf("status = %+v", status)
	}
}

func TestManagerRequiredReadiness(t *testing.T) {
	store := durablememory.New()
	manager, err := NewManager(t.TempDir(), store, Config{Version: "1"}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RequireHealthy("opentraces"); !errors.Is(err, ErrPluginUnavailable) {
		t.Fatalf("missing required plugin error = %v", err)
	}
}

func TestManagerChecksExternalHealthWithoutWaitingForAnEvent(t *testing.T) {
	ctx := context.Background()
	store := durablememory.New()
	config := helperPluginConfig(t, "health-error", "")
	manager, err := NewManager(t.TempDir(), store, Config{Version: "1", Plugins: []PluginConfig{config}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(ctx)
	if err := manager.Sync(ctx); err == nil {
		t.Fatal("expected external health failure")
	}
	if status := manager.Status()[0]; status.State != HealthDegraded || status.LastError == "" {
		t.Fatalf("status = %+v", status)
	}
}

func TestManagerReplaysDuplicateAfterPluginCrashBeforeCheckpoint(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	recordPath := filepath.Join(dataDir, "trace-effects.txt")
	store := durablememory.New()
	sessionID := seedObserverSession(t, store, 1)
	crashing := helperPluginConfig(t, "crash-after-effect", recordPath)
	first, err := NewManager(dataDir, store, Config{Version: "1", Plugins: []PluginConfig{crashing}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Sync(ctx); err == nil {
		t.Fatal("expected crash before acknowledgement")
	}
	_ = first.Close(ctx)
	if got := recordedEffects(t, recordPath); len(got) != 1 {
		t.Fatalf("external effects after crash = %v", got)
	}

	healthy := helperPluginConfig(t, "healthy", recordPath)
	restarted, err := NewManager(dataDir, store, Config{Version: "1", Plugins: []PluginConfig{healthy}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(ctx)
	if err := restarted.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recordedEffects(t, recordPath); len(got) != 1 {
		t.Fatalf("replay duplicated external effect: %v", got)
	}
	if link, ok := restarted.TraceLink("opentraces", sessionID); !ok || link.AcknowledgedSequence != 1 {
		t.Fatalf("replayed trace link = %+v, %v", link, ok)
	}
}

func TestManagerUpgradesExternalAdapterWithoutLosingCheckpoint(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	recordPath := filepath.Join(dataDir, "trace-effects.txt")
	store := durablememory.New()
	sessionID := seedObserverSession(t, store, 1)
	v1 := helperPluginConfig(t, "healthy", recordPath)
	v1.Environment["AGENTD_OBSERVER_VERSION"] = "1.0.0"
	first, err := NewManager(dataDir, store, Config{Version: "1", Plugins: []PluginConfig{v1}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	appendObserverEvent(t, store, sessionID, 2)
	v2 := helperPluginConfig(t, "healthy", recordPath)
	v2.Environment["AGENTD_OBSERVER_VERSION"] = "2.0.0"
	second, err := NewManager(dataDir, store, Config{Version: "1", Plugins: []PluginConfig{v2}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(ctx)
	if err := second.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if status := second.Status()[0]; status.Version != "2.0.0" || status.Unacknowledged != 0 {
		t.Fatalf("upgraded status = %+v", status)
	}
	if got := recordedEffects(t, recordPath); len(got) != 2 {
		t.Fatalf("upgraded effects = %v", got)
	}
}

func TestManagerReportsMissingAndIncompatibleAdaptersDown(t *testing.T) {
	ctx := context.Background()
	store := durablememory.New()
	missing := PluginConfig{Name: "opentraces", Enabled: true, Command: "/definitely/missing/opentraces-adapter", Policy: PolicyBestEffort, Timeout: 20 * time.Millisecond}
	manager, err := NewManager(t.TempDir(), store, Config{Version: "1", Plugins: []PluginConfig{missing}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Sync(ctx); err == nil || manager.Status()[0].State != HealthDown {
		t.Fatalf("missing adapter status=%+v err=%v", manager.Status(), err)
	}

	incompatible := helperPluginConfig(t, "incompatible", "")
	manager, err = NewManager(t.TempDir(), store, Config{Version: "1", Plugins: []PluginConfig{incompatible}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Sync(ctx); err == nil || manager.Status()[0].State != HealthDown {
		t.Fatalf("incompatible adapter status=%+v err=%v", manager.Status(), err)
	}
}

func TestManagerDeliversTradingFloorProviderAndSandboxLinkage(t *testing.T) {
	ctx := context.Background()
	store := durablememory.New()
	sessionID := seedObserverSession(t, store, 1)
	config := helperPluginConfig(t, "require-context", "")
	manager, err := NewManager(t.TempDir(), store, Config{Version: "1", Plugins: []PluginConfig{config}}, "test-agentd", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(ctx)
	if err := manager.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if link, ok := manager.TraceLink("opentraces", sessionID); !ok || link.TraceID == "" {
		t.Fatalf("trace link = %+v, %v", link, ok)
	}
}

func helperPluginConfig(t *testing.T, mode, recordPath string) PluginConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"AGENTD_OBSERVER_HELPER": mode}
	if recordPath != "" {
		environment["AGENTD_OBSERVER_RECORD"] = recordPath
	}
	return PluginConfig{
		Name: "opentraces", Enabled: true, Command: executable,
		Args: []string{"-test.run=TestObserverHelperProcess"}, Environment: environment,
		Policy: PolicyBestEffort, Timeout: 2 * time.Second,
	}
}

func seedObserverSession(t *testing.T, store durable.Store, events int) string {
	t.Helper()
	ctx := context.Background()
	sessionID := "718258fe-2921-4f67-91c9-cb70720264b4"
	_, err := store.CreateSession(ctx, durable.CreateSessionParams{
		SessionID: sessionID, IdempotencyKey: "job-observer", RequestHash: "sha256:observer",
		RequestManifest: json.RawMessage(`{"agent":"codex"}`), Agent: "codex", Runtime: "docker", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateGeneration(ctx, durable.CreateGenerationParams{
		SessionID: sessionID, Runtime: "docker", ContainerID: "container-observer",
		ImageReference: "fixture:latest", ImageDigest: "sha256:fixture", SandboxProfile: "fixture-v1",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= events; sequence++ {
		appendObserverEvent(t, store, sessionID, sequence)
	}
	return sessionID
}

func appendObserverEvent(t *testing.T, store durable.Store, sessionID string, ordinal int) {
	t.Helper()
	_, err := store.AppendEvent(context.Background(), durable.AppendEventParams{
		SchemaVersion: "1.0", EventID: fmt.Sprintf("event-%d", ordinal), SessionID: sessionID,
		Generation: 1, Timestamp: time.Now().UTC(), Type: "content.delta", Stream: durable.StreamProviderStdout,
		Payload: json.RawMessage(`{"text":"hello"}`), Raw: []byte(`{"delta":"hello"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func recordedEffects(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var effects []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		effects = append(effects, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return effects
}
