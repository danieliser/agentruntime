package nativeprotocol

import "fmt"

// NewAdapter returns the sole wire adapter for a supported provider.
// Optional runtimeName is host-supplied context used only for approval labels.
func NewAdapter(provider Provider, runtimeName ...string) (Adapter, error) {
	switch provider {
	case ProviderClaude:
		return claudeAdapter{}, nil
	case ProviderCodex:
		name := ""
		if len(runtimeName) > 0 {
			name = runtimeName[0]
		}
		return codexAdapter{runtimeName: name}, nil
	default:
		return nil, newError(CodeInvalidArgument, "new_adapter", fmt.Sprintf("unsupported provider %q", provider), nil)
	}
}
