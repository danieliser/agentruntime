package api

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/observer"
)

type replayCapabilities struct {
	SequenceCursor     bool `json:"sequence_cursor"`
	StoredThenLive     bool `json:"stored_then_live"`
	RestartPersistence bool `json:"restart_persistence"`
}

type authenticationCapabilities struct {
	Mode      string `json:"mode"`
	Transport string `json:"transport"`
}

type v1Capabilities struct {
	AgentDVersion        string                     `json:"agentd_version"`
	APIVersions          []string                   `json:"api_versions"`
	EventSchemaVersions  []string                   `json:"event_schema_versions"`
	NativeProviders      []string                   `json:"native_providers"`
	Runtimes             []string                   `json:"runtimes"`
	LifecycleControls    []string                   `json:"lifecycle_controls"`
	Replay               replayCapabilities         `json:"replay"`
	DockerReconstruction bool                       `json:"docker_reconstruction"`
	PluginAPIVersions    []string                   `json:"plugin_api_versions"`
	Plugins              []observer.PluginStatus    `json:"plugins"`
	ListenerScope        string                     `json:"listener_scope"`
	Authentication       authenticationCapabilities `json:"authentication"`
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
	authentication := authenticationCapabilities{Mode: "none", Transport: "none"}
	if s.authEnabled {
		authentication = authenticationCapabilities{Mode: "bearer_token_file", Transport: "authorization_header"}
	}
	c.JSON(http.StatusOK, gin.H{"api_version": "v1", "data": v1Capabilities{
		AgentDVersion: s.version, APIVersions: []string{"v1"},
		EventSchemaVersions: []string{eventstream.SchemaVersion}, NativeProviders: providers,
		LifecycleControls: []string{"start", "list", "inspect", "replay", "attach", "prompt", "steer", "interrupt", "cancel", "terminate", "resume", "receipt"},
		Runtimes:          runtimes, Replay: replayCapabilities{
			SequenceCursor: durableReplay, StoredThenLive: durableReplay, RestartPersistence: durableReplay,
		},
		DockerReconstruction: dockerReconstruction && durableReplay,
		PluginAPIVersions:    []string{observer.APIVersion}, Plugins: plugins,
		ListenerScope: s.listenerScope, Authentication: authentication,
	}})
}
