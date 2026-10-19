package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type streamEvent struct {
	Sequence  int64           `json:"sequence"`
	Type      string          `json:"type"`
	Stream    string          `json:"stream"`
	Payload   json.RawMessage `json:"payload"`
	RawBase64 string          `json:"raw_base64"`
}

func init() { commandRegistry["since"] = cmdSince; commandRegistry["wait"] = cmdWait }

func streamFlags(ctx *Context, name string, args []string) (*flag.FlagSet, *string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(ctx.Log)
	dir := fs.String("cursor-dir", "", "cursor directory")
	fs.BoolVar(&ctx.Config.Raw, "raw", ctx.Config.Raw, "print raw event records")
	if err := fs.Parse(normalizeStreamArgs(args, map[string]bool{"cursor-dir": true})); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	return fs, dir, nil
}

// summarizeEvents renders events for people. Streamed text fragments join
// into running text, and a completed message is skipped when its fragments
// were already shown.
func summarizeEvents(events []streamEvent, raw bool) string {
	var b strings.Builder
	var streamed strings.Builder // fragments of the message in progress
	midLine := false
	endLine := func() {
		if midLine {
			b.WriteByte('\n')
			midLine = false
		}
	}
	writeText := func(text string) {
		if text == "" {
			return
		}
		endLine()
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}
	finishMessage := func(text string) {
		shown := streamed.String()
		streamed.Reset()
		switch {
		case shown == "":
			writeText(text)
		case text == shown || text == "":
			endLine()
		case strings.HasPrefix(text, shown):
			// The stream stopped early; finish the line with the rest.
			rest := text[len(shown):]
			b.WriteString(rest)
			midLine = !strings.HasSuffix(rest, "\n")
			endLine()
		default:
			writeText(text)
		}
	}
	for _, e := range events {
		if raw {
			if e.RawBase64 != "" {
				data, err := base64.StdEncoding.DecodeString(e.RawBase64)
				if err == nil {
					b.Write(data)
					b.WriteByte('\n')
				}
			}
			continue
		}
		var p struct {
			SubagentPath string          `json:"subagent_path"`
			Text         string          `json:"text"`
			Name         string          `json:"name"`
			Tool         string          `json:"tool_name"`
			Item         json.RawMessage `json:"item"`
			Calls        []struct {
				Name string `json:"name"`
			} `json:"calls"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if strings.HasPrefix(e.Type, "subagent.") {
			if strings.HasSuffix(e.Type, ".delta") {
				continue
			}
			summary := p.Text
			if summary == "" {
				summary = strings.TrimPrefix(e.Type, "subagent.")
				if len(p.Item) > 0 {
					summary += " " + string(p.Item)
				}
			}
			endLine()
			fmt.Fprintf(&b, "[sub:%s] %s\n", p.SubagentPath, summary)
			continue
		}
		switch e.Type {
		case "content.delta":
			if p.Text != "" {
				b.WriteString(p.Text)
				streamed.WriteString(p.Text)
				midLine = !strings.HasSuffix(p.Text, "\n")
			}
		case "content.completed":
			finishMessage(p.Text)
		case "turn.completed", "turn.failed":
			streamed.Reset()
			endLine()
		case "tool.call", "tool.result":
			if e.Type == "tool.call" && p.Text != "" {
				// Claude sends text and tool use as one message.
				finishMessage(p.Text)
			}
			item := itemSummary(p.Item)
			if item.kind == "agentMessage" {
				if e.Type == "tool.result" {
					finishMessage(item.text)
				}
				continue
			}
			if item.kind == "userMessage" || item.kind == "reasoning" {
				continue
			}
			streamed.Reset()
			name := p.Name
			if name == "" {
				name = p.Tool
			}
			if name == "" && len(p.Calls) > 0 {
				names := make([]string, 0, len(p.Calls))
				for _, call := range p.Calls {
					names = append(names, call.Name)
				}
				name = strings.Join(names, ", ")
			}
			if name == "" {
				name = item.label
			}
			if name == "" {
				continue // e.g. Claude tool output carried in a user message
			}
			endLine()
			fmt.Fprintf(&b, "[%s %s]\n", e.Type, name)
		}
	}
	endLine()
	return b.String()
}

type itemDetails struct{ kind, text, label string }

// itemSummary reads a Codex item: its type, its text, and a short label.
func itemSummary(raw json.RawMessage) itemDetails {
	if len(raw) == 0 {
		return itemDetails{}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return itemDetails{label: string(raw)}
	}
	str := func(key string) string {
		var v string
		_ = json.Unmarshal(fields[key], &v)
		return v
	}
	d := itemDetails{kind: str("type"), text: str("text")}
	d.label = d.kind
	for _, key := range []string{"command", "tool", "name", "path"} {
		if v := str(key); v != "" {
			if r := []rune(v); len(r) > 120 {
				v = string(r[:117]) + "..."
			}
			d.label = strings.TrimSpace(d.label + " " + v)
			break
		}
	}
	if d.label == "" {
		d.label = string(raw)
	}
	return d
}

func hiddenSubagentDelta(kind string) bool {
	return strings.HasPrefix(kind, "subagent.") && strings.HasSuffix(kind, ".delta")
}

// normalizeStreamArgs allows stream flags on either side of the positional ID,
// matching the global agentctl flag behavior.
func normalizeStreamArgs(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		flags = append(flags, a)
		if valueFlags[name] && !hasValue && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func mustJSON(m map[string]json.RawMessage) []byte { b, _ := json.Marshal(m); return b }
func matchingReason(e streamEvent) string {
	switch {
	case e.Type == "turn.failed":
		return "error"
	case e.Type == "turn.completed" || strings.HasPrefix(e.Type, "turn.") && strings.HasSuffix(e.Type, ".ended"):
		return "turn_end"
	case e.Type == "control.approval.request":
		return "approval"
	case strings.HasPrefix(e.Type, "error."):
		return "error"
	case strings.HasPrefix(e.Type, "session.") && e.Stream == "terminal":
		return "done"
	}
	return ""
}
func summarySuffix(e streamEvent) string {
	var p struct {
		Text    string `json:"text"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	if p.Text != "" {
		return ": " + p.Text
	}
	if p.Message != "" {
		return ": " + p.Message
	}
	return ""
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func processEnv() map[string]string {
	m := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}
