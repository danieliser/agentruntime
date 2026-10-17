package nativeprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

func TestCodexSteerAndInterruptFollowObservedActiveTurn(t *testing.T) {
	process := newFakeProcess()
	adapter, _ := NewAdapter(ProviderCodex)
	transport := NewStreamTransport(adapter, process.IO(), RecoveryMetadata{})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		process.finish(Exit{Code: 0})
		<-transport.Wait()
	})
	for _, turn := range []string{"turn-1", "turn-2"} {
		process.writeStdout(t, []byte(fmt.Sprintf(`{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":%q}}}`, turn)))
		_ = receiveRecord(t, transport.Records())
		for _, kind := range []InputKind{InputSteer, InputInterrupt} {
			if err := transport.Send(context.Background(), Input{Kind: kind, Text: "replacement"}); err != nil {
				t.Fatal(err)
			}
			request := decodeInputObject(t, process.readInput(t))
			params := request["params"].(map[string]any)
			method, turnKey := "turn/steer", "expectedTurnId"
			if kind == InputInterrupt {
				method, turnKey = "turn/interrupt", "turnId"
			}
			if request["method"] != method || params["threadId"] != "thread-1" || params[turnKey] != turn {
				t.Fatalf("%s did not target the observed active turn: %+v", kind, request)
			}
		}
		process.writeStdout(t, []byte(fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":%q,"status":"interrupted"}}}`, turn)))
		_ = receiveRecord(t, transport.Records())
	}
}

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
