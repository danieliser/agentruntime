package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestClaudeRestrictedLocalFileTools(t *testing.T) {
	for _, mode := range []string{"default", "dontAsk"} {
		cmd, err := (&ClaudeAgent{}).BuildCmd("", AgentConfig{NativeStream: true, IsolateConfig: true, PermissionMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"--restricted", "--print", "--strict-mcp-config", "--permission-prompts", "Read,Glob,Grep,Edit,Write"} {
			if !slices.Contains(cmd, flag) {
				t.Errorf("missing %s: %v", flag, cmd)
			}
		}
		for _, arg := range cmd {
			for _, bad := range []string{"dangerously", "bypassPermissions", "Bash", "PowerShell", "WebFetch", "--ide"} {
				if strings.Contains(arg, bad) {
					t.Errorf("unsafe argument %s", arg)
				}
			}
		}
	}
}
