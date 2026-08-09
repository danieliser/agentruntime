package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestV1CapabilitiesExposeNativeReplayAndRuntimeCompatibility(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatalf("open durable store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		Version: "test-version", LogDir: filepath.Join(t.TempDir(), "logs"),
		ExtraRuntimes: []runtime.Runtime{&recoveryTestRuntime{}},
		DurableStore:  store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	response, err := http.Get(httpServer.URL + "/api/v1/capabilities")
	if err != nil {
		t.Fatalf("get capabilities: %v", err)
	}
	defer response.Body.Close()
	var envelope struct {
		APIVersion string `json:"api_version"`
		Data       struct {
			AgentDVersion       string   `json:"agentd_version"`
			EventSchemaVersions []string `json:"event_schema_versions"`
			NativeProviders     []string `json:"native_providers"`
			Runtimes            []string `json:"runtimes"`
			LifecycleControls   []string `json:"lifecycle_controls"`
			Replay              struct {
				SequenceCursor     bool `json:"sequence_cursor"`
				StoredThenLive     bool `json:"stored_then_live"`
				RestartPersistence bool `json:"restart_persistence"`
			} `json:"replay"`
			DockerReconstruction bool     `json:"docker_reconstruction"`
			PluginAPIVersions    []string `json:"plugin_api_versions"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if response.StatusCode != http.StatusOK || envelope.APIVersion != "v1" || envelope.Data.AgentDVersion == "" {
		t.Fatalf("capability envelope status=%d value=%+v", response.StatusCode, envelope)
	}
	if len(envelope.Data.EventSchemaVersions) != 1 || envelope.Data.EventSchemaVersions[0] != "1.0" ||
		!containsString(envelope.Data.NativeProviders, "claude") || !containsString(envelope.Data.NativeProviders, "codex") ||
		!containsString(envelope.Data.Runtimes, "local") || !envelope.Data.Replay.SequenceCursor ||
		!containsString(envelope.Data.LifecycleControls, "terminate") || !containsString(envelope.Data.LifecycleControls, "resume") ||
		!envelope.Data.Replay.StoredThenLive || !envelope.Data.Replay.RestartPersistence ||
		!envelope.Data.DockerReconstruction || !containsString(envelope.Data.PluginAPIVersions, "1.0") {
		t.Fatalf("capability data = %+v", envelope.Data)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
