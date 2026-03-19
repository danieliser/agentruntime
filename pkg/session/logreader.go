package session

import (
	"fmt"
	"io"
	"os"
)

// ReadLogRange reads bytes from the session's NDJSON log file in [offset, endOffset).
// Returns the data read. If the file is shorter than endOffset, returns available data.
// Returns an error if the file cannot be opened or read.
func ReadLogRange(logDir, sessionID string, offset, endOffset int64) ([]byte, error) {
	logPath, exists, err := ExistingLogFilePath(logDir, sessionID)
	if err != nil {
		return nil, fmt.Errorf("log file lookup: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("log file not found for session %s", sessionID)
	}

	f, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()

	if offset < 0 {
		offset = 0
	}

	size := endOffset - offset
	if size <= 0 {
		return nil, nil
	}

	buf := make([]byte, size)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return buf[:n], fmt.Errorf("read log file: %w", err)
	}
	return buf[:n], nil
}
