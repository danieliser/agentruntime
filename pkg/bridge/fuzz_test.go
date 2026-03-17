package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

const fuzzMaxBytes = 8 << 10

func clampBytes(data []byte) []byte {
	if len(data) > fuzzMaxBytes {
		return data[:fuzzMaxBytes]
	}
	return data
}

func FuzzUnmarshalClientFrame(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		{},
		[]byte("{}"),
		[]byte(`{"type":"stdin","data":"hello"}`),
		[]byte(`{"type":"ping"}`),
		[]byte(`{"type":"resize","cols":80,"rows":24}`),
		[]byte(`{"type":"steer","data":"new direction"}`),
		[]byte(`{"type":"interrupt"}`),
		[]byte(`{"type":"context","context":{"text":"some context","file_path":"/tmp/a.go"}}`),
		[]byte(`{"type":"mention","mention":{"file_path":"/app/main.go","line_start":10,"line_end":20}}`),
		[]byte(`{"type":"unknown_type","data":"whatever"}`),
		[]byte(`{"type":"","data":null,"cols":-1,"rows":-1}`),
		[]byte(`[1,2,3]`),
		[]byte(`{"type":"stdin","data":"` + strings.Repeat("x", 4096) + `"}`),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		data = clampBytes(data)
		frame, err := UnmarshalClientFrame(data)
		if err != nil {
			return
		}
		// Re-marshal should also not panic.
		_, _ = json.Marshal(frame)
	})
}

func FuzzClientFrameRouting(f *testing.F) {
	for _, seed := range []string{
		"stdin",
		"ping",
		"resize",
		"steer",
		"interrupt",
		"context",
		"mention",
		"",
		"unknown",
		"STDIN",
		"stdin\x00",
	} {
		f.Add(seed, `hello world`, 80, 24, `/tmp/a.go`, `some context`, 10, 20)
	}

	f.Fuzz(func(t *testing.T, frameType, data string, cols, rows int, filePath, contextText string, lineStart, lineEnd int) {
		frame := ClientFrame{
			Type: frameType,
			Data: data,
			Cols: cols,
			Rows: rows,
		}

		// Attach optional sub-structs based on type.
		if frameType == "context" || len(frameType)%2 == 0 {
			frame.Context = &ClientFrameContext{
				Text:     contextText,
				FilePath: filePath,
			}
		}
		if frameType == "mention" || len(frameType)%3 == 0 {
			frame.Mention = &ClientFrameMention{
				FilePath:  filePath,
				LineStart: lineStart,
				LineEnd:   lineEnd,
			}
		}

		// Marshal → Unmarshal round-trip should never panic.
		raw, err := json.Marshal(frame)
		if err != nil {
			return
		}
		got, err := UnmarshalClientFrame(raw)
		if err != nil {
			t.Fatalf("failed to unmarshal marshalled frame: %v", err)
		}
		if got.Type != frame.Type {
			t.Fatalf("type mismatch: %q vs %q", got.Type, frame.Type)
		}
	})
}
