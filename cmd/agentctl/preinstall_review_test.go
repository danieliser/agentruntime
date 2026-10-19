package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestForceFlagPreservesPromptText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	original := commandRegistry["prompt"]
	defer func() { commandRegistry["prompt"] = original }()
	for _, tc := range []struct {
		args  []string
		force bool
		last  string
	}{
		{[]string{"prompt", "s", "--force"}, false, "--force"},
		{[]string{"prompt", "s", "--", "--force"}, false, "--force"},
		{[]string{"--force", "prompt", "s", "text"}, true, "text"},
		{[]string{"prompt", "--force", "s", "text"}, true, "text"},
	} {
		called := false
		commandRegistry["prompt"] = func(ctx *Context, args []string) error {
			called = true
			if ctx.Force != tc.force || args[len(args)-1] != tc.last {
				t.Errorf("%v force=%v args=%v", tc.args, ctx.Force, args)
			}
			return nil
		}
		var out bytes.Buffer
		if code := run(tc.args, &out, &out, map[string]string{"AGENTD_TOKEN_FILE": path}); code != 0 || !called {
			t.Fatalf("%v code=%d output=%s", tc.args, code, out.String())
		}
	}
}
