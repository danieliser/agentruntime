package session

// EstimateCost returns a ballpark cost in USD based on token counts and model name.
// Uses published per-token pricing. Returns 0 if model is unknown.
// This is a heuristic fallback — prefer agent-reported cost_usd when available.
func EstimateCost(model string, inputTokens, outputTokens int) float64 {
	p, ok := modelPricing[model]
	if !ok {
		// Longest prefix match for versioned model IDs (e.g. "claude-sonnet-4-6-20260101").
		// Longest wins so "claude-opus-5-..." matches "claude-opus-5", not the "claude" alias.
		best := -1
		for prefix, pricing := range modelPricing {
			if len(model) > len(prefix) && model[:len(prefix)] == prefix && len(prefix) > best {
				p = pricing
				best = len(prefix)
				ok = true
			}
		}
		if !ok {
			return 0
		}
	}
	return float64(inputTokens)*p.InputPerToken + float64(outputTokens)*p.OutputPerToken
}

type tokenPricing struct {
	InputPerToken  float64
	OutputPerToken float64
}

// Pricing as of August 2026. Per-token = per-million-token price / 1_000_000.
var modelPricing = map[string]tokenPricing{
	// Claude 5 family
	"claude-fable-5":  {InputPerToken: 10.0 / 1e6, OutputPerToken: 50.0 / 1e6},
	"claude-opus-5":   {InputPerToken: 5.0 / 1e6, OutputPerToken: 25.0 / 1e6},
	"claude-sonnet-5": {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},
	// Claude 4.x
	"claude-opus-4-8":   {InputPerToken: 5.0 / 1e6, OutputPerToken: 25.0 / 1e6},
	"claude-opus-4-7":   {InputPerToken: 5.0 / 1e6, OutputPerToken: 25.0 / 1e6},
	"claude-opus-4-6":   {InputPerToken: 5.0 / 1e6, OutputPerToken: 25.0 / 1e6},
	"claude-sonnet-4-6": {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},
	"claude-haiku-4-5":  {InputPerToken: 1.0 / 1e6, OutputPerToken: 5.0 / 1e6},
	// Claude 3.5 (retired; kept for historical sessions)
	"claude-sonnet-3-5": {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},
	// Codex / OpenAI (rough estimates; codex CLI OAuth usage is subscription-billed)
	"gpt-5.6-sol": {InputPerToken: 2.50 / 1e6, OutputPerToken: 20.0 / 1e6},
	"gpt-5.5":     {InputPerToken: 1.25 / 1e6, OutputPerToken: 10.0 / 1e6},
	"o3":          {InputPerToken: 2.0 / 1e6, OutputPerToken: 8.0 / 1e6},
	"o4-mini":     {InputPerToken: 1.10 / 1e6, OutputPerToken: 4.40 / 1e6},
	// Fallback aliases (current-generation rates)
	"claude": {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6}, // default to Sonnet pricing
	"fable":  {InputPerToken: 10.0 / 1e6, OutputPerToken: 50.0 / 1e6},
	"sonnet": {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},
	"opus":   {InputPerToken: 5.0 / 1e6, OutputPerToken: 25.0 / 1e6},
	"haiku":  {InputPerToken: 1.0 / 1e6, OutputPerToken: 5.0 / 1e6},
}
