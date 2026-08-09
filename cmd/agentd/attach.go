package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

var attachHTTPClient = &http.Client{Timeout: 15 * time.Second}

func runAttachCommand(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	port := fs.Int("port", 8090, "Daemon port")
	since := fs.Int64("since", 0, "Durable event sequence (default 0 = full history)")
	noReplay := fs.Bool("no-replay", false, "Skip replay and only show live output")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: agentd attach <session-id> [options]\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if fs.NArg() < 1 {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "attach: session ID is required")
		return 2
	}

	sessionID := fs.Arg(0)

	// If the argument doesn't look like a UUID, try to resolve it as a chat name.
	if !isUUID(sessionID) {
		chatResp, err := resolveChatSession(sessionID, *port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "attach: %v\n", err)
			return 1
		}
		sessionID = chatResp.CurrentSession
	}

	if err := attach(sessionID, *port, *since, *noReplay); err != nil {
		fmt.Fprintf(os.Stderr, "attach: %v\n", err)
		return 1
	}

	return 0
}

func attach(sessionID string, port int, since int64, noReplay bool, stdinOverride ...*os.File) error {
	stdinFile := os.Stdin
	if len(stdinOverride) > 0 && stdinOverride[0] != nil {
		stdinFile = stdinOverride[0]
	}
	afterSequence := since
	if noReplay {
		var err error
		afterSequence, err = currentSessionSequence(port, sessionID)
		if err != nil {
			return err
		}
	}
	q := url.Values{}
	q.Set("after_sequence", fmt.Sprintf("%d", afterSequence))
	wsURL := fmt.Sprintf("ws://localhost:%d/api/v1/ws/sessions/%s/events?%s", port, url.PathEscape(sessionID), q.Encode())

	// Connect to WebSocket
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", wsURL, err)
	}
	defer conn.Close()

	// Setup signal handling for Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Channel to signal interrupt sent (for Ctrl+C twice)
	interruptSent := false

	// Read loop: receive from WebSocket and print to terminal
	readDone := make(chan error, 1)
	go func() {
		var replayThrough int64
		for {
			var frame EventStreamFrame
			if err := conn.ReadJSON(&frame); err != nil {
				readDone <- err
				return
			}
			if frame.FrameType == "stream.ready" {
				replayThrough = frame.ReplayThrough
			}
			if err := handleEventStreamFrame(&frame, replayThrough); err != nil {
				if errors.Is(err, errSessionExit) {
					readDone <- nil
				} else {
					readDone <- err
				}
				return
			}
		}
	}()

	// Write loop: read from stdin and send to WebSocket
	writeDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdinFile)
		for scanner.Scan() {
			line := scanner.Text()

			kind := "prompt"
			text := line
			operation := "input"
			if strings.HasPrefix(line, "/steer ") {
				kind = "steer"
				text = strings.TrimPrefix(line, "/steer ")
			} else if strings.HasPrefix(line, "/interrupt") {
				operation = "interrupt"
				text = ""
			}
			if err := sendAttachControl(port, sessionID, operation, kind, text); err != nil {
				writeDone <- err
				return
			}
		}

		if err := scanner.Err(); err != nil {
			writeDone <- fmt.Errorf("read stdin: %w", err)
			return
		}

		// stdin closed, close the write side
		writeDone <- nil
	}()

	// Main loop: wait for signals, read, write, or exit
	for {
		select {
		case <-sigCh:
			if !interruptSent {
				if err := sendAttachControl(port, sessionID, "interrupt", "", ""); err != nil {
					return err
				}
				interruptSent = true
				fmt.Fprintf(os.Stderr, "\nsent interrupt, Ctrl+C again to detach\n")
			} else {
				// Second Ctrl+C: disconnect
				_ = conn.Close()
				return nil
			}

		case err := <-readDone:
			if err != nil && !websocket.IsCloseError(err, websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				return fmt.Errorf("websocket read: %w", err)
			}
			return nil

		case err := <-writeDone:
			if err != nil {
				return err
			}
			// stdin closed, wait for session to exit
			<-readDone
			return nil
		}
	}
}

type EventStreamFrame struct {
	FrameType     string          `json:"frame_type,omitempty"`
	SchemaVersion string          `json:"schema_version,omitempty"`
	EventID       string          `json:"event_id,omitempty"`
	SessionID     string          `json:"session_id,omitempty"`
	Generation    int64           `json:"generation,omitempty"`
	Sequence      int64           `json:"sequence,omitempty"`
	Type          string          `json:"type,omitempty"`
	Stream        string          `json:"stream,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	RawBase64     string          `json:"raw_base64,omitempty"`
	ReplayThrough int64           `json:"replay_through,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
}

