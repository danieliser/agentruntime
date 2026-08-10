package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/danieliser/agentruntime/pkg/durable"
)

const (
	DefaultStructuredOutputMaxBytes = 1 << 20
	MaximumStructuredOutputBytes    = 4 << 20
	maximumOutputSchemaBytes        = 64 << 10
)

type resolvedStructuredOutput struct {
	Contract *StructuredOutput
	Hash     string
}

func resolveStructuredOutput(request *SessionRequest) (resolvedStructuredOutput, error) {
	const op = "resolve_structured_output"
	if request == nil || request.StructuredOutput == nil {
		return resolvedStructuredOutput{}, nil
	}
	if request.Agent != "claude" && request.Agent != "codex" {
		return resolvedStructuredOutput{}, durable.NewError(durable.CodeStructuredOutputUnsupported, op, "provider cannot enforce structured output", nil)
	}
	if request.Interactive {
		return resolvedStructuredOutput{}, durable.NewError(durable.CodeStructuredOutputUnsupported, op, "interactive sessions cannot enforce one terminal structured result", nil)
	}
	contract := *request.StructuredOutput
	if len(contract.JSONSchema) == 0 || len(contract.JSONSchema) > maximumOutputSchemaBytes {
		return resolvedStructuredOutput{}, durable.NewError(durable.CodeInvalidArgument, op, "json_schema must be non-empty and no larger than 64 KiB", nil)
	}
	var schemaValue any
	if err := json.Unmarshal(contract.JSONSchema, &schemaValue); err != nil {
		return resolvedStructuredOutput{}, durable.NewError(durable.CodeInvalidArgument, op, "json_schema is invalid JSON", err)
	}
	canonical, err := json.Marshal(schemaValue)
	if err != nil {
		return resolvedStructuredOutput{}, durable.NewError(durable.CodeInvalidArgument, op, "canonicalize json_schema", err)
	}
	if _, err := compileOutputSchema(canonical); err != nil {
		return resolvedStructuredOutput{}, durable.NewError(durable.CodeInvalidArgument, op, "json_schema is not a supported schema", err)
	}
	contract.JSONSchema = canonical
	if contract.MaxBytes == 0 {
		contract.MaxBytes = DefaultStructuredOutputMaxBytes
	}
	if contract.MaxBytes < 1 || contract.MaxBytes > MaximumStructuredOutputBytes {
		return resolvedStructuredOutput{}, durable.NewError(durable.CodeInvalidArgument, op, "max_bytes must be between 1 and 4194304", nil)
	}
	request.StructuredOutput = &contract
	digest := sha256.Sum256(canonical)
	return resolvedStructuredOutput{Contract: request.StructuredOutput, Hash: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func compileOutputSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	var schemaValue any
	if err := json.Unmarshal(raw, &schemaValue); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", schemaValue); err != nil {
		return nil, err
	}
	return compiler.Compile("schema.json")
}

type structuredResult struct {
	Bytes []byte
	Hash  string
}

type structuredResultCollector struct {
	provider string
	limit    int
	schema   *jsonschema.Schema
	buffer   bytes.Buffer
	final    []byte
	failure  error
}

func newStructuredResultCollector(provider string, contract *StructuredOutput) (*structuredResultCollector, error) {
	if contract == nil {
		return nil, nil
	}
	schema, err := compileOutputSchema(contract.JSONSchema)
	if err != nil {
		return nil, err
	}
	return &structuredResultCollector{provider: provider, limit: contract.MaxBytes, schema: schema}, nil
}

func (collector *structuredResultCollector) Observe(eventType string, payload json.RawMessage) error {
	if collector == nil || collector.failure != nil {
		return collector.failure
	}
	if eventType == "content.delta" {
		var delta struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(payload, &delta) == nil && delta.Text != "" {
			if collector.buffer.Len()+len(delta.Text) > collector.limit {
				collector.failure = durable.NewError(durable.CodeStructuredOutputTooLarge, "collect_structured_output", "final output exceeds max_bytes", nil)
				return collector.failure
			}
			_, _ = collector.buffer.WriteString(delta.Text)
		}
	}
	if collector.provider == "claude" && eventType == "turn.completed" {
		if result := claudeResultBytes(payload); len(result) != 0 {
			if len(result) > collector.limit {
				collector.failure = durable.NewError(durable.CodeStructuredOutputTooLarge, "collect_structured_output", "final output exceeds max_bytes", nil)
				return collector.failure
			}
			collector.final = result
		}
	}
	return nil
}

func claudeResultBytes(payload json.RawMessage) []byte {
	var derived struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(payload, &derived) != nil || len(derived.Result) == 0 {
		return nil
	}
	var providerResult struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(derived.Result, &providerResult) != nil || len(providerResult.Result) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(providerResult.Result, &text) == nil {
		return []byte(text)
	}
	return append([]byte(nil), providerResult.Result...)
}

func (collector *structuredResultCollector) Finalize() (structuredResult, error) {
	if collector == nil {
		return structuredResult{}, nil
	}
	if collector.failure != nil {
		return structuredResult{}, collector.failure
	}
	raw := collector.final
	if len(raw) == 0 {
		raw = collector.buffer.Bytes()
	}
	if len(raw) == 0 || !json.Valid(raw) {
		return structuredResult{}, durable.NewError(durable.CodeStructuredOutputInvalid, "validate_structured_output", "final output is not valid JSON", nil)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return structuredResult{}, durable.NewError(durable.CodeStructuredOutputInvalid, "validate_structured_output", "decode final JSON", err)
	}
	if err := collector.schema.Validate(instance); err != nil {
		return structuredResult{}, durable.NewError(durable.CodeStructuredOutputInvalid, "validate_structured_output", "final JSON does not satisfy json_schema", err)
	}
	digest := sha256.Sum256(raw)
	return structuredResult{Bytes: append([]byte(nil), raw...), Hash: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func structuredOutputFromManifest(manifest json.RawMessage) (*StructuredOutput, string) {
	var stored struct {
		Contract *StructuredOutput `json:"structured_output"`
		Hash     string            `json:"output_schema_hash"`
	}
	if err := json.Unmarshal(manifest, &stored); err != nil {
		return nil, ""
	}
	return stored.Contract, stored.Hash
}
