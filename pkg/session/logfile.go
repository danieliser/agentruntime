package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LogWriter writes all session output to a persistent NDJSON log file.
// It implements io.Writer so it can be composed with the ReplayBuffer
// via io.MultiWriter in the drain goroutine.
type LogWriter struct {
	file *os.File
	path string
}

// NewLogWriter creates a log file at the given path. Creates parent dirs
// if needed. The file is opened in append mode — safe for daemon restarts.
func NewLogWriter(dir, sessionID string) (*LogWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return &LogWriter{file: f, path: path}, nil
}

// Write appends data to the log file.
func (l *LogWriter) Write(p []byte) (int, error) {
	return l.file.Write(p)
}

// Close flushes and closes the log file.
func (l *LogWriter) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Path returns the absolute path to the log file.
func (l *LogWriter) Path() string {
	return l.path
}

// DrainWriter returns an io.Writer that writes to both the replay buffer
// and the log file. Use this as the target for drain goroutines.
func DrainWriter(replay *ReplayBuffer, logw *LogWriter) io.Writer {
	if logw == nil {
		return replay
	}
	return io.MultiWriter(replay, logw)
}
