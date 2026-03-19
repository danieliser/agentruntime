package session

// EstimateCost returns a ballpark cost in USD based on token counts and model name.
// Uses published per-token pricing. Returns 0 if model is unknown.
// This is a heuristic fallback — prefer agent-reported cost_usd when available.
func EstimateCost(model string, inputTokens, outputTokens int) float64 {
	p, ok := modelPricing[model]
	if !ok {
		// Try prefix match for versioned model IDs (e.g. "claude-sonnet-4-6-20260101").
		for prefix, pricing := range modelPricing {
			if len(model) > len(prefix) && model[:len(prefix)] == prefix {
				p = pricing
				ok = true
				break
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

// Pricing as of March 2026. Per-token = per-million-token price / 1_000_000.
var modelPricing = map[string]tokenPricing{
	// Claude 4.6
	"claude-opus-4-6":   {InputPerToken: 15.0 / 1e6, OutputPerToken: 75.0 / 1e6},
	"claude-sonnet-4-6": {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},
	// Claude 4.5
	"claude-haiku-4-5": {InputPerToken: 0.80 / 1e6, OutputPerToken: 4.0 / 1e6},
	// Claude 3.5
	"claude-sonnet-3-5": {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},
	// Codex / OpenAI (rough estimates)
	"o3":      {InputPerToken: 2.0 / 1e6, OutputPerToken: 8.0 / 1e6},
	"o4-mini": {InputPerToken: 1.10 / 1e6, OutputPerToken: 4.40 / 1e6},
	// Fallback aliases
	"claude":  {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},  // default to Sonnet pricing
	"sonnet":  {InputPerToken: 3.0 / 1e6, OutputPerToken: 15.0 / 1e6},
	"opus":    {InputPerToken: 15.0 / 1e6, OutputPerToken: 75.0 / 1e6},
	"haiku":   {InputPerToken: 0.80 / 1e6, OutputPerToken: 4.0 / 1e6},
}
