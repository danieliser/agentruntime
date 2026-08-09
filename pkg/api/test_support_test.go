package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type fakeRuntime struct {
	rt             *runtime.LocalRuntime
	sessionDirRoot string
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	return &fakeRuntime{rt: runtime.NewLocalRuntime(), sessionDirRoot: t.TempDir()}
}

func (fake *fakeRuntime) Name() string { return "test" }

func (fake *fakeRuntime) Spawn(ctx context.Context, config runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	if config.SessionDir != nil {
		dir := filepath.Join(fake.sessionDirRoot, config.SessionID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		*config.SessionDir = dir
	}
	return fake.rt.Spawn(ctx, config)
}

func (*fakeRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) { return nil, nil }
func (*fakeRuntime) Cleanup(context.Context) error                            { return nil }

type echoAgent struct{}

func (*echoAgent) Name() string { return "echo-test" }
func (*echoAgent) BuildCmd(prompt string, _ agent.AgentConfig) ([]string, error) {
	return []string{"/bin/sh", "-c", "/bin/echo \"$1\" && sleep 2", "--", prompt}, nil
}
func (*echoAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }

type catAgent struct{}

func (*catAgent) Name() string { return "cat-test" }
func (*catAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	return []string{"cat"}, nil
}
func (*catAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }

type sleepAgent struct{}

func (*sleepAgent) Name() string { return "sleep-test" }
func (*sleepAgent) BuildCmd(string, agent.AgentConfig) ([]string, error) {
	return []string{"sleep", "60"}, nil
}
func (*sleepAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }

type captureAgent struct {
	name string
	mu   sync.Mutex
	cfg  agent.AgentConfig
}

func (agent *captureAgent) Name() string { return agent.name }
func (agent *captureAgent) BuildCmd(prompt string, config agent.AgentConfig) ([]string, error) {
	agent.mu.Lock()
	agent.cfg = config
	agent.mu.Unlock()
	return []string{"/bin/echo", prompt}, nil
}
func (*captureAgent) ParseOutput([]byte) (*agent.AgentResult, bool) { return nil, false }
func (agent *captureAgent) LastConfig() agent.AgentConfig {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.cfg
}

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	registry := agent.NewRegistry()
	registry.Register(&echoAgent{})
	registry.Register(&catAgent{})
	registry.Register(&sleepAgent{})
	root := t.TempDir()
	server := NewServer(session.NewManager(), newFakeRuntime(t), registry, ServerConfig{
		DataDir: root,
		LogDir:  filepath.Join(root, "logs"),
	})
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	return httpServer, server
}

func newConfiguredTestServer(t *testing.T, registry *agent.Registry, config ServerConfig) (*httptest.Server, *Server) {
	t.Helper()
	server := NewServer(session.NewManager(), newFakeRuntime(t), registry, config)
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	return httpServer, server
}

func post(t *testing.T, server *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	response, err := http.Post(server.URL+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return response
}

func get(t *testing.T, server *httptest.Server, path string) *http.Response {
	t.Helper()
	response, err := http.Get(server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return response
}

func decodeJSON(t *testing.T, reader io.Reader, target any) {
	t.Helper()
	if err := json.NewDecoder(reader).Decode(target); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create JSON directory: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write JSON: %v", err)
	}
}

func TestHealth(t *testing.T) {
	server, _ := newTestServer(t)
	response := get(t, server, "/health")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
}
