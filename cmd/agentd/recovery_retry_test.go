package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/runtime"
)

type flakyRuntime struct {
	failures int
	calls    int
}

func (r *flakyRuntime) Spawn(context.Context, runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	return nil, errors.New("unused")
}
func (r *flakyRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) {
	r.calls++
	if r.calls <= r.failures {
		return nil, errors.New("docker daemon not running")
	}
	return nil, nil
}
func (r *flakyRuntime) Name() string                  { return "docker" }
func (r *flakyRuntime) Cleanup(context.Context) error { return nil }

type fakeWakeStore struct {
	sessions    []durable.Session
	generations map[string]durable.Generation
	listErr     error
}

func (s fakeWakeStore) ListSessions(context.Context) ([]durable.Session, error) {
	return s.sessions, s.listErr
}
func (s fakeWakeStore) GetGeneration(_ context.Context, id string, _ int64) (durable.Generation, error) {
	g, ok := s.generations[id]
	if !ok {
		return durable.Generation{}, errors.New("missing")
	}
	return g, nil
}

func withFastRecovery(t *testing.T, window time.Duration) *[]time.Duration {
	t.Helper()
	oldWindow, oldSleep, oldNow := runtimeRecoveryWindow, recoverySleep, recoveryNow
	var slept []time.Duration
	clock := time.Unix(0, 0)
	runtimeRecoveryWindow = window
	recoveryNow = func() time.Time { return clock }
	recoverySleep = func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) }
	t.Cleanup(func() { runtimeRecoveryWindow, recoverySleep, recoveryNow = oldWindow, oldSleep, oldNow })
	return &slept
}

func TestRecoveryRetriesWhileDockerSessionsAreUnfinished(t *testing.T) {
	slept := withFastRecovery(t, time.Hour)
	rt := &flakyRuntime{failures: 3}
	store := fakeWakeStore{sessions: []durable.Session{{ID: "s1", Runtime: "docker", State: durable.StateRunning}}}
	if _, err := recoverRuntimeWithBackoff(rt, store); err != nil {
		t.Fatal(err)
	}
	if rt.calls != 4 || len(*slept) != 3 || (*slept)[0] != 2*time.Second || (*slept)[2] != 8*time.Second {
		t.Fatalf("calls=%d slept=%v", rt.calls, *slept)
	}
}

func TestRecoveryDoesNotRetryWithoutUnfinishedSessions(t *testing.T) {
	slept := withFastRecovery(t, time.Hour)
	rt := &flakyRuntime{failures: 10}
	store := fakeWakeStore{sessions: []durable.Session{
		{ID: "done", Runtime: "docker", State: durable.StateCompleted},
		{ID: "local", Runtime: "local", State: durable.StateRunning},
	}}
	if _, err := recoverRuntimeWithBackoff(rt, store); err == nil {
		t.Fatal("expected the recovery error")
	}
	if rt.calls != 1 || len(*slept) != 0 {
		t.Fatalf("calls=%d slept=%v", rt.calls, *slept)
	}
}

func TestRecoveryGivesUpAtWindow(t *testing.T) {
	slept := withFastRecovery(t, 20*time.Second) // room for 2s+4s+8s, not +16s
	rt := &flakyRuntime{failures: 100}
	store := fakeWakeStore{listErr: errors.New("store busy")}
	if _, err := recoverRuntimeWithBackoff(rt, store); err == nil {
		t.Fatal("expected the recovery error")
	}
	if rt.calls != 4 || len(*slept) != 3 {
		t.Fatalf("calls=%d slept=%v", rt.calls, *slept)
	}
}

func TestStrandedWakeOnlyFaultsOnRecoveredRuntime(t *testing.T) {
	store := fakeWakeStore{
		sessions: []durable.Session{
			{ID: "wake", Runtime: "docker", State: durable.StateStarting, ActiveGeneration: 2},
			{ID: "live", Runtime: "local", State: durable.StateStarting, ActiveGeneration: 1},
		},
		generations: map[string]durable.Generation{
			"wake": {State: durable.GenerationExited},
			"live": {State: durable.GenerationStarting},
		},
	}
	if strandedWakeOnRecoveredRuntime(store, []string{"local"}) {
		t.Fatal("docker wake should wait for docker")
	}
	if !strandedWakeOnRecoveredRuntime(store, []string{"local", "docker"}) {
		t.Fatal("wake on a recovered runtime is a fault")
	}
	if !strandedWakeOnRecoveredRuntime(fakeWakeStore{listErr: errors.New("x")}, nil) {
		t.Fatal("lookup errors must count as a fault")
	}
}
