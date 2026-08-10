package schema

import (
	"strings"
	"testing"
)

func TestExplicitCodexAuthJSONRequiresRestrictedCodexAndValidObject(t *testing.T) {
	valid := &SessionRequest{
		Agent: "codex", ExecutionPolicy: &ExecutionPolicy{Version: "1.0"},
		Env:          map[string]string{CodexAuthJSONEnv: `{"tokens":{"access_token":"secret"}}`},
		SecretGrants: []string{CodexAuthJSONEnv},
	}
	raw, err := ExplicitCodexAuthJSON(valid)
	if err != nil || string(raw) != valid.Env[CodexAuthJSONEnv] {
		t.Fatalf("valid explicit auth = %q err=%v", raw, err)
	}

	tests := []struct {
		name   string
		mutate func(*SessionRequest)
	}{
		{name: "undeclared", mutate: func(request *SessionRequest) { request.SecretGrants = nil }},
		{name: "wrong provider", mutate: func(request *SessionRequest) { request.Agent = "claude" }},
		{name: "legacy", mutate: func(request *SessionRequest) { request.ExecutionPolicy = nil }},
		{name: "malformed", mutate: func(request *SessionRequest) { request.Env[CodexAuthJSONEnv] = `{not-json` }},
		{name: "non-object", mutate: func(request *SessionRequest) { request.Env[CodexAuthJSONEnv] = `[]` }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := &SessionRequest{
				Agent: "codex", ExecutionPolicy: &ExecutionPolicy{Version: "1.0"},
				Env:          map[string]string{CodexAuthJSONEnv: valid.Env[CodexAuthJSONEnv]},
				SecretGrants: []string{CodexAuthJSONEnv},
			}
			test.mutate(request)
			if _, err := ExplicitCodexAuthJSON(request); err == nil || strings.Contains(err.Error(), "access_token") {
				t.Fatalf("invalid grant error = %v", err)
			}
		})
	}
}
