#!/usr/bin/env python3
"""Test-only Docker proxy: namespace discovery and pin existing base images."""
import os
import json
from pathlib import Path
import sys
import subprocess


def scoped_args(args: list[str], scope: str) -> list[str]:
    tag = os.environ.get("AGENTCTL_LIVE_IMAGE_TAG", "2.3.3")
    pinned = {name + ":latest": name + ":" + tag for name in (
        "agentruntime-agent", "agentruntime-agent-claude", "agentruntime-agent-codex", "agentruntime-proxy")}
    args = [pinned.get(arg, arg) for arg in args]
    names = {name: name + '-' + scope for name in ('agentruntime-proxy', 'agentruntime-agents', 'agentruntime-policy-v1')}
    proxy_run = args[:1] == ['run'] and '--name' in args and args[args.index('--name') + 1] == 'agentruntime-proxy'
    args = [arg if i > 0 and args[i-1] == '--alias' else names.get(arg, arg) for i, arg in enumerate(args)]
    args = [arg.replace('com.docker.network.bridge.name=br-agentruntime', 'com.docker.network.bridge.name=br-' + scope[-10:]) for arg in args]
    if proxy_run:
        args[1:1] = ['--network-alias', 'agentruntime-proxy']
    if args[:2] == ['volume', 'create']:
        return ['volume', 'create', '--label', 'agentctl.live_test=' + scope, *args[2:]]
    if args[:2] == ["network", "create"]:
        subnets = json.loads(os.environ.get('AGENTCTL_DOCKER_SUBNETS', '{}'))
        for name, subnet in subnets.items():
            if names.get(name) in args:
                args[2:2] = ['--subnet', subnet]
                break
        return ["network", "create", "--label", "agentctl.live_test=" + scope, *args[2:]]
    if args and args[0] == "ps":
        return ["ps", "--filter", "label=agentctl.live_test=" + scope, *args[1:]]
    if args and args[0] in ("run", "create"):
        return [args[0], "--label", "agentctl.live_test=" + scope, *args[1:]]
    return args


if __name__ == "__main__":
    if (Path(__file__).parent.parent / 'docker-unavailable').exists():
        os.write(2, b'isolated fault injection: docker executable unavailable\n')
        sys.exit(127)
    diagnostic = os.environ.get("AGENTCTL_DOCKER_DIAGNOSTICS")
    if diagnostic and sys.argv[1:2] == ["run"]:
        mounts = []
        for i, arg in enumerate(sys.argv):
            if arg != "-v" or i + 1 >= len(sys.argv):
                continue
            source = sys.argv[i+1].split(":", 1)[0]
            if not source.startswith("/"):
                continue
            path = Path(source)
            if not any(part in source for part in ("claude-sessions", "codex-sessions")):
                continue
            entries = []
            if path.is_dir():
                for entry in path.iterdir():
                    entries.append({"name": entry.name, "mode": oct(entry.stat().st_mode & 0o777), "uid": entry.stat().st_uid})
            mounts.append({"source": source, "mount": sys.argv[i+1], "entries": entries})
        with open(diagnostic, "a") as stream:
            stream.write(json.dumps(mounts) + "\n")
    if diagnostic and sys.argv[1:2] == ['create']:
        command = [os.environ['AGENTCTL_LIVE_REAL_DOCKER'], *scoped_args(sys.argv[1:], os.environ['AGENTCTL_LIVE_SCOPE'])]
        result = subprocess.run(command, capture_output=True)
        with open(diagnostic, 'a') as stream:
            stream.write(json.dumps({'create_exit': result.returncode, 'stderr': result.stderr.decode(errors='replace')}) + '\n')
        os.write(1, result.stdout)
        os.write(2, result.stderr)
        sys.exit(result.returncode)
    os.execv(os.environ["AGENTCTL_LIVE_REAL_DOCKER"],
             ["docker", *scoped_args(sys.argv[1:], os.environ["AGENTCTL_LIVE_SCOPE"])])
