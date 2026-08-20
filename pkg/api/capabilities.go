package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/observer"
)

type replayCapabilities struct {
	SequenceCursor     bool `json:"sequence_cursor"`
	StoredThenLive     bool `json:"stored_then_live"`
	RestartPersistence bool `json:"restart_persistence"`
}

type authenticationCapabilities struct {
	Mode               string `json:"mode"`
	Transport          string `json:"transport"` // Deprecated: use HTTPTransport.
	HTTPTransport      string `json:"http_transport"`
	WebSocketTransport string `json:"websocket_transport"`
}

type structuredOutputCapabilities struct {
	Providers       []string `json:"providers"`
	NativeEnforced  bool     `json:"native_enforced"`
	SchemaHash      string   `json:"schema_hash"`
	DefaultMaxBytes int      `json:"default_max_bytes"`
	MaximumBytes    int      `json:"maximum_bytes"`
	ResultEvent     string   `json:"result_event"`
	ResultEndpoint  string   `json:"result_endpoint"`
}

type workspaceProfileCapabilities struct {
	Name               string   `json:"name"`
	Retention          string   `json:"retention"`
	Filesystems        []string `json:"filesystems"`
	Network            string   `json:"network"`
	HostMounts         bool     `json:"host_mounts"`
	AmbientCredentials bool     `json:"ambient_credentials"`
}

type credentialGrantCapabilities struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	RequestEnv      string `json:"request_env"`
	Materialization string `json:"materialization"`
	Persistence     string `json:"persistence"`
}

type egressPolicyCapabilities struct {
	PolicyField            string              `json:"policy_field"`
	DefaultDeny            bool                `json:"default_deny"`
	ExactHostsOnly         bool                `json:"exact_hosts_only"`
	ProxyRequired          bool                `json:"proxy_required"`
	DirectDNSIPEgress      bool                `json:"direct_dns_ip_egress"`
	EnvironmentProxyBypass bool                `json:"environment_proxy_bypass"`
	ProviderEndpoints      map[string][]string `json:"provider_endpoints"`
	ToolEndpoints          map[string][]string `json:"tool_endpoints"`
}

type resourceLimitCapabilities struct {
	PolicyField string         `json:"policy_field"`
	Defaults    ResourceLimits `json:"defaults"`
	Minimums    ResourceLimits `json:"minimums"`
	Maximums    ResourceLimits `json:"maximums"`
	BreachCode  string         `json:"breach_code"`
}

type v1Capabilities struct {
	AgentDVersion           string                         `json:"agentd_version"`
	CommitHash              string                         `json:"commit_hash"`
	APIVersions             []string                       `json:"api_versions"`
	EventSchemaVersions     []string                       `json:"event_schema_versions"`
	ExecutionPolicyVersions []string                       `json:"execution_policy_versions"`
	NativeProviders         []string                       `json:"native_providers"`
	Runtimes                []string                       `json:"runtimes"`
	LifecycleControls       []string                       `json:"lifecycle_controls"`
	Replay                  replayCapabilities             `json:"replay"`
	DockerReconstruction    bool                           `json:"docker_reconstruction"`
	PluginAPIVersions       []string                       `json:"plugin_api_versions"`
	Plugins                 []observer.PluginStatus        `json:"plugins"`
	ListenerScope           string                         `json:"listener_scope"`
	Authentication          authenticationCapabilities     `json:"authentication"`
	StructuredOutput        structuredOutputCapabilities   `json:"structured_output"`
	WorkspaceProfiles       []workspaceProfileCapabilities `json:"workspace_profiles"`
	CredentialGrants        []credentialGrantCapabilities  `json:"credential_grants"`
	EgressPolicy            egressPolicyCapabilities       `json:"egress_policy"`
	ResourceLimits          resourceLimitCapabilities      `json:"resource_limits"`
}

func (s *Server) handleV1Capabilities(c *gin.Context) {
	runtimes := make([]string, 0, len(s.runtimes))
	dockerReconstruction := false
	for name := range s.runtimes {
		runtimes = append(runtimes, name)
		if name == "docker" {
			dockerReconstruction = true
		}
	}
	sort.Strings(runtimes)
	providers := make([]string, 0, 2)
	for _, name := range []string{"claude", "codex"} {
		if s.agents.Get(name) != nil {
			providers = append(providers, name)
		}
	}
	durableReplay := s.durableStore != nil && s.eventBroker != nil
	plugins := []observer.PluginStatus{}
	if s.observers != nil {
		plugins = s.observers.Status()
	}
	authentication := authenticationCapabilities{Mode: "none", Transport: "none", HTTPTransport: "none", WebSocketTransport: "none"}
	if s.authEnabled {
		authentication = authenticationCapabilities{
			Mode: "bearer_token_file", Transport: "authorization_header",
			HTTPTransport: "authorization_header", WebSocketTransport: "authenticated_subprotocol",
		}
	}
	c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": v1Capabilities{
		AgentDVersion: s.version, CommitHash: s.commitHash, APIVersions: []string{"v1"},
		EventSchemaVersions: []string{eventstream.SchemaVersion}, ExecutionPolicyVersions: []string{LegacyExecutionPolicyVersion, ExecutionPolicyVersion}, NativeProviders: providers,
		LifecycleControls: []string{"start", "list", "inspect", "replay", "attach", "prompt", "steer", "interrupt", "cancel", "terminate", "resume", "receipt"},
		Runtimes:          runtimes, Replay: replayCapabilities{
			SequenceCursor: durableReplay, StoredThenLive: durableReplay, RestartPersistence: durableReplay,
		},
		DockerReconstruction: dockerReconstruction && durableReplay,
		PluginAPIVersions:    []string{observer.APIVersion}, Plugins: plugins,
		ListenerScope: s.listenerScope, Authentication: authentication,
		StructuredOutput: structuredOutputCapabilities{
			Providers: providers, NativeEnforced: true, SchemaHash: "sha256",
			DefaultMaxBytes: DefaultStructuredOutputMaxBytes, MaximumBytes: MaximumStructuredOutputBytes,
			ResultEvent: "output.final", ResultEndpoint: "/api/v1/sessions/{session_id}/result",
		},
		WorkspaceProfiles: []workspaceProfileCapabilities{{
			Name: "ephemeral", Retention: "terminal_receipt", Filesystems: []string{"read_only", "workspace_write"},
			Network: "public_https", HostMounts: false, AmbientCredentials: false,
		}},
		CredentialGrants: []credentialGrantCapabilities{{
			Name: "codex_auth_json", Provider: "codex", RequestEnv: CodexAuthJSONEnv,
			Materialization: "private_session_auth_file", Persistence: "name_only",
		}},
		EgressPolicy: egressPolicyCapabilities{
			PolicyField: "egress_allowlist", DefaultDeny: true, ExactHostsOnly: true, ProxyRequired: true,
			DirectDNSIPEgress: false, EnvironmentProxyBypass: false,
			ProviderEndpoints: map[string][]string{"claude": {"api.anthropic.com"}, "codex": {"chatgpt.com"}},
			ToolEndpoints:     map[string][]string{"web_search": {"api.openai.com"}},
		},
		ResourceLimits: resourceLimitCapabilities{
			PolicyField: "resources", Defaults: DefaultResourceLimits, Minimums: MinimumResourceLimits, Maximums: MaximumResourceLimits,
			BreachCode: string(durable.CodeResourceLimitExceeded),
		},
	}})
}
