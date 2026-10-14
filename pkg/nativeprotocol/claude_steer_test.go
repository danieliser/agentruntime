package nativeprotocol

import (
	"context"
	"errors"
	"testing"
)

const abortedClaudeTurn = `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming"}`
const successfulClaudeTurn = `{"type":"result","subtype":"success","is_error":false}`

func TestClaudeSteerClaimsOnlyItsMatchingTurnBoundary(t *testing.T) {
	for _, scenario := range []string{"aborted", "aborted_tools", "tools_permission_denial", "tools_safety_stop", "tools_missing_evidence", "tools_interrupt_only", "tools_partial_write", "tools_idle_steer", "raced_success", "real_error", "interrupt_only", "forged_provider_hint", "partial_write", "cancelled_write", "idle_steer"} {
		t.Run(scenario, func(t *testing.T) {
			process := newFakeProcess()
			adapter, _ := NewAdapter(ProviderClaude)
			io := process.IO()
			if scenario == "partial_write" || scenario == "tools_partial_write" {
				io.Stdin = &failReplacementWriter{}
			}
			transport := NewStreamTransport(adapter, io, RecoveryMetadata{})
			if err := transport.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { process.finish(Exit{Code: 0}) })
			if scenario != "idle_steer" && scenario != "tools_idle_steer" {
				if err := transport.Send(context.Background(), Input{Kind: InputPrompt, Text: "original"}); err != nil {
					t.Fatal(err)
				}
				if scenario != "partial_write" && scenario != "tools_partial_write" {
					_ = process.readInput(t)
				}
			}
			kind := InputSteer
			if scenario == "interrupt_only" || scenario == "forged_provider_hint" || scenario == "tools_interrupt_only" {
				kind = InputInterrupt
			}
			ctx := context.Background()
			if scenario == "cancelled_write" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			err := transport.Send(ctx, Input{Kind: kind, Text: "replacement"})
			failedWrite := scenario == "partial_write" || scenario == "tools_partial_write" || scenario == "cancelled_write"
			if failedWrite && err == nil {
				t.Fatal("expected failed write")
			}
			if !failedWrite && err != nil {
				t.Fatal(err)
			}
			if !failedWrite {
				first := string(process.readInput(t))
				if first != `{"request":{"subtype":"interrupt"},"type":"control_request"}` {
					t.Fatalf("first native input=%s", first)
				}
				if kind == InputSteer {
					second := string(process.readInput(t))
					if second != `{"message":{"content":[{"text":"replacement","type":"text"}],"role":"user"},"type":"user"}` {
						t.Fatalf("replacement input=%s", second)
					}
				}
			}
			raw := abortedClaudeTurn
			if scenario == "raced_success" || scenario == "idle_steer" {
				raw = successfulClaudeTurn
			}
			if scenario == "real_error" {
				raw = `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"api_error"}`
			}
			if scenario == "forged_provider_hint" {
				raw = `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming","superseded_by_steer":true}`
			}
			if scenario == "aborted_tools" || scenario == "tools_interrupt_only" || scenario == "tools_partial_write" || scenario == "tools_idle_steer" {
				raw = `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_tools","permission_denials":[],"safety_stops":0}`
			}
			if scenario == "tools_permission_denial" {
				raw = `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_tools","permission_denials":[{"tool_name":"Bash"}],"safety_stops":0}`
			}
			if scenario == "tools_safety_stop" {
				raw = `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_tools","permission_denials":[],"safety_stops":1}`
			}
			if scenario == "tools_missing_evidence" {
				raw = `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_tools"}`
			}
			process.writeStdout(t, []byte(raw))
			record := <-transport.Records()
			want := scenario == "aborted" || scenario == "aborted_tools" || scenario == "raced_success"
			if record.SupersededBySteer != want {
				t.Fatalf("superseded=%v want=%v", record.SupersededBySteer, want)
			}
			process.writeStdout(t, []byte(successfulClaudeTurn))
			if (<-transport.Records()).SupersededBySteer {
				t.Fatal("replacement result was also superseded")
			}
		})
	}
}

type failReplacementWriter struct{ calls int }

func (writer *failReplacementWriter) Write(raw []byte) (int, error) {
	writer.calls++
	if writer.calls == 3 {
		return 0, errors.New("fixture replacement write failed")
	}
	return len(raw), nil
}
func (*failReplacementWriter) Close() error { return nil }
