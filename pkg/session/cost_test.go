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
		{"claude-sonnet-4-6", 1000, 500, 0.0105},   // 1000*3/1e6 + 500*15/1e6
		{"claude-opus-4-6", 1000, 500, 0.0525},      // 1000*15/1e6 + 500*75/1e6
		{"claude-haiku-4-5", 10000, 2000, 0.016},     // 10000*0.8/1e6 + 2000*4/1e6
		{"claude", 1000, 500, 0.0105},                // alias → sonnet pricing
		{"claude-sonnet-4-6-20260101", 1000, 500, 0.0105}, // versioned prefix match
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
