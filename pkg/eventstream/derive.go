package eventstream

import (
	"encoding/json"
	"github.com/danieliser/agentruntime/pkg/durable"
	"github.com/danieliser/agentruntime/pkg/nativeprotocol"
)

func derive(record nativeprotocol.Record) (string, json.RawMessage, durable.Stream, string, error) {
	switch record.Stream {
	case nativeprotocol.StreamProviderStdout:
		adapter, err := nativeprotocol.NewAdapter(record.Provider, record.RuntimeName)
		if err != nil {
			return "", nil, "", "", durable.NewError(durable.CodeInvalidArgument, "derive_native_record", "unsupported provider", err)
		}
		derived, err := adapter.Decode(record.Raw)
		if err != nil {
			return "", nil, "", "", durable.NewError(durable.CodeIndeterminate, "derive_native_record", "decode provider record", err)
		}
		if record.SupersededBySteer && record.Provider == nativeprotocol.ProviderClaude && nativeprotocol.ClaudeSteerBoundary(record.Raw) {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(derived.Payload, &payload); err != nil {
				return "", nil, "", "", err
			}
			payload["superseded_by_steer"] = json.RawMessage("true")
			previous, err := json.Marshal(derived.Type)
			if err != nil {
				return "", nil, "", "", err
			}
			payload["superseded_event_type"] = previous
			derived.Payload, err = json.Marshal(payload)
			if err != nil {
				return "", nil, "", "", err
			}
			derived.Type = "turn.superseded"
		}
		return derived.Type, derived.Payload, durable.StreamProviderStdout, derived.ProviderID, nil
	case nativeprotocol.StreamRuntimeStderr:
		payload, err := json.Marshal(map[string]string{"text": string(record.Raw)})
		if err != nil {
			return "", nil, "", "", durable.NewError(durable.CodeIndeterminate, "derive_native_record", "encode stderr payload", err)
		}
		return "runtime.stderr", payload, durable.StreamRuntimeStderr, "", nil
	default:
		return "", nil, "", "", durable.NewError(durable.CodeInvalidArgument, "derive_native_record", "unsupported native stream", nil)
	}
}
