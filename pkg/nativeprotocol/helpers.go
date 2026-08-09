package nativeprotocol

import "encoding/json"

func marshalMessages(op string, messages []any) ([][]byte, error) {
	encoded := make([][]byte, 0, len(messages))
	for _, message := range messages {
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, newError(CodeEncode, op, "marshal native message", err)
		}
		encoded = append(encoded, raw)
	}
	return encoded, nil
}

func mustPayload(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"error":"derived payload encoding failed"}`)
	}
	return raw
}

func unparsedEvent(provider Provider, raw []byte, decodeErr error) DerivedEvent {
	return DerivedEvent{
		Type: "error.protocol",
		Payload: mustPayload(map[string]any{
			"provider": provider,
			"error":    decodeErr.Error(),
			"text":     string(raw),
		}),
	}
}
