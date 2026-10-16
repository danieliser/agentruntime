#!/usr/bin/env python3
"""Test-only Docker proxy: namespace discovery and pin existing base images."""
import os
import sys


def scoped_args(args: list[str], scope: str) -> list[str]:
    pinned = {name + ":latest": name + ":2.3.3-persist.10" for name in (
        "agentruntime-agent", "agentruntime-agent-claude", "agentruntime-agent-codex", "agentruntime-proxy")}
    args = [pinned.get(arg, arg) for arg in args]
    if args and args[0] == "ps":
        return ["ps", "--filter", "label=agentctl.live_test=" + scope, *args[1:]]
    if args and args[0] == "run":
        return ["run", "--label", "agentctl.live_test=" + scope, *args[1:]]
    return args


if __name__ == "__main__":
    os.execv(os.environ["AGENTCTL_LIVE_REAL_DOCKER"],
             ["docker", *scoped_args(sys.argv[1:], os.environ["AGENTCTL_LIVE_SCOPE"])])