func handleEventStreamFrame(frame *EventStreamFrame, replayThrough int64) error {
	if frame.FrameType == "stream.ready" {
		fmt.Fprintf(os.Stderr, "Connected to session %s at sequence %d\n", frame.SessionID, replayThrough)
		fmt.Fprintln(os.Stderr, "Type a follow-up. /steer redirects the active turn. /interrupt stops it. Ctrl+C detaches.")
		return nil
	}
	if frame.FrameType == "error" {
		return fmt.Errorf("event stream: %s", strings.TrimSpace(string(frame.Error)))
	}
	if frame.EventID == "" || frame.Sequence < 1 {
		return fmt.Errorf("event stream returned an invalid frame")
	}
	isReplay := frame.Sequence <= replayThrough
	var payload map[string]any
	_ = json.Unmarshal(frame.Payload, &payload)
	prefix := ""
	if isReplay {
		prefix = "[replay] "
	}
	switch frame.Type {
	case "content.delta", "content.completed":
		if text, ok := payload["text"].(string); ok && text != "" {
			fmt.Fprintf(os.Stdout, "%s%s", prefix, text)
			if frame.Type == "content.completed" {
				fmt.Fprintln(os.Stdout)
			}
		}
	case "tool.call":
		fmt.Fprintf(os.Stdout, "\n%s[tool] %s\n", prefix, compactJSON(frame.Payload))
	case "tool.result":
		fmt.Fprintf(os.Stdout, "%s[result] %s\n", prefix, compactJSON(frame.Payload))
	case "error.provider", "turn.failed":
		fmt.Fprintf(os.Stderr, "%s[error] %s\n", prefix, compactJSON(frame.Payload))
	}
	if frame.Stream == "runtime_stderr" && frame.RawBase64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(frame.RawBase64); err == nil {
			fmt.Fprintf(os.Stderr, "%s%s\n", prefix, raw)
		}
	}
	if frame.Stream == "terminal" || strings.HasPrefix(frame.Type, "session.") {
		exitCode, _ := payload["exit_code"].(float64)
		fmt.Fprintf(os.Stderr, "\nSession %s with code %d at sequence %d\n", strings.TrimPrefix(frame.Type, "session."), int(exitCode), frame.Sequence)
		return errSessionExit
	}
	return nil
}

func compactJSON(raw json.RawMessage) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buffer.String()
}

func sendAttachControl(port int, sessionID, operation, kind, text string) error {
	body := map[string]any{"idempotency_key": "attach-" + uuid.NewString()}
	if operation == "input" {
		body["kind"], body["text"] = kind, text
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("http://localhost:%d/api/v1/sessions/%s/%s", port, url.PathEscape(sessionID), operation)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := attachHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("send %s: %w", operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("send %s: status %d: %s", operation, response.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

func currentSessionSequence(port int, sessionID string) (int64, error) {
	endpoint := fmt.Sprintf("http://localhost:%d/api/v1/sessions/%s", port, url.PathEscape(sessionID))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("inspect session: %w", err)
	}
	response, err := attachHTTPClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("inspect session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("inspect session: status %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			LastSequence int64 `json:"last_sequence"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return 0, fmt.Errorf("inspect session: %w", err)
	}
	return envelope.Data.LastSequence, nil
}

var errSessionExit = errors.New("session exited")

// isUUID reports whether s looks like a UUID (8-4-4-4-12 hex, 36 chars with hyphens).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// resolveChatSession calls GET /chats/{name} and returns the chat record.
// Returns an error if the chat is not in running state or has no current session.
func resolveChatSession(name string, port int) (*apischema.ChatResponse, error) {
	resp, err := chatGet(port, "/chats/"+name)
	if err != nil {
		return nil, fmt.Errorf("resolve chat %q: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("chat %q not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("resolve chat %q: server error %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var chatResp apischema.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("decode chat %q: %w", name, err)
	}
	if chatResp.State != "running" {
		return nil, fmt.Errorf("chat %q is not running (state: %s)", name, chatResp.State)
	}
	if chatResp.CurrentSession == "" {
		return nil, fmt.Errorf("chat %q is running but has no current session", name)
	}
	return &chatResp, nil
}

// printNDJSON parses and pretty-prints NDJSON event data.
// If isReplay is true, output is dimmed (for history).
func printNDJSON(data string, isReplay bool) {
	lines := strings.Split(strings.TrimSpace(data), "\n")

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// Not JSON, print raw
			if isReplay {
				fmt.Fprintf(os.Stdout, "[replay] %s\n", line)
			} else {
				fmt.Fprintf(os.Stdout, "%s\n", line)
			}
			continue
		}

		eventType, ok := event["type"].(string)
		if !ok {
			continue
		}

		data, ok := event["data"].(map[string]interface{})
		if !ok {
			continue
		}

		prefix := ""
		if isReplay {
			prefix = "[replay] "
		}

		switch eventType {
		case "agent_message":
			if text, ok := data["text"].(string); ok {
				fmt.Fprintf(os.Stdout, "%s%s\n", prefix, text)
			}

		case "tool_use":
			if name, ok := data["name"].(string); ok {
				fmt.Fprintf(os.Stdout, "%s[tool] %s\n", prefix, name)
			}

		case "tool_result":
			if name, ok := data["name"].(string); ok {
				fmt.Fprintf(os.Stdout, "%s[result] %s\n", prefix, name)
			}

		case "error":
			if detail, ok := data["error_detail"].(string); ok && detail != "" {
				fmt.Fprintf(os.Stderr, "%s[error] %s\n", prefix, detail)
			}

		case "system":
			if text, ok := data["text"].(string); ok {
				fmt.Fprintf(os.Stderr, "%s[system] %s\n", prefix, text)
			}

		case "progress":
			if text, ok := data["text"].(string); ok {
				fmt.Fprintf(os.Stderr, "%s[progress] %s\n", prefix, text)
			}

		case "result":
			// Result event at session end
			if status, ok := data["status"].(string); ok {
				fmt.Fprintf(os.Stderr, "%s[result] status=%s\n", prefix, status)
			}
		}
	}
}
