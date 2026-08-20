package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/durable"
	durablesqlite "github.com/danieliser/agentruntime/pkg/durable/sqlite"
	"github.com/danieliser/agentruntime/pkg/eventstream"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

// TestDockerImageRemovalMakesReadinessAndAdmissionFailClosed proves the image
// gate against a real Docker daemon without deleting the developer's canonical
// image: it creates and then removes a temporary tag for the same image.
func TestDockerImageRemovalMakesReadinessAndAdmissionFailClosed(t *testing.T) {
	if os.Getenv("AGENTRUNTIME_DOCKER_INTEGRATION") != "1" {
		t.Skip("set AGENTRUNTIME_DOCKER_INTEGRATION=1 to run Docker qualification tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	temporaryImage := "agentruntime-agent:readiness-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if output, err := exec.Command("docker", "image", "tag", runtime.DefaultDockerImage, temporaryImage).CombinedOutput(); err != nil {
		t.Skipf("canonical runtime image is unavailable: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", temporaryImage).Run() })

	store, err := durablesqlite.Open(filepath.Join(t.TempDir(), "agentd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := agent.NewRegistry()
	registry.Register(&sleepAgent{})
	dockerRuntime := runtime.NewDockerRuntime(runtime.DockerConfig{Image: temporaryImage, DataDir: t.TempDir()})
	server := NewServer(session.NewManager(), dockerRuntime, registry, ServerConfig{
		LogDir: filepath.Join(t.TempDir(), "logs"), DurableStore: store, EventBroker: eventstream.New(store),
	})
	httpServer := httptest.NewServer(server.router)
	defer httpServer.Close()

	ready, err := http.Get(httpServer.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = ready.Body.Close()
	if ready.StatusCode != http.StatusOK {
		t.Fatalf("health before image removal = %d", ready.StatusCode)
	}
	if output, err := exec.Command("docker", "image", "rm", temporaryImage).CombinedOutput(); err != nil {
		t.Fatalf("remove temporary runtime image: %v: %s", err, output)
	}

	unready, err := http.Get(httpServer.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = unready.Body.Close()
	if unready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("health after image removal = %d, want 503", unready.StatusCode)
	}
	response := postV1Session(t, httpServer.URL, map[string]any{
		"idempotency_key": "missing-runtime-image", "agent": "sleep-test", "runtime": "docker", "prompt": "must not run",
	})
	defer response.Body.Close()
	var envelope struct {
		Error apiErrorEnvelope `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || envelope.Error.Code != durable.CodeRuntimeUnavailable {
		t.Fatalf("missing-image admission status=%d error=%+v", response.StatusCode, envelope.Error)
	}
	stored, err := store.ListSessions(context.Background())
	if err != nil || len(stored) != 0 {
		t.Fatalf("missing-image admission created durable sessions=%+v err=%v", stored, err)
	}
}
