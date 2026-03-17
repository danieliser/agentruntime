package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// dashSession tracks a single agent session with timing, events, and WS state.
type dashSession struct {
	ID           string     `json:"id"`
	Agent        string     `json:"agent"`
	Mode         string     `json:"mode"`
	Status       string     `json:"status"`
	Events       int        `json:"events"`
	InputTokens  int        `json:"input_tokens"`
	OutputTokens int        `json:"output_tokens"`
	LastEvent    string     `json:"last_event"`
	LastText     string     `json:"last_text"`
	WSOpen       bool       `json:"ws_open"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	SpawnedAt    time.Time  `json:"spawned_at"`
	ConnectedAt  *time.Time `json:"connected_at,omitempty"`
	FirstTokenAt *time.Time `json:"first_token_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	BootMs       int64      `json:"boot_ms"`
	TtftMs       int64      `json:"ttft_ms"`
	DurationMs   int64      `json:"duration_ms"`

	config     map[string]any    `json:"-"`
	prompt     string            `json:"-"`
	conn       *websocket.Conn   `json:"-"`
	connMu     sync.Mutex        `json:"-"`
	eventBuf   []json.RawMessage `json:"-"`
	eventMu    sync.Mutex        `json:"-"`
	generation int               `json:"-"`
}

var (
	mu         sync.RWMutex
	sessions   []*dashSession
	sessionGen atomic.Int64
	daemonURL  string
	workDir    string
)

