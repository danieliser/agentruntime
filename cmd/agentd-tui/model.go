package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
)

// inputMode tracks what the user is doing.
type inputMode int

const (
	modeNormal inputMode = iota
	modePrompt           // agent asked a question, waiting for user response
)

type model struct {
	conn     *websocket.Conn
	meta     chatMeta
	renderer *renderer

	viewport viewport.Model
	input    textarea.Model

	// Content buffer — accumulated rendered lines.
	lines []string

	// State tracking.
	mode       inputMode
	connected  bool
	exited     bool
	exitCode   int
	streaming  bool            // currently receiving delta chunks
	streamBuf  strings.Builder // accumulated delta text

	// Metrics from events.
	inputTokens  int
	outputTokens int
	costUSD      float64
	toolCalls    int

	// Scroll tracking.
	followMode bool // auto-scroll when at bottom

	// Layout.
	width  int
	height int
}

func newModel(conn *websocket.Conn, meta chatMeta) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (/steer, /interrupt, /info, esc to quit)"
	ta.SetHeight(1)
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()

	vp := viewport.New(80, 20)

	return model{
		conn:     conn,
		meta:     meta,
		renderer: newRenderer(80),
		viewport: vp,
		input:    ta,
		lines:      make([]string, 0, 256),
		followMode: true,
	}
}

// renderTickMsg triggers periodic glamour re-render during streaming.
type renderTickMsg struct{}

func renderTick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg {
		return renderTickMsg{}
	})
}

func (m model) Init() tea.Cmd {
	return textarea.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.exited {
				return m, tea.Quit
			}
			_ = m.conn.WriteJSON(map[string]string{"type": "interrupt"})
			m.appendLine(systemStyle.Render("sent interrupt (ctrl+c again to quit)"))
			m.updateViewport()
			return m, nil

		case "esc":
			return m, tea.Quit

		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()

			if m.mode == modePrompt {
				m.sendStdin(text)
				m.mode = modeNormal
				m.input.Placeholder = "Type a message..."
				m.appendLine(userMsgStyle.Render("▶ " + text))
			} else if strings.HasPrefix(text, "/") {
				m.handleCommand(text)
			} else {
				m.sendStdin(text)
				m.appendLine(userMsgStyle.Render("▶ " + text))
			}
			m.updateViewport()
			return m, nil

		case "pgup", "pgdown":
			// Manual scroll — disable follow mode.
			m.followMode = false
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			cmds = append(cmds, cmd)
			// Re-enable follow if user scrolled back to bottom.
			if m.viewport.AtBottom() {
				m.followMode = true
			}
			return m, tea.Batch(cmds...)
		}

		// All other keys go to textarea.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.renderer.setWidth(msg.Width)

		inputHeight := 3
		headerHeight := 2 // header + status bar
		vpHeight := msg.Height - inputHeight - headerHeight - 1
		if vpHeight < 5 {
			vpHeight = 5
		}
		m.viewport.Width = msg.Width
		m.viewport.Height = vpHeight
		m.input.SetWidth(msg.Width - 2)
		m.updateViewport()

	case connectedMsg:
		m.connected = true
		m.meta.SessionID = msg.sessionID
		label := m.meta.Name
		if label == "" && len(msg.sessionID) >= 8 {
			label = msg.sessionID[:8]
		}
		m.appendLine(systemStyle.Render(fmt.Sprintf("Connected to %s (%s)", label, m.meta.Agent)))
		m.appendLine("")
		m.updateViewport()

	case agentEventMsg:
		cmd := m.handleAgentEvent(msg.event, msg.replay)
		m.updateViewport()
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case sessionExitMsg:
		m.flushStream()
		m.exited = true
		m.exitCode = msg.code
		m.appendLine("")
		m.appendLine(resultStyle.Render(fmt.Sprintf("── session exited (code %d) — esc to quit ──", msg.code)))
		m.updateViewport()

	case renderTickMsg:
		if m.streaming && m.streamBuf.Len() > 0 {
			// Periodic glamour re-render of accumulated stream content.
			content := m.streamBuf.String()
			rendered, err := m.renderer.glamour.Render(content)
			if err != nil {
				rendered = agentStyle.Render(content)
			} else {
				rendered = strings.TrimSpace(rendered)
			}
			m.replaceOrAppendStream(rendered)
			m.updateViewport()
			return m, renderTick() // schedule next tick
		}
		// Not streaming — no more ticks needed.
		return m, nil

	case wsErrorMsg:
		m.appendLine(errorStyle.Render(fmt.Sprintf("WS error: %v", msg.err)))
		m.exited = true
		m.updateViewport()
	}

	return m, tea.Batch(cmds...)
}

