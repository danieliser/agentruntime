package nativeprotocol

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestCodexInterruptIncludesActiveTurnID(t *testing.T) {
	for _, throughTransport := range []bool{false, true} {
		t.Run(map[bool]string{false: "adapter", true: "transport"}[throughTransport], func(t *testing.T) {
			adapter, _ := NewAdapter(ProviderCodex)
			var raw []byte
			if throughTransport {
				stdin := new(interruptWriteCloser)
				tr := NewStreamTransport(adapter, ProcessIO{Stdin: stdin}, RecoveryMetadata{})
				tr.started = true
				tr.providerID = "thread-1"
				tr.turnID = "turn-2"
				if err := tr.Interrupt(context.Background()); err != nil {
					t.Fatal(err)
				}
				raw = stdin.data
			} else {
				messages, err := adapter.Encode(Input{Kind: InputInterrupt, ProviderID: "thread-1", TurnID: "turn-2"})
				if err != nil {
					t.Fatal(err)
				}
				raw = messages[0]
			}
			var req struct {
				Method string `json:"method"`
				Params struct {
					Thread string `json:"threadId"`
					Turn   string `json:"turnId"`
				} `json:"params"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatal(err)
			}
			if req.Method != "turn/interrupt" || req.Params.Thread != "thread-1" || req.Params.Turn != "turn-2" {
				t.Fatalf("request=%s", raw)
			}
		})
	}
}

type interruptWriteCloser struct{ data []byte }

func (w *interruptWriteCloser) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}
func (w *interruptWriteCloser) Close() error { return nil }

var _ io.WriteCloser = (*interruptWriteCloser)(nil)
