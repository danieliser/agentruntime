package session

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	logFileExt               = ".ndjson"
	legacyLogFileExt         = ".jsonl"
	maxDiagnosticRecordBytes = 4 << 20
)

var diagnosticCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)("[^"]*(?:access[_-]?token|refresh[_-]?token|api[_-]?key|token|secret|password|authorization|cookie|credential)[^"]*"\s*:\s*")[^"]*(")`),
	regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|KEY|SECRET|PASSWORD|AUTH|COOKIE|CREDENTIAL)[A-Z0-9_]*=)[^\s"']+`),
}

// LogWriter writes all session output to a persistent NDJSON log file.
// It implements io.Writer so it can be composed with the ReplayBuffer
// via io.MultiWriter in the drain goroutine.
type LogWriter struct {
	file       *os.File
	path       string
	redactions [][]byte
	pending    []byte
	discarding bool
	mu         sync.Mutex
}

type logWriterConfig struct {
	redactions []string
}

// LogWriterOption configures diagnostic-only log behavior.
type LogWriterOption func(*logWriterConfig)

// WithDiagnosticRedactions removes exact prompt/credential values, their JSON
// string encodings, line values, and base64 encodings from the diagnostic
// mirror. The canonical replay/event path remains byte-exact.
func WithDiagnosticRedactions(values ...string) LogWriterOption {
	copyValues := append([]string(nil), values...)
	return func(config *logWriterConfig) {
		config.redactions = append(config.redactions, copyValues...)
	}
}

// NewLogWriter creates a log file at the given path. Creates parent dirs
// if needed. The file is opened in append mode — safe for daemon restarts.
func NewLogWriter(dir, sessionID string, options ...LogWriterOption) (*LogWriter, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure log dir: %w", err)
	}
	path := LogFilePath(dir, sessionID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure log file: %w", err)
	}
	config := logWriterConfig{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	return &LogWriter{file: f, path: path, redactions: diagnosticRedactionVariants(config.redactions)}, nil
}

// LogFilePath returns the canonical NDJSON log file path for a session.
func LogFilePath(dir, sessionID string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, sessionID+logFileExt)
}

// ExistingLogFilePath returns the current or legacy log path if one exists.
func ExistingLogFilePath(dir, sessionID string) (string, bool, error) {
	if dir == "" {
		return "", false, nil
	}
	paths := []string{
		LogFilePath(dir, sessionID),
		filepath.Join(dir, sessionID+legacyLogFileExt),
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path, true, nil
		} else if !os.IsNotExist(err) {
			return "", false, err
		}
	}
	return "", false, nil
}

// Write appends data to the log file.
func (l *LogWriter) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, fmt.Errorf("diagnostic log is closed")
	}
	l.pending = append(l.pending, p...)
	for {
		if l.discarding {
			newline := bytes.IndexByte(l.pending, '\n')
			if newline < 0 {
				l.pending = l.pending[:0]
				return len(p), nil
			}
			l.pending = l.pending[newline+1:]
			l.discarding = false
		}
		newline := bytes.IndexByte(l.pending, '\n')
		if newline < 0 {
			if len(l.pending) <= maxDiagnosticRecordBytes {
				return len(p), nil
			}
			if _, err := l.file.Write([]byte("{\"diagnostic\":\"redacted_oversize_record\"}\n")); err != nil {
				return 0, err
			}
			l.pending = l.pending[:0]
			l.discarding = true
			return len(p), nil
		}
		record := append([]byte(nil), l.pending[:newline+1]...)
		l.pending = l.pending[newline+1:]
		if len(record) > maxDiagnosticRecordBytes {
			record = []byte("{\"diagnostic\":\"redacted_oversize_record\"}\n")
		} else {
			record = l.sanitize(record)
		}
		if _, err := l.file.Write(record); err != nil {
			return 0, err
		}
	}
}

// Close flushes and closes the log file.
func (l *LogWriter) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	if len(l.pending) > 0 && !l.discarding {
		record := l.pending
		if len(record) > maxDiagnosticRecordBytes {
			record = []byte("{\"diagnostic\":\"redacted_oversize_record\"}\n")
		} else {
			record = l.sanitize(record)
		}
		if _, err := l.file.Write(record); err != nil {
			_ = l.file.Close()
			l.file = nil
			return err
		}
	}
	l.pending = nil
	err := l.file.Close()
	l.file = nil
	return err
}

// Path returns the absolute path to the log file.
func (l *LogWriter) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *LogWriter) sanitize(record []byte) []byte {
	result := append([]byte(nil), record...)
	for _, value := range l.redactions {
		result = bytes.ReplaceAll(result, value, []byte("[REDACTED]"))
	}
	for _, pattern := range diagnosticCredentialPatterns {
		result = pattern.ReplaceAll(result, []byte("${1}[REDACTED]${2}"))
	}
	return result
}

func diagnosticRedactionVariants(values []string) [][]byte {
	unique := make(map[string]struct{})
	add := func(value string) {
		if value != "" && value != "[REDACTED]" {
			unique[value] = struct{}{}
		}
	}
	for _, value := range values {
		add(value)
		for _, line := range strings.Split(value, "\n") {
			add(strings.TrimSpace(line))
		}
		if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
			add(string(encoded[1 : len(encoded)-1]))
		}
		if len(value) >= 8 {
			add(base64.StdEncoding.EncodeToString([]byte(value)))
			add(base64.RawStdEncoding.EncodeToString([]byte(value)))
		}
	}
	result := make([][]byte, 0, len(unique))
	for value := range unique {
		result = append(result, []byte(value))
	}
	sort.Slice(result, func(i, j int) bool { return len(result[i]) > len(result[j]) })
	return result
}

// SecureDiagnosticLogs tightens every retained regular diagnostic log to
// owner-only mode without following symlinks or touching unrelated files.
func SecureDiagnosticLogs(dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read diagnostic log directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			continue
		}
		extension := filepath.Ext(entry.Name())
		if extension != logFileExt && extension != legacyLogFileExt {
			continue
		}
		if err := os.Chmod(filepath.Join(dir, entry.Name()), 0o600); err != nil {
			return fmt.Errorf("secure diagnostic log %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// PruneDiagnosticLogs removes only expired regular diagnostic NDJSON/JSONL
// files. A zero retention keeps files indefinitely; a negative value is
// rejected. Symlinks and unrelated files are never followed or removed.
func PruneDiagnosticLogs(dir string, retention time.Duration, now time.Time) (int, error) {
	if dir == "" || retention == 0 {
		return 0, nil
	}
	if retention < 0 {
		return 0, fmt.Errorf("diagnostic log retention must be nonnegative")
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read diagnostic log directory: %w", err)
	}
	cutoff := now.Add(-retention)
	removed := 0
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			continue
		}
		extension := filepath.Ext(entry.Name())
		if extension != logFileExt && extension != legacyLogFileExt {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return removed, fmt.Errorf("inspect diagnostic log %s: %w", entry.Name(), err)
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return removed, fmt.Errorf("remove expired diagnostic log %s: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// DrainWriter returns an io.Writer that writes to both the replay buffer
// and the log file. Use this as the target for drain goroutines.
func DrainWriter(replay *ReplayBuffer, logw *LogWriter) io.Writer {
	if logw == nil {
		return replay
	}
	return io.MultiWriter(replay, logw)
}
