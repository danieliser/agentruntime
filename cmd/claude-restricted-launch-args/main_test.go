package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
)

func TestLaunchRecipe(t *testing.T) {
	root := t.TempDir()
	for _, policy := range []string{"on-request", "never"} {
		var output bytes.Buffer
		if err := printLaunch([]string{"--work-dir", root, "--home", filepath.Join(root, "private"), "--approval-policy", policy}, &output); err != nil {
			t.Fatal(err)
		}
		var recipe struct {
			Argv         []string `json:"argv"`
			SettingsPath string   `json:"settings_path"`
			Settings     struct {
				Sandbox struct {
					Enabled          bool `json:"enabled"`
					AllowUnsandboxed bool `json:"allowUnsandboxedCommands"`
				} `json:"sandbox"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(output.Bytes(), &recipe); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(recipe.Argv, "--restricted") || !slices.Contains(recipe.Argv, "Read,Glob,Grep,Edit,Write") || !recipe.Settings.Sandbox.Enabled || recipe.Settings.Sandbox.AllowUnsandboxed || recipe.Argv[len(recipe.Argv)-1] != recipe.SettingsPath {
			t.Fatal("wrong launch recipe", output.String())
		}
	}
	for _, args := range [][]string{nil, {"--work-dir", root, "--home", root, "--approval-policy", "bypass"}} {
		if err := printLaunch(args, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid recipe arguments accepted")
		}
	}
}