func main() {
	daemon := flag.String("daemon", "http://127.0.0.1:8090", "agentd base URL")
	port := flag.Int("port", 3030, "dashboard port")
	wd := flag.String("work-dir", "", "work_dir for sessions (default: cwd)")
	flag.Parse()

	daemonURL = *daemon
	if *wd == "" {
		cwd, _ := os.Getwd()
		workDir = cwd
	} else {
		workDir = *wd
	}

	resp, err := http.Get(daemonURL + "/health")
	if err != nil {
		log.Fatalf("daemon not reachable at %s: %v", daemonURL, err)
	}
	resp.Body.Close()

	log.Printf("dashboard at http://127.0.0.1:%d — daemon at %s", *port, daemonURL)

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/sessions", handleSessions)
	http.HandleFunc("/api/spawn", handleSpawn)
	http.HandleFunc("/api/steer", handleSteer)
	http.HandleFunc("/api/kill", handleKill)
	http.HandleFunc("/api/kill-all", handleKillAll)
	http.HandleFunc("/api/retry", handleRetry)
	http.HandleFunc("/api/resume", handleResume)
	http.HandleFunc("/api/events", handleEvents)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

// --- Spawn ---

func handleSpawn(w http.ResponseWriter, r *http.Request) {
	// Accept JSON body or query params
	var req struct {
		Agent      string            `json:"agent"`
		Mode       string            `json:"mode"`
		Count      int               `json:"count"`
		Prompt     string            `json:"prompt"`
		Env        map[string]string `json:"env,omitempty"`
		MCPServers []any             `json:"mcp_servers,omitempty"`
		ClaudeMD   string            `json:"claude_md,omitempty"`
		Container  map[string]any    `json:"container,omitempty"`
	}

	if r.Header.Get("Content-Type") == "application/json" {
		json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Agent == "" {
		req.Agent = r.URL.Query().Get("agent")
	}
	if req.Agent == "" {
		req.Agent = "claude"
	}
	if req.Mode == "" {
		req.Mode = r.URL.Query().Get("mode")
	}
	if req.Mode == "" {
		req.Mode = "prompt"
	}
	if req.Count == 0 {
		fmt.Sscanf(r.URL.Query().Get("count"), "%d", &req.Count)
	}
	if req.Count < 1 {
		req.Count = 1
	}
	if req.Prompt == "" {
		req.Prompt = r.URL.Query().Get("prompt")
	}
	if req.Prompt == "" {
		req.Prompt = "Count from 1 to 60, printing one number per line. Sleep 1 second between each number. Use the Bash tool to run: for i in $(seq 1 60); do echo $i; sleep 1; done"
	}

	interactive := req.Mode == "interactive"
	var spawned []string

	for i := 0; i < req.Count; i++ {
		sess := spawnOne(req.Agent, req.Mode, interactive, req.Prompt, req.Env, req.MCPServers, req.ClaudeMD, req.Container)
		if sess != nil {
			spawned = append(spawned, sess.ID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"spawned": len(spawned), "ids": spawned})
}

func spawnOne(agent, mode string, interactive bool, prompt string, env map[string]string, mcpServers []any, claudeMD string, container map[string]any) *dashSession {
	mu.RLock()
	idx := len(sessions)
	mu.RUnlock()

	label := fmt.Sprintf("%s-%s-%d", agent, mode, idx)

	body := map[string]any{
		"agent":       agent,
		"interactive": interactive,
		"work_dir":    workDir,
		"task_id":     label,
		"name":        label,
	}

	// For prompt mode, include prompt in the request (fire-and-forget).
	// For interactive mode, omit it — we send via WS stdin after connected.
	if !interactive {
		body["prompt"] = prompt
	}

	if len(env) > 0 {
		body["env"] = env
	}
	if len(mcpServers) > 0 {
		body["mcp_servers"] = mcpServers
	}
	// Always include the agent config block so materialization triggers
	// (settings.json, .claude.json, credentials). Without this, the Docker
	// runtime skips config materialization entirely.
	if agent == "claude" {
		claudeBlock := map[string]any{}
		if claudeMD != "" {
			claudeBlock["claude_md"] = claudeMD
		}
		body["claude"] = claudeBlock
	} else if agent == "codex" {
		body["codex"] = map[string]any{}
	}
	if len(container) > 0 {
		body["container"] = container
	}

	data, err := json.Marshal(body)
	if err != nil {
		log.Printf("spawn %s: marshal: %v", label, err)
		return nil
	}
	resp, err := http.Post(daemonURL+"/sessions", "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("spawn %s: %v", label, err)
		return nil
	}
	defer resp.Body.Close()

	var sessResp struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessResp); err != nil {
		log.Printf("spawn %s: decode response: %v", label, err)
		return nil
	}

	if sessResp.SessionID == "" {
		return nil
	}

	gen := int(sessionGen.Add(1))
	sess := &dashSession{
		ID:         sessResp.SessionID,
		Agent:      agent,
		Mode:       mode,
		Status:     "connecting",
		SpawnedAt:  time.Now(),
		config:     body,
		prompt:     prompt,
		generation: gen,
	}

	// Record the initial prompt as the first event in the stream
	if prompt != "" {
		sess.recordUserEvent(prompt)
	}

	mu.Lock()
	sessions = append(sessions, sess)
	mu.Unlock()

	go connectWS(sess, interactive, prompt, gen)
	return sess
}

// --- WS Connection ---

func connectWS(sess *dashSession, interactive bool, prompt string, gen int) {
	wsURL := fmt.Sprintf("ws://%s/ws/sessions/%s?since=0",
		daemonURL[len("http://"):], sess.ID)

	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		mu.Lock()
		if sess.generation == gen {
			sess.Status = "ws-failed"
		}
		mu.Unlock()
		return
	}

	sess.connMu.Lock()
	sess.conn = conn
	sess.connMu.Unlock()

	// Don't set WSOpen here — wait for the "connected" frame from the daemon bridge.
	// This ensures the green dot only appears when the session is truly active.

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			mu.Lock()
			if sess.generation == gen {
				sess.WSOpen = false
				if sess.Status != "exited" && sess.Status != "killed" {
					sess.Status = "disconnected"
				}
			}
			mu.Unlock()
			return
		}

		// Stale goroutine check
		mu.RLock()
		stale := sess.generation != gen
		mu.RUnlock()
		if stale {
			conn.Close()
			return
		}

		var frame map[string]any
		if json.Unmarshal(msg, &frame) != nil {
			continue
		}

		frameType, _ := frame["type"].(string)
		now := time.Now()

		mu.Lock()
		sess.Events++
		sess.LastEvent = frameType

		switch frameType {
		case "connected":
			sess.WSOpen = true
			sess.Status = "connected"
			sess.ConnectedAt = &now
			sess.BootMs = now.Sub(sess.SpawnedAt).Milliseconds()

			// For interactive sessions, send the prompt now that we're connected
			if interactive && prompt != "" {
				go func() {
					time.Sleep(300 * time.Millisecond)
					sess.sendStdin(prompt + "\n")
				}()
			}

		case "stdout", "replay":
			if dataStr, ok := frame["data"].(string); ok {
				var inner map[string]any
				if json.Unmarshal([]byte(dataStr), &inner) == nil {
					extractTokens(sess, inner, now)
				}
			}

		case "exit":
			sess.Status = "exited"
			sess.WSOpen = false
			sess.CompletedAt = &now
			sess.DurationMs = now.Sub(sess.SpawnedAt).Milliseconds()
			if exitCode, ok := frame["exit_code"].(float64); ok {
				code := int(exitCode)
				sess.ExitCode = &code
			}

		case "error":
			if sess.Status != "exited" {
				sess.Status = "error"
			}
		}

		// Buffer events (cap at 100)
		sess.eventMu.Lock()
		sess.eventBuf = append(sess.eventBuf, msg)
		if len(sess.eventBuf) > 100 {
			sess.eventBuf = sess.eventBuf[len(sess.eventBuf)-100:]
		}
		sess.eventMu.Unlock()

		mu.Unlock()
	}
}

func extractTokens(sess *dashSession, event map[string]any, now time.Time) {
	evType, _ := event["type"].(string)
	data, _ := event["data"].(map[string]any)
	if data == nil {
		return
	}

	if evType == "agent_message" {
		// First non-system agent message = time to first token
		if sess.FirstTokenAt == nil {
			sess.FirstTokenAt = &now
			sess.TtftMs = now.Sub(sess.SpawnedAt).Milliseconds()
		}
		if text, ok := data["text"].(string); ok && len(text) > 0 {
			if len(text) > 80 {
				text = text[:80] + "..."
			}
			sess.LastText = text
		}
		if usage, ok := data["usage"].(map[string]any); ok {
			if v, ok := usage["input_tokens"].(float64); ok && v > 0 {
				sess.InputTokens = int(v)
			}
			if v, ok := usage["output_tokens"].(float64); ok && v > 0 {
				sess.OutputTokens = int(v)
			}
		}
	}
	if evType == "result" {
		if usage, ok := data["usage"].(map[string]any); ok {
			if v, ok := usage["input_tokens"].(float64); ok && v > 0 {
				sess.InputTokens = int(v)
			}
			if v, ok := usage["output_tokens"].(float64); ok && v > 0 {
				sess.OutputTokens = int(v)
			}
		}
	}
}

