#!/usr/bin/env python3
"""Real-provider acceptance, with every workflow action through public CLIs."""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import select
import signal
import subprocess
import sys
import time

OUTCOME = '\nEnd your final response with a standalone line: PERSIST_TASK_OUTCOME: {"version":1,"status":"completed","reason":"live acceptance fixture complete"}'


class LiveTest:
    def __init__(self, args):
        self.args = args
        self.work = args.root / 'work'
        self.persist = [args.persist_cli, '--dir', str(args.persist_root), 'exec', 'tsx', str(args.persist_root / 'src/cli/index.ts')]
        self.ctl = [args.agentctl, '--json']
        self.provider = 'anthropic' if args.provider == 'claude' else 'openai'
        self.model = 'claude-haiku-4-5' if args.provider == 'claude' else 'gpt-6-luna'
        self.log = args.root / 'cli.jsonl'
        self.results = []
        self.secrets = [p.read_text().strip() for p in (args.root / 'agentd-data/auth.token', args.root / 'persist-data/initial_token.txt') if p.is_file()]

    def run(self, command, *, timeout=240, codes=(0,)):
        if any(secret and any(secret in arg for arg in command) for secret in self.secrets):
            raise RuntimeError('Bearer token in command arguments')
        result = subprocess.run(command, text=True, capture_output=True, timeout=timeout)
        record = json.dumps({'argv': command, 'code': result.returncode, 'out': result.stdout, 'err': result.stderr})
        if any(secret and secret in record for secret in self.secrets):
            raise RuntimeError('Bearer token in CLI output (not retained)')
        with self.log.open('a') as stream:
            stream.write(record + '\n')
        if result.returncode not in codes:
            raise RuntimeError(f'CLI exit {result.returncode} for {command[len(self.persist):] if command[:1] == self.persist[:1] else command[1:]}: {result.stderr[:1500]}')
        return result.stdout

    def pc(self, *args, **kwargs):
        return self.run(self.persist + list(args), **kwargs)

    def ac(self, *args, **kwargs):
        return self.run(self.ctl + list(args), **kwargs)

    def status(self, ref):
        return json.loads(self.ac('status', ref))

    def events(self, ref, pattern='.', kind=None):
        args = ['grep', ref, pattern]
        if kind:
            args += ['--type', kind]
        return json.loads(self.ac(*args)) or []

    def assistant_has(self, ref, marker, after=0, generation=None):
        for event in self.events(ref, marker):
            event = event.get('event', event)
            if event['sequence'] <= after or (generation is not None and event.get('generation') != generation):
                continue
            p = event.get('payload', {})
            if event['type'] in ('content.delta', 'content.completed') and marker in p.get('text', ''):
                return True
            item = p.get('item', {})
            if item.get('type') == 'agentMessage' and marker in item.get('text', ''):
                return True
        return False

    def until(self, check, seconds=180, label='condition'):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            result = check()
            if result:
                return result
            time.sleep(0.5)
        raise RuntimeError(f'Timed out: {label}')

    def record(self, flow, detail):
        self.results.append({'provider': self.args.provider, 'flow': flow, 'status': 'PASS', 'detail': detail})
        (self.args.root / 'results.json').write_text(json.dumps(self.results, indent=2))
        print(f'PASS {self.args.provider} {flow}: {detail}', flush=True)

    def task(self, task_id):
        return json.loads(self.pc('task', 'get', task_id))

    def session(self, task_id):
        return self.until(lambda: self.task(task_id).get('session_id'), label='task assigned an AgentD session')

    def dispatch(self, title, prompt, runtime='docker', restricted=False):
        args = ['task', 'dispatch', '--title', title, '--content', prompt + OUTCOME, '--agent', self.args.provider,
                '--model', self.model, '--cwd', str(self.work), '--runtime', runtime, '--json']
        if not restricted:
            args += ['--skip-permissions']
        task = json.loads(self.pc(*args))
        task['session_id'] = self.session(task['id'])
        return task

    def completed(self, ref, generation=1):
        def check():
            s = self.status(ref)
            if s['state'] in ('failed', 'cancelled', 'crashed', 'timed_out', 'indeterminate'):
                raise RuntimeError(f'{ref} ended {s["state"]}: ' + self.ac('result', ref))
            return s if s['state'] == 'completed' and s.get('generation') == generation else None
        return self.until(check, label=f'{ref} completed generation {generation}')

    def wake(self, runtime):
        marker = 'RECALL_' + secrets.token_hex(8)
        args = ['model', 'dispatch-auto', '--title', f'Wake {runtime}', '--prompt', f'This is an integration test. Reply with the random test word {marker}. No tools.' + OUTCOME,
                '--provider', self.provider, '--cwd', str(self.work), '--complexity', 'trivial', '--objectives', 'cost,speed']
        if runtime == 'local':
            args += ['--skip-permissions']
        receipt = json.loads(self.pc(*args))
        if receipt['decision']['selected']['route']['provider'] != self.provider:
            raise RuntimeError('Auto selected a worker outside the requested provider')
        task_id = receipt['task']['id']
        sid = self.session(task_id)
        ref = 'task:' + task_id
        first = self.completed(ref)
        if first['runtime'] != runtime or first.get('container_lease') or not first.get('continuable'):
            raise RuntimeError(f'Unexpected initial no-lease state: {first}')
        if not self.assistant_has(ref, marker):
            raise RuntimeError('Turn 1 did not contain the test word')
        before = first['last_event_seq']
        provider_id = first['provider_session_id']
        self.ac('prompt', ref, 'Repeat the random test word from your immediately preceding response. No tools.' + OUTCOME)
        second = self.completed(ref, 2)
        if second['provider_session_id'] != provider_id or not self.assistant_has(ref, marker, before, 2):
            raise RuntimeError('Native provider context was not preserved across wake')
        self.pc('task', 'wait', task_id, '--timeout', '30s', '--json')
        self.record(f'a({runtime})', f'task:{task_id} session={sid} generation 1→2; no lease; same provider session; recalled {marker}')

    def watch_plan(self, plan):
        command = self.persist + ['plan', 'watch', plan]
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        required = {'node_started', 'node_completed', 'plan_status_changed'}
        observed = set()
        data = b''
        pending = b''
        deadline = time.monotonic() + 45
        try:
            while not required.issubset(observed):
                if time.monotonic() >= deadline or process.poll() is not None:
                    raise RuntimeError('Plan watch did not replay durable lifecycle events after CLI startup')
                if not select.select([process.stdout], [], [], 0.5)[0]: continue
                chunk = os.read(process.stdout.fileno(), 65536)
                data += chunk
                pending += chunk
                lines = pending.split(b'\n')
                pending = lines.pop()
                for line in lines:
                    try: observed.add(json.loads(line).get('type'))
                    except (ValueError, AttributeError): pass
            if any(secret.encode() in data for secret in self.secrets): raise RuntimeError('Bearer token in plan output')
            (self.args.root / 'plan-watch.jsonl').write_bytes(data)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try: process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)
            _, errors = process.communicate(timeout=5)
            if any(secret.encode() in errors for secret in self.secrets): raise RuntimeError('Bearer token in plan watch error')
            with self.log.open('a') as stream:
                stream.write(json.dumps({'argv': command, 'code': process.returncode, 'out': data.decode(), 'err': errors.decode()}) + '\n')

    def dag(self):
        prompt = ('Create exactly two tasks with titles A and B. B depends on A. A must use the shell tool to run `sleep 15` in the foreground, then reply A_DONE_TOKEN. B replies B_DONE_TOKEN with no tools. '
                  'Both tasks must end with PERSIST_TASK_OUTCOME: {"version":1,"status":"completed","reason":"fixture done"}. No other work or tasks.')
        proposal = json.loads(self.pc('plan', 'auto', prompt, '--cwd', str(self.work), '--provider', self.provider, '--skip-permissions', '--json'))
        nodes = proposal.get('tasks', [])
        if len(nodes) != 2 or not any(n.get('depends_on') for n in nodes):
            raise RuntimeError('Planner did not produce exactly two dependent tasks')
        receipt = json.loads(self.pc('plan', 'approve', proposal['proposal_id'], '--json'))
        plan = receipt['plan_id']
        ref = 'plan:' + plan
        def dispatched_root():
            rows = json.loads(self.ac('plan', 'show', plan))
            roots = [n for n in rows if n.get('session_id')]
            blocked = [n for n in rows if n.get('blocked_on') and not n.get('session_id')]
            return (roots[0], blocked[0]) if roots and blocked else None
        a, b = self.until(dispatched_root, label='A dispatched while B has null session and blocked_on A')
        if a['node'] not in b['blocked_on']:
            raise RuntimeError('B is not blocked on A')
        # Same caller consumes successive start events; the second start must name B.
        starts = []
        for _ in range(2):
            starts.append(self.ac('--as', 'dag-starts', 'wait', ref, '--until', 'node_started', '--timeout', '5m', timeout=310, codes=(14,)))
        if f"node={b['node']}" not in starts[-1] or 'session=-' in starts[-1]:
            raise RuntimeError('Plan waiter did not report B with its assigned session')
        self.ac('--as', 'dag-done', 'wait', ref, '--until', 'failed,done', '--timeout', '5m', timeout=310, codes=(13,))
        output = self.ac('since', ref)
        if 'A_DONE_TOKEN' not in output or 'B_DONE_TOKEN' not in output:
            raise RuntimeError('Plan since omitted node output')
        found = self.events(ref, 'A_DONE_TOKEN|B_DONE_TOKEN')
        if {e['node'] for e in found} != {a['node'], b['node']}:
            raise RuntimeError('Plan grep did not tag both nodes')
        self.ac('since', ref + '/' + b['node'])
        self.watch_plan(plan)
        self.record('b', f'{ref}; A session={a["session_id"]}; B initially blocked_on={b["blocked_on"]}, null session; wait: {starts[-1].strip()}; both output tokens; done; SSE replay')

    def approval(self, decision):
        marker = 'fixture_' + secrets.token_hex(4)
        # A single command has an outside-workspace approval trigger and a
        # workspace effect independently visible on the host after consent.
        command = f'printf {marker} > /var/tmp/{marker} && printf {marker} > /workspace/{marker}'
        prompt = (f'This is an operator-authorized integration test in a disposable Docker container. Please run `{command}` using the shell tool to create two test marker files. '
                  'The /var/tmp write intentionally requires human approval. Use the provider tool approval mechanism and wait for the operator decision before running it. '
                  'The operator will decide in the approval dialog. If denied, stop without retrying or choosing another command and report the denial.')
        task = self.dispatch(f'Approval {decision}', prompt, restricted=True)
        ref = 'task:' + task['id']
        self.ac('--as', 'approvals', 'wait', ref, '--until', 'approval,error,turn_end', '--timeout', '180s', codes=(11,))
        pending = self.until(lambda: json.loads(self.ac('approvals', ref)).get('approvals'), label='PERSIST mirrored pending approval')
        broker_id = pending[0]['id']
        self.ac(decision, ref, broker_id)
        self.completed(ref)
        exists = (self.work / marker).is_file()
        if exists != (decision == 'approve'):
            raise RuntimeError(f'{decision}: unexpected independent file effect')
        if exists and (self.work / marker).read_text() != marker:
            raise RuntimeError('Approved write has wrong content')
        audit = self.pc('audit', 'list', '--event', 'stream-agent.approval.decided', '--limit', '100', '--json')
        events = json.loads(audit)['events']
        if not any(broker_id in str(e) and ('accept' if decision == 'approve' else 'decline') in str(e) for e in events):
            raise RuntimeError('Missing matching PERSIST approval audit record')
        self.record('c(' + decision + ')', f'{ref} session={task["session_id"]}; broker request={broker_id}; host file exists={exists}; matching audit row')

    def active(self, action):
        prefix = action + '-' + secrets.token_hex(4)
        prompt = (f'Use the shell tool to run `bash ./long-turn.sh {prefix}` with a 120000ms timeout in the foreground. '
                  'Wait for completion, then reply ORIGINAL_LONG_TURN_FINISHED. Do not background it.')
        task = self.dispatch('Active Docker ' + action, prompt)
        ref = 'task:' + task['id']
        self.until(lambda: (self.work / f'{prefix}.started').exists() and self.events(ref, '.', 'tool.call'), label='real foreground Docker workload started')
        active = self.status(ref)
        if active['runtime'] != 'docker':
            raise RuntimeError('Active control fixture did not run in Docker')
        before = active['last_event_seq']
        if action == 'steer':
            self.ac('steer', ref, 'Stop the sleep task immediately. Reply exactly STEERED_TOKEN. Do not use any more tools.' + OUTCOME)
            self.until(lambda: self.assistant_has(ref, 'STEERED_TOKEN', before), seconds=100, label='changed output after active Docker steer')
            self.completed(ref)
        else:
            self.ac('interrupt', ref)
            def interrupted():
                ends = [e for e in self.events(ref) if e['sequence'] > before and e['type'] in ('turn.completed', 'turn.failed')]
                return any(e['payload'].get('status') == 'interrupted' or e['payload'].get('result', {}).get('terminal_reason') == 'aborted_tools' for e in ends)
            self.until(interrupted, seconds=60, label='native interrupted turn event')
        if (self.work / f'{prefix}.finished').exists() or self.assistant_has(ref, 'ORIGINAL_LONG_TURN_FINISHED', before):
            raise RuntimeError('Original foreground workload completed; control did not interrupt it')
        self.record('d(' + action + ')', f'{ref} session={task["session_id"]}; active 60s shell; {"STEERED_TOKEN after control" if action == "steer" else "native interrupted turn event"}; original workload did not finish')

    def flow(self):
        status = self.pc('daemon', 'status')
        if 'Status:  running' not in status:
            raise RuntimeError('CLI is not connected to isolated foreground daemon')
        self.pc('model', 'catalog')
        self.ac('ls')
        (self.work / 'long-turn.sh').write_text('#!/bin/sh\nset -eu\nprintf started > "$1.started"\nsleep 60\nprintf finished > "$1.finished"\nprintf ORIGINAL_LONG_TURN_FINISHED\n')
        flows = self.args.flows.split(',')
        if 'a-local' in flows: self.wake('local')
        if 'a-docker' in flows: self.wake('docker')
        if 'b' in flows: self.dag()
        if 'c' in flows:
            self.approval('approve')
            self.approval('deny')
        if 'd' in flows:
            self.active('steer')
            self.active('interrupt')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--provider', choices=('claude', 'codex'), required=True)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--agentd-port', required=True)
    parser.add_argument('--persist-port', required=True)
    parser.add_argument('--persist-root', type=Path, required=True)
    parser.add_argument('--persist-cli', required=True)
    parser.add_argument('--agentctl', required=True)
    parser.add_argument('--flows', default='a-local,a-docker,b,c,d')
    args = parser.parse_args()
    test = LiveTest(args)
    try:
        test.flow()
        return 0
    except Exception as error:
        print(f'FAIL {args.provider}: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
