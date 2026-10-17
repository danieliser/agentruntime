#!/usr/bin/env python3
"""Own isolated processes, credentials, ports and teardown for live acceptance."""
import argparse
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

FORBIDDEN_PORTS = {8803, 38093}


def resolve_node(root: Path, env: dict[str, str]) -> str:
    pin = next((p.read_text().strip().removeprefix('v') for p in
                (root / '.node-version', root / '.nvmrc') if p.is_file()), '')
    nvm = Path(env.get('NVM_DIR', str(Path.home() / '.nvm')))
    if not pin and (nvm / 'alias/default').is_file():
        pin = (nvm / 'alias/default').read_text().strip().removeprefix('v')
    candidates = list((nvm / 'versions/node').glob(f'v{pin}*/bin/node')) if pin else []
    candidates.sort(key=lambda p: tuple(int(n) for n in re.findall(r'\d+', p.parts[-3])), reverse=True)
    candidates += [Path(p) for p in (shutil.which('node', path=env.get('PATH')), '/opt/homebrew/opt/node/bin/node', '/usr/local/bin/node') if p]
    for node in candidates:
        if not node.is_file():
            continue
        version = subprocess.check_output([str(node), '--version'], text=True).strip().removeprefix('v')
        if not pin or version == pin or version.startswith(pin + '.'):
            return str(node)
    raise RuntimeError(f'Node {pin or "(repository pin)"} is not installed; install that version before running')


def isolated_env(root: Path, persist_port: int, agentd_port: int, parent: dict[str, str]) -> dict[str, str]:
    if {persist_port, agentd_port} & FORBIDDEN_PORTS or persist_port == agentd_port:
        raise RuntimeError('Live or duplicate ports are forbidden')
    env = {k: v for k, v in parent.items() if not k.startswith(('PERSIST_', 'AGENTD_', 'AGENTRUNTIME_', 'AGENTCTL_'))}
    env.update(PERSIST_HTTP__PORT=str(persist_port), PERSIST_HTTP__HOST='127.0.0.1',
               PERSIST_AGENTRUNTIME__CLEAN_CONTEXT='true',
               PERSIST_DATA_DIR=str(root / 'persist-data'), PERSIST_CONFIG_DIR=str(root / 'persist-config'),
               PERSIST_URL=f'http://127.0.0.1:{persist_port}',
               PERSIST_MODEL_ROUTING_MARKDOWN=str(root / 'persist-data/model-routing.md'),
               PERSIST_AGENT_CONFIG_HOME=str(root / 'persist-data/agent-home'),
               PERSIST_TOKEN_FILE=str(root / 'persist-data/initial_token.txt'),
               PERSIST_V3_TOKEN_FILE=str(root / 'persist-data/initial_token.txt'),
               AGENTD_URL=f'http://127.0.0.1:{agentd_port}',
               AGENTD_TOKEN_FILE=str(root / 'agentd-data/auth.token'),
               AGENTCTL_CURSOR_DIR=str(root / 'cursors'))
    return env


