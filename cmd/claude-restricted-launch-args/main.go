// Command claude-restricted-launch-args prints a launch recipe without running Claude.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	apischema "github.com/danieliser/agentruntime/pkg/api/schema"
	"github.com/danieliser/agentruntime/pkg/materialize"
)

func main() {
	if err := printLaunch(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printLaunch(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("claude-restricted-launch-args", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	work := flags.String("work-dir", "", "absolute project working directory")
	home := flags.String("home", "", "absolute private launch home")
	approval := flags.String("approval-policy", "on-request", "on-request or never")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*work) || !filepath.IsAbs(*home) {
		return fmt.Errorf("usage: claude-restricted-launch-args --work-dir /project --home /private-launch-home [--approval-policy on-request|never]")
	}
	mode := "default"
	if *approval == "never" {
		mode = "dontAsk"
	} else if *approval != "on-request" {
		return fmt.Errorf("approval-policy must be on-request or never")
	}
	*work, *home = filepath.Clean(*work), filepath.Clean(*home)
	providerHome := filepath.Join(*home, ".claude")
	settings, err := materialize.RestrictedLocalClaudeSettings(&apischema.SessionRequest{Agent: "claude", WorkDir: *work}, *home, providerHome)
	if err != nil {
		return err
	}
	path := filepath.Join(providerHome, "restricted-settings.json")
	argv := append(append([]string{"claude"}, materialize.RestrictedLocalClaudeArgs(mode)...), "--settings", path)
	// This is inspection only: it neither creates homes/settings nor authenticates.
	result := struct {
		Argv         []string        `json:"argv"`
		WorkDir      string          `json:"work_dir"`
		SettingsPath string          `json:"settings_path"`
		Settings     json.RawMessage `json:"settings"`
	}{argv, *work, path, settings}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