// --- Session Actions ---

func (s *dashSession) sendStdin(data string) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("no connection")
	}
	// Record user message in event buffer so it shows in the stream viewer
	s.recordUserEvent(strings.TrimRight(data, "\n\r"))
	return s.conn.WriteJSON(map[string]any{
		"type": "stdin",
		"data": data,
	})
}

// recordUserEvent injects a synthetic event into the buffer for display.
func (s *dashSession) recordUserEvent(content string) {
	frame, _ := json.Marshal(map[string]any{
		"type": "user_message",
		"data": map[string]any{"content": content},
		"timestamp": time.Now().UnixMilli(),
	})
	s.eventMu.Lock()
	s.eventBuf = append(s.eventBuf, frame)
	if len(s.eventBuf) > 100 {
		s.eventBuf = s.eventBuf[len(s.eventBuf)-100:]
	}
	s.eventMu.Unlock()
}

func (s *dashSession) closeConn() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func killOnDaemon(sessionID string) {
	req, _ := http.NewRequest(http.MethodDelete, daemonURL+"/sessions/"+sessionID, nil)
	http.DefaultClient.Do(req)
}

func findSession(id string) *dashSession {
	mu.RLock()
	defer mu.RUnlock()
	for _, s := range sessions {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// --- Handlers ---

func handleSessions(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

func handleSteer(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	prompt := r.URL.Query().Get("prompt")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}
	if prompt == "" {
		prompt = "What is 1+1? Reply with just the number."
	}

	target := findSession(id)
	if target == nil {
		http.Error(w, "session not found", 404)
		return
	}

	if err := target.sendStdin(prompt + "\n"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}

func handleKill(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}

	target := findSession(id)
	if target != nil {
		mu.Lock()
		target.Status = "killed"
		target.WSOpen = false
		mu.Unlock()
		target.closeConn()
	}

	killOnDaemon(id)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "killed"})
}

func handleKillAll(w http.ResponseWriter, r *http.Request) {
	agentFilter := r.URL.Query().Get("agent")

	mu.Lock()
	var toKill []*dashSession
	var toKeep []*dashSession
	for _, s := range sessions {
		if agentFilter == "" || s.Agent == agentFilter {
			toKill = append(toKill, s)
		} else {
			toKeep = append(toKeep, s)
		}
	}
	sessions = toKeep
	mu.Unlock()

	for _, s := range toKill {
		s.closeConn()
		if s.ID != "" {
			killOnDaemon(s.ID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"killed": len(toKill)})
}

func handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}

	target := findSession(id)
	if target == nil {
		http.Error(w, "session not found", 404)
		return
	}

	// Kill old
	mu.Lock()
	target.Status = "killed"
	target.WSOpen = false
	mu.Unlock()
	target.closeConn()
	killOnDaemon(id)

	// Respawn with saved config
	interactive := target.Mode == "interactive"
	newSess := spawnOne(target.Agent, target.Mode, interactive, target.prompt, nil, nil, "", nil)
	if newSess == nil {
		http.Error(w, "respawn failed", 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "retried", "new_id": newSess.ID})
}

func handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}

	target := findSession(id)
	if target == nil {
		http.Error(w, "session not found", 404)
		return
	}

	// Spawn new session that resumes the old one
	mu.RLock()
	idx := len(sessions)
	mu.RUnlock()

	label := fmt.Sprintf("%s-resume-%d", target.Agent, idx)
	body := map[string]any{
		"agent":          target.Agent,
		"interactive":    true,
		"work_dir":       workDir,
		"task_id":        label,
		"name":           label,
		"resume_session": target.ID,
	}

	data, _ := json.Marshal(body)
	resp, err := http.Post(daemonURL+"/sessions", "application/json", bytes.NewReader(data))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	var sessResp struct {
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(resp.Body).Decode(&sessResp)
	resp.Body.Close()

	if sessResp.SessionID == "" {
		http.Error(w, "resume returned no session", 500)
		return
	}

	gen := int(sessionGen.Add(1))
	newSess := &dashSession{
		ID:         sessResp.SessionID,
		Agent:      target.Agent,
		Mode:       "interactive",
		Status:     "connecting",
		SpawnedAt:  time.Now(),
		prompt:     "",
		generation: gen,
	}

	mu.Lock()
	sessions = append(sessions, newSess)
	mu.Unlock()

	go connectWS(newSess, true, "", gen)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resumed", "new_id": sessResp.SessionID})
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}

	filter := r.URL.Query().Get("filter")

	target := findSession(id)
	if target == nil {
		http.Error(w, "session not found", 404)
		return
	}

	target.eventMu.Lock()
	buf := make([]json.RawMessage, len(target.eventBuf))
	copy(buf, target.eventBuf)
	target.eventMu.Unlock()

	if filter != "" {
		var filtered []json.RawMessage
		for _, raw := range buf {
			var frame map[string]any
			if json.Unmarshal(raw, &frame) == nil {
				ft, _ := frame["type"].(string)
				// For stdout/replay, check the inner event type
				if ft == "stdout" || ft == "replay" {
					if dataStr, ok := frame["data"].(string); ok {
						var inner map[string]any
						if json.Unmarshal([]byte(dataStr), &inner) == nil {
							if it, ok := inner["type"].(string); ok {
								ft = it
							}
						}
					}
				}
				if strings.Contains(ft, filter) {
					filtered = append(filtered, raw)
				}
			}
		}
		buf = filtered
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buf)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, dashboardHTML)
}

