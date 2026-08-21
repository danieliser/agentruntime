package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/danieliser/agentruntime/pkg/runtime"
)

const (
	defaultRuntimeProbeInterval   = 15 * time.Second
	defaultRuntimeProbeTimeout    = 60 * time.Second
	defaultRuntimeProbeStaleAfter = 45 * time.Second
)

type runtimeReadinessConfig struct {
	interval   time.Duration
	timeout    time.Duration
	staleAfter time.Duration
}

type runtimeReadinessSnapshot struct {
	Status    string                   `json:"status"`
	CheckedAt time.Time                `json:"checked_at,omitempty"`
	Stale     bool                     `json:"stale"`
	LastError string                   `json:"last_error,omitempty"`
	Details   *runtime.AdmissionReport `json:"details,omitempty"`
}

type runtimeReadinessMonitor struct {
	runtimes map[string]runtime.Runtime
	config   runtimeReadinessConfig

	mu        sync.RWMutex
	snapshots map[string]runtimeReadinessSnapshot
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	started   bool
}

func configuredRuntimeReadiness(configs ...ServerConfig) runtimeReadinessConfig {
	config := runtimeReadinessConfig{
		interval: defaultRuntimeProbeInterval, timeout: defaultRuntimeProbeTimeout,
		staleAfter: defaultRuntimeProbeStaleAfter,
	}
	if len(configs) == 0 {
		return config
	}
	if configs[0].RuntimeProbeInterval > 0 {
		config.interval = configs[0].RuntimeProbeInterval
	}
	if configs[0].RuntimeProbeTimeout > 0 {
		config.timeout = configs[0].RuntimeProbeTimeout
	}
	if configs[0].RuntimeProbeStaleAfter > 0 {
		config.staleAfter = configs[0].RuntimeProbeStaleAfter
	}
	return config
}

func newRuntimeReadinessMonitor(runtimes map[string]runtime.Runtime, config runtimeReadinessConfig) *runtimeReadinessMonitor {
	now := time.Now().UTC()
	snapshots := make(map[string]runtimeReadinessSnapshot, len(runtimes))
	for name, rt := range runtimes {
		if _, checked := rt.(runtime.AdmissionChecker); checked {
			snapshots[name] = runtimeReadinessSnapshot{Status: "checking"}
			continue
		}
		snapshots[name] = runtimeReadinessSnapshot{Status: "ready", CheckedAt: now}
	}
	return &runtimeReadinessMonitor{runtimes: runtimes, config: config, snapshots: snapshots}
}

func (s *Server) startRuntimeReadiness() {
	if s.readiness == nil {
		return
	}
	s.readiness.start()
}

func (s *Server) stopRuntimeReadiness() {
	if s.readiness == nil {
		return
	}
	s.readiness.stop()
}

func (s *Server) refreshRuntimeReadiness(ctx context.Context) {
	if s.readiness == nil {
		return
	}
	s.readiness.refresh(ctx)
}

func (monitor *runtimeReadinessMonitor) start() {
	monitor.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		monitor.cancel = cancel
		monitor.done = make(chan struct{})
		monitor.mu.Lock()
		monitor.started = true
		monitor.mu.Unlock()
		go func() {
			defer close(monitor.done)
			var wait sync.WaitGroup
			for name, rt := range monitor.runtimes {
				wait.Add(1)
				go func(name string, rt runtime.Runtime) {
					defer wait.Done()
					monitor.runRuntime(ctx, name, rt)
				}(name, rt)
			}
			wait.Wait()
		}()
	})
}

func (monitor *runtimeReadinessMonitor) runRuntime(ctx context.Context, name string, rt runtime.Runtime) {
	monitor.refreshRuntime(ctx, name, rt)
	ticker := time.NewTicker(monitor.config.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			monitor.refreshRuntime(ctx, name, rt)
		}
	}
}

func (monitor *runtimeReadinessMonitor) admission(name string, now time.Time) (bool, error) {
	monitor.mu.RLock()
	defer monitor.mu.RUnlock()
	if !monitor.started {
		return false, nil
	}
	snapshot, ok := monitor.snapshots[name]
	if !ok {
		return true, fmt.Errorf("runtime %q has no readiness snapshot", name)
	}
	if snapshot.CheckedAt.IsZero() {
		return true, fmt.Errorf("runtime %q readiness is still checking", name)
	}
	if now.Sub(snapshot.CheckedAt) > monitor.config.staleAfter {
		return true, fmt.Errorf("runtime %q readiness snapshot is stale", name)
	}
	if snapshot.Status != "ready" {
		if snapshot.LastError != "" {
			return true, fmt.Errorf("runtime %q is unavailable: %s", name, snapshot.LastError)
		}
		return true, fmt.Errorf("runtime %q is unavailable", name)
	}
	return true, nil
}

func (monitor *runtimeReadinessMonitor) stop() {
	monitor.stopOnce.Do(func() {
		if monitor.cancel == nil {
			return
		}
		monitor.cancel()
		<-monitor.done
	})
}

func (monitor *runtimeReadinessMonitor) refresh(parent context.Context) {
	var wait sync.WaitGroup
	for name, rt := range monitor.runtimes {
		wait.Add(1)
		go func(name string, rt runtime.Runtime) {
			defer wait.Done()
			monitor.refreshRuntime(parent, name, rt)
		}(name, rt)
	}
	wait.Wait()
}

func (monitor *runtimeReadinessMonitor) refreshRuntime(parent context.Context, name string, rt runtime.Runtime) {
	checker, checked := rt.(runtime.AdmissionChecker)
	if !checked {
		monitor.mu.Lock()
		monitor.snapshots[name] = runtimeReadinessSnapshot{
			Status:    "ready",
			CheckedAt: time.Now().UTC(),
		}
		monitor.mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(parent, monitor.config.timeout)
	defer cancel()
	var err error
	if prewarmer, ok := rt.(runtime.Prewarmer); ok {
		err = prewarmer.Prewarm(ctx)
	}
	var details *runtime.AdmissionReport
	if err == nil {
		if reporter, ok := rt.(runtime.AdmissionReporter); ok {
			report, reportErr := reporter.CheckAdmissionReport(ctx)
			err = reportErr
			if reportErr == nil {
				details = &report
			}
		} else {
			err = checker.CheckAdmission(ctx)
		}
	}
	snapshot := runtimeReadinessSnapshot{Status: "ready", CheckedAt: time.Now().UTC(), Details: details}
	if err != nil {
		snapshot.Status = "unavailable"
		snapshot.LastError = err.Error()
	}
	monitor.mu.Lock()
	monitor.snapshots[name] = snapshot
	monitor.mu.Unlock()
}

func (monitor *runtimeReadinessMonitor) snapshot(now time.Time) map[string]runtimeReadinessSnapshot {
	monitor.mu.RLock()
	defer monitor.mu.RUnlock()
	result := make(map[string]runtimeReadinessSnapshot, len(monitor.snapshots))
	for name, snapshot := range monitor.snapshots {
		if !snapshot.CheckedAt.IsZero() && now.Sub(snapshot.CheckedAt) > monitor.config.staleAfter {
			snapshot.Stale = true
		}
		result[name] = snapshot
	}
	return result
}
