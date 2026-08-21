package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/agent"
	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

func TestHealthReadsLatestRuntimeSnapshotWithoutRunningLiveProbe(t *testing.T) {
	rt := &snapshotTestRuntime{name: "docker"}
	rt.setCheck(func(context.Context) error { return nil })
	server := NewServer(session.NewManager(), rt, agent.DefaultRegistry(), ServerConfig{
		RuntimeProbeInterval:   time.Hour,
		RuntimeProbeTimeout:    time.Second,
		RuntimeProbeStaleAfter: time.Hour,
	})
	server.startRuntimeReadiness()
	t.Cleanup(server.stopRuntimeReadiness)
	waitForCondition(t, time.Second, func() bool { return rt.checks.Load() == 1 }, "initial runtime probe")

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	rt.setCheck(func(ctx context.Context) error {
		close(probeStarted)
		select {
		case <-releaseProbe:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	go server.refreshRuntimeReadiness(context.Background())
	<-probeStarted

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()
	started := time.Now()
	server.router.ServeHTTP(recorder, request)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cached health took %s while probe was blocked", elapsed)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		RuntimeStatus map[string]string `json:"runtime_status"`
		RuntimeChecks map[string]struct {
			Status    string    `json:"status"`
			CheckedAt time.Time `json:"checked_at"`
			Stale     bool      `json:"stale"`
		} `json:"runtime_checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	check := body.RuntimeChecks["docker"]
	if body.RuntimeStatus["docker"] != "ready" || check.Status != "ready" || check.CheckedAt.IsZero() || check.Stale {
		t.Fatalf("unexpected cached health: %+v", body)
	}
	if got := rt.checks.Load(); got != 2 {
		t.Fatalf("health request ran a live probe: check count = %d", got)
	}
	close(releaseProbe)
}

func TestHealthRejectsStaleRuntimeSnapshot(t *testing.T) {
	rt := &snapshotTestRuntime{name: "docker"}
	rt.setCheck(func(context.Context) error { return nil })
	server := NewServer(session.NewManager(), rt, agent.DefaultRegistry(), ServerConfig{
		RuntimeProbeInterval:   time.Hour,
		RuntimeProbeTimeout:    time.Second,
		RuntimeProbeStaleAfter: 20 * time.Millisecond,
	})
	server.startRuntimeReadiness()
	t.Cleanup(server.stopRuntimeReadiness)
	waitForCondition(t, time.Second, func() bool { return rt.checks.Load() == 1 }, "initial runtime probe")
	time.Sleep(30 * time.Millisecond)

	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale health status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		RuntimeStatus map[string]string `json:"runtime_status"`
		RuntimeChecks map[string]struct {
			Status string `json:"status"`
			Stale  bool   `json:"stale"`
		} `json:"runtime_checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body.RuntimeStatus["docker"] != "stale" || !body.RuntimeChecks["docker"].Stale {
		t.Fatalf("stale snapshot not reported: %+v", body)
	}
}

func TestRuntimeReadinessRefreshesPassiveRuntimeTimestamp(t *testing.T) {
	monitor := newRuntimeReadinessMonitor(map[string]runtime.Runtime{
		"local": &passiveSnapshotTestRuntime{name: "local"},
	}, runtimeReadinessConfig{
		interval: time.Hour, timeout: time.Second, staleAfter: time.Hour,
	})
	initial := monitor.snapshot(time.Now())["local"].CheckedAt
	time.Sleep(5 * time.Millisecond)

	monitor.refresh(context.Background())

	refreshed := monitor.snapshot(time.Now())["local"]
	if !refreshed.CheckedAt.After(initial) {
		t.Fatalf("passive runtime timestamp was not refreshed: initial=%s refreshed=%s", initial, refreshed.CheckedAt)
	}
	if refreshed.Status != "ready" || refreshed.Stale {
		t.Fatalf("passive runtime snapshot = %+v, want fresh ready", refreshed)
	}
}

func TestSlowAdmissionProbeDoesNotStalePassiveRuntime(t *testing.T) {
	probeStarted := make(chan struct{})
	docker := &snapshotTestRuntime{name: "docker"}
	docker.setCheck(func(ctx context.Context) error {
		select {
		case <-probeStarted:
		default:
			close(probeStarted)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	monitor := newRuntimeReadinessMonitor(map[string]runtime.Runtime{
		"docker": docker,
		"local":  &passiveSnapshotTestRuntime{name: "local"},
	}, runtimeReadinessConfig{
		interval: 5 * time.Millisecond, timeout: time.Second, staleAfter: 30 * time.Millisecond,
	})
	monitor.start()
	t.Cleanup(monitor.stop)
	<-probeStarted
	time.Sleep(50 * time.Millisecond)

	local := monitor.snapshot(time.Now())["local"]
	if local.Status != "ready" || local.Stale {
		t.Fatalf("slow Docker probe stalled passive runtime refresh: %+v", local)
	}
}

func TestSessionAdmissionUsesPassiveRuntimeSnapshot(t *testing.T) {
	rt := &snapshotTestRuntime{name: "docker"}
	rt.setCheck(func(context.Context) error { return nil })
	server := NewServer(session.NewManager(), rt, agent.DefaultRegistry(), ServerConfig{
		RuntimeProbeInterval: time.Hour, RuntimeProbeTimeout: time.Second, RuntimeProbeStaleAfter: time.Hour,
	})
	server.startRuntimeReadiness()
	t.Cleanup(server.stopRuntimeReadiness)
	waitForCondition(t, time.Second, func() bool { return rt.checks.Load() == 1 }, "initial runtime probe")
	if err := server.checkRuntimeAdmission(context.Background(), SessionRequest{IdempotencyKey: "passive-admission"}, rt); err != nil {
		t.Fatalf("cached admission failed: %v", err)
	}
	if got := rt.checks.Load(); got != 1 {
		t.Fatalf("session admission ran live runtime probe; checks=%d", got)
	}
}

type snapshotTestRuntime struct {
	name   string
	checks atomic.Int32
	mu     sync.RWMutex
	check  func(context.Context) error
}

func (rt *snapshotTestRuntime) setCheck(check func(context.Context) error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.check = check
}

func (rt *snapshotTestRuntime) CheckAdmission(ctx context.Context) error {
	rt.checks.Add(1)
	rt.mu.RLock()
	check := rt.check
	rt.mu.RUnlock()
	return check(ctx)
}

func (rt *snapshotTestRuntime) Name() string { return rt.name }
func (*snapshotTestRuntime) Spawn(context.Context, runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	panic("not used")
}
func (*snapshotTestRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) {
	return nil, nil
}
func (*snapshotTestRuntime) Cleanup(context.Context) error { return nil }

type passiveSnapshotTestRuntime struct {
	name string
}

func (rt *passiveSnapshotTestRuntime) Name() string { return rt.name }
func (*passiveSnapshotTestRuntime) Spawn(context.Context, runtime.SpawnConfig) (runtime.ProcessHandle, error) {
	panic("not used")
}
func (*passiveSnapshotTestRuntime) Recover(context.Context) ([]runtime.ProcessHandle, error) {
	return nil, nil
}
func (*passiveSnapshotTestRuntime) Cleanup(context.Context) error { return nil }
