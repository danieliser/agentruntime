package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// chatMeta holds metadata about the connected chat/session.
type chatMeta struct {
	Name         string // chat name (empty if raw session)
	SessionID    string
	SessionChain []string
	Agent        string        // "claude", "codex", etc.
	State        string        // "running", "idle", etc.
	History      []chatMessage // prior conversation messages
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
		meta.SessionChain = append([]string(nil), chatResp.SessionChain...)

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
		inspected, inspectErr := inspectDurableSession(port, target)
		if inspectErr != nil {
			return nil, meta, fmt.Errorf("inspect session: %w", inspectErr)
		}
		meta.Agent = inspected.Agent
		meta.State = inspected.State
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

	afterSequence := int64(0)
	// Load chat history from the same durable event ledgers used for live
	// streaming, then continue the active session after its loaded tail.
	if meta.Name != "" && !noReplay {
		meta.History, afterSequence = loadChatHistory(port, meta.SessionChain, meta.SessionID)
	}
	if noReplay {
		inspected, inspectErr := inspectDurableSession(port, meta.SessionID)
		if inspectErr != nil {
			return nil, meta, fmt.Errorf("inspect replay boundary: %w", inspectErr)
		}
		afterSequence = inspected.LastSequence
	}
	wsURL := eventStreamURL(port, meta.SessionID, afterSequence)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, meta, fmt.Errorf("connect WS: %w", err)
	}

	return conn, meta, nil
}

type durableSessionResponse struct {
	SessionID    string `json:"session_id"`
	Agent        string `json:"agent"`
	State        string `json:"state"`
	LastSequence int64  `json:"last_sequence"`
}

func inspectDurableSession(port int, sessionID string) (durableSessionResponse, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/api/v1/sessions/%s", port, url.PathEscape(sessionID)))
	if err != nil {
		return durableSessionResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return durableSessionResponse{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	var envelope struct {
		Data durableSessionResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return durableSessionResponse{}, err
	}
	return envelope.Data, nil
}

func eventStreamURL(port int, sessionID string, afterSequence int64) string {
	return fmt.Sprintf("ws://localhost:%d/api/v1/ws/sessions/%s/events?after_sequence=%d",
		port, url.PathEscape(sessionID), afterSequence)
}

func sendSessionInput(port int, sessionID, kind, text string) error {
	return postDurableControl(port, sessionID, "input", map[string]string{
		"idempotency_key": "tui-input:" + uuid.NewString(), "kind": kind, "text": text,
	})
}

func interruptSession(port int, sessionID string) error {
	return postDurableControl(port, sessionID, "interrupt", map[string]string{
		"idempotency_key": "tui-interrupt:" + uuid.NewString(),
	})
}

func postDurableControl(port int, sessionID, operation string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/api/v1/sessions/%s/%s", port, url.PathEscape(sessionID), operation),
		"application/json", bytes.NewReader(encoded),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, responseBody)
	}
	return nil
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

// chatMessage is the TUI's renderable projection of durable native events.
type chatMessage struct {
	SessionID string                 `json:"session_id"`
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
	Offset    int64                  `json:"offset"`
	Timestamp int64                  `json:"timestamp"`
}

// loadChatHistory derives the renderable conversation from immutable v1
// events. The returned cursor belongs only to the current active session.
func loadChatHistory(port int, chain []string, currentSessionID string) ([]chatMessage, int64) {
	ids := append([]string(nil), chain...)
	if currentSessionID != "" && !containsString(ids, currentSessionID) {
		ids = append(ids, currentSessionID)
	}
	messages := make([]chatMessage, 0)
	var currentCursor int64
	for _, sessionID := range ids {
		cursor := int64(0)
		var deltas strings.Builder
		flushDeltas := func() {
			if deltas.Len() == 0 {
				return
			}
			messages = append(messages, chatMessage{
				SessionID: sessionID, Type: "agent_message",
				Data: map[string]interface{}{"text": deltas.String()}, Offset: cursor,
			})
			deltas.Reset()
		}
		for {
			path := fmt.Sprintf("http://localhost:%d/api/v1/sessions/%s/events?after_sequence=%d&limit=1000",
				port, url.PathEscape(sessionID), cursor)
			resp, err := http.Get(path)
			if err != nil {
				break
			}
			var envelope struct {
				Data struct {
					Events  []durableEvent `json:"events"`
					HasMore bool           `json:"has_more"`
				} `json:"data"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || decodeErr != nil {
				break
			}
			for _, event := range envelope.Data.Events {
				cursor = event.Sequence
				if event.Type == "content.delta" {
					text, _ := event.Payload["text"].(string)
					deltas.WriteString(text)
					continue
				}
				flushDeltas()
				mapped, ok := mapDurableEvent(event)
				if ok {
					messages = append(messages, chatMessage{
						SessionID: sessionID, Type: mapped.Type, Data: mapped.Data,
						Offset: event.Sequence, Timestamp: event.Timestamp.UnixMilli(),
					})
				}
			}
			if !envelope.Data.HasMore {
				break
			}
		}
		flushDeltas()
		if sessionID == currentSessionID {
			currentCursor = cursor
		}
	}
	return messages, currentCursor
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
