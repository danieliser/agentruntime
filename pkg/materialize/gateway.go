package materialize

import (
	"encoding/hex"
	"net"
	"os"
	"runtime"
	"strings"
)

const hostGatewayVar = "${HOST_GATEWAY}"

// ResolveHostGateway returns the host-reachable gateway address for containers.
func ResolveHostGateway() string {
	switch runtime.GOOS {
	case "darwin":
		return "host.docker.internal"
	case "linux":
		if gateway := resolveLinuxGateway(); gateway != "" {
			return gateway
		}
		return "172.17.0.1"
	default:
		return "host.docker.internal"
	}
}

// ResolveVars substitutes supported runtime variables in a string.
func ResolveVars(s string) string {
	if !strings.Contains(s, hostGatewayVar) {
		return s
	}
	return strings.ReplaceAll(s, hostGatewayVar, ResolveHostGateway())
}

func resolveLinuxGateway() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}

		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}

		return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
	}

	return ""
}
