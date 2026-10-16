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

func summarizeEvents(events []streamEvent, raw bool) string {
	var b strings.Builder
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
			Text string          `json:"text"`
			Name string          `json:"name"`
			Tool string          `json:"tool_name"`
			Item json.RawMessage `json:"item"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		switch e.Type {
		case "content.delta", "content.completed":
			if p.Text != "" {
				b.WriteString(p.Text)
				if !strings.HasSuffix(p.Text, "\n") {
					b.WriteByte('\n')
				}
			}
		case "tool.call", "tool.result":
			name := p.Name
			if name == "" {
				name = p.Tool
			}
			if name == "" && len(p.Item) > 0 {
				name = string(p.Item)
			}
			fmt.Fprintf(&b, "[%s %s]\n", e.Type, name)
		}
	}
	return b.String()
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
