package main

import (
	"bufio"
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

	"github.com/gorilla/websocket"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
)

func runAttachCommand(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	port := fs.Int("port", 8090, "Daemon port")
	since := fs.Int64("since", 0, "Replay offset (default 0 = full history)")
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
	wsURL := fmt.Sprintf("ws://localhost:%d/ws/sessions/%s", port, sessionID)

	// Add replay offset unless --no-replay is set
	if !noReplay {
		q := url.Values{}
		q.Set("since", fmt.Sprintf("%d", since))
		wsURL += "?" + q.Encode()
	}

	// Connect to WebSocket
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", wsURL, err)
	}
	defer conn.Close()

	// Setup signal handling for Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Channel to signal interrupt sent (for Ctrl+C twice)
	interruptSent := false

	// Read loop: receive from WebSocket and print to terminal
	readDone := make(chan error, 1)
	go func() {
		for {
			var frame ServerFrame
			if err := conn.ReadJSON(&frame); err != nil {
				readDone <- err
				return
			}

			if err := handleServerFrame(&frame); err != nil {
				readDone <- err
				return
			}

			if frame.Type == "exit" {
				readDone <- nil
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

			var clientFrame ClientFrame

			// Check for special commands
			if strings.HasPrefix(line, "/steer ") {
				clientFrame.Type = "steer"
				clientFrame.Data = strings.TrimPrefix(line, "/steer ")
			} else if strings.HasPrefix(line, "/interrupt") {
				clientFrame.Type = "interrupt"
			} else {
				// Regular stdin
				clientFrame.Type = "stdin"
				clientFrame.Data = line + "\n"
			}

			if err := conn.WriteJSON(clientFrame); err != nil {
				writeDone <- fmt.Errorf("send stdin: %w", err)
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
				// First Ctrl+C: send interrupt frame
				_ = conn.WriteJSON(ClientFrame{Type: "interrupt"})
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

// ServerFrame is received from the daemon WebSocket.
type ServerFrame struct {
	Type      string `json:"type"`
	Data      string `json:"data,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Error     string `json:"error,omitempty"`
	Gap       bool   `json:"gap,omitempty"`
	Recovered bool   `json:"recovered,omitempty"`
}

// ClientFrame is sent to the daemon WebSocket.
type ClientFrame struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

// handleServerFrame processes a frame from the server and prints to terminal.
func handleServerFrame(frame *ServerFrame) error {
	switch frame.Type {
	case "connected":
		fmt.Fprintf(os.Stderr, "Connected to session %s\n", frame.SessionID)
		fmt.Fprintf(os.Stderr, "Type to send stdin. /steer to redirect. /interrupt to stop. Ctrl+C to detach.\n\n")

	case "replay":
		data, err := base64.StdEncoding.DecodeString(frame.Data)
		if err == nil {
			printNDJSON(string(data), true)
		}

	case "stdout":
		data, err := base64.StdEncoding.DecodeString(frame.Data)
		if err == nil {
			printNDJSON(string(data), false)
		}

	case "error":
		fmt.Fprintf(os.Stderr, "[error] %s\n", frame.Error)

	case "exit":
		if frame.ExitCode != nil {
			fmt.Fprintf(os.Stderr, "\nSession exited with code %d\n", *frame.ExitCode)
		}
		return errSessionExit

	case "pong":
		// Ignore keepalive pongs

	default:
		// Ignore unknown frame types
	}

	return nil
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
