package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

var (
	// Styles
	agentStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green
	toolStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // orange
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	systemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241")) // gray
	resultStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))  // blue
	promptStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Bold(true) // yellow bold
	userMsgStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)  // blue bold
	replayDim     = lipgloss.NewStyle().Faint(true)
)

// renderer handles markdown rendering with glamour.
type renderer struct {
	glamour *glamour.TermRenderer
	width   int
}

func newRenderer(width int) *renderer {
	r, _ := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width-4),
	)
	return &renderer{glamour: r, width: width}
}

func (r *renderer) setWidth(w int) {
	if w != r.width && w > 20 {
		r.width = w
		r.glamour, _ = glamour.NewTermRenderer(
			glamour.WithAutoStyle(),
			glamour.WithWordWrap(w-4),
		)
	}
}

// renderEvent converts an agent event to styled terminal output.
func (r *renderer) renderEvent(ev agentEvent, isReplay bool) string {
	var out string

	switch ev.Type {
	case "agent_message":
		text, _ := ev.Data["text"].(string)
		if text == "" {
			return ""
		}

		isDelta, _ := ev.Data["delta"].(bool)
		if isDelta {
			// Delta chunks rendered raw (streaming).
			out = agentStyle.Render(text)
		} else {
			// Complete message — render markdown.
			rendered, err := r.glamour.Render(text)
			if err != nil {
				out = text
			} else {
				out = strings.TrimSpace(rendered)
			}
		}

	case "tool_use":
		name, _ := ev.Data["name"].(string)
		if name == "" {
			return ""
		}
		out = toolStyle.Render(fmt.Sprintf("  ▸ %s", name))

	case "tool_result":
		name, _ := ev.Data["name"].(string)
		if name == "" {
			return ""
		}
		out = toolStyle.Render(fmt.Sprintf("  ✓ %s", name))

	case "result":
		status, _ := ev.Data["status"].(string)
		out = resultStyle.Render(fmt.Sprintf("── session complete: %s ──", status))

	case "error":
		detail, _ := ev.Data["error_detail"].(string)
		if detail == "" {
			detail, _ = ev.Data["text"].(string)
		}
		if detail != "" {
			out = errorStyle.Render("✗ " + detail)
		}

	case "system":
		subtype, _ := ev.Data["subtype"].(string)
		text, _ := ev.Data["text"].(string)
		if subtype == "heartbeat" || subtype == "init" {
			return "" // suppress noise
		}
		if text != "" {
			out = systemStyle.Render("[system] " + text)
		}

	case "progress":
		text, _ := ev.Data["text"].(string)
		if text != "" {
			out = systemStyle.Render("⟳ " + text)
		}

	default:
		return ""
	}

	if isReplay && out != "" {
		out = replayDim.Render(out)
	}

	return out
}

// renderPrompt formats an interactive question/choice from the agent.
func (r *renderer) renderPrompt(question string, options []string) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(promptStyle.Render("━━━ Agent is waiting for input ━━━"))
	b.WriteString("\n\n")
	b.WriteString(question)
	b.WriteString("\n\n")
	for i, opt := range options {
		b.WriteString(fmt.Sprintf("  [%d] %s\n", i+1, opt))
	}
	b.WriteString("\n")
	b.WriteString(promptStyle.Render("Type a number, or type your response:"))
	return b.String()
}
