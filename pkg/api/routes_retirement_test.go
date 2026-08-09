package api

import (
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestUnversionedSessionRoutesAreRetired(t *testing.T) {
	root := t.TempDir()
	server := NewServer(session.NewManager(), runtime.NewLocalRuntime(), agent.DefaultRegistry(), ServerConfig{
		DataDir: root,
		LogDir:  filepath.Join(root, "logs"),
	})
	legacy := map[string]bool{
		"POST /sessions":         true,
		"GET /sessions":          true,
		"GET /sessions/history":  true,
		"GET /sessions/:id":      true,
		"GET /sessions/:id/info": true,
		"GET /sessions/:id/logs": true,
		"GET /sessions/:id/log":  true,
		"DELETE /sessions/:id":   true,
		"GET /ws/sessions/:id":   true,
	}
	for _, route := range server.router.Routes() {
		if legacy[route.Method+" "+route.Path] {
			t.Errorf("legacy route remains registered: %s %s", route.Method, route.Path)
		}
	}
}
