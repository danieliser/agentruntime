package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danieliser/agentruntime/pkg/agent"
	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func TestValidateContextMode(t *testing.T) {
	cases := []struct {
		name        string
		req         SessionRequest
		runtimeName string
		wantErr     bool
	}{
		{"default mode", SessionRequest{Agent: "claude"}, "local", false},
		{"clean local", SessionRequest{Agent: "claude", Context: "clean"}, "local", false},
		{"clean docker claude", SessionRequest{Agent: "claude", Context: "clean"}, "docker", false},
		{"unknown mode", SessionRequest{Agent: "claude", Context: "sparkling"}, "local", true},
		{"clean local-pipe", SessionRequest{Agent: "claude", Context: "clean"}, "local-pipe", true},
		{"clean docker grok", SessionRequest{Agent: "grok", Context: "clean"}, "docker", true},
		{"clean docker cursor", SessionRequest{Agent: "cursor", Context: "clean"}, "docker", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateContextMode(&tc.req, tc.runtimeName)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateContextMode() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateContextMode_CleanForcesAutoDiscoverOff(t *testing.T) {
	req := SessionRequest{Agent: "claude", Context: "clean", AutoDiscover: true}
	if err := validateContextMode(&req, "local"); err != nil {
		t.Fatalf("validateContextMode() error = %v", err)
	}
	if req.AutoDiscover != false {
		t.Fatalf("AutoDiscover = %#v, want false in clean context", req.AutoDiscover)
	}
}

// TestSpawnSession_CleanContextMetadata covers the chat/internal spawn path:
// it must validate context and populate contamination like the HTTP path.
func TestSpawnSession_CleanContextMetadata(t *testing.T) {
	reg := agent.NewRegistry()
	// Register under the name "grok" so contamination metadata resolves,
	// while the spawned process is plain /bin/echo.
	reg.Register(&captureAgent{name: "grok"})
	dataDir := t.TempDir()
	_, srv := newConfiguredTestServer(t, reg, ServerConfig{
		DataDir: dataDir,
		LogDir:  filepath.Join(dataDir, "logs"),
	})

	sess, err := srv.SpawnSession(context.Background(), SessionRequest{
		Agent:   "grok",
		Prompt:  "probe",
		Context: "clean",
	})
	if err != nil {
		t.Fatalf("SpawnSession() error = %v", err)
	}
	t.Cleanup(func() {
		_ = sess.Kill()
		srv.sessions.Remove(sess.ID)
	})

	want := agent.KnownContamination("grok", true)
	snap := sess.Snapshot()
	if len(snap.Contamination) != len(want) {
		t.Fatalf("Contamination = %v, want %v", snap.Contamination, want)
	}
	for i := range want {
		if snap.Contamination[i] != want[i] {
			t.Fatalf("Contamination = %v, want %v", snap.Contamination, want)
		}
	}
}

func TestSpawnSession_RejectsUnknownContext(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(&captureAgent{name: "grok"})
	dataDir := t.TempDir()
	_, srv := newConfiguredTestServer(t, reg, ServerConfig{
		DataDir: dataDir,
		LogDir:  filepath.Join(dataDir, "logs"),
	})

	if _, err := srv.SpawnSession(context.Background(), SessionRequest{
		Agent:   "grok",
		Prompt:  "probe",
		Context: "sparkling",
	}); err == nil {
		t.Fatal("SpawnSession() must reject unknown context modes")
	}
	if got := len(srv.sessions.List()); got != 0 {
		t.Fatalf("rejected spawn must not leave sessions behind, got %d", got)
	}
}

func TestChatConfigConversions_PreserveContext(t *testing.T) {
	api := apischema.ChatAPIConfig{Agent: "claude", Context: "clean"}
	internal := chatConfigFromAPI(api)
	if internal.Context != "clean" {
		t.Fatalf("chatConfigFromAPI dropped Context: %#v", internal)
	}
	back := chatConfigToAPI(internal)
	if back.Context != "clean" {
		t.Fatalf("chatConfigToAPI dropped Context: %#v", back)
	}
}
