package session

import (
	"math"
	"testing"
)

func TestEstimateCost_KnownModels(t *testing.T) {
	tests := []struct {
		model  string
		input  int
		output int
		want   float64 // approximate
	}{
		{"claude-sonnet-4-6", 1000, 500, 0.0105},          // 1000*3/1e6 + 500*15/1e6
		{"claude-opus-4-6", 1000, 500, 0.0175},            // 1000*5/1e6 + 500*25/1e6
		{"claude-haiku-4-5", 10000, 2000, 0.02},           // 10000*1/1e6 + 2000*5/1e6
		{"claude-fable-5", 1000, 500, 0.035},              // 1000*10/1e6 + 500*50/1e6
		{"claude-opus-5", 1000, 500, 0.0175},              // 1000*5/1e6 + 500*25/1e6
		{"claude-sonnet-5", 1000, 500, 0.0105},            // 1000*3/1e6 + 500*15/1e6
		{"gpt-5.5", 1000, 500, 0.00625},                   // 1000*1.25/1e6 + 500*10/1e6
		{"gpt-5.6-sol", 1000, 500, 0.0125},                // 1000*2.5/1e6 + 500*20/1e6
		{"claude", 1000, 500, 0.0105},                     // alias → sonnet pricing
		{"claude-sonnet-4-6-20260101", 1000, 500, 0.0105}, // versioned prefix match
		{"claude-opus-5-20260901", 1000, 500, 0.0175},     // longest prefix wins over "claude" alias
	}

	for _, tt := range tests {
		got := EstimateCost(tt.model, tt.input, tt.output)
		if math.Abs(got-tt.want) > 0.0001 {
			t.Errorf("EstimateCost(%q, %d, %d) = %f, want ~%f", tt.model, tt.input, tt.output, got, tt.want)
		}
	}
}

func TestEstimateCost_UnknownModel(t *testing.T) {
	got := EstimateCost("unknown-model-v9", 1000, 500)
	if got != 0 {
		t.Errorf("EstimateCost for unknown model = %f, want 0", got)
	}
}

func TestEstimateCost_ZeroTokens(t *testing.T) {
	got := EstimateCost("claude-sonnet-4-6", 0, 0)
	if got != 0 {
		t.Errorf("EstimateCost with zero tokens = %f, want 0", got)
	}
}
