package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestStallWarningFiresAfterSilenceThreshold(t *testing.T) {
	warningTimeout := 100 * time.Millisecond
	killTimeout := 1 * time.Second
	resultGrace := 1 * time.Second
	tickInterval := 30 * time.Millisecond

	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelCalled := false
	var mu sync.Mutex
	cancelFn := func() {
		mu.Lock()
		cancelCalled = true
		mu.Unlock()
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: warningTimeout,
			KillTimeout:    killTimeout,
			ResultGrace:    resultGrace,
		},
		emitEvent,
		cancelFn,
	)
	detector.TickInterval = tickInterval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Record an event to start the timer.
	detector.RecordEvent("agent_message")

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// Wait for warning to fire (warningTimeout + 2 ticks).
	time.Sleep(200 * time.Millisecond)

	// Stop the detector.
	cancel()
	<-done

	// Should have emitted a stall_warning system event.
	found := false
	for _, evt := range emitted {
		if evt.Type == "system" {
			if data, ok := evt.Data.(map[string]any); ok {
				if subtype, ok := data["subtype"].(string); ok && subtype == "stall_warning" {
					found = true
					break
				}
			}
		}
	}

	if !found {
		t.Errorf("expected stall_warning event, but got %v", emitted)
	}

	mu.Lock()
	if cancelCalled {
		t.Errorf("expected no cancel at warning threshold")
	}
	mu.Unlock()
}

func TestStallKillFiresAfterKillThreshold(t *testing.T) {
	warningTimeout := 50 * time.Millisecond
	killTimeout := 100 * time.Millisecond
	resultGrace := 1 * time.Second
	tickInterval := 30 * time.Millisecond

	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelCalled := false
	var mu sync.Mutex
	cancelFn := func() {
		mu.Lock()
		cancelCalled = true
		mu.Unlock()
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: warningTimeout,
			KillTimeout:    killTimeout,
			ResultGrace:    resultGrace,
		},
		emitEvent,
		cancelFn,
	)
	detector.TickInterval = tickInterval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Record an event to start the timer.
	detector.RecordEvent("agent_message")

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// Wait for kill to fire (killTimeout + 2 ticks).
	time.Sleep(200 * time.Millisecond)

	// Check that we got a stall_kill event and the cancel was called.
	found := false
	for _, evt := range emitted {
		if evt.Type == "system" {
			if data, ok := evt.Data.(map[string]any); ok {
				if subtype, ok := data["subtype"].(string); ok && subtype == "stall_kill" {
					found = true
					break
				}
			}
		}
	}

	if !found {
		t.Errorf("expected stall_kill event, but got %v", emitted)
	}

	mu.Lock()
	if !cancelCalled {
		t.Errorf("expected cancel to be called on kill threshold")
	}
	mu.Unlock()

	<-done
}

func TestResultGracePeriodKillsHungProcess(t *testing.T) {
	warningTimeout := 1 * time.Second
	killTimeout := 10 * time.Second
	resultGrace := 100 * time.Millisecond
	tickInterval := 30 * time.Millisecond

	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelCalled := false
	var mu sync.Mutex
	cancelFn := func() {
		mu.Lock()
		cancelCalled = true
		mu.Unlock()
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: warningTimeout,
			KillTimeout:    killTimeout,
			ResultGrace:    resultGrace,
		},
		emitEvent,
		cancelFn,
	)
	detector.TickInterval = tickInterval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Record a result event.
	detector.RecordEvent("result")

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// Wait for result grace to expire (resultGrace + 2 ticks).
	time.Sleep(200 * time.Millisecond)

	// Check for stall_kill event with reason="result_timeout".
	found := false
	for _, evt := range emitted {
		if evt.Type == "system" {
			if data, ok := evt.Data.(map[string]any); ok {
				if subtype, ok := data["subtype"].(string); ok && subtype == "stall_kill" {
					if reason, ok := data["reason"].(string); ok && reason == "result_timeout" {
						found = true
						break
					}
				}
			}
		}
	}

	if !found {
		t.Errorf("expected stall_kill event with reason=result_timeout, but got %v", emitted)
	}

	mu.Lock()
	if !cancelCalled {
		t.Errorf("expected cancel to be called for result timeout")
	}
	mu.Unlock()

	<-done
}

func TestResultGracePeriodCancelledByNewPrompt(t *testing.T) {
	warningTimeout := 1 * time.Second
	killTimeout := 10 * time.Second
	resultGrace := 200 * time.Millisecond
	tickInterval := 30 * time.Millisecond

	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelCalled := false
	var mu sync.Mutex
	cancelFn := func() {
		mu.Lock()
		cancelCalled = true
		mu.Unlock()
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: warningTimeout,
			KillTimeout:    killTimeout,
			ResultGrace:    resultGrace,
		},
		emitEvent,
		cancelFn,
	)
	detector.TickInterval = tickInterval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Record a result event.
	detector.RecordEvent("result")

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// At 80ms, send a prompt to clear the result.
	time.Sleep(80 * time.Millisecond)
	detector.RecordEvent("prompt")
	detector.ClearResult()

	// Wait past the original grace period.
	time.Sleep(250 * time.Millisecond)

	// Should not have called cancel or emitted stall_kill.
	mu.Lock()
	if cancelCalled {
		t.Errorf("expected no cancel after prompt clears result")
	}
	mu.Unlock()

	found := false
	for _, evt := range emitted {
		if evt.Type == "system" {
			if data, ok := evt.Data.(map[string]any); ok {
				if subtype, ok := data["subtype"].(string); ok && subtype == "stall_kill" {
					found = true
					break
				}
			}
		}
	}

	if found {
		t.Errorf("expected no stall_kill after prompt clears result")
	}

	cancel()
	<-done
}

