package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

var stallTickInterval = 5 * time.Second // exported for testing

// StallConfig provides timeout values for stall detection.
type StallConfig struct {
	WarningTimeout time.Duration
	KillTimeout    time.Duration
	ResultGrace    time.Duration
}

// StallDetector monitors event-stream activity and process state to detect stalls.
type StallDetector struct {
	// Activity timestamps (atomic for lock-free updates).
	lastEventNano atomic.Int64 // Unix nanos of last event; 0 = not started
	resultNano    atomic.Int64 // Unix nanos when result event was seen
	resultSeen    atomic.Bool  // true after first "result" event

	// Config (read-only after construction).
	warningTimeout time.Duration
	killTimeout    time.Duration
	resultGrace    time.Duration

	// Callback to emit events.
	emitEvent func(Event) error

	// Cancel function to kill the session.
	cancelFn func()

	// TickInterval is the polling interval (exported for testing).
	TickInterval time.Duration
}

// NewStallDetector creates a new stall detector with the given config.
func NewStallDetector(cfg StallConfig, emitEvent func(Event) error, cancelFn func()) *StallDetector {
	return &StallDetector{
		warningTimeout: cfg.WarningTimeout,
		killTimeout:    cfg.KillTimeout,
		resultGrace:    cfg.ResultGrace,
		emitEvent:      emitEvent,
		cancelFn:       cancelFn,
		TickInterval:   stallTickInterval,
	}
}

// RecordEvent updates the last-activity timestamp. Called on every event.
func (d *StallDetector) RecordEvent(eventType string) {
	d.lastEventNano.Store(time.Now().UnixNano())

	// Detect result events for result grace period tracking.
	if eventType == "result" && !d.resultSeen.Load() {
		d.resultNano.Store(time.Now().UnixNano())
		d.resultSeen.Store(true)
	}
}

// ClearResult clears the result-seen flag. Called when a new prompt arrives.
func (d *StallDetector) ClearResult() {
	d.resultSeen.Store(false)
}

// Run starts the background monitoring goroutine. Blocks until ctx is cancelled.
func (d *StallDetector) Run(ctx context.Context) {
	// All detection phases disabled.
	if d.warningTimeout < 0 && d.killTimeout < 0 && d.resultGrace < 0 {
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(d.TickInterval)
	defer ticker.Stop()

	warningEmitted := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UnixNano()

			// Phase 0: Result grace period (highest priority — overrides other phases).
			if d.resultGrace > 0 && d.resultSeen.Load() {
				resultNano := d.resultNano.Load()
				if resultNano > 0 {
					elapsed := time.Duration(now - resultNano)
					if elapsed >= d.resultGrace {
						d.handleStallKill("result_timeout",
							"agent process did not exit within result grace period")
						return
					}
					// Once result is seen, only the grace period matters.
					// Skip warning/kill checks — the agent has finished its work.
					continue
				}
			}

			// No events yet; agent still starting.
			lastEvent := d.lastEventNano.Load()
			if lastEvent == 0 {
				continue
			}

			silence := time.Duration(now - lastEvent)

			// Phase 2: Hard kill (checked before warning so we don't warn then immediately kill).
			if d.killTimeout > 0 && silence >= d.killTimeout {
				d.handleStallKill("stall_timeout",
					fmt.Sprintf("no events for %s, force-killing agent", silence.Truncate(time.Second)))
				return
			}

			// Phase 1: Advisory warning.
			if d.warningTimeout > 0 && !warningEmitted && silence >= d.warningTimeout {
				d.emitStallWarning(silence)
				warningEmitted = true
			}
		}
	}
}

// emitStallWarning emits a system event warning about stall condition.
func (d *StallDetector) emitStallWarning(silence time.Duration) {
	_ = d.emitEvent(Event{
		Type: "system",
		Data: map[string]any{
			"subtype":   "stall_warning",
			"message":   fmt.Sprintf("no events for %s", silence.Truncate(time.Second)),
			"silence":   silence.Seconds(),
			"threshold": d.warningTimeout.Seconds(),
		},
	})
	log.Printf("stall warning: no events for %s", silence.Truncate(time.Second))
}

// handleStallKill emits a kill event and initiates the kill sequence.
func (d *StallDetector) handleStallKill(reason, message string) {
	// Emit stall_kill system event (best-effort, before killing anything).
	_ = d.emitEvent(Event{
		Type: "system",
		Data: map[string]any{
			"subtype": "stall_kill",
			"reason":  reason,
			"message": message,
		},
	})
	log.Printf("stall kill (%s): %s", reason, message)

	// Cancel the session context to trigger exit.
	d.cancelFn()
}
