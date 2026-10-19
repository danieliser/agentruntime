package main

import (
	"context"
	"log"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/runtime"
)

// Docker Desktop can take a minute or more to come up after boot. A runtime
// that still has unfinished durable sessions is retried for this long before
// startup gives up on it, so its containers are not misjudged as missing.
var (
	runtimeRecoveryWindow  = 90 * time.Second
	runtimeRecoveryAttempt = 20 * time.Second
	runtimeRecoveryBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	recoverySleep          = time.Sleep
	recoveryNow            = time.Now
)

type sessionLister interface {
	ListSessions(ctx context.Context) ([]durable.Session, error)
}

// recoverRuntimeWithBackoff runs r.Recover. If it fails while the durable
// store still holds unfinished sessions on that runtime, it retries with
// growing delays until runtimeRecoveryWindow runs out. A runtime with no
// unfinished sessions is not retried, so a machine without Docker starts at
// once.
func recoverRuntimeWithBackoff(r runtime.Runtime, store sessionLister) ([]runtime.ProcessHandle, error) {
	deadline := recoveryNow().Add(runtimeRecoveryWindow)
	for attempt := 0; ; attempt++ {
		limit := runtimeRecoveryAttempt
		if remaining := deadline.Sub(recoveryNow()); attempt > 0 && remaining < limit {
			limit = max(remaining, time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		recovered, err := r.Recover(ctx)
		cancel()
		if err == nil {
			if attempt > 0 {
				log.Printf("%s runtime recovery succeeded after %d retries", r.Name(), attempt)
			}
			return recovered, nil
		}
		if attempt == 0 && !hasUnfinishedSessions(store, r.Name()) {
			return nil, err
		}
		delay := runtimeRecoveryBackoff[min(attempt, len(runtimeRecoveryBackoff)-1)]
		if recoveryNow().Add(delay).After(deadline) {
			return nil, err
		}
		log.Printf("%s runtime recovery failed (attempt %d), retrying in %s: %v", r.Name(), attempt+1, delay, err)
		recoverySleep(delay)
	}
}

func hasUnfinishedSessions(store sessionLister, runtimeName string) bool {
	if store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		// Unknown: retrying is the safe choice for recovery.
		return true
	}
	for _, s := range sessions {
		if s.Runtime == runtimeName && !s.State.Terminal() {
			return true
		}
	}
	return false
}

type wakeLister interface {
	sessionLister
	GetGeneration(ctx context.Context, sessionID string, number int64) (durable.Generation, error)
}

// strandedWakeOnRecoveredRuntime reports whether a wake whose generation
// already exited is still pending on a runtime that recovered. Startup
// reconciliation settles those, so one left over is a real fault. A stranded
// wake on a runtime that did not recover is expected and waits for a later
// start. Lookup errors count as a fault.
func strandedWakeOnRecoveredRuntime(store wakeLister, recoveredRuntimes []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := store.ListSessions(ctx)
	if err != nil {
		return true
	}
	for _, s := range sessions {
		if s.State != durable.StateStarting || s.ActiveGeneration < 1 {
			continue
		}
		generation, err := store.GetGeneration(ctx, s.ID, s.ActiveGeneration)
		if err != nil {
			return true
		}
		if generation.State != durable.GenerationExited {
			continue
		}
		for _, name := range recoveredRuntimes {
			if name == s.Runtime {
				return true
			}
		}
	}
	return false
}