func TestNormalExitPreemptsStallDetection(t *testing.T) {
	warningTimeout := 100 * time.Millisecond
	killTimeout := 200 * time.Millisecond
	resultGrace := 1 * time.Second
	tickInterval := 30 * time.Millisecond

	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelFn := func() {
		t.Errorf("cancel should not be called during normal exit")
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: warningTimeout,
			KillTimeout:    killTimeout,
			ResultGrace:    resultGrace,
		},
		emitEvent,
		cancelFn,
	)
	detector.TickInterval = tickInterval

	ctx, cancel := context.WithCancel(context.Background())

	// Record an event to start the timer.
	detector.RecordEvent("agent_message")

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// Cancel the context immediately (simulating normal exit).
	cancel()

	// Wait a bit, then verify no stall events were emitted.
	time.Sleep(250 * time.Millisecond)

	found := false
	for _, evt := range emitted {
		if evt.Type == "system" {
			if data, ok := evt.Data.(map[string]any); ok {
				if subtype, ok := data["subtype"].(string); ok {
					if subtype == "stall_kill" || subtype == "stall_warning" {
						found = true
						break
					}
				}
			}
		}
	}

	if found {
		t.Errorf("expected no stall events after normal exit")
	}

	<-done
}

func TestAllPhasesDisabled(t *testing.T) {
	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelCalled := false
	var mu sync.Mutex
	cancelFn := func() {
		mu.Lock()
		cancelCalled = true
		mu.Unlock()
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: -1,
			KillTimeout:    -1,
			ResultGrace:    -1,
		},
		emitEvent,
		cancelFn,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Record an event.
	detector.RecordEvent("agent_message")

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// Wait to ensure no stall events fire.
	time.Sleep(100 * time.Millisecond)

	if len(emitted) > 0 {
		t.Errorf("expected no events when all phases disabled, but got %v", emitted)
	}

	mu.Lock()
	if cancelCalled {
		t.Errorf("expected no cancel when all phases disabled")
	}
	mu.Unlock()

	cancel()
	<-done
}

func TestNoEventsBeforeFirstEvent(t *testing.T) {
	warningTimeout := 100 * time.Millisecond
	killTimeout := 200 * time.Millisecond
	resultGrace := 1 * time.Second
	tickInterval := 30 * time.Millisecond

	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelCalled := false
	var mu sync.Mutex
	cancelFn := func() {
		mu.Lock()
		cancelCalled = true
		mu.Unlock()
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: warningTimeout,
			KillTimeout:    killTimeout,
			ResultGrace:    resultGrace,
		},
		emitEvent,
		cancelFn,
	)
	detector.TickInterval = tickInterval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Don't record any events (simulating slow agent startup).

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// Wait past the warning threshold (warningTimeout + 2 ticks).
	time.Sleep(200 * time.Millisecond)

	// Should not have emitted stall events.
	if len(emitted) > 0 {
		t.Errorf("expected no stall events before first event, but got %v", emitted)
	}

	mu.Lock()
	if cancelCalled {
		t.Errorf("expected no cancel before first event")
	}
	mu.Unlock()

	cancel()
	<-done
}

func TestContinuousEventsPreventStall(t *testing.T) {
	warningTimeout := 100 * time.Millisecond
	killTimeout := 200 * time.Millisecond
	resultGrace := 1 * time.Second
	tickInterval := 30 * time.Millisecond

	emitted := make([]Event, 0)
	emitEvent := func(event Event) error {
		emitted = append(emitted, event)
		return nil
	}

	cancelCalled := false
	var mu sync.Mutex
	cancelFn := func() {
		mu.Lock()
		defer mu.Unlock()
		cancelCalled = true
	}

	detector := NewStallDetector(
		StallConfig{
			WarningTimeout: warningTimeout,
			KillTimeout:    killTimeout,
			ResultGrace:    resultGrace,
		},
		emitEvent,
		cancelFn,
	)
	detector.TickInterval = tickInterval

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run detector in a goroutine.
	done := make(chan struct{})
	go func() {
		detector.Run(ctx)
		close(done)
	}()

	// Record events continuously, just before the warning threshold.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	go func() {
		for i := 0; i < 10; i++ {
			<-ticker.C
			detector.RecordEvent("agent_message")
		}
	}()

	// Wait for the ticker to finish.
	time.Sleep(550 * time.Millisecond)

	// Should not have emitted stall events.
	found := false
	for _, evt := range emitted {
		if evt.Type == "system" {
			if data, ok := evt.Data.(map[string]any); ok {
				if subtype, ok := data["subtype"].(string); ok {
					if subtype == "stall_kill" || subtype == "stall_warning" {
						found = true
						break
					}
				}
			}
		}
	}

	if found {
		t.Errorf("expected no stall events with continuous activity")
	}

	mu.Lock()
	if cancelCalled {
		t.Errorf("expected no cancel with continuous activity")
	}
	mu.Unlock()

	cancel()
	<-done
}
