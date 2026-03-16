package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/runtime"
	"github.com/danieliser/agentruntime/pkg/session"
)

type recoveryTestHandle struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter
	done    chan runtime.ExitResult
}

func newRecoveryTestHandle() *recoveryTestHandle {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &recoveryTestHandle{
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		stderrR: stderrR,
		stderrW: stderrW,
		done:    make(chan runtime.ExitResult, 1),
	}
}

func (h *recoveryTestHandle) Stdin() io.WriteCloser { return nil }
func (h *recoveryTestHandle) Stdout() io.ReadCloser { return h.stdoutR }
func (h *recoveryTestHandle) Stderr() io.ReadCloser { return h.stderrR }
func (h *recoveryTestHandle) Wait() <-chan runtime.ExitResult {
	return h.done
}
func (h *recoveryTestHandle) Kill() error { return nil }
func (h *recoveryTestHandle) PID() int    { return 0 }
func (h *recoveryTestHandle) RecoveryInfo() *runtime.RecoveryInfo {
	return nil
}

func TestDaemonRecovery_ReplayBufferPopulated(t *testing.T) {
	logDir := t.TempDir()
	handle := newRecoveryTestHandle()
	defer handle.stdoutW.Close()
	defer handle.stderrW.Close()

	sess := &session.Session{
		ID:        "sess-recovered",
		State:     session.StateOrphaned,
		Replay:    session.NewReplayBuffer(1024),
		Handle:    handle,
		CreatedAt: time.Now(),
	}

	logPath := session.LogFilePath(logDir, sess.ID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("prior output\n"), 0o644); err != nil {
		t.Fatalf("write prior log: %v", err)
	}

	restoreRecoveredSessions(logDir, []*session.Session{sess})

	replayed, next := sess.Replay.ReadFrom(0)
	if string(replayed) != "prior output\n" {
		t.Fatalf("expected replay buffer restored from log file, got %q", string(replayed))
	}
	if next != int64(len("prior output\n")) {
		t.Fatalf("expected replay offset %d, got %d", len("prior output\n"), next)
	}

	go func() {
		_, _ = handle.stdoutW.Write([]byte("live output\n"))
		_ = handle.stdoutW.Close()
		_ = handle.stderrW.Close()
		handle.done <- runtime.ExitResult{Code: 0}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := sess.Replay.ReadFrom(0)
		if string(data) == "prior output\nlive output\n" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	data, _ := sess.Replay.ReadFrom(0)
	t.Fatalf("expected live output to append after recovery attach, got %q", string(data))
}
