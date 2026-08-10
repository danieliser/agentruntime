package schema

import (
	"encoding/json"
	"fmt"
	"slices"
)

const (
	// CodexAuthJSONEnv carries one explicitly granted Codex auth.json payload.
	// AgentD consumes it into the private session home before Docker starts; it
	// is never exposed in the provider environment or durable manifest.
	CodexAuthJSONEnv = "AGENTD_CODEX_AUTH_JSON"
	maxAuthJSONBytes = 256 << 10
)

// ExplicitCodexAuthJSON validates and returns a caller-granted Codex auth.json.
// A non-nil result is available only to restricted Codex sessions and requires
// the exact environment name to appear in secret_grants.
func ExplicitCodexAuthJSON(request *SessionRequest) ([]byte, error) {
	if request == nil {
		return nil, nil
	}
	value, present := request.Env[CodexAuthJSONEnv]
	granted := slices.Contains(request.SecretGrants, CodexAuthJSONEnv)
	if !present && !granted {
		return nil, nil
	}
	if request.Agent != "codex" || request.ExecutionPolicy == nil {
		return nil, fmt.Errorf("%s is supported only for an explicit-policy Codex session", CodexAuthJSONEnv)
	}
	if !present || !granted {
		return nil, fmt.Errorf("%s must be present in env and secret_grants", CodexAuthJSONEnv)
	}
	raw := []byte(value)
	if len(raw) == 0 || len(raw) > maxAuthJSONBytes {
		return nil, fmt.Errorf("%s must contain at most 262144 bytes", CodexAuthJSONEnv)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must contain one JSON object", CodexAuthJSONEnv)
	}
	return raw, nil
}
