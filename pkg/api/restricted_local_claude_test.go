package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/internal/testutil"
	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func qualifiedClaudeFixture(t *testing.T) {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	dir := t.TempDir()
	for name, script := range map[string]string{"claude": testutil.ClaudeQualificationScript("exit 1"), "codex": "#!/bin/sh\nexit 1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runtime.InitializeRestrictedLocalQualification(t.TempDir())
	t.Cleanup(runtime.StopRestrictedLocalQualification)
	deadline := time.Now().Add(15 * time.Second)
	for runtime.RestrictedLocalQualificationForAgent("claude").State == "pending" {
		if time.Now().After(deadline) {
			t.Fatal("Claude qualification timed out")
		}
		time.Sleep(time.Millisecond)
	}
	if !runtime.RestrictedLocalQualificationForAgent("claude").Qualified {
		t.Fatal("fixture unqualified")
	}
}

func TestRestrictedLocalClaudeAdmissionAndCapability(t *testing.T) {
	qualifiedClaudeFixture(t)
	req := SessionRequest{Agent: "claude", Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request", Tools: "file_only"}}
	if err := validateProviderPermissions(&req, "local"); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveNativeExecution(req, &agent.ClaudeAgent{}, req.WorkDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if policy := nativePolicy(req); policy.PermissionProfile != "" || policy.NetworkAccess {
		t.Fatal("Claude inherited Codex profile or network", policy)
	}
	if !slices.Contains(resolved.Command, "--restricted") {
		t.Fatal("not restricted")
	}
	if got := requestSandboxProfile("local", true, req); got != "claude-file-tools-only-v1" {
		t.Fatal(got)
	}
	for _, mutate := range []func(*SessionRequest){func(r *SessionRequest) { r.Claude = &ClaudeConfig{AllowedTools: []string{"Bash"}} }, func(r *SessionRequest) { r.Codex = &CodexConfig{} }, func(r *SessionRequest) { r.Skills = &SkillSet{} }, func(r *SessionRequest) { r.Trace = &TraceConfig{Plugin: "fixture"} }} {
		copy := req
		mutate(&copy)
		if err := validateProviderPermissions(&copy, "local"); !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
			t.Fatal("provider override accepted", err)
		}
	}
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{LogDir: filepath.Join(t.TempDir(), "logs")})
	rec := httptest.NewRecorder()
	server.router.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
	var body struct {
		Data struct {
			Permissions struct {
				Agents          []string                              `json:"restricted_local_agents"`
				FileToolsAgents []string                              `json:"restricted_local_file_tools_agents"`
				Runtimes        []string                              `json:"restricted_runtimes"`
				Qualification   runtime.RestrictedLocalQualifications `json:"restricted_local_qualification"`
			} `json:"provider_permissions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	p := body.Data.Permissions
	if p.Agents == nil || len(p.Agents) != 0 || !slices.Equal(p.FileToolsAgents, []string{"claude"}) || !slices.Contains(p.Runtimes, "local") || p.Qualification.Agents["claude"].Mode != "file-tools-only" || !p.Qualification.Agents["claude"].Qualified || p.Qualification.Agent != "codex" {
		t.Fatalf("capability mismatch %+v", p)
	}
	if len(runtime.QualifiedRestrictedLocalAgents()) != 0 {
		t.Fatal("file-only Claude advertised as a shell-qualified worker")
	}
	if !slices.Equal(runtime.QualifiedRestrictedLocalFileToolsAgents(), []string{"claude"}) {
		t.Fatal("file-only Claude missing from runtime capability")
	}
}

func TestRestrictedLocalClaudeRequiresFileOnlyOptIn(t *testing.T) {
	qualifiedClaudeFixture(t)
	for _, tools := range []string{"", "bash", "file-tools-only"} {
		req := SessionRequest{Agent: "claude", Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request", Tools: tools}}
		err := validateProviderPermissions(&req, "local")
		if !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) || !strings.Contains(err.Error(), "file_only") {
			t.Fatalf("tools=%q must require file_only: %v", tools, err)
		}
		if err := runtime.CheckRestrictedLocalProviderPermissions(req.Agent, tools); !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
			t.Fatal("runtime spawn gate accepted missing/invalid opt-in", tools, err)
		}
	}
	// The field must survive JSON request decoding, including durable request manifests.
	var req SessionRequest
	if err := json.Unmarshal([]byte(`{"agent":"claude","runtime":"local","provider_permissions":{"mode":"restricted","approval_policy":"never","tools":"file_only"}}`), &req); err != nil {
		t.Fatal(err)
	}
	req.WorkDir = t.TempDir()
	if err := validateProviderPermissions(&req, "local"); err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := durableRequestManifest(req, "local")
	if err != nil || !strings.Contains(string(manifest), `"tools":"file_only"`) {
		t.Fatal("file-only opt-in lost from persisted request", err)
	}
	runtime.StopRestrictedLocalQualification()
	if err := validateProviderPermissions(&req, "local"); !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
		t.Fatal("file-only opt-in authorized an unqualified worker", err)
	}
}

func TestRestrictedLocalCodexAdmissionErrorCompatibility(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	req := SessionRequest{Agent: "codex", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "never"}}
	err := validateProviderPermissions(&req, "local")
	if err == nil || err.Error() != "provider_permissions: execution_policy_unsupported: provider/version/platform has no qualified read-confined local sandbox" {
		t.Fatal("v2.4.0 admission error changed", err)
	}
}
