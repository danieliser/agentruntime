# Spec: Linux iptables Rules for Inter-Container Isolation

**Status:** Draft
**Task:** #7
**Effort:** Quick win (~60 LOC)
**Source:** persistence `executor/docker_network.py`

## Problem

Agentruntime's Docker runtime creates a bridge network (`agentruntime-agents`) and
runs a Squid proxy for egress filtering, but does nothing to prevent lateral movement
between agent containers on the same bridge. A compromised agent container could
reach other agent containers directly.

## Solution

Apply an iptables rule on the host (Linux only) that drops all TCP traffic from
the bridge network to non-bridge destinations, except traffic to the daemon API port.

## The Rule

```bash
iptables -I DOCKER-USER \
  -i br-agentruntime \        # Traffic FROM the bridge interface
  ! -o br-agentruntime \      # Going to non-bridge destinations
  -p tcp \
  ! --dport 8090 \            # Except daemon API
  -j DROP
```

## Implementation

### File: `pkg/runtime/network.go` (existing)

Add to `NetworkManager`:

```go
func (nm *NetworkManager) applyIPTablesRules() error {
    if runtime.GOOS != "linux" {
        return nil  // macOS uses Docker Desktop's built-in isolation
    }

    bridge := "br-agentruntime"  // or read from network config
    port := nm.daemonPort

    // Check if rule exists (idempotent)
    err := exec.Command("iptables", "-C", "DOCKER-USER",
        "-i", bridge, "!", "-o", bridge,
        "-p", "tcp", "!", "--dport", strconv.Itoa(port),
        "-j", "DROP").Run()
    if err == nil {
        return nil  // Already exists
    }

    // Insert rule
    return exec.Command("iptables", "-I", "DOCKER-USER",
        "-i", bridge, "!", "-o", bridge,
        "-p", "tcp", "!", "--dport", strconv.Itoa(port),
        "-j", "DROP").Run()
}
```

### Call site

Call `applyIPTablesRules()` after `ensureNetwork()` in the Docker runtime startup.

### Bridge name

The persistence code used `--opt com.docker.network.bridge.name=br-paop` when
creating the network. Agentruntime should do the same:
```bash
docker network create --driver bridge \
  --opt com.docker.network.bridge.name=br-agentruntime \
  agentruntime-agents
```

Check if agentruntime already sets a custom bridge name. If not, add it.

### Verify after insert

After inserting, re-check with `-C` to confirm. Log a warning with the manual
command if it fails (user may need to run with sudo):

```
WARNING: iptables rule insert failed. Run manually:
  sudo iptables -I DOCKER-USER -i br-agentruntime ! -o br-agentruntime -p tcp ! --dport 8090 -j DROP
```

## Testing

- Linux only — skip on Darwin
- Verify idempotence (run twice, rule exists once)
- Verify the bridge name matches the network config
