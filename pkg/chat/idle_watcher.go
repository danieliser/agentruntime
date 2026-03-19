package chat

import (
	"context"
	"log"
	"time"

	"github.com/danieliser/agentruntime/pkg/session"
)

const defaultWatchInterval = 30 * time.Second

// IdleWatcher is a daemon-level goroutine that polls chat-backed sessions
// for inactivity and kills them when the configured idle timeout expires.
// It does not transition state directly — it kills the process and relies
// on WatchSession to handle the resulting exit event.
type IdleWatcher struct {
	registry *Registry
	sessions *session.Manager
	manager  *Manager
	interval time.Duration
	done     chan struct{}
}

// NewIdleWatcher creates an IdleWatcher with the default 30s poll interval.
func NewIdleWatcher(
	registry *Registry,
	sessions *session.Manager,
	manager *Manager,
) *IdleWatcher {
	return &IdleWatcher{
		registry: registry,
		sessions: sessions,
		manager:  manager,
		interval: defaultWatchInterval,
		done:     make(chan struct{}),
	}
}

// SetInterval overrides the default poll interval. Must be called before Start.
func (w *IdleWatcher) SetInterval(d time.Duration) {
	w.interval = d
}

// Start launches the watcher goroutine. It stops when ctx is cancelled
// or Stop is called.
func (w *IdleWatcher) Start(ctx context.Context) {
	go w.loop(ctx)
}

// Stop signals the watcher to stop and waits for it to exit.
func (w *IdleWatcher) Stop() {
	<-w.done
}

func (w *IdleWatcher) loop(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick()
		}
	}
}

func (w *IdleWatcher) tick() {
	chats, err := w.registry.List()
	if err != nil {
		log.Printf("[idle-watcher] failed to list chats: %v", err)
		return
	}

	for _, rec := range chats {
		if rec.State != ChatStateRunning {
			continue
		}
		if rec.CurrentSession == "" {
			continue
		}

		sess := w.sessions.Get(rec.CurrentSession)
		if sess == nil {
			// Session gone — WatchSession will handle it.
			continue
		}

		snap := sess.Snapshot()
		if isTerminalState(snap.State) {
			// Already exited — WatchSession will handle it.
			continue
		}

		idleTimeout := rec.Config.EffectiveIdleTimeout()
		if snap.LastActivity == nil {
			// No activity recorded yet — use session creation time.
			if time.Since(snap.CreatedAt) <= idleTimeout {
				continue
			}
		} else if time.Since(*snap.LastActivity) <= idleTimeout {
			continue
		}

		log.Printf("[idle-watcher] chat %q idle for %s, killing session %s",
			rec.Name, idleTimeout, rec.CurrentSession)
		if err := sess.Kill(); err != nil {
			log.Printf("[idle-watcher] failed to kill session %s: %v", rec.CurrentSession, err)
		}
		// WatchSession goroutine handles the state transition.
	}
}
