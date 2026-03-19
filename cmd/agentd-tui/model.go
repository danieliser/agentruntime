package main

import (
	"encoding/json"
	"fmt"
	"strings"

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
	mode         inputMode
	promptText   string   // current question text
	promptOpts   []string // current options
	connected    bool
	exited       bool
	exitCode     int
	streaming    bool     // currently receiving deltas
	streamBuf    strings.Builder // accumulated delta text

	// Layout.
	width  int
	height int
}

func newModel(conn *websocket.Conn, meta chatMeta) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message..."
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
		lines:    make([]string, 0, 256),
	}
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
			// Send interrupt to the agent.
			_ = m.conn.WriteJSON(map[string]string{"type": "interrupt"})
			m.appendLine(systemStyle.Render("sent interrupt (ctrl+c again to quit)"))
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
				// Send response to agent via stdin.
				m.sendStdin(text)
				m.mode = modeNormal
				m.appendLine(fmt.Sprintf("\n%s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render("▶ "+text)))
			} else if strings.HasPrefix(text, "/") {
				m.handleCommand(text)
			} else {
				// Send as stdin to the agent.
				m.sendStdin(text)
				m.appendLine(fmt.Sprintf("\n%s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true).Render("▶ "+text)))
			}
			m.updateViewport()
			return m, nil

		case "pgup", "pgdown", "up", "down":
			// Pass to viewport for scrolling.
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			cmds = append(cmds, cmd)
			return m, tea.Batch(cmds...)
		}

		// Pass other keys to textarea.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.renderer.setWidth(msg.Width)

		inputHeight := 3
		headerHeight := 1
		vpHeight := msg.Height - inputHeight - headerHeight - 2
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
		if label == "" {
			label = msg.sessionID[:8]
		}
		m.appendLine(systemStyle.Render(fmt.Sprintf("Connected to %s (%s)", label, m.meta.Agent)))
		m.updateViewport()

	case agentEventMsg:
		m.handleAgentEvent(msg.event, msg.replay)
		m.updateViewport()

	case sessionExitMsg:
		m.exited = true
		m.exitCode = msg.code
		m.appendLine("")
		m.appendLine(resultStyle.Render(fmt.Sprintf("── Session exited (code %d) — press esc to quit ──", msg.code)))
		m.updateViewport()

	case wsErrorMsg:
		m.appendLine(errorStyle.Render(fmt.Sprintf("WS error: %v", msg.err)))
		m.exited = true
		m.updateViewport()
	}

	return m, tea.Batch(cmds...)
}

func (m *model) handleAgentEvent(ev agentEvent, isReplay bool) {
	// Check for interactive prompts (tool_use with input_request subtype, or system prompts).
	if ev.Type == "agent_message" {
		delta, _ := ev.Data["delta"].(string)
		text, _ := ev.Data["text"].(string)

		if delta != "" {
			// Streaming delta — accumulate.
			m.streaming = true
			m.streamBuf.WriteString(delta)
			// Re-render the accumulated stream.
			m.replaceLastStream()
			return
		}

		if text != "" && m.streaming {
			// Final complete message replaces the stream buffer.
			m.streaming = false
			m.streamBuf.Reset()
			rendered := m.renderer.renderEvent(ev, isReplay)
			if rendered != "" {
				m.replaceLastStream()
				m.appendLine(rendered)
			}
			return
		}
	}

	// Flush any pending stream before other event types.
	if ev.Type != "agent_message" && m.streaming {
		m.flushStream()
	}

	// Detect interactive prompts from the agent.
	if ev.Type == "system" {
		subtype, _ := ev.Data["subtype"].(string)
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
			m.promptText = question
			m.promptOpts = opts
			m.appendLine(m.renderer.renderPrompt(question, opts))
			m.input.Placeholder = "Your response..."
			return
		}
	}

	rendered := m.renderer.renderEvent(ev, isReplay)
	if rendered != "" {
		m.appendLine(rendered)
	}
}

func (m *model) appendLine(s string) {
	m.lines = append(m.lines, s)
}

func (m *model) replaceLastStream() {
	// Render the current stream buffer as a streaming block.
	content := m.streamBuf.String()
	if content == "" {
		return
	}
	// Find and replace the last stream marker, or append.
	streamLine := agentStyle.Render(content)
	for i := len(m.lines) - 1; i >= 0; i-- {
		if m.lines[i] == "<<STREAM>>" {
			m.lines[i] = streamLine
			return
		}
	}
	// No existing stream marker — add one.
	m.lines = append(m.lines, "<<STREAM>>")
	m.lines[len(m.lines)-1] = streamLine
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
	// Render final accumulated text through glamour.
	rendered, err := m.renderer.glamour.Render(content)
	if err != nil {
		rendered = content
	}
	rendered = strings.TrimSpace(rendered)
	// Replace the stream marker.
	for i := len(m.lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(m.lines[i], "\x1b") || m.lines[i] == "<<STREAM>>" {
			m.lines[i] = rendered
			return
		}
	}
	m.appendLine(rendered)
}

func (m *model) updateViewport() {
	content := strings.Join(m.lines, "\n")
	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
}

func (m *model) sendStdin(text string) {
	_ = m.conn.WriteJSON(map[string]string{
		"type": "stdin",
		"data": text + "\n",
	})
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
		// Will be handled by the next Update cycle.
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
	status := "●"
	statusColor := lipgloss.Color("42") // green
	if m.exited {
		status = "○"
		statusColor = lipgloss.Color("241")
	}
	header := lipgloss.NewStyle().
		Foreground(statusColor).
		Render(status) + " " +
		lipgloss.NewStyle().Bold(true).Render(title)
	if m.meta.Agent != "" {
		header += systemStyle.Render(" (" + m.meta.Agent + ")")
	}

	// Mode indicator.
	modeLabel := ""
	if m.mode == modePrompt {
		modeLabel = promptStyle.Render(" [WAITING FOR INPUT]")
	}
	header += modeLabel

	headerBar := lipgloss.NewStyle().
		Width(m.width).
		Padding(0, 1).
		Render(header)

	// Divider.
	divider := lipgloss.NewStyle().
		Foreground(lipgloss.Color("238")).
		Render(strings.Repeat("─", m.width))

	// Input area.
	inputBox := m.input.View()

	return fmt.Sprintf("%s\n%s\n%s\n%s", headerBar, m.viewport.View(), divider, inputBox)
}
