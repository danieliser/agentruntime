package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectEvents returns a broadcast function and a way to drain collected events.
func collectEvents() (func(Event) error, func() []Event) {
	var mu sync.Mutex
	var events []Event
	broadcast := func(e Event) error {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		return nil
	}
	drain := func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), events...)
	}
	return broadcast, drain
}

func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunBlockingSuccess(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "hook.sh", "echo hello from hook\necho line two")

	broadcast, drain := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{PreInit: script},
		sessionID: "sess-1",
		taskID:    "task-1",
		agentName: "claude",
		workDir:   dir,
		broadcast: broadcast,
	}

	err := h.RunBlocking(context.Background(), "pre_init", script, 30, nil)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	events := drain()
	// Should have: start message, "hello from hook", "line two", success message
	var hookOutputs []string
	for _, e := range events {
		data, _ := e.Data.(map[string]any)
		if data == nil {
			continue
		}
		if source, _ := data["source"].(string); source == "hook:pre_init" {
			if text, ok := data["text"].(string); ok {
				hookOutputs = append(hookOutputs, text)
			}
		}
	}
	found := false
	for _, text := range hookOutputs {
		if strings.Contains(text, "hello from hook") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected hook output 'hello from hook' in events, got: %v", hookOutputs)
	}
}

func TestRunBlockingFailure(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "fail.sh", "echo failing >&2\nexit 1")

	broadcast, _ := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{PreInit: script},
		sessionID: "sess-1",
		taskID:    "task-1",
		agentName: "claude",
		workDir:   dir,
		broadcast: broadcast,
	}

	err := h.RunBlocking(context.Background(), "pre_init", script, 30, nil)
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "pre_init") {
		t.Errorf("error should mention hook name, got: %v", err)
	}
}

func TestRunBlockingTimeout(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "slow.sh", "sleep 60")

	broadcast, _ := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{PreInit: script},
		sessionID: "sess-1",
		taskID:    "task-1",
		agentName: "claude",
		workDir:   dir,
		broadcast: broadcast,
	}

	start := time.Now()
	err := h.RunBlocking(context.Background(), "pre_init", script, 1, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("should have timed out quickly, took: %v", elapsed)
	}
}

func TestRunBlockingMissing(t *testing.T) {
	broadcast, _ := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{PreInit: "/nonexistent/script.sh"},
		sessionID: "sess-1",
		taskID:    "task-1",
		agentName: "claude",
		workDir:   "/tmp",
		broadcast: broadcast,
	}

	// Missing script should be silently skipped (not an error).
	err := h.RunBlocking(context.Background(), "pre_init", "/nonexistent/script.sh", 30, nil)
	if err != nil {
		t.Fatalf("expected nil for missing script, got: %v", err)
	}
}

func TestRunBlockingEmpty(t *testing.T) {
	broadcast, _ := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{},
		sessionID: "sess-1",
		taskID:    "task-1",
		agentName: "claude",
		workDir:   "/tmp",
		broadcast: broadcast,
	}

	// Empty path should be a no-op.
	err := h.RunBlocking(context.Background(), "pre_init", "", 30, nil)
	if err != nil {
		t.Fatalf("expected nil for empty path, got: %v", err)
	}
}

func TestSpawnBackground(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "sidecar.sh", "while true; do echo tick; sleep 0.1; done")

	broadcast, drain := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{Sidecar: script},
		sessionID: "sess-1",
		taskID:    "task-1",
		agentName: "claude",
		workDir:   dir,
		broadcast: broadcast,
	}

	cancelFn, err := h.SpawnBackground(context.Background(), "sidecar", script, nil)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if cancelFn == nil {
		t.Fatal("expected non-nil cancel function")
	}

	// Let it run briefly.
	time.Sleep(300 * time.Millisecond)

	// Should have some output events.
	events := drain()
	var ticks int
	for _, e := range events {
		data, _ := e.Data.(map[string]any)
		if data == nil {
			continue
		}
		if text, _ := data["text"].(string); text == "tick" {
			ticks++
		}
	}
	if ticks == 0 {
		t.Error("expected at least one 'tick' event from sidecar")
	}

	// Cancel should kill it.
	cancelFn()
}

