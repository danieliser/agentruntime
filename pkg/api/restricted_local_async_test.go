package api

import (
	"context"
	"encoding/json"
	"github.com/danieliser/agentruntime/internal/testutil"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
	"github.com/gin-gonic/gin"
)

func TestRestrictedLocalPendingQualificationDoesNotBlockServerOrAdmission(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "probe-started")
	binary := filepath.Join(root, "codex")
	// Only a sandbox probe stalls; all provider behavior is simulated.
	script := "#!/bin/sh\nif [ \"$1\" = sandbox ]; then /usr/bin/touch '" + marker + "'; /bin/sleep 30; exit 1; fi\n" + testutil.CodexQualificationScript("exit 1")
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{DataDir: root, LogDir: filepath.Join(root, "logs")})
	ready := make(chan struct{}, 1)
	s.router.GET("/test-ready", func(c *gin.Context) { ready <- struct{}{}; c.Status(http.StatusOK) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.serve(listener) }()
	t.Cleanup(func() {
		listener.Close() // Unblock Serve even if the assertion fails before readiness.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server/qualification workers did not stop")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	client := &http.Client{Timeout: 500 * time.Millisecond}
	base := "http://" + listener.Addr().String()
	response, err := client.Get(base + "/test-ready")
	if err != nil {
		t.Fatal("listening server blocked on qualification", err)
	}
	response.Body.Close()
	<-ready // Publish server initialization before shutdown inspects it.
	if goruntime.GOOS != "darwin" {
		// Unsupported platforms never launch a local qualification worker.
		return
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background measurement never started")
		}
		time.Sleep(time.Millisecond)
	}
	response, err = client.Get(base + "/api/v1/capabilities")
	if err != nil {
		t.Fatal("capabilities blocked on pending probe", err)
	}
	defer response.Body.Close()
	var envelope struct {
		Data struct {
			Permissions struct {
				Qualification runtime.RestrictedLocalQualification `json:"restricted_local_qualification"`
				Agents        []string                             `json:"restricted_local_agents"`
				Runtimes      []string                             `json:"restricted_runtimes"`
			} `json:"provider_permissions"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	permissions := envelope.Data.Permissions
	if permissions.Qualification.State != "pending" || permissions.Qualification.Qualified || len(permissions.Agents) != 0 || strings.Contains(strings.Join(permissions.Runtimes, ","), "local") {
		t.Fatal("pending qualification advertised local authority", permissions)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = s.SpawnSession(ctx, SessionRequest{Agent: "codex", Runtime: "local", WorkDir: t.TempDir(), ProviderPermissions: &ProviderPermissions{Mode: "restricted", ApprovalPolicy: "on-request"}})
	if !durable.IsCode(err, durable.CodeExecutionPolicyUnsupported) || time.Since(started) > 500*time.Millisecond {
		t.Fatal("pending admission blocked or was accepted", err)
	}
	// Serve continues handling ordinary health requests while the probe is stalled.
	response, err = client.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Error("health unavailable during pending qualification")
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if ctx.Err() != nil {
		t.Fatal("admission consumed its deadline")
	}
}
