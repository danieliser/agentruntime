package session

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// ReadLogRange reads [offset,endOffset) across the concatenated diagnostic logs
// of every generation. Offsets remain session-wide when another wake appends a log.
func ReadLogRange(logDir, sessionID string, offset, endOffset int64) ([]byte, error) {
	paths, err := SessionLogPaths(logDir, sessionID)
	if err != nil {
		return nil, fmt.Errorf("log file lookup: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("log file not found for session %s", sessionID)
	}
	if offset < 0 {
		offset = 0
	}
	if endOffset <= offset {
		return nil, nil
	}
	remaining := endOffset - offset
	var result bytes.Buffer
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return result.Bytes(), fmt.Errorf("open log file: %w", err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return result.Bytes(), err
		}
		if offset >= info.Size() {
			offset -= info.Size()
			f.Close()
			continue
		}
		size := min(remaining, info.Size()-offset)
		_, err = io.CopyN(&result, io.NewSectionReader(f, offset, size), size)
		closeErr := f.Close()
		if err != nil {
			return result.Bytes(), fmt.Errorf("read log file: %w", err)
		}
		if closeErr != nil {
			return result.Bytes(), closeErr
		}
		remaining -= size
		offset = 0
		if remaining == 0 {
			break
		}
	}
	return result.Bytes(), nil
}
