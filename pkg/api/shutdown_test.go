package api

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	runtimepkg "github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type shutdownHandle struct{ kills atomic.Int32 }

func (*shutdownHandle) Stdin() io.WriteCloser              { return nil }
func (*shutdownHandle) Stdout() io.ReadCloser              { return nil }
func (*shutdownHandle) Stderr() io.ReadCloser              { return nil }
func (*shutdownHandle) Wait() <-chan runtimepkg.ExitResult { return make(chan runtimepkg.ExitResult) }
func (handle *shutdownHandle) Kill() error                 { handle.kills.Add(1); return nil }
func (*shutdownHandle) PID() int                           { return 0 }
func (*shutdownHandle) RecoveryInfo() *runtimepkg.RecoveryInfo {
	return nil
}

type shutdownRuntime struct {
	name     string
	cleanups atomic.Int32
}

func (runtime *shutdownRuntime) Name() string { return runtime.name }
func (*shutdownRuntime) Spawn(context.Context, runtimepkg.SpawnConfig) (runtimepkg.ProcessHandle, error) {
	return nil, errors.New("unexpected spawn")
}
func (*shutdownRuntime) Recover(context.Context) ([]runtimepkg.ProcessHandle, error) {
	return nil, nil
}
func (runtime *shutdownRuntime) Cleanup(context.Context) error {
	runtime.cleanups.Add(1)
	return nil
}

func TestGracefulShutdownStopsLocalSessionsAndClosesAdmission(t *testing.T) {
	manager := session.NewManager()
	handles := make([]*shutdownHandle, 10)
	for index := range handles {
		handles[index] = &shutdownHandle{}
		sess := session.NewSession("task", "claude", "local")
		sess.SetRunning(handles[index])
		if err := manager.Add(sess); err != nil {
			t.Fatalf("add local session: %v", err)
		}
	}
	localRuntime := &shutdownRuntime{name: "local"}
	server := NewServer(manager, localRuntime, agent.DefaultRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("local sessions retained after shutdown: %d", len(manager.List()))
	}
	for index, handle := range handles {
		if handle.kills.Load() != 1 {
			t.Fatalf("local handle %d kills = %d", index, handle.kills.Load())
		}
	}
	if server.beginAdmission() {
		t.Fatal("shutdown server accepted new admission")
	}
}

func TestGracefulShutdownPreservesActiveDockerGeneration(t *testing.T) {
	manager := session.NewManager()
	handle := &shutdownHandle{}
	sess := session.NewSession("task", "claude", "docker")
	sess.SetRunning(handle)
	if err := manager.Add(sess); err != nil {
		t.Fatalf("add Docker session: %v", err)
	}
	dockerRuntime := &shutdownRuntime{name: "docker"}
	server := NewServer(manager, dockerRuntime, agent.DefaultRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("controlled Docker handoff: %v", err)
	}
	if manager.Get(sess.ID) == nil || handle.kills.Load() != 0 {
		t.Fatalf("Docker generation removed or killed: session=%v kills=%d", manager.Get(sess.ID), handle.kills.Load())
	}
	if dockerRuntime.cleanups.Load() != 0 {
		t.Fatalf("Docker infrastructure cleaned while generation active: %d", dockerRuntime.cleanups.Load())
	}
}
