package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const fuzzMaxBytes = 8 << 10

func FuzzNormalizeClaudeAgentMessage(f *testing.F) {
	addNormalizeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = normalizeClaudeAgentMessage(fuzzMapFromBytes(data))
	})
}

func FuzzNormalizeCodexAgentMessage(f *testing.F) {
	addNormalizeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = normalizeCodexAgentMessage(fuzzMapFromBytes(data))
	})
}

func FuzzNormalizeClaudeResult(f *testing.F) {
	addNormalizeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = normalizeClaudeResult(fuzzMapFromBytes(data))
	})
}

func FuzzNormalizeCodexResult(f *testing.F) {
	addNormalizeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = normalizeCodexResult(fuzzMapFromBytes(data))
	})
}

func FuzzNormalizeClaudeToolUse(f *testing.F) {
	addNormalizeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = normalizeClaudeToolUse(fuzzMapFromBytes(data))
	})
}

func FuzzNormalizeCodexToolUse(f *testing.F) {
	addNormalizeSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = normalizeCodexToolUse(fuzzMapFromBytes(data))
	})
}

func FuzzDecodeReplay(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		{},
		[]byte("\n"),
		[]byte("{}\n"),
		[]byte(`{"type":"agent_message","data":{"text":"hello"},"offset":1,"timestamp":2}` + "\n"),
		[]byte(`{"type":"agent_message","data":{"text":"hello"}}` + "\n" + `{"type":"tool_use","data":{"name":"Edit"}}` + "\n"),
		[]byte(`{"type":"broken"` + "\n" + `not-json` + "\n"),
		[]byte(strings.Repeat(`{"type":"agent_message","data":{"text":"`+strings.Repeat("x", 128)+`"}}`+"\n", 8)),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		_ = decodeReplay(clampBytes(data))
	})
}

