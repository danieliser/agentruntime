package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestDiagnosticRedactionsIncludePromptsAndCredentialValuesOnly(t *testing.T) {
	request := SessionRequest{
		Prompt: "private prompt\nsecond private line",
		Env: map[string]string{
			"PUBLIC_SETTING": "keep-visible",
			"API_TOKEN":      "credential-token",
			"OPAQUE_GRANT":   "opaque-secret",
			CodexAuthJSONEnv: `{"tokens":{"access_token":"access-secret","refresh_token":"refresh-secret"},"account_id":"private-account"}`,
		},
		SecretGrants: []string{"OPAQUE_GRANT"},
	}
	values := diagnosticRedactions(request)
	for _, want := range []string{
		"private prompt\nsecond private line", "private prompt", "second private line",
		"credential-token", "opaque-secret", "access-secret", "refresh-secret", "private-account",
	} {
		if !slices.Contains(values, want) {
			t.Errorf("diagnostic redactions missing %q: %v", want, values)
		}
	}
	if slices.Contains(values, "keep-visible") {
		t.Fatalf("non-sensitive environment value was redacted: %v", values)
	}
}

func TestDisabledDiagnosticLogsCreateNoSessionFileEndToEnd(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logDir := filepath.Join(t.TempDir(), "logs")
	runtimeImpl := &countingRuntime{Runtime: runtime.NewLocalRuntime(), count: &atomic.Int32{}}
	registry := agent.NewRegistry()
	registry.Register(&nativeClaudeFixtureAgent{})
	manager := session.NewManager()
	t.Cleanup(manager.ShutdownAll)
	server := NewServer(manager, runtimeImpl, registry, ServerConfig{
		DataDir: t.TempDir(), LogDir: logDir,
		DiagnosticLogs: &DiagnosticLogConfig{Enabled: false, Retention: 7 * 24 * time.Hour},
		DurableStore:   store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	response := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "diagnostic-logs-disabled", "agent": "claude", "runtime": "test", "prompt": "private prompt",
	})
	defer response.Body.Close()
	var created struct {
		Data v1SessionData `json:"data"`
	}
	decodeJSON(t, response.Body, &created)
	waitForDurableTerminal(t, store, created.Data.SessionID)
	if _, err := store.GetTerminalReceipt(context.Background(), created.Data.SessionID); err != nil {
		t.Fatalf("disabled diagnostics changed terminal receipt availability: %v", err)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("disabled diagnostics created log directory: %v", err)
	}
}

func TestServerDiagnosticLogPolicySupportsDisableAndRetention(t *testing.T) {
	disabled := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		LogDir:         filepath.Join(t.TempDir(), "disabled-logs"),
		DiagnosticLogs: &DiagnosticLogConfig{Enabled: false, Retention: 7 * 24 * time.Hour},
	})
	if disabled.logDir != "" {
		t.Fatalf("disabled diagnostic log dir = %q", disabled.logDir)
	}

	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "expired.ndjson")
	retained := filepath.Join(dir, "retained.ndjson")
	if err := os.WriteFile(old, []byte("expired"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retained, []byte("retained"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	enabled := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		LogDir: dir, DiagnosticLogs: &DiagnosticLogConfig{Enabled: true, Retention: 7 * 24 * time.Hour},
	})
	if enabled.logDir != dir {
		t.Fatalf("enabled diagnostic log dir = %q", enabled.logDir)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired diagnostic log remains: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("diagnostic directory mode=%v err=%v", info, err)
	}
	if info, err := os.Stat(retained); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("retained diagnostic mode=%v err=%v", info, err)
	}
}