func (m *model) handleAgentEvent(ev agentEvent, isReplay bool) tea.Cmd {
	switch ev.Type {
	case "agent_message":
		isDelta, _ := ev.Data["delta"].(bool)
		text, _ := ev.Data["text"].(string)
		if text == "" {
			return nil
		}

		if isDelta {
			wasStreaming := m.streaming
			m.streaming = true
			m.streamBuf.WriteString(text)
			m.replaceStreamLine()
			if !wasStreaming {
				return renderTick() // start the render tick cycle
			}
			return nil
		}

		// Complete message — render through glamour.
		if m.streaming {
			m.streaming = false
			m.streamBuf.Reset()
		}
		rendered := m.renderer.renderEvent(ev, isReplay)
		if rendered != "" {
			m.replaceOrAppendStream(rendered)
		}

	case "tool_use":
		m.flushStream()
		m.toolCalls++
		rendered := m.renderer.renderEvent(ev, isReplay)
		if rendered != "" {
			m.appendLine(rendered)
		}

	case "tool_result":
		rendered := m.renderer.renderEvent(ev, isReplay)
		if rendered != "" {
			m.appendLine(rendered)
		}

	case "result":
		m.flushStream()
		// Extract metrics.
		if data, ok := ev.Data["usage"].(map[string]interface{}); ok {
			if v, ok := data["input_tokens"].(float64); ok {
				m.inputTokens += int(v)
			}
			if v, ok := data["output_tokens"].(float64); ok {
				m.outputTokens += int(v)
			}
		}
		if v, ok := ev.Data["cost_usd"].(float64); ok {
			m.costUSD += v
		}
		rendered := m.renderer.renderEvent(ev, isReplay)
		if rendered != "" {
			m.appendLine(rendered)
		}

	case "system":
		subtype, _ := ev.Data["subtype"].(string)
		if subtype == "heartbeat" {
			return nil
		}
		// Detect interactive prompts.
		if subtype == "input_request" || subtype == "question" {
			question, _ := ev.Data["text"].(string)
			var opts []string
			if rawOpts, ok := ev.Data["options"].([]interface{}); ok {
				for _, o := range rawOpts {
					if s, ok := o.(string); ok {
						opts = append(opts, s)
					}
				}
			}
			m.mode = modePrompt
			m.appendLine(m.renderer.renderPrompt(question, opts))
			m.input.Placeholder = "Your response..."
			return nil
		}
		rendered := m.renderer.renderEvent(ev, isReplay)
		if rendered != "" {
			m.appendLine(rendered)
		}

	default:
		m.flushStream()
		rendered := m.renderer.renderEvent(ev, isReplay)
		if rendered != "" {
			m.appendLine(rendered)
		}
	}
	return nil
}

const streamMarker = "<<STREAM>>"

func (m *model) replaceStreamLine() {
	content := m.streamBuf.String()
	if content == "" {
		return
	}
	styled := agentStyle.Render(content)
	// Replace existing stream marker.
	for i := len(m.lines) - 1; i >= 0; i-- {
		if m.lines[i] == streamMarker || (i == len(m.lines)-1 && m.streaming) {
			m.lines[i] = styled
			return
		}
	}
	m.lines = append(m.lines, styled)
}

