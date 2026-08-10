package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveStructuredOutputCanonicalizesSchemaAndHash(t *testing.T) {
	request := SessionRequest{Agent: "codex", StructuredOutput: &StructuredOutput{
		JSONSchema: json.RawMessage(`{ "required": ["url"], "properties": {"url":{"type":"string"}}, "type":"object", "additionalProperties":false }`),
	}}
	resolved, err := resolveStructuredOutput(&request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Hash == "" || !strings.HasPrefix(resolved.Hash, "sha256:") || request.StructuredOutput.MaxBytes != DefaultStructuredOutputMaxBytes {
		t.Fatalf("resolved structured output = %+v request=%+v", resolved, request.StructuredOutput)
	}
	if string(request.StructuredOutput.JSONSchema) != `{"additionalProperties":false,"properties":{"url":{"type":"string"}},"required":["url"],"type":"object"}` {
		t.Fatalf("canonical schema = %s", request.StructuredOutput.JSONSchema)
	}
}

func TestResolveStructuredOutputRejectsUnsupportedInvalidAndOversizedContracts(t *testing.T) {
	for _, test := range []struct {
		name    string
		request SessionRequest
		code    string
	}{
		{name: "provider", request: SessionRequest{Agent: "other", StructuredOutput: &StructuredOutput{JSONSchema: json.RawMessage(`{"type":"object"}`)}}, code: "structured_output_unsupported"},
		{name: "schema", request: SessionRequest{Agent: "claude", StructuredOutput: &StructuredOutput{JSONSchema: json.RawMessage(`{"type":"not-real"}`)}}, code: "invalid_argument"},
		{name: "limit", request: SessionRequest{Agent: "codex", StructuredOutput: &StructuredOutput{JSONSchema: json.RawMessage(`{"type":"object"}`), MaxBytes: MaximumStructuredOutputBytes + 1}}, code: "invalid_argument"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolveStructuredOutput(&test.request)
			if err == nil || !strings.Contains(err.Error(), test.code) {
				t.Fatalf("resolve structured output error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestStructuredResultCollectorValidatesExactBoundedBytes(t *testing.T) {
	contract := StructuredOutput{JSONSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"],"additionalProperties":false}`), MaxBytes: 64}
	collector, err := newStructuredResultCollector("codex", &contract)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`{"url":`, `"https://example.com"}`} {
		if err := collector.Observe("content.delta", json.RawMessage(`{"text":`+mustJSONString(t, text)+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := collector.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Bytes) != `{"url":"https://example.com"}` || !strings.HasPrefix(result.Hash, "sha256:") {
		t.Fatalf("structured result = %+v", result)
	}

	invalid, _ := newStructuredResultCollector("codex", &contract)
	_ = invalid.Observe("content.delta", json.RawMessage(`{"text":"{\"wrong\":true}"}`))
	if _, err := invalid.Finalize(); err == nil || !strings.Contains(err.Error(), "structured_output_invalid") {
		t.Fatalf("invalid result error = %v", err)
	}

	tooLargeContract := contract
	tooLargeContract.MaxBytes = 4
	tooLarge, _ := newStructuredResultCollector("codex", &tooLargeContract)
	if err := tooLarge.Observe("content.delta", json.RawMessage(`{"text":"12345"}`)); err == nil || !strings.Contains(err.Error(), "structured_output_too_large") {
		t.Fatalf("oversized result error = %v", err)
	}
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
