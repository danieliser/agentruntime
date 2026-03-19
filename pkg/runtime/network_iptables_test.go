package runtime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestApplyIPTablesRules_NonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("test only runs on non-linux")
	}

	manager := &NetworkManager{}
	err := manager.ApplyIPTablesRules(context.Background())
	if err != nil {
		t.Fatalf("ApplyIPTablesRules should be no-op on non-linux, got error: %v", err)
	}
}

func TestEnsureNetwork_BridgeNameOption(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "docker.log")
	installFakeDocker(t, `#!/bin/sh
set -eu
LOG_FILE="`+logFile+`"
printf '%s\n' "$*" >> "$LOG_FILE"
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then
  exit 1
fi
if [ "$1" = "network" ] && [ "$2" = "create" ]; then
  printf 'network-created\n'
  exit 0
fi
echo "unexpected docker command: $*" >&2
exit 2
`)

	manager := &NetworkManager{}
	if err := manager.EnsureNetwork(context.Background()); err != nil {
		t.Fatalf("EnsureNetwork failed: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	logContent := string(data)

	// Check that the bridge driver option is included
	if !strings.Contains(logContent, "--driver") || !strings.Contains(logContent, "bridge") {
		t.Fatalf("expected --driver bridge options in docker command, got %q", logContent)
	}

	// Check that the bridge name option is included
	if !strings.Contains(logContent, "--opt") || !strings.Contains(logContent, "com.docker.network.bridge.name=br-agentruntime") {
		t.Fatalf("expected --opt com.docker.network.bridge.name=br-agentruntime in docker command, got %q", logContent)
	}
}