func (m *model) replaceOrAppendStream(rendered string) {
	// Replace the last stream line with the final glamour-rendered version.
	for i := len(m.lines) - 1; i >= 0; i-- {
		if m.lines[i] == streamMarker {
			m.lines[i] = rendered
			return
		}
	}
	// Replace the last line if it was a stream line.
	if len(m.lines) > 0 && m.lines[len(m.lines)-1] != "" {
		m.lines[len(m.lines)-1] = rendered
		return
	}
	m.appendLine(rendered)
}

func (m *model) flushStream() {
	if !m.streaming {
		return
	}
	m.streaming = false
	content := m.streamBuf.String()
	m.streamBuf.Reset()
	if content == "" {
		return
	}
	rendered, err := m.renderer.glamour.Render(content)
	if err != nil {
		rendered = content
	}
	rendered = strings.TrimSpace(rendered)
	m.replaceOrAppendStream(rendered)
}

func (m *model) appendLine(s string) {
	m.lines = append(m.lines, s)
}

func (m *model) updateViewport() {
	content := strings.Join(m.lines, "\n")
	m.viewport.SetContent(content)
	if m.followMode {
		m.viewport.GotoBottom()
	}
}

func (m *model) sendStdin(text string) {
	_ = m.conn.WriteJSON(map[string]string{
		"type": "stdin",
		"data": text,
	})
	m.appendLine(systemStyle.Render("⏳ thinking..."))
}

func (m *model) handleCommand(cmd string) {
	parts := strings.SplitN(cmd, " ", 2)
	switch parts[0] {
	case "/steer":
		if len(parts) > 1 {
			_ = m.conn.WriteJSON(map[string]string{"type": "steer", "data": parts[1]})
			m.appendLine(systemStyle.Render("steered: " + parts[1]))
		}
	case "/interrupt":
		_ = m.conn.WriteJSON(map[string]string{"type": "interrupt"})
		m.appendLine(systemStyle.Render("sent interrupt"))
	case "/info":
		info, _ := json.MarshalIndent(m.meta, "", "  ")
		m.appendLine(systemStyle.Render(string(info)))
	case "/quit", "/exit":
		// Handled by esc in Update.
		m.appendLine(systemStyle.Render("use esc to quit"))
	default:
		m.appendLine(systemStyle.Render("unknown command: " + parts[0]))
	}
}

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	// Header bar.
	title := m.meta.Name
	if title == "" {
		title = m.meta.SessionID
		if len(title) > 8 {
			title = title[:8]
		}
	}

	statusDot := "●"
	statusColor := lipgloss.Color("42") // green
	if m.exited {
		statusDot = "○"
		statusColor = lipgloss.Color("241")
	} else if m.streaming {
		statusDot = "◉"
	}

	header := lipgloss.NewStyle().Foreground(statusColor).Render(statusDot) + " " +
		lipgloss.NewStyle().Bold(true).Render(title)
	if m.meta.Agent != "" {
		header += systemStyle.Render(" (" + m.meta.Agent + ")")
	}
	if m.mode == modePrompt {
		header += promptStyle.Render(" [WAITING FOR INPUT]")
	}

	// Status bar with metrics.
	metrics := systemStyle.Render(fmt.Sprintf(
		"tokens: %d in / %d out  tools: %d  cost: $%.4f",
		m.inputTokens, m.outputTokens, m.toolCalls, m.costUSD,
	))

	headerBar := lipgloss.NewStyle().Width(m.width).Padding(0, 1).Render(header + "  " + metrics)

	divider := lipgloss.NewStyle().
		Foreground(lipgloss.Color("238")).
		Render(strings.Repeat("─", m.width))

	return fmt.Sprintf("%s\n%s\n%s\n%s", headerBar, m.viewport.View(), divider, m.input.View())
}
