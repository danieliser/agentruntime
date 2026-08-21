package api

import (
	"context"
	"testing"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
	runtimepkg "github.com/danieliser/agentruntime/pkg/runtime"
)

func TestResolveContainerLeaseMaintainsNativeDockerConversation(t *testing.T) {
	request := SessionRequest{
		Agent: "codex",
		ContainerLease: &apischema.ContainerLease{
			Mode: "maintain", IdleTTL: "10m", PortableResume: true,
		},
	}
	resolved, err := resolveContainerLease(request, "docker", true)
	if err != nil {
		t.Fatalf("resolve maintained lease: %v", err)
	}
	if !resolved.Maintain || resolved.IdleTTL != 10*time.Minute || !resolved.PortableResume {
		t.Fatalf("resolved lease = %+v", resolved)
	}
}

func TestResolveContainerLeaseRejectsAuthorityWidening(t *testing.T) {
	tests := []struct {
		name    string
		request SessionRequest
		runtime string
		native  bool
	}{
		{
			name: "local",
			request: SessionRequest{Agent: "codex", ContainerLease: &apischema.ContainerLease{
				Mode: "maintain", IdleTTL: "10m",
			}},
			runtime: "local", native: true,
		},
		{
			name: "restricted",
			request: SessionRequest{Agent: "codex", ExecutionPolicy: &ExecutionPolicy{}, ContainerLease: &apischema.ContainerLease{
				Mode: "maintain", IdleTTL: "10m",
			}},
			runtime: "docker", native: true,
		},
		{
			name: "compatibility transport",
			request: SessionRequest{Agent: "custom", ContainerLease: &apischema.ContainerLease{
				Mode: "maintain", IdleTTL: "10m",
			}},
			runtime: "docker", native: false,
		},
		{
			name: "missing ttl",
			request: SessionRequest{Agent: "codex", ContainerLease: &apischema.ContainerLease{
				Mode: "maintain",
			}},
			runtime: "docker", native: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolveContainerLease(test.request, test.runtime, test.native); err == nil {
				t.Fatal("expected lease validation error")
			}
		})
	}
}

func TestNativeContainerLeaseExpiresOnlyWhileIdle(t *testing.T) {
	expired := make(chan struct{}, 1)
	lease := newNativeContainerLease(30*time.Millisecond, func() { expired <- struct{}{} })
	defer lease.Close()

	lease.TurnCompleted()
	if err := lease.BeginInput("prompt"); err != nil {
		t.Fatalf("begin warm prompt: %v", err)
	}
	select {
	case <-expired:
		t.Fatal("lease expired while a follow-up turn was active")
	case <-time.After(60 * time.Millisecond):
	}

	lease.TurnCompleted()
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("idle lease did not expire")
	}
}

func TestNativeContainerLeasePinsIdleTransportDuringSnapshot(t *testing.T) {
	lease := newNativeContainerLease(time.Hour, nil)
	lease.TurnCompleted()
	release, err := lease.BeginSnapshot()
	if err != nil {
		t.Fatalf("begin snapshot: %v", err)
	}
	if lease.IsIdle() {
		t.Fatal("snapshotting lease reported input-ready idle state")
	}
	if err := lease.BeginInput(string(nativeprotocol.InputPrompt)); err == nil {
		t.Fatal("prompt was accepted while snapshotting")
	}
	release()
	if !lease.IsIdle() {
		t.Fatal("snapshot release did not restore idle state")
	}
}

func TestNativeContainerLeaseAppliesTimeoutToEveryTurn(t *testing.T) {
	timedOut := make(chan struct{}, 2)
	lease := newNativeContainerLease(time.Hour, nil)
	lease.ConfigureTurnTimeout(20*time.Millisecond, time.Now(), func() { timedOut <- struct{}{} })
	select {
	case <-timedOut:
	case <-time.After(time.Second):
		t.Fatal("initial maintained turn did not time out")
	}

	lease = newNativeContainerLease(time.Hour, nil)
	lease.ConfigureTurnTimeout(20*time.Millisecond, time.Now(), func() { timedOut <- struct{}{} })
	lease.TurnCompleted()
	if err := lease.BeginInput(string(nativeprotocol.InputPrompt)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-timedOut:
	case <-time.After(time.Second):
		t.Fatal("follow-up maintained turn did not time out")
	}
}

type terminalContainerRuntime struct {
	released chan string
}

type stoppedContainerPruningRuntime struct {
	called chan time.Time
}

func (rt *stoppedContainerPruningRuntime) Name() string { return "docker" }
func (rt *stoppedContainerPruningRuntime) Spawn(context.Context, runtimepkg.SpawnConfig) (runtimepkg.ProcessHandle, error) {
	return nil, nil
}
func (rt *stoppedContainerPruningRuntime) Recover(context.Context) ([]runtimepkg.ProcessHandle, error) {
	return nil, nil
}
func (rt *stoppedContainerPruningRuntime) Cleanup(context.Context) error { return nil }
func (rt *stoppedContainerPruningRuntime) PruneStoppedContainers(_ context.Context, before time.Time) (int, error) {
	rt.called <- before
	return 3, nil
}

func TestServerPrunesStoppedContainersAfterBoundedGrace(t *testing.T) {
	rt := &stoppedContainerPruningRuntime{called: make(chan time.Time, 1)}
	server := &Server{runtimes: map[string]runtimepkg.Runtime{"docker": rt}, runtime: rt}
	now := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	if removed := server.pruneStoppedContainers(context.Background(), now); removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	select {
	case before := <-rt.called:
		want := now.Add(-defaultStoppedContainerGrace)
		if !before.Equal(want) {
			t.Fatalf("prune cutoff = %s, want %s", before, want)
		}
	default:
		t.Fatal("Docker pruner was not called")
	}
}

func (rt *terminalContainerRuntime) Name() string { return "docker" }
func (rt *terminalContainerRuntime) Spawn(context.Context, runtimepkg.SpawnConfig) (runtimepkg.ProcessHandle, error) {
	return nil, nil
}
func (rt *terminalContainerRuntime) Recover(context.Context) ([]runtimepkg.ProcessHandle, error) {
	return nil, nil
}
func (rt *terminalContainerRuntime) Cleanup(context.Context) error { return nil }
func (rt *terminalContainerRuntime) ReleaseContainer(_ context.Context, sessionID string) error {
	rt.released <- sessionID
	return nil
}

func TestReleaseTerminalContainerCleansOrdinaryDockerSession(t *testing.T) {
	rt := &terminalContainerRuntime{released: make(chan string, 1)}
	server := &Server{runtimes: map[string]runtimepkg.Runtime{"docker": rt}, runtime: rt}
	server.releaseTerminalContainer(durable.Session{ID: "terminal-container", Runtime: "docker", State: durable.StateCompleted})
	select {
	case got := <-rt.released:
		if got != "terminal-container" {
			t.Fatalf("released session = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal Docker container was not released")
	}
}
