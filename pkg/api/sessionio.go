package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"sync"

	"github.com/danieliser/agentruntime/pkg/session"
)

// AttachSessionIO starts stdout/stderr drain goroutines and an exit watcher for
// a session handle, mirroring the normal create-session lifecycle.
func AttachSessionIO(sess *session.Session, logDir string) {
	if sess == nil || sess.Handle == nil {
		return
	}

	logw, err := session.NewLogWriter(logDir, sess.ID)
	if err != nil {
		log.Printf("[session %s] warning: log file creation failed: %v", sess.ID, err)
	}
	drainTarget := session.DrainWriter(sess.Replay, logw)

	handle := sess.Handle
	stdout := handle.Stdout()
	stderr := handle.Stderr()

	var drainWg sync.WaitGroup
	if stdout != nil {
		drainWg.Add(1)
		go func() {
			defer drainWg.Done()
			drainTo(sess, "stdout", stdout, drainTarget)
		}()
	}
	if stderr != nil {
		drainWg.Add(1)
		go func() {
			defer drainWg.Done()
			drainTo(sess, "stderr", stderr, drainTarget)
		}()
	}

	go func() {
		result := <-handle.Wait()
		drainWg.Wait()
		sess.Replay.Close()
		if logw != nil {
			if err := logw.Close(); err != nil {
				log.Printf("[session %s] warning: close log failed: %v", sess.ID, err)
			} else {
				log.Printf("[session %s] log saved: %s", sess.ID, logw.Path())
			}
		}
		log.Printf("[session %s] exited: code=%d err=%v replay_bytes=%d", sess.ID, result.Code, result.Err, sess.Replay.TotalBytes())
		sess.SetCompleted(result.Code)
	}()
}

// drainTo reads from r and writes all data to w (typically a MultiWriter
// wrapping both the replay buffer and a log file). It parses NDJSON events
// and tracks metrics on the session.
func drainTo(sess *session.Session, stream string, r io.ReadCloser, w io.Writer) {
	if r == nil || w == nil {
		return
	}
	sessionID := sess.ID
	buf := make([]byte, 32*1024)
	total := 0
	var lineBuf bytes.Buffer // buffer for accumulating a complete line

	for {
		n, err := r.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			total += n
			if total == n {
				log.Printf("[session %s] first %s data: %d bytes", sessionID, stream, n)
			}

			// Parse lines from the buffer for event tracking.
			// Events are NDJSON (one JSON object per line).
			lineBuf.Write(buf[:n])
			for {
				// Find the next newline in the line buffer.
				idx := bytes.IndexByte(lineBuf.Bytes(), '\n')
				if idx < 0 {
					break // incomplete line, wait for more data
				}

				// Extract and parse the complete line.
				line := lineBuf.Bytes()[:idx]
				if len(line) > 0 {
					parseAndTrackEvent(sess, line)
				}

				// Remove the processed line from the buffer.
				lineBuf.Next(idx + 1)
			}
		}
		if err != nil {
			log.Printf("[session %s] %s closed: total=%d err=%v", sessionID, stream, total, err)
			// Parse any remaining partial line (shouldn't happen with well-formed output).
			if lineBuf.Len() > 0 {
				parseAndTrackEvent(sess, lineBuf.Bytes())
			}
			return
		}
	}
}

// parseAndTrackEvent scans an NDJSON line for event type and updates session metrics.
// Performs minimal parsing — just enough to detect type and extract usage fields.
func parseAndTrackEvent(sess *session.Session, line []byte) {
	if sess == nil || len(line) == 0 {
		return
	}

	// Parse minimal event structure to get type.
	var event map[string]interface{}
	if err := json.Unmarshal(line, &event); err != nil {
		return // Not JSON, skip
	}

	eventType, ok := event["type"].(string)
	if !ok {
		return
	}

	// Any event triggers activity.
	sess.RecordActivity()

	// Track specific event types.
	switch eventType {
	case "tool_use":
		sess.RecordToolCall()
	case "result":
		// Extract usage and cost from result event.
		if data, ok := event["data"].(map[string]interface{}); ok {
			var inputToks, outputToks int
			var costUSD float64
			if usage, ok := data["usage"].(map[string]interface{}); ok {
				if v, ok := usage["input_tokens"].(float64); ok {
					inputToks = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok {
					outputToks = int(v)
				}
			}
			if v, ok := data["cost_usd"].(float64); ok {
				costUSD = v
			}
			// Fall back to token-based estimate if agent didn't report cost.
			if costUSD == 0 && (inputToks > 0 || outputToks > 0) {
				// Try model from result data, then session agent name.
				model, _ := data["model"].(string)
				if model == "" {
					model = sess.AgentName
				}
				costUSD = session.EstimateCost(model, inputToks, outputToks)
			}
			sess.RecordUsage(inputToks, outputToks, costUSD)
			// Capture Claude session ID for resume across respawns.
			if sessionID, ok := data["session_id"].(string); ok && sessionID != "" {
				sess.SetTag("claude_session_id", sessionID)
			}
		}
	}
}
