#!/usr/bin/env python3
"""Own isolated processes, credentials, ports and teardown for live acceptance."""
import argparse
import getpass
import json
import ipaddress
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import threading
import urllib.request

FORBIDDEN_PORTS = {38093}


def isolated_env(root: Path, agentd_port: int, parent: dict[str, str]) -> dict[str, str]:
    if agentd_port in FORBIDDEN_PORTS or not 1024 <= agentd_port <= 65535:
        raise RuntimeError('Live or invalid ports are forbidden')
    # Inherit only the host settings needed by local provider CLIs and Docker.
    allowed = {'PATH', 'HOME', 'USER', 'LOGNAME', 'SHELL', 'LANG', 'LC_ALL',
               'TMPDIR', 'DOCKER_HOST', 'DOCKER_CONTEXT', 'DOCKER_CONFIG',
               'SSL_CERT_FILE', 'SSL_CERT_DIR', 'HTTP_PROXY', 'HTTPS_PROXY', 'NO_PROXY'}
    env = {k: v for k, v in parent.items() if k in allowed}
    env.update(AGENTD_URL=f'http://127.0.0.1:{agentd_port}',
               AGENTD_TOKEN_FILE=str(root / 'agentd-data/auth.token'),
               AGENTCTL_CURSOR_DIR=str(root / 'cursors'))
    return env


def forbidden_connections(output: str) -> list[str]:
    return [line for line in output.splitlines() if re.search(r'->[^ ]+:38093(?:$|\s)', line)]


def private_claude_credentials() -> str:
    # Match AgentD's native-account selection; the service can contain stale
    # credentials for other accounts on the same Mac.
    result = subprocess.run(['security', 'find-generic-password', '-a', getpass.getuser(),
                             '-s', 'Claude Code-credentials', '-w'],
                            capture_output=True, text=True, check=True, timeout=10)
    return result.stdout.strip()


