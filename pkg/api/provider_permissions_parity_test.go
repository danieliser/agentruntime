package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/materialize"
)

// Run against v2.4.0 and the working tree with the same synthetic fixtures.
// The opt-in output is compared byte-for-byte; no provider is executed.
func TestProviderPermissionsV240Parity(t *testing.T) {
	output := os.Getenv("AGENTD_PERMISSION_PARITY_FILE")
	if output == "" {
		t.Skip("opt-in baseline comparison")
	}
	root := filepath.Clean(t.TempDir())
	home := filepath.Join(root, "host-home")
	work := filepath.Join(root, "project")
	for _, path := range []string{home, work} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	cases := map[string]json.RawMessage{}
	for _, provider := range []string{"claude", "codex"} {
		for _, runtimeName := range []string{"docker", "local"} {
			for _, mode := range []string{"restricted", "unrestricted", "omitted"} {
				if runtimeName == "local" && mode == "restricted" {
					continue
				}
				for _, approval := range []string{"on-request", "never"} {
					if mode != "restricted" && approval != "never" {
						continue
					}
					name := provider + "/" + runtimeName + "/" + mode + "/" + approval
					req := SessionRequest{Agent: provider, Runtime: runtimeName, WorkDir: work, Model: "fixture-model", Effort: "high", Fast: true}
					if mode != "omitted" {
						req.ProviderPermissions = &ProviderPermissions{Mode: mode, ApprovalPolicy: approval}
					}
					if err := validateProviderPermissions(&req, runtimeName); err != nil {
						t.Fatal(name, err)
					}
					providerWork := work
					if runtimeName == "docker" {
						providerWork = "/workspace"
					}
					resolved, err := resolveNativeExecution(req, agent.DefaultRegistry().Get(provider), providerWork, "")
					if err != nil {
						t.Fatal(err)
					}
					snapshot := map[string]any{"command": resolved.Command, "policy": nativePolicy(req), "sandbox_profile": requestSandboxProfile(runtimeName, true, req)}
					if runtimeName == "docker" {
						data := filepath.Join(root, strings.ReplaceAll(name, "/", "-"))
						if err := os.MkdirAll(data, 0700); err != nil {
							t.Fatal(err)
						}
						result, err := materialize.Materialize(&req, "fixture", data)
						if err != nil {
							t.Fatal(name, err)
						}
						files := map[string]string{}
						err = filepath.Walk(result.SessionDir, func(path string, info os.FileInfo, err error) error {
							if err != nil {
								return err
							}
							if info.IsDir() {
								return nil
							}
							contents, err := os.ReadFile(path)
							if err != nil {
								return err
							}
							rel, err := filepath.Rel(result.SessionDir, path)
							if err != nil {
								return err
							}
							files[rel] = string(contents)
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
						snapshot["files"] = files
						snapshot["mounts"] = result.Mounts
					}
					manifest, _, _, err := durableRequestManifest(req, runtimeName)
					if err != nil {
						t.Fatal(name, err)
					}
					snapshot["manifest"] = json.RawMessage(manifest)
					encoded, err := json.Marshal(snapshot)
					if err != nil {
						t.Fatal(err)
					}
					// JSON-encoded paths need replacement after escaping, for portable receipts.
					normalized := strings.ReplaceAll(string(encoded), root, "<ROOT>")
					cases[name] = json.RawMessage(normalized)
				}
			}
		}
	}
	encoded, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("snapshots", len(cases))
}