// --- Frontend ---
// All DOM content built with textContent/createElement. No raw HTML insertion.
// Session data is server-controlled (our own agentd), not user-supplied.

var dashboardHTML = `<!DOCTYPE html>
<html>
<head>
<title>agentruntime dashboard</title>
<meta charset="utf-8">
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { background: #0a0a0a; color: #e0e0e0; font-family: 'SF Mono', 'Menlo', monospace; padding: 20px; }
h1 { font-size: 16px; color: #555; margin-bottom: 12px; letter-spacing: 1px; }

/* Spawn controls */
.controls { margin-bottom: 16px; }
.btn-row { display: flex; gap: 6px; flex-wrap: wrap; margin-bottom: 8px; }
.btn {
  background: #141420; color: #999; border: 1px solid #2a2a3a; border-radius: 4px;
  padding: 5px 12px; font-size: 11px; cursor: pointer; font-family: inherit; transition: all 0.15s;
}
.btn:hover { background: #1e1e30; border-color: #444; color: #ccc; }
.btn.claude { border-color: #4c1d95; color: #a78bfa; }
.btn.claude:hover { background: #1a0a2e; color: #c084fc; }
.btn.codex { border-color: #1e3a5f; color: #60a5fa; }
.btn.codex:hover { background: #0a1a2e; }
.btn.danger { border-color: #5a1a1a; color: #f87171; }
.btn.danger:hover { background: #2e0a0a; }
.btn.active { background: #1a2a1a; border-color: #2a5a2a; color: #4ade80; }

.btn-group { position: relative; display: inline-flex; }
.btn-group .btn:first-child { border-radius: 4px 0 0 4px; }
.btn-dd { border-radius: 0 4px 4px 0 !important; padding: 5px 6px !important; border-left: none !important; font-size: 8px !important; }
.dd-menu { display: none; position: absolute; top: 100%; right: 0; background: #1a1a1a; border: 1px solid #333; border-radius: 4px; z-index: 50; min-width: 60px; margin-top: 2px; }
.dd-menu.open { display: block; }
.dd-item { padding: 4px 10px; font-size: 10px; color: #999; cursor: pointer; white-space: nowrap; }
.dd-item:hover { background: #222; color: #ccc; }
.dd-item.active { color: #4ade80; }

/* Config panel */
.config-panel { display: none; background: #111; border: 1px solid #222; border-radius: 6px; padding: 12px; margin-bottom: 12px; }
.config-panel.open { display: block; }
.config-tabs { display: flex; gap: 4px; margin-bottom: 10px; }
.config-tab { padding: 3px 10px; font-size: 10px; border-radius: 3px; cursor: pointer; background: #1a1a1a; color: #666; border: 1px solid #222; }
.config-tab.active { background: #1a2a1a; color: #4ade80; border-color: #2a4a2a; }
.config-content { display: none; }
.config-content.active { display: block; }
.config-content textarea { width: 100%; height: 80px; background: #0a0a0a; color: #ccc; border: 1px solid #333; border-radius: 4px; padding: 8px; font-family: inherit; font-size: 11px; resize: vertical; }
.config-content input { background: #0a0a0a; color: #ccc; border: 1px solid #333; border-radius: 3px; padding: 4px 8px; font-family: inherit; font-size: 11px; margin-right: 4px; }
.config-content label { font-size: 10px; color: #666; display: block; margin-bottom: 4px; }

/* Stats bar */
.stats { font-size: 11px; color: #555; margin-bottom: 14px; padding: 6px 0; border-bottom: 1px solid #1a1a1a; }

/* Session grid */
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(250px, 1fr)); gap: 8px; }
.card {
  background: #111; border: 1px solid #1a1a1a; border-radius: 6px;
  padding: 10px 12px; position: relative; transition: all 0.2s; cursor: pointer;
}
.card:hover { border-color: #333; }
.card.ws-open { border-color: #1a3a1a; }
.card.exited { opacity: 0.45; }
.card.error { border-color: #3a1a1a; }
.card.killed { opacity: 0.3; }

.dot { width: 7px; height: 7px; border-radius: 50%; display: inline-block; margin-right: 5px; vertical-align: middle; }
.dot.green { background: #22c55e; box-shadow: 0 0 6px #22c55e; animation: pulse 1.5s infinite; }
.dot.red { background: #ef4444; }
.dot.yellow { background: #eab308; animation: pulse 1s infinite; }
.dot.gray { background: #333; }
@keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.3; } }

.agent-badge { font-size: 10px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.5px; }
.agent-badge.claude { color: #a78bfa; }
.agent-badge.codex { color: #60a5fa; }
.mode-label { font-size: 9px; color: #444; margin-left: 4px; }
.meta-line { font-size: 9px; color: #444; margin-top: 4px; }
.meta-line .tok { color: #555; }
.timing { color: #3a6a3a; }
.text-preview { font-size: 9px; color: #333; margin-top: 3px; max-height: 22px; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }

.card-actions { position: absolute; top: 6px; right: 6px; display: flex; gap: 3px; }
.card-actions .btn { padding: 1px 6px; font-size: 8px; }

/* Event modal */
.modal-overlay { display: none; position: fixed; top: 0; left: 0; right: 0; bottom: 0; background: rgba(0,0,0,0.8); z-index: 100; }
.modal-overlay.open { display: flex; align-items: center; justify-content: center; }
.modal { background: #111; border: 1px solid #333; border-radius: 8px; width: 80vw; max-width: 900px; max-height: 80vh; display: flex; flex-direction: column; }
.modal-header { padding: 12px 16px; border-bottom: 1px solid #222; display: flex; justify-content: space-between; align-items: center; }
.modal-header h2 { font-size: 13px; color: #888; font-weight: 600; }
.modal-filters { padding: 8px 16px; display: flex; gap: 4px; border-bottom: 1px solid #1a1a1a; }
.filter-chip { font-size: 9px; padding: 2px 8px; border-radius: 3px; cursor: pointer; background: #1a1a1a; color: #555; border: 1px solid #222; }
.filter-chip.active { background: #1a2a1a; color: #4ade80; border-color: #2a4a2a; }
.modal-body { flex: 1; overflow-y: auto; padding: 8px 16px; font-size: 10px; }
.event-line { padding: 2px 0; border-bottom: 1px solid #0a0a0a; color: #555; word-break: break-all; }
.event-line .ev-time { color: #333; }
.event-line .ev-type { color: #666; font-weight: 600; }
.event-line .ev-type.agent_message { color: #a78bfa; }
.event-line .ev-type.tool_use { color: #f59e0b; }
.event-line .ev-type.tool_result { color: #22c55e; }
.event-line .ev-type.result { color: #60a5fa; }
.event-line .ev-type.error { color: #ef4444; }
.event-line .ev-type.user_message { color: #2dd4bf; font-style: italic; }
.event-line .ev-type.progress { color: #a3a3a3; }
.event-line .ev-type.system { color: #737373; }
.event-line.system-init { background: #0d1117; border: 1px solid #1a2a1a; border-radius: 4px; padding: 6px 8px; margin: 4px 0; font-size: 9px; white-space: pre-wrap; }
.modal-footer { padding: 8px 16px; border-top: 1px solid #222; display: flex; gap: 6px; }
</style>
</head>
<body>
<h1>AGENTRUNTIME</h1>

<div class="controls">
  <div class="btn-row">
    <div class="btn-group">
      <button class="btn claude" onclick="spawnN('claude','interactive')">+<span id="cnt-ci">1</span> Claude Interactive</button>
      <button class="btn claude btn-dd" onclick="toggleDD('dd-ci')">&#9662;</button>
      <div class="dd-menu" id="dd-ci"></div>
    </div>
    <div class="btn-group">
      <button class="btn claude" onclick="spawnN('claude','prompt')">+<span id="cnt-cp">1</span> Claude Prompt</button>
      <button class="btn claude btn-dd" onclick="toggleDD('dd-cp')">&#9662;</button>
      <div class="dd-menu" id="dd-cp"></div>
    </div>
    <div class="btn-group">
      <button class="btn codex" onclick="spawnN('codex','interactive')">+<span id="cnt-xi">1</span> Codex Interactive</button>
      <button class="btn codex btn-dd" onclick="toggleDD('dd-xi')">&#9662;</button>
      <div class="dd-menu" id="dd-xi"></div>
    </div>
    <div class="btn-group">
      <button class="btn codex" onclick="spawnN('codex','prompt')">+<span id="cnt-xp">1</span> Codex Prompt</button>
      <button class="btn codex btn-dd" onclick="toggleDD('dd-xp')">&#9662;</button>
      <div class="dd-menu" id="dd-xp"></div>
    </div>
    <button class="btn" id="configToggle" onclick="toggleConfig()">Config</button>
    <div class="btn-group">
      <button class="btn danger" onclick="killAll()">Kill All</button>
      <button class="btn danger btn-dd" onclick="toggleDD('dd-kill')">&#9662;</button>
      <div class="dd-menu" id="dd-kill">
        <div class="dd-item" onclick="killByAgent('claude')">Kill All Claude</div>
        <div class="dd-item" onclick="killByAgent('codex')">Kill All Codex</div>
      </div>
    </div>
  </div>
</div>

<div class="config-panel" id="configPanel">
  <div class="config-tabs">
    <div class="config-tab active" onclick="switchTab('prompt')">Prompt</div>
    <div class="config-tab" onclick="switchTab('env')">Env</div>
    <div class="config-tab" onclick="switchTab('claudemd')">CLAUDE.md</div>
    <div class="config-tab" onclick="switchTab('mcp')">MCP Servers</div>
    <div class="config-tab" onclick="switchTab('container')">Container</div>
  </div>
  <div class="config-content active" id="tab-prompt">
    <label>Custom prompt (used for next spawn)</label>
    <textarea id="customPrompt">Count from 1 to 60, printing one number per line. Sleep 1 second between each number. Use the Bash tool to run: for i in $(seq 1 60); do echo $i; sleep 1; done</textarea>
  </div>
  <div class="config-content" id="tab-env">
    <label>Environment variables (KEY=VALUE, one per line)</label>
    <textarea id="customEnv" placeholder="MY_VAR=value&#10;ANOTHER=value2"></textarea>
  </div>
  <div class="config-content" id="tab-claudemd">
    <label>CLAUDE.md content (injected into agent session)</label>
    <textarea id="customClaudeMD" placeholder="# Custom Instructions&#10;Stay focused on this task."></textarea>
  </div>
  <div class="config-content" id="tab-mcp">
    <label>MCP servers (JSON array)</label>
    <textarea id="customMCP" placeholder='[{"name":"filesystem","type":"stdio","cmd":["mcp-server-filesystem","/workspace"]}]'></textarea>
  </div>
  <div class="config-content" id="tab-container">
    <label>Container config (JSON)</label>
    <textarea id="customContainer" placeholder='{"memory":"4g","cpus":2.0}'></textarea>
  </div>
</div>

<div class="stats" id="stats"></div>
<div class="grid" id="grid"></div>

<div class="modal-overlay" id="modalOverlay" onclick="closeModal(event)">
  <div class="modal" onclick="event.stopPropagation()">
    <div class="modal-header">
      <h2 id="modalTitle">Events</h2>
      <button class="btn" onclick="closeModal()">Close</button>
    </div>
    <div class="modal-filters" id="modalFilters"></div>
    <div class="modal-body" id="modalBody"></div>
    <div class="modal-footer">
      <button class="btn" onclick="downloadLog()">Download Log</button>
    </div>
  </div>
</div>

<script>
// Spawn count state per button group
var spawnCounts = {'ci': 1, 'cp': 1, 'xi': 1, 'xp': 1};

// Init dropdown menus with count options
(function() {
  var counts = [1, 5, 10, 15, 20, 30];
  ['ci','cp','xi','xp'].forEach(function(key) {
    var dd = document.getElementById('dd-' + key);
    if (!dd) return;
    counts.forEach(function(n) {
      var item = document.createElement('div');
      item.className = 'dd-item' + (n === 1 ? ' active' : '');
      item.textContent = n;
      item.onclick = function(e) {
        e.stopPropagation();
        spawnCounts[key] = n;
        document.getElementById('cnt-' + key).textContent = n;
        dd.querySelectorAll('.dd-item').forEach(function(d) { d.classList.remove('active'); });
        item.classList.add('active');
        dd.classList.remove('open');
      };
      dd.appendChild(item);
    });
  });
})();

function toggleDD(id) {
  var el = document.getElementById(id);
  var wasOpen = el.classList.contains('open');
  document.querySelectorAll('.dd-menu').forEach(function(m) { m.classList.remove('open'); });
  if (!wasOpen) el.classList.add('open');
}

// Close dropdowns on outside click
document.addEventListener('click', function(e) {
  if (!e.target.closest('.btn-group')) {
    document.querySelectorAll('.dd-menu').forEach(function(m) { m.classList.remove('open'); });
  }
});

function spawnN(agent, mode) {
  var key = (agent === 'claude' ? 'c' : 'x') + (mode === 'interactive' ? 'i' : 'p');
  spawn(agent, mode, spawnCounts[key]);
}

function killByAgent(agent) {
  document.querySelectorAll('.dd-menu').forEach(function(m) { m.classList.remove('open'); });
  fetch('/api/kill-all?agent=' + agent, {method: 'DELETE'});
}

var modalSessionId = null;
var modalFilter = '';
var modalPollInterval = null;

function getConfig() {
  var cfg = {};
  var p = document.getElementById('customPrompt').value.trim();
  if (p) cfg.prompt = p;
  var env = document.getElementById('customEnv').value.trim();
  if (env) {
    cfg.env = {};
    env.split('\n').forEach(function(line) {
      var eq = line.indexOf('=');
      if (eq > 0) cfg.env[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
    });
  }
  var md = document.getElementById('customClaudeMD').value.trim();
  if (md) cfg.claude_md = md;
  try { var mcp = document.getElementById('customMCP').value.trim(); if (mcp) cfg.mcp_servers = JSON.parse(mcp); } catch(e) {}
  try { var ct = document.getElementById('customContainer').value.trim(); if (ct) cfg.container = JSON.parse(ct); } catch(e) {}
  return cfg;
}

function spawn(agent, mode, count) {
  var cfg = getConfig();
  cfg.agent = agent;
  cfg.mode = mode;
  cfg.count = count;
  fetch('/api/spawn', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(cfg)});
}

function steer(id, ev) {
  if (ev) ev.stopPropagation();
  var p = prompt('Steer prompt:', 'What is 1+1? Reply with just the number.');
  if (!p) return;
  fetch('/api/steer?id=' + encodeURIComponent(id) + '&prompt=' + encodeURIComponent(p));
}

function killSession(id, ev) {
  if (ev) ev.stopPropagation();
  fetch('/api/kill?id=' + encodeURIComponent(id), {method: 'DELETE'});
}

function retrySession(id, ev) {
  if (ev) ev.stopPropagation();
  fetch('/api/retry?id=' + encodeURIComponent(id), {method: 'POST'});
}

function resumeSession(id, ev) {
  if (ev) ev.stopPropagation();
  fetch('/api/resume?id=' + encodeURIComponent(id), {method: 'POST'});
}

function killAll() {
  if (!confirm('Kill all sessions?')) return;
  fetch('/api/kill-all', {method: 'DELETE'});
}

function toggleConfig() {
  var panel = document.getElementById('configPanel');
  var btn = document.getElementById('configToggle');
  panel.classList.toggle('open');
  btn.classList.toggle('active');
}

function switchTab(name) {
  document.querySelectorAll('.config-tab').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.config-content').forEach(function(c) { c.classList.remove('active'); });
  event.target.classList.add('active');
  document.getElementById('tab-' + name).classList.add('active');
}

function dotClass(s) {
  if (s.ws_open) return 'green';
  if (s.status === 'error' || s.status === 'create-failed' || s.status === 'ws-failed') return 'red';
  if (s.status === 'connecting') return 'yellow';
  return 'gray';
}

function fmtMs(ms) {
  if (!ms) return '';
  if (ms < 1000) return ms + 'ms';
  return (ms / 1000).toFixed(1) + 's';
}

function render(sessions) {
  if (!sessions) sessions = [];
  var wsOpen = 0, exited = 0, errors = 0, totalIn = 0, totalOut = 0, totalEvents = 0, bootTimes = [], ttftTimes = [];
  sessions.forEach(function(s) {
    if (s.ws_open) wsOpen++;
    if (s.status === 'exited') exited++;
    if (s.status === 'error' || s.status === 'create-failed') errors++;
    totalIn += s.input_tokens;
    totalOut += s.output_tokens;
    totalEvents += s.events;
    if (s.boot_ms > 0) bootTimes.push(s.boot_ms);
    if (s.ttft_ms > 0) ttftTimes.push(s.ttft_ms);
  });

  var avgBoot = bootTimes.length ? Math.round(bootTimes.reduce(function(a,b){return a+b;},0) / bootTimes.length) : 0;
  var avgTtft = ttftTimes.length ? Math.round(ttftTimes.reduce(function(a,b){return a+b;},0) / ttftTimes.length) : 0;

  var statsEl = document.getElementById('stats');
  var parts = [
    sessions.length + ' sessions',
    wsOpen + ' connected',
    exited + ' exited'
  ];
  if (errors) parts.push(errors + ' errors');
  parts.push(totalEvents + ' events');
  parts.push(totalIn.toLocaleString() + '/' + totalOut.toLocaleString() + ' tok');
  if (avgBoot) parts.push('boot: ' + fmtMs(avgBoot));
  if (avgTtft) parts.push('ttft: ' + fmtMs(avgTtft));
  statsEl.textContent = parts.join('  \u2022  ');

  var grid = document.getElementById('grid');
  while (grid.children.length > sessions.length) grid.removeChild(grid.lastChild);
  while (grid.children.length < sessions.length) grid.appendChild(document.createElement('div'));

  sessions.forEach(function(s, i) {
    var card = grid.children[i];
    card.className = 'card' + (s.ws_open ? ' ws-open' : '') + (s.status === 'exited' ? ' exited' : '') + (s.status === 'error' ? ' error' : '') + (s.status === 'killed' ? ' killed' : '');
    card.onclick = function() { openModal(s.id, s.agent, s.mode); };
    card.textContent = '';

    var header = document.createElement('div');
    var dot = document.createElement('span');
    dot.className = 'dot ' + dotClass(s);
    header.appendChild(dot);
    var badge = document.createElement('span');
    badge.className = 'agent-badge ' + s.agent;
    badge.textContent = s.agent;
    header.appendChild(badge);
    var mode = document.createElement('span');
    mode.className = 'mode-label';
    mode.textContent = s.mode;
    header.appendChild(mode);
    card.appendChild(header);

    var meta = document.createElement('div');
    meta.className = 'meta-line';
    var metaParts = [s.status, s.events + ' events'];
    if (s.input_tokens) metaParts.push(s.input_tokens + '/' + s.output_tokens + ' tok');
    if (s.boot_ms) metaParts.push('boot:' + fmtMs(s.boot_ms));
    if (s.ttft_ms) metaParts.push('ttft:' + fmtMs(s.ttft_ms));
    meta.textContent = metaParts.join(' \u00b7 ');
    card.appendChild(meta);

    if (s.last_text) {
      var txt = document.createElement('div');
      txt.className = 'text-preview';
      txt.textContent = s.last_text;
      card.appendChild(txt);
    }

    var actions = document.createElement('div');
    actions.className = 'card-actions';
    if (s.ws_open && s.mode === 'interactive') {
      var steerBtn = document.createElement('button');
      steerBtn.className = 'btn';
      steerBtn.textContent = 'steer';
      steerBtn.onclick = function(e) { steer(s.id, e); };
      actions.appendChild(steerBtn);
    }
    if (s.status === 'exited' || s.status === 'disconnected') {
      var retryBtn = document.createElement('button');
      retryBtn.className = 'btn';
      retryBtn.textContent = 'retry';
      retryBtn.onclick = function(e) { retrySession(s.id, e); };
      actions.appendChild(retryBtn);
      var resumeBtn = document.createElement('button');
      resumeBtn.className = 'btn';
      resumeBtn.textContent = 'resume';
      resumeBtn.onclick = function(e) { resumeSession(s.id, e); };
      actions.appendChild(resumeBtn);
    }
    if (s.status !== 'killed' && s.status !== 'exited') {
      var killBtn = document.createElement('button');
      killBtn.className = 'btn danger';
      killBtn.textContent = '\u00d7';
      killBtn.onclick = function(e) { killSession(s.id, e); };
      actions.appendChild(killBtn);
    }
    card.appendChild(actions);
  });
}

function openModal(id, agent, mode) {
  modalSessionId = id;
  modalFilter = '';
  document.getElementById('modalTitle').textContent = agent.toUpperCase() + ' ' + mode + ' \u2014 ' + id.slice(0, 8);
  document.getElementById('modalOverlay').classList.add('open');

  var filters = document.getElementById('modalFilters');
  filters.textContent = '';
  ['all','user_message','agent_message','tool_use','tool_result','result','progress','error','system'].forEach(function(f) {
    var chip = document.createElement('span');
    chip.className = 'filter-chip' + (f === 'all' ? ' active' : '');
    chip.textContent = f;
    chip.onclick = function() {
      modalFilter = f === 'all' ? '' : f;
      filters.querySelectorAll('.filter-chip').forEach(function(c) { c.classList.remove('active'); });
      chip.classList.add('active');
      loadEvents();
    };
    filters.appendChild(chip);
  });

  loadEvents();
  modalPollInterval = setInterval(loadEvents, 1000);
}

function closeModal(ev) {
  if (ev && ev.target !== document.getElementById('modalOverlay')) return;
  document.getElementById('modalOverlay').classList.remove('open');
  modalSessionId = null;
  if (modalPollInterval) { clearInterval(modalPollInterval); modalPollInterval = null; }
}

function loadEvents() {
  if (!modalSessionId) return;
  var url = '/api/events?id=' + modalSessionId;
  if (modalFilter) url += '&filter=' + modalFilter;
  fetch(url).then(function(r) { return r.json(); }).then(function(events) {
    if (!events) events = [];
    var body = document.getElementById('modalBody');
    var wasAtBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 30;
    body.textContent = '';
    events.forEach(function(raw) {
      var frame = typeof raw === 'string' ? JSON.parse(raw) : raw;
      var line = document.createElement('div');
      line.className = 'event-line';

      var ft = frame.type || '?';
      var innerType = ft;
      var dataPreview = '';

      var inner = null;
      if ((ft === 'stdout' || ft === 'replay') && typeof frame.data === 'string') {
        try {
          inner = JSON.parse(frame.data);
          innerType = inner.type || ft;
        } catch(e) {}
      }

      // For user_message (synthetic), show directly
      if (ft === 'user_message') {
        innerType = 'user_message';
        dataPreview = (frame.data && frame.data.content) || '';
      }
      // System init event — show key fields expanded
      else if (innerType === 'system' && inner && inner.data && inner.data.subtype === 'init') {
        var d = inner.data;
        var parts = [];
        if (d.model) parts.push('model: ' + d.model);
        if (d.tools) parts.push('tools: ' + d.tools.length);
        if (d.mcp_servers) parts.push('mcp: ' + d.mcp_servers.length);
        if (d.agents) parts.push('agents: ' + d.agents.length);
        if (d.skills) parts.push('skills: ' + d.skills.length);
        if (d.session_id) parts.push('sid: ' + d.session_id.slice(0, 8));
        if (d.permissionMode) parts.push('perms: ' + d.permissionMode);
        if (d.cwd) parts.push('cwd: ' + d.cwd);
        dataPreview = parts.join(' | ');
        line.className = 'event-line system-init';
      }
      // Standard inner events
      else if (inner && inner.data) {
        if (inner.data.text) dataPreview = inner.data.text;
        else if (inner.data.message) dataPreview = inner.data.message;
        else if (inner.data.name) dataPreview = inner.data.name;
        else if (inner.data.content) dataPreview = inner.data.content;
        else dataPreview = JSON.stringify(inner.data).slice(0, 200);
      }
      // Raw frames
      else {
        dataPreview = JSON.stringify(frame.data || frame).slice(0, 200);
      }

      var timeSpan = document.createElement('span');
      timeSpan.className = 'ev-time';
      var ts = frame.offset ? '[' + frame.offset + '] ' : (frame.timestamp ? '[' + new Date(frame.timestamp).toLocaleTimeString() + '] ' : '');
      timeSpan.textContent = ts;
      line.appendChild(timeSpan);

      var typeSpan = document.createElement('span');
      typeSpan.className = 'ev-type ' + innerType;
      typeSpan.textContent = innerType;
      line.appendChild(typeSpan);

      var dataSpan = document.createElement('span');
      dataSpan.textContent = ' ' + (dataPreview.length > 150 ? dataPreview.slice(0, 150) + '...' : dataPreview);
      line.appendChild(dataSpan);

      body.appendChild(line);
    });
    if (wasAtBottom) body.scrollTop = body.scrollHeight;
  });
}

function downloadLog() {
  if (!modalSessionId) return;
  window.open('/api/events?id=' + modalSessionId, '_blank');
}

setInterval(function() {
  fetch('/api/sessions').then(function(r) { return r.json(); }).then(render);
}, 500);
render([]);
</script>
</body>
</html>
` + ""
