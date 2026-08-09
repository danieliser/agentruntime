package nativeprotocol

import "fmt"

// NewAdapter returns the sole wire adapter for a supported provider.
func NewAdapter(provider Provider) (Adapter, error) {
	switch provider {
	case ProviderClaude:
		return claudeAdapter{}, nil
	case ProviderCodex:
		return codexAdapter{}, nil
	default:
		return nil, newError(CodeInvalidArgument, "new_adapter", fmt.Sprintf("unsupported provider %q", provider), nil)
	}
}
