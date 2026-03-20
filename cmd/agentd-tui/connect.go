package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

// chatMeta holds metadata about the connected chat/session.
type chatMeta struct {
	Name      string         // chat name (empty if raw session)
	SessionID string
	Agent     string         // "claude", "codex", etc.
	State     string         // "running", "idle", etc.
	History   []chatMessage  // prior conversation messages
}

type connectOpts struct {
	create      bool
	agent       string
	idleTimeout string
}

// connect resolves the target (chat name or session ID) and opens a WS connection.
func connect(target string, port int, noReplay bool, opts connectOpts) (*websocket.Conn, chatMeta, error) {
	meta := chatMeta{}

	// Try to resolve as a chat name first.
	chatResp, err := getChat(port, target)
	if err == nil {
		meta.Name = chatResp.Name
		meta.Agent = chatResp.Config.Agent
		meta.State = chatResp.State

		if chatResp.State == "idle" || chatResp.State == "created" || (chatResp.State == "running" && chatResp.CurrentSession != "") {
			// Attach via the chat manager — spawns interactive session if needed,
			// or returns the running session. Tracks lifecycle and resume.
			sid, attachErr := attachChat(port, target)
			if attachErr != nil {
				return nil, meta, fmt.Errorf("attach: %w", attachErr)
			}
			meta.SessionID = sid
			meta.State = "running"
		} else {
			return nil, meta, fmt.Errorf("chat %q is in state %q", target, chatResp.State)
		}
	} else if isUUID(target) {
		// Looks like a raw session ID.
		meta.SessionID = target
	} else if opts.create {
		// Auto-create the chat.
		if err := createChat(port, target, opts.agent, opts.idleTimeout); err != nil {
			return nil, meta, fmt.Errorf("create chat: %w", err)
		}
		meta.Name = target
		meta.Agent = opts.agent
		meta.State = "created"
		// Attach via chat manager.
		sid, attachErr := attachChat(port, target)
		if attachErr != nil {
			return nil, meta, fmt.Errorf("attach: %w", attachErr)
		}
		meta.SessionID = sid
		meta.State = "running"
	} else {
		return nil, meta, fmt.Errorf("chat %q not found. Create it with --create or:\n  agentd chat create %s --agent claude", target, target)
	}

	// Connect to the session WS.
	// Load conversation history from prior sessions' logs.
	if meta.Name != "" && !noReplay {
		meta.History = loadChatHistory(port, meta.Name)
	}

	// Connect WS — skip replay since we loaded history from logs.
	wsURL := fmt.Sprintf("ws://localhost:%d/ws/sessions/%s", port, meta.SessionID)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, meta, fmt.Errorf("connect WS: %w", err)
	}

	return conn, meta, nil
}

type chatAPIResponse struct {
	Name             string            `json:"name"`
	State            string            `json:"state"`
	CurrentSession   string            `json:"current_session"`
	SessionChain     []string          `json:"session_chain"`
	ClaudeSessionIDs map[string]string `json:"claude_session_ids"`
	Config           struct {
		Agent string `json:"agent"`
	} `json:"config"`
}

func getChat(port int, name string) (*chatAPIResponse, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/chats/%s", port, url.PathEscape(name)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	var cr chatAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	return &cr, nil
}

func createChat(port int, name, agent, idleTimeout string) error {
	body := fmt.Sprintf(`{"name":%q,"config":{"agent":%q,"idle_timeout":%q}}`, name, agent, idleTimeout)
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/chats", port),
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 409 {
		return nil // already exists, that's fine
	}
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}

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

// chatMessage is a message from GET /chats/:name/messages.
type chatMessage struct {
	SessionID string                 `json:"session_id"`
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
	Offset    int64                  `json:"offset"`
	Timestamp int64                  `json:"timestamp"`
}

// loadChatHistory fetches prior conversation messages for display on connect.
func loadChatHistory(port int, name string) []chatMessage {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/chats/%s/messages?limit=100", port, url.PathEscape(name)))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var result struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	return result.Messages
}

// attachChat calls POST /chats/:name/attach to spawn (or reuse) an interactive
// session through the chat manager. Tracks lifecycle and resume.
func attachChat(port int, name string) (string, error) {
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/chats/%s/attach", port, url.PathEscape(name)),
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.SessionID == "" {
		return "", fmt.Errorf("no session_id in response")
	}
	return result.SessionID, nil
}
