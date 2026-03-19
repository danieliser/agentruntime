package chat

import (
	"testing"
	"time"
)

func TestChatState_String(t *testing.T) {
	tests := []struct {
		state ChatState
		want  string
	}{
		{ChatStateCreated, "created"},
		{ChatStateRunning, "running"},
		{ChatStateIdle, "idle"},
		{ChatStateDeleted, "deleted"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("ChatState(%q).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestChatState_IsTerminal(t *testing.T) {
	terminal := map[ChatState]bool{
		ChatStateCreated: false,
		ChatStateRunning: false,
		ChatStateIdle:    false,
		ChatStateDeleted: true,
	}
	for state, want := range terminal {
		if got := state.IsTerminal(); got != want {
			t.Errorf("%q.IsTerminal() = %v, want %v", state, got, want)
		}
	}
}

func TestChatConfig_EffectiveIdleTimeout_Default(t *testing.T) {
	cfg := ChatConfig{}
	if got := cfg.EffectiveIdleTimeout(); got != 30*time.Minute {
		t.Errorf("empty IdleTimeout → %v, want 30m", got)
	}
}

func TestChatConfig_EffectiveIdleTimeout_Custom(t *testing.T) {
	cfg := ChatConfig{IdleTimeout: "1h"}
	if got := cfg.EffectiveIdleTimeout(); got != time.Hour {
		t.Errorf("IdleTimeout=1h → %v, want 1h", got)
	}
}

func TestChatConfig_EffectiveIdleTimeout_Invalid(t *testing.T) {
	cfg := ChatConfig{IdleTimeout: "not-a-duration"}
	if got := cfg.EffectiveIdleTimeout(); got != 30*time.Minute {
		t.Errorf("invalid IdleTimeout → %v, want 30m default", got)
	}
}

func TestChatRecord_LastSessionID_Empty(t *testing.T) {
	rec := ChatRecord{}
	if got := rec.LastSessionID(); got != "" {
		t.Errorf("empty chain → %q, want empty", got)
	}
}

func TestChatRecord_LastSessionID_NonEmpty(t *testing.T) {
	rec := ChatRecord{SessionChain: []string{"s1", "s2", "s3"}}
	if got := rec.LastSessionID(); got != "s3" {
		t.Errorf("chain [s1,s2,s3] → %q, want s3", got)
	}
}
