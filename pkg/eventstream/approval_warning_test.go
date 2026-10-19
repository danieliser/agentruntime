package eventstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danieliser/agentruntime/pkg/durable/memory"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func TestApprovalEventsUseHostSuppliedRuntimeForWarnings(t *testing.T) {
	for _, runtimeName := range []string{"local", "docker"} {
		for _, method := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"} {
			t.Run(runtimeName+"/"+method, func(t *testing.T) {
				store := memory.New()
				t.Cleanup(func() { store.Close() })
				createStreamSession(t, context.Background(), store, "warning")
				raw := []byte(`{"id":42,"method":"` + method + `","params":{"runtime":"local"}}`)
				event, err := New(store).Ingest(context.Background(), IngestParams{SessionID: "warning", Generation: 1, Record: nativeprotocol.Record{RuntimeName: runtimeName, Provider: nativeprotocol.ProviderCodex, Stream: nativeprotocol.StreamProviderStdout, Ordinal: 1, Timestamp: time.Now(), Raw: raw}})
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					Warning string `json:"warning"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				expected := "runs outside the sandbox in the container"
				if runtimeName == "local" {
					expected = "runs outside the sandbox on the host"
				}
				if !strings.Contains(payload.Warning, expected) {
					t.Fatal("runtime warning missing", payload.Warning)
				}
				if runtimeName == "docker" && strings.Contains(payload.Warning, "host") {
					t.Fatal("Docker warning claims host access")
				}
				if string(event.Raw) != string(raw) {
					t.Fatal("provider record was modified")
				}
			})
		}
	}
}
