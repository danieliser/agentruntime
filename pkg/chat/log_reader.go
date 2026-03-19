package chat

import (
	"bufio"
	"encoding/json"
	"os"
	"time"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/session"
)

// LogReader reads NDJSON event log files across a session chain and returns
// filtered, paginated events with stable cross-session global offsets.
type LogReader struct {
	logDir string
}

// NewLogReader creates a LogReader that reads from the given log directory.
func NewLogReader(logDir string) *LogReader {
	return &LogReader{logDir: logDir}
}

// ReadMessages reads events from the session chain in order (oldest session first).
// Global offset: chainIndex*1e9 + lineOffset within that session's log file.
//
//   - types: event types to include; nil means all types.
//   - before: skip events with globalOffset >= before; 0 means no cursor (return all).
//   - limit: max entries to return; 0 means no limit.
//
// Returns entries, hasMore (true when additional events exist beyond limit), and any
// error encountered. Missing log files are silently skipped.
func (r *LogReader) ReadMessages(
	chain []string,
	limit int,
	before int64,
	types []string,
) ([]apischema.ChatMessageEntry, bool, error) {
	if len(chain) == 0 {
		return nil, false, nil
	}

	typeSet := make(map[string]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}
	filterByType := len(types) > 0

	var entries []apischema.ChatMessageEntry
	cap := limit + 1 // collect one extra to detect hasMore

	for chainIndex, sessID := range chain {
		logPath, exists, err := session.ExistingLogFilePath(r.logDir, sessID)
		if err != nil || !exists {
			continue
		}

		f, err := os.Open(logPath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var lineOffset int64
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				lineOffset++
				continue
			}

			globalOffset := int64(chainIndex)*1_000_000_000 + lineOffset
			lineOffset++

			// Apply before cursor: skip events at or past the cursor.
			if before > 0 && globalOffset >= before {
				continue
			}

			var evt map[string]json.RawMessage
			if err := json.Unmarshal(line, &evt); err != nil {
				continue
			}

			var eventType string
			if raw, ok := evt["type"]; ok {
				_ = json.Unmarshal(raw, &eventType)
			}

			if filterByType && !typeSet[eventType] {
				continue
			}

			var ts time.Time
			if raw, ok := evt["timestamp"]; ok {
				var tsMillis float64
				if json.Unmarshal(raw, &tsMillis) == nil {
					ts = time.UnixMilli(int64(tsMillis))
				}
			}

			data := json.RawMessage("{}")
			if raw, ok := evt["data"]; ok {
				data = raw
			}

			entries = append(entries, apischema.ChatMessageEntry{
				SessionID: sessID,
				Type:      eventType,
				Data:      data,
				Offset:    globalOffset,
				Timestamp: ts,
			})

			// Stop early once we have limit+1: enough to detect hasMore.
			if limit > 0 && len(entries) >= cap {
				f.Close()
				return entries[:limit], true, nil
			}
		}
		f.Close()
	}

	return entries, false, nil
}