def forbidden_connections(output: str) -> list[str]:
    return [line for line in output.splitlines() if re.search(r'->[^ ]+:(?:8803|38093)(?:$|\s)', line)]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('provider', choices=('claude', 'codex'))
    parser.add_argument('--preflight', action='store_true')
    parser.add_argument('--flows', default='a-local,a-docker,b,c,d')
    parser.add_argument('--observability', action='store_true', help='AgentD-only observability proof; never starts PERSIST')
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    persist = Path(os.environ.get('PERSIST_ROOT', str(repo.parent / 'persistence--agentctl'))).resolve()
    if persist == repo.parent / 'persistence':
        raise RuntimeError('Live PERSIST checkout is forbidden')
    node = resolve_node(persist, dict(os.environ))
    env = dict(os.environ, PATH=str(Path(node).parent) + os.pathsep + os.environ['PATH'])
    probe = subprocess.run([node, '-e', "const db=require('better-sqlite3')(':memory:');db.prepare('SELECT 1').get();db.close()"], cwd=persist, env=env, capture_output=True, text=True)
    if probe.returncode:
        raise RuntimeError(f'Native better-sqlite3 ABI mismatch or load failure with {node}: {probe.stderr.strip()}\nRebuild only this checkout with: PATH={Path(node).parent}:$PATH node scripts/ensure-native.cjs (cwd={persist})')
    print(f'Preflight Node: {node}; native SQLite loaded', flush=True)
    if args.preflight:
        return 0
    for command in ('go', 'pnpm', 'docker'):
        if not shutil.which(command, path=env['PATH']):
            raise RuntimeError(f'Missing required command: {command}')
    port = int(os.environ.get('AGENTCTL_PERSIST_PORT', '18803' if args.provider == 'claude' else '18804'))
    agentport = int(os.environ.get('AGENTCTL_PERSIST_AGENTD_PORT', '18091' if args.provider == 'claude' else '18092'))
    base = repo / '.agentctl-live-test' / ('persist-' + args.provider)
    base.mkdir(parents=True, exist_ok=True)
    subprocess.run(['git', 'check-ignore', '-q', str(base)], cwd=repo, check=True)
    root = Path(tempfile.mkdtemp(prefix='run.', dir=base))
    root.chmod(0o700)
    env = isolated_env(root, port, agentport, env)
    print(f'Private evidence: {root}', flush=True)
    for directory in ('bin', 'work', 'persist-data', 'persist-config', 'agentd-data', 'cursors'):
        (root / directory).mkdir(mode=0o700)
    for number in (port, agentport):
        with socket.socket() as sock:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(('127.0.0.1', number))
    config = f'''data_dir: {root / 'persist-data'}
config_dir: {root / 'persist-config'}
http:
  port: {port}
  host: 127.0.0.1
agentruntime:
  url: {env['AGENTD_URL']}
  token_file: {env['AGENTD_TOKEN_FILE']}
  clean_context: true
  resume_idempotency_placement: header
  resume_idempotency_header: idempotency-key
model_routing:
  worker_runtime: agentruntime
'''
    (root / 'persist-config/config.yaml').write_text(config)
    (root / 'persist-config/config.yaml').chmod(0o600)
    guard = repo / 'scripts/agentctl_persist_guard.cjs'
    forbidden = [str(Path.home() / '.local/share/persist-v3'), str(Path.home() / '.config/persist-v3'), str(Path.home() / '.agentd'), str(repo.parent / 'persistence'), str(repo.parent / 'agentruntime')]
    env.update(NODE_OPTIONS=f'--require={guard}', AGENTCTL_NETWORK_VIOLATIONS=str(root / 'network-violations.log'), AGENTCTL_FORBIDDEN_ROOTS=json.dumps(forbidden))
    if args.observability:
        # Local providers use only private configuration. In particular, do
        # not load the host MCP catalog (which can connect to live PERSIST).
        codex_home = root / 'provider-home/codex'
        claude_home = root / 'provider-home/claude'
        codex_home.mkdir(parents=True, mode=0o700)
        claude_home.mkdir(parents=True, mode=0o700)
        private_tmp = root / 'tmp'
        private_tmp.mkdir(mode=0o700)
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
        subprocess.run(['go', 'build', '-buildvcs=false', '-ldflags=-X github.com/danieliser/agentruntime/pkg/buildinfo.Version=2.3.3', '-o', str(root / 'bin' / name), './cmd/' + name], cwd=repo, check=True)
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
            ('agentd', [str(root / 'bin/agentd'), '--host', '127.0.0.1', '--port', str(agentport), '--runtime', 'docker', '--credential-sync', '--data-dir', str(root / 'agentd-data')], root / 'work'),
        ]
        if not args.observability:
            services.append(('persist', [node, '--import', 'tsx', str(persist / 'src/main-daemon.ts')], persist))
        for name, command, cwd in services:
            log = (root / f'{name}.log').open('w')
            logs.append(log)
            processes.append(subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True))
        endpoints = [(env['AGENTD_URL'], processes[0])]
        if not args.observability:
            endpoints.append((env['PERSIST_URL'], processes[1]))
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
        driver_name = 'agentctl_observability_live_test.py' if args.observability else 'agentctl_persist_live_test.py'
        driver = subprocess.Popen([sys.executable, str(repo / 'scripts' / driver_name),
            '--provider', args.provider, '--root', str(root), '--agentd-port', str(agentport), '--persist-port', str(port),
            '--persist-root', str(persist), '--persist-cli', shutil.which('pnpm', path=env['PATH']),
            '--agentctl', str(root / 'bin/agentctl'), '--flows', args.flows], env=env, cwd=root / 'work', start_new_session=True)
        processes.append(driver)
        while driver.poll() is None:
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
        for log in logs:
            log.close()
        (root / 'cleanup.json').write_text(json.dumps({'processes_stopped': all(p.poll() is not None for p in processes), 'containers_removed': containers, 'networks_removed': networks, 'forbidden_connection_attempts': (root / 'network-violations.log').exists(), 'connection_monitor_errors': monitor_errors, 'local_child_groups_stopped': [p for p, _ in child_groups]}))
        print(f'Isolated cleanup complete: {root}', flush=True)


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f'FAIL bootstrap: {error}', file=sys.stderr)
        raise SystemExit(1)
