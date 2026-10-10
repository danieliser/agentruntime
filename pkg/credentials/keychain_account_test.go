package credentials

import (
	"errors"
	"testing"
)

func TestClaudeKeychainAccount(t *testing.T) {
	for _, tc := range []struct {
		name, envUser, systemUser, want string
		lookupErr                       error
	}{
		{name: "native environment account", envUser: "danieliser", systemUser: "different", want: "danieliser"},
		{name: "system account fallback", systemUser: "local-user", want: "local-user"},
		{name: "lookup failure uses native fallback", lookupErr: errors.New("unavailable"), want: "claude-code-user"},
		{name: "invalid environment uses native fallback", envUser: "bad/account", systemUser: "local-user", want: "claude-code-user"},
		{name: "invalid system account uses native fallback", systemUser: "bad account", want: "claude-code-user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeKeychainAccount(tc.envUser, func() (string, error) { return tc.systemUser, tc.lookupErr })
			if got != tc.want {
				t.Fatalf("account = %q, want %q", got, tc.want)
			}
		})
	}
}
