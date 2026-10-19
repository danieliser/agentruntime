package api

import (
	"context"
	"encoding/json"
	"github.com/danieliser/agentruntime/internal/testutil"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
)

func TestRestrictedLocalValidationMatrix(t *testing.T) {
	qualifiedCodexFixture(t)
	for _, runtimeName := range []string{"local", "docker", "local-pipe", "remote"} {
		for _, provider := range []string{"claude", "codex", "grok", "cursor", "autojack"} {
			for _, mode := range []string{"restricted", "unrestricted"} {
				for _, approval := range []string{"on-request", "never"} {
					t.Run(runtimeName+"/"+provider+"/"+mode+"/"+approval, func(t *testing.T) {
						req := SessionRequest{Agent: provider, WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: mode, ApprovalPolicy: approval}}
						err := validateProviderPermissions(&req, runtimeName)
						want := (provider == "claude" || provider == "codex") && ((mode == "unrestricted" && approval == "never") || (mode == "restricted" && (runtimeName == "docker" || (runtimeName == "local" && provider == "codex" && goruntime.GOOS == "darwin"))))
						if (err == nil) != want {
							t.Fatalf("accepted=%v want=%v err=%v", err == nil, want, err)
						}
						if err != nil && !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
							t.Fatal(err)
						}
						if want && mode == "restricted" && (req.Context != "clean" || req.AutoDiscover != false) {
							t.Fatal("ambient discovery retained")
						}
					})
				}
			}
		}
	}
}

func TestRestrictedLocalCommandsAndProfiles(t *testing.T) {
	qualifiedCodexFixture(t)
	if goruntime.GOOS != "darwin" {
		t.Skip("only macOS is qualified")
	}
	for _, provider := range []string{"codex"} {
		for _, approval := range []string{"on-request", "never"} {
			t.Run(provider+"/"+approval, func(t *testing.T) {
				req := SessionRequest{Agent: provider, Runtime: "local", WorkDir: t.TempDir(), Fast: true, ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: approval}}
				if err := validateProviderPermissions(&req, "local"); err != nil {
					t.Fatal(err)
				}
				result, err := resolveNativeExecution(req, agent.DefaultRegistry().Get(provider), req.WorkDir, "")
				if err != nil {
					t.Fatal(err)
				}
				joined := strings.Join(result.Command, " ")
				for _, bad := range []string{"dangerously", "bypassPermissions", "danger-full-access", "--full-auto"} {
					if strings.Contains(joined, bad) {
						t.Fatalf("unsafe command: %s", joined)
					}
				}
				expected := []string{`default_permissions="agentd-local-v1"`, `approval_policy="` + approval + `"`}
				profile := "codex-read-confined-v1"
				if provider == "claude" {
					mode := "default"
					if approval == "never" {
						mode = "dontAsk"
					}
					expected = []string{"--permission-mode " + mode, "--permission-prompt-tool stdio", "--setting-sources ", "--strict-mcp-config", "--safe-mode", `"disableAllHooks":true`}
					profile = "claude-permission-prompts"
				}
				for _, flag := range expected {
					if !strings.Contains(joined, flag) {
						t.Errorf("missing %q: %s", flag, joined)
					}
				}
				if got := requestSandboxProfile("local", true, req); got != profile {
					t.Errorf("profile %q want %q", got, profile)
				}
			})
		}
	}
}

func TestRestrictedLocalRejectsOverrides(t *testing.T) {
	qualifiedCodexFixture(t)
	tests := map[string]func(*SessionRequest){
		"missing workspace": func(r *SessionRequest) { r.WorkDir = "" },
		"settings": func(r *SessionRequest) {
			r.Claude = &ClaudeConfig{SettingsJSON: map[string]any{"disableAllHooks": false}}
		},
		"allowed tools": func(r *SessionRequest) { r.Claude = &ClaudeConfig{AllowedTools: []string{"Bash"}} },
		"mcp json": func(r *SessionRequest) {
			r.Claude = &ClaudeConfig{McpJSON: map[string]any{"mcpServers": map[string]any{}}}
		},
		"codex config": func(r *SessionRequest) {
			r.Codex = &CodexConfig{ConfigTOML: map[string]any{"sandbox_mode": "danger-full-access"}}
		},
		"team":         func(r *SessionRequest) { r.Team = &TeamConfig{} },
		"lifecycle":    func(r *SessionRequest) { r.Lifecycle = &LifecycleConfig{} },
		"home":         func(r *SessionRequest) { r.Env = map[string]string{"HOME": "/ambient"} },
		"config env":   func(r *SessionRequest) { r.Env = map[string]string{"CODEX_HOME": "/ambient"} },
		"node options": func(r *SessionRequest) { r.Env = map[string]string{"NODE_OPTIONS": "--require=/ambient/hook.js"} },
		"bash env":     func(r *SessionRequest) { r.Env = map[string]string{"BASH_ENV": "/ambient/hook"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			req := SessionRequest{Agent: "codex", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
			mutate(&req)
			if err := validateProviderPermissions(&req, "local"); !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
				t.Fatalf("override accepted or wrong code: %v", err)
			}
		})
	}
}