def restart_owned_agentd(proc, command, cwd, env, log):
    """Restart only the process handle created by this bootstrap."""
    os.killpg(proc.pid, signal.SIGTERM)
    proc.wait(timeout=30)
    return subprocess.Popen(command, cwd=cwd, env=env, stdout=log,
                            stderr=subprocess.STDOUT, start_new_session=True)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('provider', choices=('claude', 'codex'))
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    env = dict(os.environ)
    for command in ('go', 'docker', args.provider):
        if not shutil.which(command, path=env['PATH']):
            raise RuntimeError(f'Missing required command: {command}')
    agentport = int(os.environ.get('AGENTCTL_LIVE_TEST_PORT', '18091' if args.provider == 'claude' else '18092'))
    base = repo / '.agentctl-live-test' / ('observability-' + args.provider)
    base.mkdir(parents=True, exist_ok=True)
    subprocess.run(['git', 'check-ignore', '-q', str(base)], cwd=repo, check=True)
    root = Path(tempfile.mkdtemp(prefix='run.', dir=base))
    root.chmod(0o700)
    env = isolated_env(root, agentport, env)
    print(f'Private evidence: {root}', flush=True)
    for directory in ('bin', 'work', 'agentd-data', 'cursors'):
        (root / directory).mkdir(mode=0o700)
    for number in (agentport,):
        with socket.socket() as sock:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(('127.0.0.1', number))
    guard = repo / 'scripts/agentctl_live_guard.cjs'
    forbidden = [str(Path.home() / '.agentd')]
    env.update(NODE_OPTIONS=f'--require={guard}', AGENTCTL_NETWORK_VIOLATIONS=str(root / 'network-violations.log'), AGENTCTL_FORBIDDEN_ROOTS=json.dumps(forbidden))
    # Local providers use only private configuration. In particular, do
    # not load the host MCP catalog.
    codex_home = root / 'provider-home/codex'
    claude_home = root / 'provider-home/claude'
    codex_home.mkdir(parents=True, mode=0o700)
    claude_home.mkdir(parents=True, mode=0o700)
    if args.provider == 'claude':
        (claude_home / '.credentials.json').write_text(private_claude_credentials())
        (claude_home / '.credentials.json').chmod(0o600)
    private_tmp = root / 'tmp'
    private_tmp.mkdir(mode=0o700)
    if args.provider == 'codex':
        shutil.copyfile(Path.home() / '.codex/auth.json', codex_home / 'auth.json')
        (codex_home / 'auth.json').chmod(0o600)
    (codex_home / 'config.toml').write_text('project_doc_max_bytes = 0\n[features]\nmulti_agent = true\n')
    claude_binary = shutil.which('claude', path=env['PATH'])
    if not claude_binary:
        raise RuntimeError('Claude CLI is required for provider acceptance')
    wrapper = root / 'bin/claude'
    wrapper.write_text('#!/usr/bin/env python3\nimport os, sys\nos.execv(' + repr(claude_binary) + ', [' + repr(claude_binary) + ', "--safe-mode", *sys.argv[1:]])\n')
    wrapper.chmod(0o700)
    env.update(CODEX_HOME=str(codex_home), CLAUDE_CONFIG_DIR=str(claude_home), TMPDIR=str(private_tmp))
    for name in ('agentd', 'agentctl'):
        subprocess.run(['go', 'build', '-buildvcs=false', '-o', str(root / 'bin' / name), './cmd/' + name], cwd=repo, check=True)
    image_tag = os.environ.get('AGENTCTL_LIVE_IMAGE_TAG')
    if image_tag:
        env['AGENTCTL_LIVE_IMAGE_TAG'] = image_tag
    realdocker = shutil.which('docker', path=env['PATH'])
    shutil.copyfile(repo / 'scripts/agentctl_docker_scope.py', root / 'bin/docker')
    (root / 'bin/docker').chmod(0o700)
    ids = subprocess.check_output([realdocker, 'network', 'ls', '-q'], text=True).split()
    used = []
    if ids:
        for network in json.loads(subprocess.check_output([realdocker, 'network', 'inspect', *ids], text=True)):
            used.extend(ipaddress.ip_network(c['Subnet']) for c in network.get('IPAM', {}).get('Config', []) or [] if c.get('Subnet'))
    free = []
    for subnet in ipaddress.ip_network('10.254.0.0/16').subnets(new_prefix=28):
        if not any(subnet.version == existing.version and subnet.overlaps(existing) for existing in used):
            free.append(str(subnet))
            if len(free) == 2: break
    if len(free) != 2: raise RuntimeError('No unused private test subnets available')
    env['AGENTCTL_DOCKER_SUBNETS'] = json.dumps(dict(zip(('agentruntime-agents', 'agentruntime-policy-v1'), free)))
    env.update(AGENTCTL_LIVE_REAL_DOCKER=realdocker, AGENTCTL_LIVE_SCOPE=root.name, AGENTCTL_DOCKER_DIAGNOSTICS=str(root / 'docker-mounts.jsonl'),
               PATH=str(root / 'bin') + os.pathsep + env['PATH'])
    processes = []
    logs = []
    stop_monitor = threading.Event()
    monitor_errors = []
    def descendants() -> list[tuple[int, int, str]]:
        rows = [line.split(None, 3) for line in subprocess.check_output(['ps', '-axo', 'pid=,ppid=,pgid=,lstart='], text=True, timeout=3).splitlines()]
        owned = {p.pid for p in processes if p.poll() is None}
        for _ in range(20):
            previous = len(owned)
            owned.update(int(r[0]) for r in rows if int(r[1]) in owned)
            if len(owned) == previous: break
        return [(int(r[0]), int(r[2]), r[3]) for r in rows if int(r[0]) in owned]
    def monitor_connections() -> None:
        while not stop_monitor.wait(0.5):
            try:
                pids = [str(r[0]) for r in descendants()]
                if not pids: continue
                evidence = subprocess.run(['lsof', '-nP', '-a', '-p', ','.join(pids), '-iTCP', '-Fpn'], capture_output=True, text=True, timeout=3)
                found = forbidden_connections(evidence.stdout)
                if found:
                    with (root / 'network-violations.log').open('a') as stream: stream.write('\n'.join(found) + '\n')
            except Exception as error:
                monitor_errors.append(str(error))
                return
    monitor = threading.Thread(target=monitor_connections, daemon=True)
    monitor.start()
    try:
        services = [
            ('agentd', [str(root / 'bin/agentd'), '--host', '127.0.0.1', '--port', str(agentport), '--runtime', 'local', '--credential-sync', '--data-dir', str(root / 'agentd-data')], root / 'work'),
        ]
        for name, command, cwd in services:
            log = (root / f'{name}.log').open('w')
            logs.append(log)
            processes.append(subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True))
        endpoints = [(env['AGENTD_URL'], processes[0])]
        for url, proc in endpoints:
            deadline = time.monotonic() + 120
            while True:
                if proc.poll() is not None:
                    raise RuntimeError(f'Isolated service exited: {url}; see private logs')
                try:
                    with urllib.request.urlopen(url + '/health', timeout=1) as response:
                        if response.status == 200:
                            break
                except (OSError, urllib.error.URLError):
                    pass
                if time.monotonic() > deadline:
                    raise RuntimeError(f'Isolated health timed out: {url}; see private logs')
                time.sleep(0.5)
            # Listener identity must be a descendant of the process we launched.
            listeners = subprocess.check_output(['lsof', '-nP', f'-iTCP:{url.rsplit(":", 1)[1]}', '-sTCP:LISTEN', '-Fp'], text=True)
            if f'p{proc.pid}\n' not in listeners:
                raise RuntimeError(f'Unexpected listener owner for {url}')
        env['AGENTCTL_LIVE_PROVIDER'] = args.provider
        driver = subprocess.Popen([sys.executable, str(repo / 'scripts/agentctl_observability_live_test.py'),
            '--provider', args.provider, '--root', str(root)], env=env, cwd=root / 'work', start_new_session=True)
        processes.append(driver)
        while driver.poll() is None:
            request = root / 'restart-agentd.request'
            if request.exists():
                request.unlink()
                old = processes[0]
                new = restart_owned_agentd(old, services[0][1], services[0][2], env, logs[0])
                processes[0] = new
                (root / 'restart-agentd.json').write_text(json.dumps({'old_pid': old.pid, 'new_pid': new.pid, 'isolated_data': str(root / 'agentd-data')}))
            if (root / 'network-violations.log').exists() or monitor_errors:
                raise RuntimeError('Isolation monitor detected a forbidden access or could not verify connections')
            time.sleep(0.2)
        if (root / 'network-violations.log').exists():
            raise RuntimeError('Forbidden live-port connection attempted; see network-violations.log')
        if monitor_errors: raise RuntimeError("Isolation monitor failed: " + monitor_errors[0])
        return driver.returncode
    finally:
        stop_monitor.set()
        monitor.join(timeout=5)
        child_groups = [(pid, birth) for pid, group, birth in descendants() if pid == group and pid not in {p.pid for p in processes}]
        for pid, birth in child_groups:
            try: os.killpg(pid, signal.SIGTERM)
            except ProcessLookupError: pass
        for proc in reversed(processes):
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGTERM)
                try:
                    proc.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait(timeout=5)
        for pid, birth in child_groups:
            current = subprocess.run(['ps', '-p', str(pid), '-o', 'lstart='], capture_output=True, text=True, timeout=3).stdout.strip()
            if current == birth:
                try: os.killpg(pid, signal.SIGKILL)
                except ProcessLookupError: pass
        containers = subprocess.run([realdocker, 'ps', '-aq', '--filter', 'label=agentctl.live_test=' + root.name], capture_output=True, text=True, timeout=20, check=True).stdout.split()
        if containers:
            subprocess.run([realdocker, 'rm', '-f', *containers], capture_output=True, timeout=30, check=True)
        networks = subprocess.run([realdocker, 'network', 'ls', '-q', '--filter', 'label=agentctl.live_test=' + root.name], capture_output=True, text=True, timeout=20, check=True).stdout.split()
        if networks:
            subprocess.run([realdocker, 'network', 'rm', *networks], capture_output=True, timeout=30, check=True)
        volumes = subprocess.run([realdocker, 'volume', 'ls', '-q', '--filter', 'label=agentctl.live_test=' + root.name], capture_output=True, text=True, timeout=20, check=True).stdout.split()
        if volumes:
            subprocess.run([realdocker, 'volume', 'rm', *volumes], capture_output=True, timeout=30, check=True)
        remaining = {}
        for kind, listing in [('containers', ['ps', '-aq']), ('networks', ['network', 'ls', '-q']), ('volumes', ['volume', 'ls', '-q'])]:
            remaining[kind] = subprocess.run([realdocker, *listing, '--filter', 'label=agentctl.live_test=' + root.name], capture_output=True, text=True, timeout=20, check=True).stdout.split()
        for log in logs:
            log.close()
        (root / 'cleanup.json').write_text(json.dumps({'processes_stopped': all(p.poll() is not None for p in processes), 'containers_removed': containers, 'networks_removed': networks, 'volumes_removed': volumes, 'remaining_resources': remaining, 'forbidden_connection_attempts': (root / 'network-violations.log').exists(), 'connection_monitor_errors': monitor_errors, 'local_child_groups_stopped': [p for p, _ in child_groups]}))
        if any(remaining.values()): raise RuntimeError('Test-owned Docker resources remain after cleanup')
        print(f'Isolated cleanup complete: {root}', flush=True)


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f'FAIL bootstrap: {error}', file=sys.stderr)
        raise SystemExit(1)
