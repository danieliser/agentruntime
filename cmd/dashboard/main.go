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
	Mode         string `json:"mode"` // "interactive" or "prompt"
	Status       string `json:"status"`
	Events       int    `json:"events"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	LastEvent    string `json:"last_event"`
	LastText     string `json:"last_text"`
	WSOpen       bool   `json:"ws_open"`
}

var (
	mu        sync.RWMutex
	sessions  []*session
	daemonURL string
)

func main() {
	daemon := flag.String("daemon", "http://127.0.0.1:8090", "agentd base URL")
	port := flag.Int("port", 3030, "dashboard port")
	count := flag.Int("count", 30, "total sessions (half claude, half codex)")
	workDir := flag.String("work-dir", "", "work_dir for sessions (default: cwd)")
	prompt := flag.String("prompt", "Print the numbers 1 through 10, each on its own line. No other text.", "prompt for prompt-mode sessions")
	flag.Parse()

	daemonURL = *daemon
	if *workDir == "" {
		wd, _ := os.Getwd()
		*workDir = wd
	}

	// Verify daemon is healthy
	resp, err := http.Get(daemonURL + "/health")
	if err != nil {
		log.Fatalf("daemon not reachable at %s: %v", daemonURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatalf("daemon unhealthy: %s", resp.Status)
	}

	half := *count / 2
	var specs []struct {
		agent       string
		interactive bool
	}
	for i := 0; i < half; i++ {
		specs = append(specs, struct {
			agent       string
			interactive bool
		}{"claude", i < half/2})
	}
	for i := 0; i < *count-half; i++ {
		specs = append(specs, struct {
			agent       string
			interactive bool
		}{"codex", i < (*count-half)/2})
	}

	log.Printf("creating %d sessions against %s...", len(specs), daemonURL)

	for i, s := range specs {
		mode := "prompt"
		if s.interactive {
			mode = "interactive"
		}

		body := map[string]any{
			"agent":       s.agent,
			"interactive": s.interactive,
			"work_dir":    *workDir,
			"task_id":     fmt.Sprintf("%s-%s-%d", s.agent, mode, i),
			"name":        fmt.Sprintf("%s-%s-%d", s.agent, mode, i),
		}
		if !s.interactive {
			body["prompt"] = *prompt
		}

		data, _ := json.Marshal(body)
		resp, err := http.Post(daemonURL+"/sessions", "application/json", bytes.NewReader(data))
		if err != nil {
			log.Printf("session %d: create failed: %v", i, err)
			sessions = append(sessions, &session{
				Agent: s.agent, Mode: mode, Status: "create-failed",
			})
			continue
		}

		var sessResp struct {
			SessionID string `json:"session_id"`
		}
		json.NewDecoder(resp.Body).Decode(&sessResp)
		resp.Body.Close()

		if sessResp.SessionID == "" {
			sessions = append(sessions, &session{
				Agent: s.agent, Mode: mode, Status: "create-failed",
			})
			continue
		}

		sess := &session{
			ID:     sessResp.SessionID,
			Agent:  s.agent,
			Mode:   mode,
			Status: "connecting",
		}
		sessions = append(sessions, sess)

		go connectWS(sess, s.interactive, *prompt)
	}

	log.Printf("all sessions dispatched, dashboard at http://127.0.0.1:%d", *port)

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/sessions", handleSessions)
	http.HandleFunc("/api/steer", handleSteer)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
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

	mu.Lock()
	sess.WSOpen = true
	sess.Status = "connected"
	mu.Unlock()

	if interactive && prompt != "" {
		time.Sleep(500 * time.Millisecond)
		_ = conn.WriteJSON(map[string]any{
			"type": "stdin",
			"data": prompt + "\n",
		})
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

	wsURL := fmt.Sprintf("ws://%s/ws/sessions/%s",
		daemonURL[len("http://"):], sessionID)

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer conn.Close()

	_ = conn.WriteJSON(map[string]any{
		"type": "stdin",
		"data": steerPrompt + "\n",
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent", "session_id": sessionID})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, dashboardHTML)
}

// dashboardHTML is the self-contained frontend. All data comes from
// the /api/sessions polling endpoint (500ms interval). Steer buttons
// call /api/steer via fetch. No external dependencies.
//
// NOTE: session data is fully server-controlled (our own agentd),
// not user-supplied, so DOM text insertion is safe here.
var dashboardHTML = `<!DOCTYPE html>
<html>
<head>
<title>agentruntime concurrency dashboard</title>
<meta charset="utf-8">
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { background: #0a0a0a; color: #e0e0e0; font-family: 'SF Mono', 'Menlo', monospace; padding: 20px; }
h1 { font-size: 18px; color: #888; margin-bottom: 16px; }
.stats { display: flex; gap: 24px; margin-bottom: 20px; font-size: 13px; color: #666; }
.stats span { color: #aaa; }
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 10px; }
.card {
  background: #141414; border: 1px solid #222; border-radius: 6px;
  padding: 12px; position: relative; transition: border-color 0.3s;
}
.card.ws-open { border-color: #1a3a1a; }
.card.exited { border-color: #333; opacity: 0.6; }
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
.agent { font-size: 12px; font-weight: 600; text-transform: uppercase; }
.agent.claude { color: #c084fc; }
.agent.codex { color: #60a5fa; }
.mode { font-size: 10px; color: #666; margin-left: 4px; }
.meta { font-size: 11px; color: #555; margin-top: 6px; }
.meta .tokens { color: #888; }
.text-preview { font-size: 10px; color: #444; margin-top: 4px; max-height: 32px; overflow: hidden; }
.steer-btn {
  position: absolute; top: 8px; right: 8px; background: #1a2a1a; color: #4ade80;
  border: 1px solid #2a4a2a; border-radius: 4px; padding: 2px 8px; font-size: 10px;
  cursor: pointer; font-family: inherit;
}
.steer-btn:hover { background: #2a4a2a; }
.steer-btn:disabled { opacity: 0.3; cursor: default; }
</style>
</head>
<body>
<h1>agentruntime concurrency dashboard</h1>
<div class="stats" id="stats"></div>
<div class="grid" id="grid"></div>
<script>
function dotClass(s) {
  if (s.ws_open) return 'green';
  if (s.status === 'error' || s.status === 'create-failed' || s.status === 'ws-failed') return 'red';
  if (s.status === 'connecting') return 'yellow';
  return 'gray';
}

function render(sessions) {
  const wsOpen = sessions.filter(s => s.ws_open).length;
  const exited = sessions.filter(s => s.status === 'exited').length;
  const errors = sessions.filter(s => s.status === 'error' || s.status === 'create-failed').length;
  const totalIn = sessions.reduce((a, s) => a + s.input_tokens, 0);
  const totalOut = sessions.reduce((a, s) => a + s.output_tokens, 0);
  const totalEvents = sessions.reduce((a, s) => a + s.events, 0);

  const statsEl = document.getElementById('stats');
  statsEl.textContent = '';
  const parts = [
    sessions.length + ' sessions',
    wsOpen + ' connected',
    exited + ' exited',
    errors ? errors + ' errors' : null,
    'Events: ' + totalEvents,
    'In: ' + totalIn.toLocaleString() + ' / Out: ' + totalOut.toLocaleString()
  ].filter(Boolean);
  statsEl.textContent = parts.join(' | ');

  const grid = document.getElementById('grid');
  while (grid.children.length < sessions.length) {
    grid.appendChild(document.createElement('div'));
  }

  sessions.forEach((s, i) => {
    const card = grid.children[i];
    card.className = 'card' + (s.ws_open ? ' ws-open' : '') + (s.status === 'exited' ? ' exited' : '') + (s.status === 'error' ? ' error' : '');

    // Build card content safely using DOM methods
    card.textContent = '';

    // Header row: dot + agent + mode
    const header = document.createElement('div');
    const dot = document.createElement('span');
    dot.className = 'dot ' + dotClass(s);
    header.appendChild(dot);
    const agentSpan = document.createElement('span');
    agentSpan.className = 'agent ' + s.agent;
    agentSpan.textContent = s.agent;
    header.appendChild(agentSpan);
    const modeSpan = document.createElement('span');
    modeSpan.className = 'mode';
    modeSpan.textContent = s.mode;
    header.appendChild(modeSpan);
    card.appendChild(header);

    // Meta row
    const meta = document.createElement('div');
    meta.className = 'meta';
    let metaText = s.status + ' \u00b7 ' + s.events + ' events';
    if (s.input_tokens) metaText += ' \u00b7 ' + s.input_tokens + '/' + s.output_tokens + ' tok';
    meta.textContent = metaText;
    card.appendChild(meta);

    // Text preview
    if (s.last_text) {
      const textDiv = document.createElement('div');
      textDiv.className = 'text-preview';
      textDiv.textContent = s.last_text;
      card.appendChild(textDiv);
    }

    // Steer button
    if (s.ws_open && s.mode === 'interactive') {
      const btn = document.createElement('button');
      btn.className = 'steer-btn';
      btn.textContent = 'steer';
      btn.addEventListener('click', function() { steer(s.id); });
      card.appendChild(btn);
    }
  });
}

function steer(id) {
  var p = prompt('Steer prompt:', 'What is 1+1? Reply with just the number.');
  if (!p) return;
  fetch('/api/steer?id=' + encodeURIComponent(id) + '&prompt=' + encodeURIComponent(p));
}

setInterval(function() {
  fetch('/api/sessions').then(function(r) { return r.json(); }).then(render);
}, 500);
fetch('/api/sessions').then(function(r) { return r.json(); }).then(render);
</script>
</body>
</html>
` + ""
