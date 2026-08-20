package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
)

func TestResolveExecutionPolicyCanonicalizesPublicResearchProfile(t *testing.T) {
	request := SessionRequest{
		Agent: "codex", Runtime: "docker", Prompt: "research",
		ExecutionPolicy: &ExecutionPolicy{
			Workspace: "ephemeral", Filesystem: "read_only", Network: "public_https",
			AllowedTools: []string{"web_search"}, EgressAllowlist: []string{"api.openai.com", "chatgpt.com"}, ApprovalPolicy: "never",
		},
	}
	resolved, err := resolveExecutionPolicy(&request, "docker")
	if err != nil {
		t.Fatalf("resolve execution policy: %v", err)
	}
	if resolved.Hash == "" || !strings.HasPrefix(resolved.Hash, "sha256:") {
		t.Fatalf("resolved policy hash = %q", resolved.Hash)
	}
	if request.ExecutionPolicy.Version != ExecutionPolicyVersion || request.ExecutionPolicy.WorkspaceRetention != "terminal_receipt" || request.Context != "clean" {
		t.Fatalf("canonical request policy=%+v context=%q", request.ExecutionPolicy, request.Context)
	}
	discovery, ok := request.AutoDiscover.(bool)
	if !ok || discovery {
		t.Fatalf("auto_discover = %#v, want explicit false", request.AutoDiscover)
	}

	manifest, _, _, err := durableRequestManifest(request, "docker")
	if err != nil {
		t.Fatalf("durable request manifest: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(manifest, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["execution_policy_hash"] != resolved.Hash {
		t.Fatalf("manifest policy hash = %#v, want %q", stored["execution_policy_hash"], resolved.Hash)
	}
}

func TestResolveExecutionPolicyEgressAllowlistIsCanonicalAndHashCovered(t *testing.T) {
	request := SessionRequest{Agent: "codex", ExecutionPolicy: &ExecutionPolicy{
		Version: ExecutionPolicyVersion, Workspace: "ephemeral", Filesystem: "read_only",
		Network: "public_https", AllowedTools: []string{"web_search"},
		EgressAllowlist: []string{"chatgpt.com", "api.openai.com"}, ApprovalPolicy: "never",
	}}
	resolved, err := resolveExecutionPolicy(&request, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := request.ExecutionPolicy.EgressAllowlist, []string{"api.openai.com", "chatgpt.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical egress allowlist = %v, want %v", got, want)
	}

	changed := request
	policyCopy := *request.ExecutionPolicy
	policyCopy.EgressAllowlist = []string{"chatgpt.com"}
	changed.ExecutionPolicy = &policyCopy
	changedResolved, err := resolveExecutionPolicy(&changed, "docker")
	if err != nil {
		t.Fatal(err)
	}
	if changedResolved.Hash == resolved.Hash {
		t.Fatalf("policy hash did not change with egress allowlist: %s", resolved.Hash)
	}

	denied := request
	deniedPolicy := *request.ExecutionPolicy
	deniedPolicy.EgressAllowlist = nil
	denied.ExecutionPolicy = &deniedPolicy
	if _, err := resolveExecutionPolicy(&denied, "docker"); err != nil {
		t.Fatalf("empty allowlist must be a valid deny-all policy: %v", err)
	}
	if denied.ExecutionPolicy.EgressAllowlist == nil || len(denied.ExecutionPolicy.EgressAllowlist) != 0 {
		t.Fatalf("deny-all allowlist was not canonicalized: %#v", denied.ExecutionPolicy.EgressAllowlist)
	}
}

func TestResolveExecutionPolicyRejectsNonExactOrUnownedEgressHosts(t *testing.T) {
	for _, hosts := range [][]string{
		{"*.openai.com"}, {".openai.com"}, {"https://api.openai.com"},
		{"api.openai.com:443"}, {"127.0.0.1"}, {"example.com"},
		{"api.anthropic.com"}, {"API.OPENAI.COM"},
	} {
		request := SessionRequest{Agent: "codex", ExecutionPolicy: &ExecutionPolicy{
			Version: ExecutionPolicyVersion, Workspace: "ephemeral", Filesystem: "read_only",
			Network: "public_https", EgressAllowlist: hosts, ApprovalPolicy: "never",
		}}
		if _, err := resolveExecutionPolicy(&request, "docker"); err == nil || !strings.Contains(err.Error(), "execution_policy_unsupported") {
			t.Fatalf("egress hosts %v error = %v, want execution_policy_unsupported", hosts, err)
		}
	}
}

func TestResolveExecutionPolicyRejectsUnsupportedOrWidenedRequests(t *testing.T) {
	base := func() SessionRequest {
		return SessionRequest{Agent: "codex", Prompt: "research", ExecutionPolicy: &ExecutionPolicy{
			Version: ExecutionPolicyVersion, Workspace: "ephemeral", Filesystem: "read_only",
			Network: "public_https", AllowedTools: []string{"web_search"}, ApprovalPolicy: "never",
		}}
	}
	for _, test := range []struct {
		name    string
		runtime string
		mutate  func(*SessionRequest)
	}{
		{name: "local runtime", runtime: "local", mutate: func(*SessionRequest) {}},
		{name: "unknown tool", runtime: "docker", mutate: func(request *SessionRequest) { request.ExecutionPolicy.AllowedTools = []string{"host_shell"} }},
		{name: "host mount", runtime: "docker", mutate: func(request *SessionRequest) { request.WorkDir = "/private/project" }},
		{name: "mcp server", runtime: "docker", mutate: func(request *SessionRequest) {
			request.MCPServers = []MCPServer{{Name: "memory", Type: "stdio", Cmd: []string{"memory"}}}
		}},
		{name: "provider tool widening", runtime: "docker", mutate: func(request *SessionRequest) { request.Claude = &ClaudeConfig{AllowedTools: []string{"Bash"}} }},
		{name: "provider config", runtime: "docker", mutate: func(request *SessionRequest) {
			request.Codex = &CodexConfig{ConfigTOML: map[string]any{"mcp_servers": map[string]any{"ambient": true}}}
		}},
		{name: "lifecycle hook", runtime: "docker", mutate: func(request *SessionRequest) { request.Lifecycle = &LifecycleConfig{PreInit: "steer.sh"} }},
		{name: "approval widening", runtime: "docker", mutate: func(request *SessionRequest) { request.ExecutionPolicy.ApprovalPolicy = "on_request" }},
		{name: "retention widening", runtime: "docker", mutate: func(request *SessionRequest) { request.ExecutionPolicy.WorkspaceRetention = "forever" }},
		{name: "claude private context", runtime: "docker", mutate: func(request *SessionRequest) { request.Claude = &ClaudeConfig{ClaudeMD: "private"} }},
		{name: "claude credential path", runtime: "docker", mutate: func(request *SessionRequest) {
			request.Claude = &ClaudeConfig{CredentialsPath: "/private/credentials.json"}
		}},
		{name: "claude memory", runtime: "docker", mutate: func(request *SessionRequest) { request.Claude = &ClaudeConfig{MemoryPath: "/private/memory"} }},
		{name: "codex private context", runtime: "docker", mutate: func(request *SessionRequest) { request.Codex = &CodexConfig{Instructions: "private"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base()
			test.mutate(&request)
			if _, err := resolveExecutionPolicy(&request, test.runtime); err == nil || !strings.Contains(err.Error(), "execution_policy_unsupported") {
				t.Fatalf("resolve policy error = %v, want execution_policy_unsupported", err)
			}
		})
	}
}

func TestHTTPRejectsUnsupportedExecutionPolicyBeforeSpawn(t *testing.T) {
	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	counter := &atomic.Int32{}
	server := newV1SessionTestServer(t, store, counter)
	response := postV1Session(t, server.URL, map[string]any{
		"idempotency_key": "unsupported-policy", "agent": "sleep-test", "runtime": "test", "prompt": "never run",
		"execution_policy": map[string]any{
			"version": ExecutionPolicyVersion, "workspace": "ephemeral", "filesystem": "read_only",
			"network": "public_https", "allowed_tools": []string{"web_search"}, "approval_policy": "never",
		},
	})
	defer response.Body.Close()
	if response.StatusCode != 400 || counter.Load() != 0 {
		t.Fatalf("unsupported policy status=%d spawn count=%d", response.StatusCode, counter.Load())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "execution_policy_unsupported" {
		t.Fatalf("error code = %q", envelope.Error.Code)
	}
	if sessions, err := store.ListSessions(context.Background()); err != nil || len(sessions) != 0 {
		t.Fatalf("sessions after rejected policy = %v err=%v", sessions, err)
	}
}
