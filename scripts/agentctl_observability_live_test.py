#!/usr/bin/env python3
"""AgentD-only provider proof, launched by the guarded AgentD bootstrap."""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import time

from agentctl_live_test import LiveTest, execute


class ObservabilityTest(LiveTest):
    def __init__(self, provider: str, root: Path) -> None:
        self.provider = provider
        self.directory = root
        self.work = root / 'work'
        self.data = root / 'agentd-data'
        self.env = dict(os.environ)
        self.model = 'gpt-6-luna' if provider == 'codex' else 'claude-haiku-4-5'
        self.sessions = []
        self.records = []
        self.commands = []
        self.binary = str(root / 'bin/agentctl')

    def run(self, prompt: str, restricted: bool = False) -> str:
        args = ['run', '--provider', self.provider, '--model', self.model,
                '--cwd', str(self.work), '--runtime', 'local', '--context', 'clean',
                '--permissions', 'unrestricted', '--approval-policy', 'never']
        if self.provider == 'codex':
            args += ['--effort', 'low']
        sid = self.value(*args, prompt)['session_id']
        self.sessions.append(sid)
        return sid

    def terminate(self, sid: str, codes: tuple[int, ...] | None = (0,)):
        return self.ctl('--force', 'terminate', sid, codes=codes)

    def proof(self) -> None:
        version = execute([self.provider, '--version'], env=self.env)
        if version.returncode:
            raise RuntimeError('Cannot verify provider version')
        (self.directory / 'provider-version.txt').write_text(version.stdout.strip() + '\n')
        if self.provider == 'claude':
            source = self.data / 'credentials/claude-credentials.json'
            self.until(source.exists, seconds=40)
            dest = Path(self.env['CLAUDE_CONFIG_DIR']) / '.credentials.json'
            shutil.copyfile(source, dest)
            dest.chmod(0o600)
        fixture = self.work / 'silent.sh'
        fixture.write_text('#!/bin/sh\nset -eu\nprintf started > "$1.started"\nsleep "$2"\nprintf finished > "$1.finished"\nprintf SILENT_DONE\n')
        # Prevent local ancestor instructions and any provider-specific MCP
        # loading. The private Codex home explicitly enables multi_agent.
        (self.work / 'AGENTS.md').write_text('This is an isolated transport test. Follow only the test prompt. Do not read other files, use MCP, or contact local services.\n')
        child = ('Spawn exactly ONE native sub-agent using spawn_agent (Codex) or Agent/Task (Claude), in the foreground. '
                 'Give it this exact assignment: use the shell/Bash tool to run `sh ./silent.sh child 12` in the foreground; '
                 'wait for completion, then reply CHILD_OBSERVABILITY_DONE. The shell timeout must exceed 20 seconds. '
                 'Do not run the command yourself. Wait for the child to finish, then reply PARENT_OBSERVABILITY_DONE. '
                 'Do not use MCP, network, or inspect other paths. This is explicitly authorized sub-agent testing.')
        sid = self.run(child)
        samples = []
        peak = 0
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            status = self.value('status', sid)
            samples.append(status)
            peak = max(peak, status['active_subagents'])
            (self.directory / 'activity-samples.json').write_text(json.dumps(samples, indent=2))
            if status['state'] in ('completed', 'failed', 'crashed', 'timed_out'):
                break
            time.sleep(.5)
        events = self.events(sid)
        (self.directory / 'subagent-events.json').write_text(json.dumps(events, indent=2))
        result = execute([self.binary, '--as', 'human', 'since', sid], env=self.env)
        (self.directory / 'since.txt').write_text(result.stdout)
        child_summary = any(line.startswith('[sub:') and 'CHILD_OBSERVABILITY_DONE' in line for line in result.stdout.splitlines())
        self.record('subagent since', result.returncode == 0 and child_summary,
                    f'{sid}; labeled summaries saved in since.txt')
        final = self.value('status', sid)
        self.record('active_subagents', peak == 1 and final['active_subagents'] == 0,
                    f'peak={peak}; final={final["active_subagents"]}; state={final["state"]}')
        child_events = [e for e in events if e['type'].startswith('subagent.')]
        kinds = sorted(set(e['type'] for e in child_events))
        self.record('durable child metadata', bool(child_events) and all(
            all(k in e['payload'] for k in ('subagent_thread_id', 'subagent_path', 'model')) for e in child_events),
            ', '.join(kinds))
        self.record('child tools and model', {'subagent.tool.call', 'subagent.tool.result'}.issubset(kinds)
                    and any(e['payload'].get('model') for e in child_events), 'child tool calls/results and provider-exposed model present')
        # Prove exact type filtering against durable history.
        for kind in kinds:
            matches = self.events(sid, '.', kind)
            if not matches or any(e['type'] != kind for e in matches):
                raise RuntimeError('grep --type did not isolate ' + kind)
        if final['state'] != 'completed' or not (self.work / 'child.finished').exists():
            raise RuntimeError('subagent workload did not finish successfully')

        silent = self.run('Use the shell/Bash tool to run exactly `sh ./silent.sh stall 90` in the foreground '
                          'with a timeout of at least 120 seconds (120000 ms). Do not background it. Do not '
                          'emit progress, delegate, use MCP, network, or inspect other paths. Wait, then reply DONE.')
        self.until(lambda: (self.work / 'stall.started').exists(), seconds=60)
        # These builds emit keep_alive (Claude) and account/token usage
        # notifications (Codex) during a plain sleep. Pause only this test's
        # provider process to reproduce actual provider silence, not merely
        # lack of assistant text. The shell child keeps sleeping.
        listener = execute(['lsof', '-nP', '-iTCP:' + self.env['AGENTD_URL'].rsplit(':', 1)[1],
                            '-sTCP:LISTEN', '-Fp'])
        listeners = [int(line[1:]) for line in listener.stdout.splitlines() if line.startswith('p')]
        if listener.returncode or len(listeners) != 1:
            raise RuntimeError('Cannot verify isolated daemon listener owner')
        daemon_pid = listeners[0]
        rows = [line.split(None, 2) for line in execute(['ps', '-axo', 'pid=,ppid=,comm=']).stdout.splitlines()]
        providers = [int(row[0]) for row in rows if int(row[1]) == daemon_pid and self.provider in row[2].lower()]
        if len(providers) != 1:
            raise RuntimeError('Cannot identify exactly one isolated provider child for silence proof')
        provider_pid = providers[0]
        (self.directory / 'silence-injection.json').write_text(json.dumps({'daemon_pid': daemon_pid,
            'provider_pid': provider_pid, 'signal': 'SIGSTOP', 'session_id': silent}))
        started = time.monotonic()
        os.kill(provider_pid, signal.SIGSTOP)
        try:
            wait = self.ctl('wait', silent, '--until', 'stalled', '--stall', '30s', '--timeout', '75s',
                            codes=(17,), timeout=85)
            status = self.value('status', silent)
            elapsed = time.monotonic() - started
        finally:
            os.kill(provider_pid, signal.SIGCONT)
        self.record('stalled wait', wait.returncode == 17 and status['running_turn'] and
                    status['idle_seconds'] >= 30 and not (self.work / 'stall.finished').exists(),
                    f'exit={wait.returncode}; elapsed={elapsed:.1f}s; idle={status["idle_seconds"]:.1f}s; state={status["state"]}; test provider paused after sleep started')
        # The waiter observes; it does not fail or kill the turn.
        self.terminate(silent)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('--provider', required=True, choices=('codex', 'claude'))
    parser.add_argument('--root', required=True, type=Path)
    args, _ = parser.parse_known_args()
    os.umask(0o077)
    test = ObservabilityTest(args.provider, args.root)
    try:
        test.proof()
    except Exception as error:
        test.record('flow', False, str(error))
    finally:
        for sid in test.sessions:
            test.terminate(sid, codes=None)
    return int(any(row['status'] == 'FAIL' for row in test.records))


if __name__ == '__main__':
    raise SystemExit(main())
