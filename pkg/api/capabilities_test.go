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
		Version: "test-version", CommitHash: "0123456789abcdef0123456789abcdef01234567", LogDir: filepath.Join(t.TempDir(), "logs"),
		ExtraRuntimes: []runtime.Runtime{&recoveryTestRuntime{}},
		DurableStore:  store, EventBroker: eventstream.New(store),
		AuthToken: "test-capability-token-that-is-long-enough-123456", ListenerScope: "loopback",
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/capabilities", nil)
	if err != nil {
		t.Fatalf("new capabilities request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer test-capability-token-that-is-long-enough-123456")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get capabilities: %v", err)
	}
	defer response.Body.Close()
	var envelope struct {
		APIVersion string `json:"api_version"`
		Data       struct {
			AgentDVersion           string   `json:"agentd_version"`
			CommitHash              string   `json:"commit_hash"`
			EventSchemaVersions     []string `json:"event_schema_versions"`
			ExecutionPolicyVersions []string `json:"execution_policy_versions"`
			NativeProviders         []string `json:"native_providers"`
			Runtimes                []string `json:"runtimes"`
			LifecycleControls       []string `json:"lifecycle_controls"`
			Replay                  struct {
				SequenceCursor     bool `json:"sequence_cursor"`
				StoredThenLive     bool `json:"stored_then_live"`
				RestartPersistence bool `json:"restart_persistence"`
			} `json:"replay"`
			DockerReconstruction bool     `json:"docker_reconstruction"`
			PluginAPIVersions    []string `json:"plugin_api_versions"`
			ListenerScope        string   `json:"listener_scope"`
			Authentication       struct {
				Mode               string `json:"mode"`
				HTTPTransport      string `json:"http_transport"`
				WebSocketTransport string `json:"websocket_transport"`
			} `json:"authentication"`
			StructuredOutput struct {
				Providers       []string `json:"providers"`
				NativeEnforced  bool     `json:"native_enforced"`
				DefaultMaxBytes int      `json:"default_max_bytes"`
				MaximumBytes    int      `json:"maximum_bytes"`
				ResultEvent     string   `json:"result_event"`
				ResultEndpoint  string   `json:"result_endpoint"`
			} `json:"structured_output"`
			WorkspaceProfiles []struct {
				Name        string   `json:"name"`
				Retention   string   `json:"retention"`
				Filesystems []string `json:"filesystems"`
				Network     string   `json:"network"`
			} `json:"workspace_profiles"`
			CredentialGrants []struct {
				Name            string `json:"name"`
				Provider        string `json:"provider"`
				RequestEnv      string `json:"request_env"`
				Materialization string `json:"materialization"`
				Persistence     string `json:"persistence"`
			} `json:"credential_grants"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if response.StatusCode != http.StatusOK || envelope.APIVersion != "v1" || envelope.Data.AgentDVersion == "" || envelope.Data.CommitHash != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("capability envelope status=%d value=%+v", response.StatusCode, envelope)
	}
	if len(envelope.Data.EventSchemaVersions) != 1 || envelope.Data.EventSchemaVersions[0] != "1.0" ||
		!containsString(envelope.Data.NativeProviders, "claude") || !containsString(envelope.Data.NativeProviders, "codex") ||
		!containsString(envelope.Data.Runtimes, "local") || !envelope.Data.Replay.SequenceCursor ||
		!containsString(envelope.Data.LifecycleControls, "terminate") || !containsString(envelope.Data.LifecycleControls, "resume") ||
		!envelope.Data.Replay.StoredThenLive || !envelope.Data.Replay.RestartPersistence ||
		!envelope.Data.DockerReconstruction || !containsString(envelope.Data.PluginAPIVersions, "1.0") ||
		envelope.Data.ListenerScope != "loopback" || envelope.Data.Authentication.Mode != "bearer_token_file" ||
		envelope.Data.Authentication.HTTPTransport != "authorization_header" || envelope.Data.Authentication.WebSocketTransport != "authenticated_subprotocol" ||
		!containsString(envelope.Data.ExecutionPolicyVersions, ExecutionPolicyVersion) ||
		!envelope.Data.StructuredOutput.NativeEnforced || envelope.Data.StructuredOutput.DefaultMaxBytes != DefaultStructuredOutputMaxBytes ||
		envelope.Data.StructuredOutput.MaximumBytes != MaximumStructuredOutputBytes || envelope.Data.StructuredOutput.ResultEvent != "output.final" ||
		envelope.Data.StructuredOutput.ResultEndpoint != "/api/v1/sessions/{session_id}/result" ||
		!containsString(envelope.Data.StructuredOutput.Providers, "codex") || len(envelope.Data.WorkspaceProfiles) != 1 ||
		envelope.Data.WorkspaceProfiles[0].Name != "ephemeral" || envelope.Data.WorkspaceProfiles[0].Retention != "terminal_receipt" ||
		envelope.Data.WorkspaceProfiles[0].Network != "public_https" || !containsString(envelope.Data.WorkspaceProfiles[0].Filesystems, "read_only") {
		t.Fatalf("capability data = %+v", envelope.Data)
	}
	if len(envelope.Data.CredentialGrants) != 1 || envelope.Data.CredentialGrants[0].Name != "codex_auth_json" ||
		envelope.Data.CredentialGrants[0].Provider != "codex" || envelope.Data.CredentialGrants[0].RequestEnv != CodexAuthJSONEnv ||
		envelope.Data.CredentialGrants[0].Materialization != "private_session_auth_file" || envelope.Data.CredentialGrants[0].Persistence != "name_only" {
		t.Fatalf("credential grant capabilities = %+v", envelope.Data.CredentialGrants)
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
