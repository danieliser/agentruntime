package observer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestLoadConfigRequiresExplicitSafeAllowlist(t *testing.T) {
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "plugins.json")
	contents := `{
  "version": "1",
  "plugins": [{
    "name": "opentraces",
    "enabled": true,
    "command": ` + quoteJSON(executable) + `,
    "args": ["-test.run=TestObserverHelperProcess"],
    "environment": {"AGENTD_OBSERVER_HELPER": "healthy"},
    "policy": "best_effort",
    "timeout": "2s"
  }]
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Plugins) != 1 || config.Plugins[0].Name != "opentraces" || config.Plugins[0].Policy != PolicyBestEffort {
		t.Fatalf("unexpected config: %+v", config)
	}
	if config.Plugins[0].Timeout.String() != "2s" {
		t.Fatalf("timeout = %s", config.Plugins[0].Timeout)
	}
}

func TestLoadConfigRejectsUnsafeOrAmbiguousEntries(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"relative command": `{"version":"1","plugins":[{"name":"opentraces","enabled":true,"command":"adapter"}]}`,
		"duplicate name":   `{"version":"1","plugins":[{"name":"opentraces","enabled":true,"command":` + quoteJSON(executable) + `},{"name":"opentraces","enabled":true,"command":` + quoteJSON(executable) + `}]}`,
		"bad environment":  `{"version":"1","plugins":[{"name":"opentraces","enabled":true,"command":` + quoteJSON(executable) + `,"environment":{"BAD-KEY":"x"}}]}`,
		"unknown policy":   `{"version":"1","plugins":[{"name":"opentraces","enabled":true,"command":` + quoteJSON(executable) + `,"policy":"steer"}]}`,
		"unknown field":    `{"version":"1","plugins":[{"name":"opentraces","enabled":true,"command":` + quoteJSON(executable) + `,"dynamic_install":true}]}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugins.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("expected invalid config")
			}
		})
	}
}

func TestLoadOptionalConfigTreatsMissingFileAsNoPlugins(t *testing.T) {
	config, err := LoadOptionalConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Plugins) != 0 {
		t.Fatalf("plugins = %+v, want none", config.Plugins)
	}
}

func TestLoadConfigRejectsWorldReadableEnvironmentGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins.json")
	if err := os.WriteFile(path, []byte(`{"version":"1","plugins":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected non-private observer config rejection")
	}
}