func TestRestrictedLocalCapabilityRuntimeList(t *testing.T) {
	qualifiedCodexFixture(t)
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{LogDir: filepath.Join(t.TempDir(), "logs")})
	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
	var body struct {
		Data struct {
			ProviderPermissions struct {
				Modes                 []string                             `json:"modes"`
				RestrictedRuntimes    []string                             `json:"restricted_runtimes"`
				RestrictedLocalAgents []string                             `json:"restricted_local_agents"`
				Qualification         runtime.RestrictedLocalQualification `json:"restricted_local_qualification"`
			} `json:"provider_permissions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantRuntimes := []string{"docker"}
	wantAgents := []string{}
	if goruntime.GOOS == "darwin" {
		wantRuntimes = append(wantRuntimes, "local")
		wantAgents = append(wantAgents, "codex")
	}
	if goruntime.GOOS == "darwin" {
		q := body.Data.ProviderPermissions.Qualification
		if !q.Qualified || q.Agent != "codex" || q.Version != "codex-cli fixture-v1" || q.CheckedAt.IsZero() || q.State != "qualified" {
			t.Fatal("missing measured qualification", q)
		}
	}
	if !slices.Equal(body.Data.ProviderPermissions.RestrictedLocalAgents, wantAgents) {
		t.Fatal("incorrect qualified agents")
	}
	if !slices.Equal(body.Data.ProviderPermissions.RestrictedRuntimes, wantRuntimes) {
		t.Fatalf("runtimes=%v", body.Data.ProviderPermissions.RestrictedRuntimes)
	}
	if !slices.Equal(body.Data.ProviderPermissions.Modes, []string{"restricted_docker", "unrestricted"}) {
		t.Fatal("existing permission modes lost")
	}
}

func TestRestrictedLocalNeverUsesLegacySpawnFallback(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{LogDir: filepath.Join(t.TempDir(), "logs")})
	_, err := server.SpawnSession(context.Background(), SessionRequest{Agent: "claude", Runtime: "local", WorkDir: t.TempDir(), Prompt: "fixture", ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}})
	if !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
		t.Fatalf("legacy fallback was attempted: %v", err)
	}
}

func TestRestrictedLocalRejectsMounts(t *testing.T) {
	req := SessionRequest{Agent: "claude", WorkDir: t.TempDir(), Mounts: []Mount{{Host: "/unmounted", Container: "/unused"}}, Container: &ContainerConfig{Network: "host", SecurityOpt: []string{"seccomp=unconfined"}}, ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "never"}}
	if err := validateProviderPermissions(&req, "local"); !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
		t.Fatal("mount was accepted", err)
	}
}

func qualifiedCodexFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(testutil.CodexQualificationScript("exit 1")), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runtime.InitializeRestrictedLocalQualification(t.TempDir())
	awaitLocalQualification(t)
}

func TestRestrictedLocalCapabilityOmitsUnqualifiedBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{LogDir: filepath.Join(t.TempDir(), "logs")})
	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
	var response struct {
		Data struct {
			Permissions struct {
				Agents   []string `json:"restricted_local_agents"`
				Runtimes []string `json:"restricted_runtimes"`
			} `json:"provider_permissions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Permissions.Agents) != 0 || slices.Contains(response.Data.Permissions.Runtimes, "local") {
		t.Fatal("unqualified local advertised")
	}
}

func TestRestrictedLocalVersionOutputAloneCannotQualify(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nprintf 'codex-cli fixture-v2\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	runtime.InitializeRestrictedLocalQualification(t.TempDir())
	awaitLocalQualification(t)
	if len(runtime.QualifiedRestrictedLocalAgents()) != 0 {
		t.Fatal("version-only fake passed confinement/bootstrap qualification")
	}
	req := SessionRequest{Agent: "codex", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}}
	if err := validateProviderPermissions(&req, "local"); !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) {
		t.Fatal("unqualified binary admitted", err)
	}
}

func awaitLocalQualification(t *testing.T) {
	t.Helper()
	t.Cleanup(runtime.StopRestrictedLocalQualification)
	deadline := time.Now().Add(5 * time.Second)
	for runtime.RestrictedLocalQualificationStatus().State == "pending" {
		if time.Now().After(deadline) {
			t.Fatal("fixture qualification did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}
