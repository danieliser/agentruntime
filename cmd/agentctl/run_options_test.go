package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danieliser/agentruntime/pkg/api"
)

func TestRunMaintainedRestrictedDocker(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.SessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Context != "clean" {
			t.Errorf("context: %q", req.Context)
		}
		if req.ContainerLease == nil || req.ContainerLease.Mode != "maintain" || req.ContainerLease.IdleTTL != "10m0s" || !req.ContainerLease.PortableResume {
			t.Errorf("lease: %+v", req.ContainerLease)
		}
		if req.ProviderPermissions == nil || req.ProviderPermissions.Mode != "restricted" || req.ProviderPermissions.ApprovalPolicy != "on-request" {
			t.Errorf("permissions: %+v", req.ProviderPermissions)
		}
		_, _ = w.Write([]byte(`{"data":{"session_id":"warm"}}`))
	}))
	defer ts.Close()
	ctx, _ := commandContext(ts, true)
	err := cmdRun(ctx, []string{"--provider", "codex", "--model", "M", "--cwd", "/repo", "--runtime", "docker", "--keep-warm", "10m", "--portable-resume", "--context", "clean", "--permissions", "restricted", "--approval-policy", "on-request", "hello"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsUnsupportedOptionsBeforeDispatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request dispatched") }))
	defer ts.Close()
	cases := [][]string{
		{"--keep-warm", "10m"}, {"--keep-warm", "-1s", "--runtime", "docker"}, {"--keep-warm", "25h", "--runtime", "docker"},
		{"--portable-resume"}, {"--permissions", "anything"},
		{"--runtime", "docker", "--permissions", "restricted", "--approval-policy", "untrusted"},
		{"--permissions", "unrestricted", "--approval-policy", "on-request"}, {"--approval-policy", "never"},
	}
	for _, opts := range cases {
		t.Run(stringsJoin(opts), func(t *testing.T) {
			ctx, _ := commandContext(ts, true)
			args := append([]string{"--provider", "claude", "--model", "M", "--cwd", "/repo"}, opts...)
			if err := cmdRun(ctx, append(args, "hello")); !errors.Is(err, ErrUsage) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
func stringsJoin(v []string) string { b, _ := json.Marshal(v); return string(b) }

func TestRunRestrictedLocal(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, approval := range []string{"on-request", "never"} {
			t.Run(provider+"/"+approval, func(t *testing.T) {
				called := make(chan struct{}, 1)
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called <- struct{}{}
					var req api.SessionRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.Runtime != "local" || req.Agent != provider || req.ProviderPermissions == nil || req.ProviderPermissions.Mode != "restricted" || req.ProviderPermissions.ApprovalPolicy != approval {
						t.Errorf("incorrect restricted local dispatch: %+v", req)
					}
					_, _ = w.Write([]byte(`{"data":{"session_id":"restricted-local"}}`))
				}))
				defer ts.Close()
				ctx, _ := commandContext(ts, true)
				if err := cmdRun(ctx, []string{"--provider", provider, "--model", "M", "--cwd", "/repo", "--runtime", "local", "--permissions", "restricted", "--approval-policy", approval, "fixture"}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-called:
				default:
					t.Fatal("restricted local request was not dispatched")
				}
			})
		}
	}
}
