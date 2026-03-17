package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkManager_EnsureNetwork(t *testing.T) {
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
	if !strings.Contains(string(data), "network create "+defaultDockerNetworkName+"\n") {
		t.Fatalf("expected docker network create call, got %q", string(data))
	}
}

func TestNetworkManager_ProxyEnv(t *testing.T) {
	manager := &NetworkManager{}
	env := manager.ProxyEnv()

	if env["HTTP_PROXY"] != "http://agentruntime-proxy:3128" {
		t.Fatalf("unexpected HTTP_PROXY: %q", env["HTTP_PROXY"])
	}
	if env["HTTPS_PROXY"] != "http://agentruntime-proxy:3128" {
		t.Fatalf("unexpected HTTPS_PROXY: %q", env["HTTPS_PROXY"])
	}
	if env["NO_PROXY"] != "localhost,127.0.0.1,host.docker.internal" {
		t.Fatalf("unexpected NO_PROXY: %q", env["NO_PROXY"])
	}
}

func TestNetworkManager_IdempotentProxy(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "docker.log")
	networkState := filepath.Join(tempDir, "network.created")
	proxyState := filepath.Join(tempDir, "proxy.running")
	installFakeDocker(t, `#!/bin/sh
set -eu
LOG_FILE="`+logFile+`"
NETWORK_STATE="`+networkState+`"
PROXY_STATE="`+proxyState+`"
printf '%s\n' "$*" >> "$LOG_FILE"
case "$1 $2" in
  "network inspect")
    if [ -f "$NETWORK_STATE" ]; then
      exit 0
    fi
    echo "Error: No such network: agentruntime-agents" >&2
    exit 1
    ;;
  "network create")
    : > "$NETWORK_STATE"
    printf 'network-created\n'
    exit 0
    ;;
  "inspect --type")
    if [ -f "$PROXY_STATE" ]; then
      printf 'true\n'
      exit 0
    fi
    echo "Error: No such object: agentruntime-proxy" >&2
    exit 1
    ;;
  "run -d")
    : > "$PROXY_STATE"
    printf 'proxy-container\n'
    exit 0
    ;;
esac
echo "unexpected docker command: $*" >&2
exit 2
`)

	manager := &NetworkManager{}
	if err := manager.EnsureProxy(context.Background()); err != nil {
		t.Fatalf("first EnsureProxy failed: %v", err)
	}
	if err := manager.EnsureProxy(context.Background()); err != nil {
		t.Fatalf("second EnsureProxy failed: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if count := strings.Count(string(data), "run -d --name agentruntime-proxy --network "+defaultDockerNetworkName+" "+defaultDockerProxyImage+"\n"); count != 1 {
		t.Fatalf("expected proxy container to start once, got log %q", string(data))
	}
}