func TestSpawnBackgroundEmpty(t *testing.T) {
	broadcast, _ := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{},
		sessionID: "sess-1",
		taskID:    "task-1",
		agentName: "claude",
		workDir:   "/tmp",
		broadcast: broadcast,
	}

	cancelFn, err := h.SpawnBackground(context.Background(), "sidecar", "", nil)
	if err != nil {
		t.Fatalf("expected nil error for empty path, got: %v", err)
	}
	if cancelFn != nil {
		t.Error("expected nil cancel for empty path")
	}
}

func TestHookEnvVars(t *testing.T) {
	dir := t.TempDir()
	// Write a script that outputs env vars.
	script := writeScript(t, dir, "env.sh",
		"echo SESSION_ID=$SESSION_ID\n"+
			"echo TASK_ID=$TASK_ID\n"+
			"echo AGENT=$AGENT\n"+
			"echo WORK_DIR=$WORK_DIR\n"+
			"echo AGENT_PID=$AGENT_PID\n")

	broadcast, drain := collectEvents()
	h := &HookRunner{
		config:    &LifecycleConfig{PostInit: script},
		sessionID: "sess-abc",
		taskID:    "task-xyz",
		agentName: "claude",
		workDir:   dir,
		broadcast: broadcast,
	}

	err := h.RunBlocking(context.Background(), "post_init", script, 30, map[string]string{
		"AGENT_PID": "12345",
	})
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}

	events := drain()
	var outputs []string
	for _, e := range events {
		data, _ := e.Data.(map[string]any)
		if data == nil {
			continue
		}
		if text, ok := data["text"].(string); ok && strings.Contains(text, "=") {
			outputs = append(outputs, text)
		}
	}

	expected := map[string]string{
		"SESSION_ID": "sess-abc",
		"TASK_ID":    "task-xyz",
		"AGENT":      "claude",
		"WORK_DIR":   dir,
		"AGENT_PID":  "12345",
	}
	for key, wantVal := range expected {
		found := false
		for _, line := range outputs {
			if strings.HasPrefix(line, key+"=") && strings.Contains(line, wantVal) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected env %s=%s in output, got: %v", key, wantVal, outputs)
		}
	}
}

func TestNewHookRunnerNil(t *testing.T) {
	// nil config returns nil runner.
	r := NewHookRunner(nil, "", "", "", "", nil)
	if r != nil {
		t.Error("expected nil for nil config")
	}

	// Empty config returns nil runner.
	r = NewHookRunner(&LifecycleConfig{}, "", "", "", "", nil)
	if r != nil {
		t.Error("expected nil for empty config")
	}
}

func TestLifecycleConfigDefaults(t *testing.T) {
	cfg := &LifecycleConfig{}
	if got := cfg.BlockingTimeout(); got != 30 {
		t.Errorf("default blocking timeout = %d, want 30", got)
	}
	if got := cfg.PostRunTimeout(); got != 60 {
		t.Errorf("default post_run timeout = %d, want 60", got)
	}

	cfg.HookTimeout = 10
	if got := cfg.BlockingTimeout(); got != 10 {
		t.Errorf("custom blocking timeout = %d, want 10", got)
	}
	if got := cfg.PostRunTimeout(); got != 20 {
		t.Errorf("custom post_run timeout = %d, want 20", got)
	}
}

func TestLifecycleConfigHasHooks(t *testing.T) {
	tests := []struct {
		name string
		cfg  *LifecycleConfig
		want bool
	}{
		{"nil", nil, false},
		{"empty", &LifecycleConfig{}, false},
		{"pre_init", &LifecycleConfig{PreInit: "/x"}, true},
		{"post_init", &LifecycleConfig{PostInit: "/x"}, true},
		{"sidecar", &LifecycleConfig{Sidecar: "/x"}, true},
		{"post_run", &LifecycleConfig{PostRun: "/x"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.HasHooks(); got != tt.want {
				t.Errorf("HasHooks() = %v, want %v", got, tt.want)
			}
		})
	}
}
