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
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type session struct {
	ID           string `json:"id"`
	Agent        string `json:"agent"`
	Mode         string `json:"mode"`
	Status       string `json:"status"`
	Events       int    `json:"events"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	LastEvent    string `json:"last_event"`
	LastText     string `json:"last_text"`
	WSOpen       bool   `json:"ws_open"`

	conn   *websocket.Conn `json:"-"`
	connMu sync.Mutex      `json:"-"`
}

var (
	mu        sync.RWMutex
	sessions  []*session
	daemonURL string
	workDir   string
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

	// Verify daemon
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
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

// POST /api/spawn?agent=claude&mode=interactive&count=7
func handleSpawn(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "claude"
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "prompt"
	}
	countStr := r.URL.Query().Get("count")
	count := 1
	if countStr != "" {
		fmt.Sscanf(countStr, "%d", &count)
	}
	prompt := r.URL.Query().Get("prompt")
	if prompt == "" {
		prompt = "Print the numbers 1 through 10, each on its own line. No other text."
	}

	interactive := mode == "interactive"
	var spawned []string

	for i := 0; i < count; i++ {
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
		if !interactive {
			body["prompt"] = prompt
		}

		data, _ := json.Marshal(body)
		resp, err := http.Post(daemonURL+"/sessions", "application/json", bytes.NewReader(data))
		if err != nil {
			log.Printf("spawn %s: %v", label, err)
			continue
		}

		var sessResp struct {
			SessionID string `json:"session_id"`
		}
		json.NewDecoder(resp.Body).Decode(&sessResp)
		resp.Body.Close()

		if sessResp.SessionID == "" {
			continue
		}

		sess := &session{
			ID:     sessResp.SessionID,
			Agent:  agent,
			Mode:   mode,
			Status: "connecting",
		}

		mu.Lock()
		sessions = append(sessions, sess)
		mu.Unlock()

		go connectWS(sess, interactive, prompt)
		spawned = append(spawned, sessResp.SessionID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"spawned": len(spawned), "ids": spawned})
}

func connectWS(sess *session, interactive bool, prompt string) {
	wsURL := fmt.Sprintf("ws://%s/ws/sessions/%s?since=0",
		daemonURL[len("http://"):], sess.ID)

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		mu.Lock()
		sess.Status = "ws-failed"
		mu.Unlock()
		return
	}

	sess.connMu.Lock()
	sess.conn = conn
	sess.connMu.Unlock()

	mu.Lock()
	sess.WSOpen = true
	sess.Status = "connected"
	mu.Unlock()

	if interactive && prompt != "" {
		time.Sleep(500 * time.Millisecond)
		sess.sendStdin(prompt + "\n")
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			mu.Lock()
			sess.WSOpen = false
			if sess.Status != "exited" {
				sess.Status = "disconnected"
			}
			mu.Unlock()
			return
		}

		var frame map[string]any
		if json.Unmarshal(msg, &frame) != nil {
			continue
		}

		frameType, _ := frame["type"].(string)

		mu.Lock()
		sess.Events++
		sess.LastEvent = frameType

		if frameType == "stdout" || frameType == "replay" {
			if dataStr, ok := frame["data"].(string); ok {
				var inner map[string]any
				if json.Unmarshal([]byte(dataStr), &inner) == nil {
					extractTokens(sess, inner)
				}
			}
		}

		switch frameType {
		case "exit":
			sess.Status = "exited"
		case "error":
			sess.Status = "error"
		}
		mu.Unlock()
	}
}

func extractTokens(sess *session, event map[string]any) {
	evType, _ := event["type"].(string)
	data, _ := event["data"].(map[string]any)
	if data == nil {
		return
	}

	if evType == "agent_message" {
		if text, ok := data["text"].(string); ok && len(text) > 0 {
			if len(text) > 80 {
				text = text[:80] + "..."
			}
			sess.LastText = text
		}
		if usage, ok := data["usage"].(map[string]any); ok {
			if v, ok := usage["input_tokens"].(float64); ok {
				sess.InputTokens = int(v)
			}
			if v, ok := usage["output_tokens"].(float64); ok {
				sess.OutputTokens = int(v)
			}
		}
	}
	if evType == "result" {
		if usage, ok := data["usage"].(map[string]any); ok {
			if v, ok := usage["input_tokens"].(float64); ok {
				sess.InputTokens = int(v)
			}
			if v, ok := usage["output_tokens"].(float64); ok {
				sess.OutputTokens = int(v)
			}
		}
	}
}

func (s *session) sendStdin(data string) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("no connection")
	}
	return s.conn.WriteJSON(map[string]any{
		"type": "stdin",
		"data": data,
	})
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

func handleSteer(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("id")
	if sessionID == "" {
		http.Error(w, "missing id", 400)
		return
	}

	steerPrompt := r.URL.Query().Get("prompt")
	if steerPrompt == "" {
		steerPrompt = "What is 1+1? Reply with just the number."
	}

	mu.RLock()
	var target *session
	for _, s := range sessions {
		if s.ID == sessionID {
			target = s
			break
		}
	}
	mu.RUnlock()

	if target == nil {
		http.Error(w, "session not found", 404)
		return
	}

	if err := target.sendStdin(steerPrompt + "\n"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}

// DELETE /api/kill?id=session-id
func handleKill(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("id")
	if sessionID == "" {
		http.Error(w, "missing id", 400)
		return
	}
	req, _ := http.NewRequest(http.MethodDelete, daemonURL+"/sessions/"+sessionID, nil)
	http.DefaultClient.Do(req)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "killed"})
}

// DELETE /api/kill-all
func handleKillAll(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	mu.RUnlock()

	for _, id := range ids {
		req, _ := http.NewRequest(http.MethodDelete, daemonURL+"/sessions/"+id, nil)
		http.DefaultClient.Do(req)
	}

	mu.Lock()
	sessions = nil
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"killed": len(ids)})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, dashboardHTML)
}

// NOTE: All DOM content is built with textContent / createElement — no
// raw HTML insertion. Session data is fully server-controlled (our own
// agentd daemon), not user-supplied.
var dashboardHTML = `<!DOCTYPE html>
<html>
<head>
<title>agentruntime dashboard</title>
<meta charset="utf-8">
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { background: #0a0a0a; color: #e0e0e0; font-family: 'SF Mono', 'Menlo', monospace; padding: 20px; }
h1 { font-size: 18px; color: #888; margin-bottom: 12px; }

.controls { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 16px; }
.controls button {
  background: #1a1a2e; color: #c0c0c0; border: 1px solid #333; border-radius: 4px;
  padding: 6px 14px; font-size: 12px; cursor: pointer; font-family: inherit;
  transition: all 0.15s;
}
.controls button:hover { background: #2a2a3e; border-color: #555; }
.controls button.claude { border-color: #6b21a8; color: #c084fc; }
.controls button.claude:hover { background: #1a0a2e; }
.controls button.codex { border-color: #1d4ed8; color: #60a5fa; }
.controls button.codex:hover { background: #0a1a2e; }
.controls button.danger { border-color: #7f1d1d; color: #f87171; }
.controls button.danger:hover { background: #2e0a0a; }

.stats { display: flex; gap: 20px; margin-bottom: 16px; font-size: 12px; color: #666; }
.stat-val { color: #aaa; font-weight: 600; }
.stat-val.green { color: #22c55e; }

.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 8px; }
.card {
  background: #141414; border: 1px solid #222; border-radius: 6px;
  padding: 10px 12px; position: relative; transition: border-color 0.3s;
}
.card.ws-open { border-color: #1a3a1a; }
.card.exited { border-color: #333; opacity: 0.5; }
.card.error { border-color: #5a1a1a; }

.dot {
  width: 8px; height: 8px; border-radius: 50%; display: inline-block;
  margin-right: 6px; vertical-align: middle;
}
.dot.green { background: #22c55e; box-shadow: 0 0 6px #22c55e; animation: pulse 2s infinite; }
.dot.red { background: #ef4444; }
.dot.yellow { background: #eab308; }
.dot.gray { background: #444; }
@keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.4; } }

.agent { font-size: 11px; font-weight: 600; text-transform: uppercase; }
.agent.claude { color: #c084fc; }
.agent.codex { color: #60a5fa; }
.mode { font-size: 10px; color: #555; margin-left: 4px; }
.meta { font-size: 10px; color: #555; margin-top: 4px; }
.tokens { color: #666; }
.text-preview { font-size: 10px; color: #3a3a3a; margin-top: 3px; max-height: 28px; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }

.card-actions { position: absolute; top: 6px; right: 8px; display: flex; gap: 4px; }
.card-actions button {
  background: #1a2a1a; color: #4ade80; border: 1px solid #2a4a2a; border-radius: 3px;
  padding: 1px 6px; font-size: 9px; cursor: pointer; font-family: inherit;
}
.card-actions button:hover { background: #2a4a2a; }
.card-actions button.kill { color: #f87171; border-color: #4a2a2a; background: #2a1a1a; }
.card-actions button.kill:hover { background: #3a1a1a; }
</style>
</head>
<body>
<h1>agentruntime dashboard</h1>

<div class="controls" id="controls">
  <button class="claude" onclick="spawn('claude','interactive',7)">7 Claude Interactive</button>
  <button class="claude" onclick="spawn('claude','prompt',8)">8 Claude Prompt</button>
  <button class="codex" onclick="spawn('codex','interactive',7)">7 Codex Interactive</button>
  <button class="codex" onclick="spawn('codex','prompt',8)">8 Codex Prompt</button>
  <button onclick="spawn('claude','interactive',1)">+1 Claude</button>
  <button onclick="spawn('codex','interactive',1)">+1 Codex</button>
  <button class="danger" onclick="killAll()">Kill All</button>
</div>

<div class="stats" id="stats"></div>
<div class="grid" id="grid"></div>

<script>
function spawn(agent, mode, count) {
  fetch('/api/spawn?agent=' + agent + '&mode=' + mode + '&count=' + count, {method:'POST'});
}

function steer(id) {
  var p = prompt('Steer prompt:', 'What is 1+1? Reply with just the number.');
  if (!p) return;
  fetch('/api/steer?id=' + encodeURIComponent(id) + '&prompt=' + encodeURIComponent(p));
}

function killSession(id) {
  fetch('/api/kill?id=' + encodeURIComponent(id), {method:'DELETE'});
}

function killAll() {
  if (!confirm('Kill all sessions?')) return;
  fetch('/api/kill-all', {method:'DELETE'});
}

function dotClass(s) {
  if (s.ws_open) return 'green';
  if (s.status === 'error' || s.status === 'create-failed' || s.status === 'ws-failed') return 'red';
  if (s.status === 'connecting') return 'yellow';
  return 'gray';
}

function render(sessions) {
  if (!sessions) sessions = [];
  var wsOpen = sessions.filter(function(s) { return s.ws_open; }).length;
  var exited = sessions.filter(function(s) { return s.status === 'exited'; }).length;
  var errors = sessions.filter(function(s) { return s.status === 'error' || s.status === 'create-failed'; }).length;
  var totalIn = sessions.reduce(function(a, s) { return a + s.input_tokens; }, 0);
  var totalOut = sessions.reduce(function(a, s) { return a + s.output_tokens; }, 0);
  var totalEvents = sessions.reduce(function(a, s) { return a + s.events; }, 0);

  var statsEl = document.getElementById('stats');
  statsEl.textContent = '';
  var parts = [
    sessions.length + ' sessions',
    wsOpen + ' connected',
    exited + ' exited',
    errors ? errors + ' errors' : null,
    totalEvents + ' events',
    'In: ' + totalIn.toLocaleString() + ' / Out: ' + totalOut.toLocaleString()
  ].filter(Boolean);
  statsEl.textContent = parts.join('  \u2022  ');

  var grid = document.getElementById('grid');

  while (grid.children.length > sessions.length) {
    grid.removeChild(grid.lastChild);
  }
  while (grid.children.length < sessions.length) {
    grid.appendChild(document.createElement('div'));
  }

  sessions.forEach(function(s, i) {
    var card = grid.children[i];
    card.className = 'card' + (s.ws_open ? ' ws-open' : '') + (s.status === 'exited' ? ' exited' : '') + (s.status === 'error' ? ' error' : '');
    card.textContent = '';

    var header = document.createElement('div');
    var dot = document.createElement('span');
    dot.className = 'dot ' + dotClass(s);
    header.appendChild(dot);
    var agentSpan = document.createElement('span');
    agentSpan.className = 'agent ' + s.agent;
    agentSpan.textContent = s.agent;
    header.appendChild(agentSpan);
    var modeSpan = document.createElement('span');
    modeSpan.className = 'mode';
    modeSpan.textContent = s.mode;
    header.appendChild(modeSpan);
    card.appendChild(header);

    var meta = document.createElement('div');
    meta.className = 'meta';
    var metaText = s.status + ' \u00b7 ' + s.events + ' events';
    if (s.input_tokens) metaText += ' \u00b7 ' + s.input_tokens + '/' + s.output_tokens + ' tok';
    meta.textContent = metaText;
    card.appendChild(meta);

    if (s.last_text) {
      var textDiv = document.createElement('div');
      textDiv.className = 'text-preview';
      textDiv.textContent = s.last_text;
      card.appendChild(textDiv);
    }

    var actions = document.createElement('div');
    actions.className = 'card-actions';
    if (s.ws_open && s.mode === 'interactive') {
      var steerBtn = document.createElement('button');
      steerBtn.textContent = 'steer';
      steerBtn.addEventListener('click', function() { steer(s.id); });
      actions.appendChild(steerBtn);
    }
    if (s.ws_open) {
      var killBtn = document.createElement('button');
      killBtn.className = 'kill';
      killBtn.textContent = '\u00d7';
      killBtn.addEventListener('click', function() { killSession(s.id); });
      actions.appendChild(killBtn);
    }
    card.appendChild(actions);
  });
}

setInterval(function() {
  fetch('/api/sessions').then(function(r) { return r.json(); }).then(render);
}, 500);
render([]);
</script>
</body>
</html>
` + ""
