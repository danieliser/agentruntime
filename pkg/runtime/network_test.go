package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	logContent := string(data)
	if !strings.Contains(logContent, "network create") || !strings.Contains(logContent, defaultDockerNetworkName) {
		t.Fatalf("expected docker network create call with network name, got %q", logContent)
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
	if env["NO_PROXY"] != "localhost,127.0.0.1,host.docker.internal,host-gateway" {
		t.Fatalf("unexpected NO_PROXY: %q", env["NO_PROXY"])
	}
}

func TestNetworkManager_RestrictedProxyEnvHasNoHostBypass(t *testing.T) {
	manager := &NetworkManager{}
	env := manager.RestrictedProxyEnv()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if env[key] != "http://agentruntime-proxy:3128" {
			t.Fatalf("%s = %q", key, env[key])
		}
	}
	if strings.Contains(env["NO_PROXY"], "host.docker.internal") || strings.Contains(env["NO_PROXY"], "host-gateway") {
		t.Fatalf("restricted NO_PROXY = %q", env["NO_PROXY"])
	}
}

func TestNetworkManager_IdempotentProxy(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "docker.log")
	networkState := filepath.Join(tempDir, "network")
	proxyState := filepath.Join(tempDir, "proxy.running")
	proxyReadyAttempt := filepath.Join(tempDir, "proxy.ready-attempt")
	installFakeDockerWithReadinessControl(t, `#!/bin/sh
set -eu
LOG_FILE="`+logFile+`"
NETWORK_STATE="`+networkState+`"
PROXY_STATE="`+proxyState+`"
PROXY_READY_ATTEMPT="`+proxyReadyAttempt+`"
printf '%s\n' "$*" >> "$LOG_FILE"
case "$1 $2" in
  "network inspect")
	if [ -f "$NETWORK_STATE.$3" ]; then
      exit 0
    fi
    echo "Error: No such network: agentruntime-agents" >&2
    exit 1
    ;;
  "network create")
	last=""
	for arg in "$@"; do last="$arg"; done
	: > "$NETWORK_STATE.$last"
    printf 'network-created\n'
    exit 0
    ;;
  "network connect")
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
  "exec agentruntime-proxy")
    if [ -f "$PROXY_READY_ATTEMPT" ]; then
      exit 0
    fi
    : > "$PROXY_READY_ATTEMPT"
    exit 1
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
	if !strings.Contains(string(data), "network create --driver bridge --internal "+defaultDockerPolicyNetworkName) ||
		!strings.Contains(string(data), "network connect --alias agentruntime-proxy "+defaultDockerPolicyNetworkName+" agentruntime-proxy") {
		t.Fatalf("expected internal policy network and dual-homed proxy, got log %q", string(data))
	}
	if count := strings.Count(string(data), "exec agentruntime-proxy sh -c "+dockerProxyReadinessProbe+"\n"); count != 3 {
		t.Fatalf("expected initial readiness retry plus cached-state revalidation, got %d attempts in log %q", count, string(data))
	}
}

func TestNetworkManager_EnsureProxyRetriesAfterReadinessTimeout(t *testing.T) {
	tempDir := t.TempDir()
	readyFile := filepath.Join(tempDir, "proxy.ready")
	installFakeDockerWithReadinessControl(t, `#!/bin/sh
set -eu
READY_FILE="`+readyFile+`"
case "$1 $2" in
  "network inspect") exit 0 ;;
  "network connect") exit 0 ;;
  "inspect --type") printf 'true\n'; exit 0 ;;
  "exec agentruntime-proxy")
    if [ -f "$READY_FILE" ]; then exit 0; fi
    exit 1
    ;;
esac
echo "unexpected docker command: $*" >&2
exit 2
`)

	manager := &NetworkManager{}
	firstCtx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := manager.EnsureProxy(firstCtx); err == nil {
		t.Fatal("first EnsureProxy unexpectedly succeeded")
	}
	if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
		t.Fatalf("mark proxy ready: %v", err)
	}
	if err := manager.EnsureProxy(context.Background()); err != nil {
		t.Fatalf("EnsureProxy did not retry after transient readiness failure: %v", err)
	}
}