func FuzzParseAgentConfig(f *testing.F) {
	for _, seed := range []string{
		"",
		"{}",
		`{"model":"claude-opus-4-5"}`,
		`{"model":"o3","resume_session":"sess-abc","env":{"FOO":"bar"},"approval_mode":"full-auto","max_turns":5,"allowed_tools":["Read","Write"]}`,
		`{"max_turns":-1,"allowed_tools":null}`,
		`[1,2,3]`,
		`{"env":{"":""}}`,
		"not-json",
		`{"model":` + strings.Repeat(`"x"`, 2048) + `}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		t.Setenv("AGENT_CONFIG", raw)
		_, _ = parseAgentConfig()
	})
}

func FuzzDecodeCommandData(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		{},
		[]byte("null"),
		[]byte("{}"),
		[]byte(`{"content":"hello world"}`),
		[]byte(`{"text":"ctx","filePath":"/tmp/a.go"}`),
		[]byte(`{"filePath":"/app/main.go","lineStart":10,"lineEnd":20}`),
		[]byte(`{"content":""}`),
		[]byte(`[1,2,3]`),
		[]byte(`{"unknown_field": true, "content": 42}`),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		data = clampBytes(data)
		// Exercise all command payload types — none should panic.
		var p promptCommand
		_ = decodeCommandData(json.RawMessage(data), &p)
		var s steerCommand
		_ = decodeCommandData(json.RawMessage(data), &s)
		var c contextCommand
		_ = decodeCommandData(json.RawMessage(data), &c)
		var m mentionCommand
		_ = decodeCommandData(json.RawMessage(data), &m)
	})
}

func FuzzParseAgentCommand(f *testing.F) {
	for _, seed := range []string{
		"",
		"[]",
		`[""]`,
		`["claude"]`,
		`["codex","app-server","--listen","stdio://"]`,
		`["/usr/local/bin/claude","--print","hello"]`,
		`{"cmd":"claude"}`,
		`["unterminated"`,
		strings.Repeat("x", 4096),
		`["` + strings.Repeat("y", 4096) + `"]`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = parseAgentCommand(raw)
	})
}

func addNormalizeSeeds(f *testing.F) {
	for _, seed := range normalizeSeeds() {
		f.Add(seed)
	}
}

func normalizeSeeds() [][]byte {
	long := strings.Repeat("x", 4096)
	return [][]byte{
		nil,
		{},
		[]byte(`{}`),
		[]byte(`{"text":"","delta":false}`),
		[]byte(`{"text":null,"usage":null,"item":null,"turn":null}`),
		[]byte(`{"text":"hello","delta":true,"model":"claude-3.7","usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`),
		[]byte(`{"subtype":"success","session_id":"sess-1","cost_usd":0.42,"duration_ms":99,"num_turns":2}`),
		[]byte(`{"id":"toolu_1","name":"Edit","input":{"file_path":"/tmp/app.go","content":"package main"}}`),
		[]byte(`{"turnId":"turn-1","itemId":"item-1","delta":"partial","text":"fallback","final":false}`),
		[]byte(`{"threadId":"thread-1","turn":{"id":"turn-2","status":"success"},"usage":{"input_tokens":10,"output_tokens":20,"cached_input_tokens":5}}`),
		[]byte(`{"item":{"id":"tool-1","tool":"Read","server":"filesystem","arguments":{"path":"/tmp/app.go"},"result":{"content":[{"text":"ok"}]}}}`),
		[]byte(`{"item":{"id":"cmd-1","type":"command_execution","command":"go test","aggregatedOutput":"pass","result":{"content":[{"text":"command ok"}]}}}`),
		[]byte(`{"item":{"id":"file-1","type":"file_change","error":{"message":"boom"}}}`),
		[]byte(`{"nested":{"array":[null,true,1.25,{"deep":["x",{"value":null}]}]}}`),
		[]byte(`{"text":"` + long + `","item":{"result":{"content":[{"text":"` + long + `"}]}}}`),
	}
}

func fuzzMapFromBytes(data []byte) map[string]any {
	data = clampBytes(data)
	if len(data) == 0 {
		return map[string]any{}
	}

	var decoded any
	if err := json.Unmarshal(data, &decoded); err == nil {
		if m, ok := decoded.(map[string]any); ok {
			return m
		}
	}

	text := fuzzStringSegment(data, 0)
	model := fuzzStringSegment(data, 1)
	command := fuzzStringSegment(data, 2)
	if command == "" {
		command = "echo fuzz"
	}

	return map[string]any{
		"text":       text,
		"delta":      len(data)%2 == 0,
		"model":      model,
		"subtype":    fuzzStringSegment(data, 3),
		"session_id": fuzzStringSegment(data, 4),
		"turnId":     fuzzStringSegment(data, 5),
		"itemId":     fuzzStringSegment(data, 6),
		"threadId":   fuzzStringSegment(data, 7),
		"usage": map[string]any{
			"input_tokens":                float64(len(data)),
			"output_tokens":               float64(sumBytes(data) % 2048),
			"cache_read_input_tokens":     float64(firstByte(data, 0)),
			"cache_creation_input_tokens": float64(firstByte(data, 1)),
			"cached_input_tokens":         float64(firstByte(data, 2)),
		},
		"input": map[string]any{
			"command": command,
			"path":    fuzzStringSegment(data, 8),
			"nested": map[string]any{
				"value": nil,
				"flags": []any{len(data)%3 == 0, float64(firstByte(data, 3))},
			},
		},
		"item": map[string]any{
			"id":        fuzzStringSegment(data, 9),
			"tool":      fuzzStringSegment(data, 10),
			"server":    fuzzStringSegment(data, 11),
			"type":      fuzzItemType(data),
			"command":   command,
			"arguments": map[string]any{"raw": text, "nested": map[string]any{"model": model}},
			"result": map[string]any{
				"content": []any{
					map[string]any{"text": fuzzStringSegment(data, 12)},
					nil,
				},
			},
			"aggregatedOutput": fuzzStringSegment(data, 13),
			"durationMs":       float64(sumBytes(data) % 10_000),
			"error":            fuzzErrorValue(data),
		},
		"turn": map[string]any{
			"id":     fuzzStringSegment(data, 14),
			"status": fuzzStringSegment(data, 15),
		},
		"nested": map[string]any{
			"array": []any{
				nil,
				text,
				float64(firstByte(data, 4)),
				map[string]any{"deep": fuzzStringSegment(data, 16)},
			},
		},
	}
}

func clampBytes(data []byte) []byte {
	if len(data) > fuzzMaxBytes {
		return data[:fuzzMaxBytes]
	}
	return data
}

func fuzzStringSegment(data []byte, segment int) string {
	if len(data) == 0 {
		return ""
	}

	data = clampBytes(data)
	size := len(data) / 17
	if size == 0 {
		size = len(data)
	}
	start := (segment * size) % len(data)
	end := start + size
	if end > len(data) {
		end = len(data)
	}
	if start >= end {
		return ""
	}

	raw := strings.ToValidUTF8(string(data[start:end]), "?")
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, raw)
	if len(raw) > 512 {
		raw = raw[:512]
	}
	return raw
}

func fuzzItemType(data []byte) string {
	switch firstByte(data, 5) % 4 {
	case 0:
		return "commandExecution"
	case 1:
		return "command_execution"
	case 2:
		return "fileChange"
	default:
		return "file_change"
	}
}

func fuzzErrorValue(data []byte) any {
	if len(data) == 0 || firstByte(data, 6)%3 != 0 {
		return nil
	}
	return map[string]any{"message": fuzzStringSegment(data, 0)}
}

func firstByte(data []byte, idx int) byte {
	if len(data) == 0 {
		return 0
	}
	return data[idx%len(data)]
}

func sumBytes(data []byte) int {
	total := 0
	for _, b := range clampBytes(data) {
		total += int(b)
	}
	return total
}
