#!/usr/bin/env python3
"""Final restart/fault/retry/default-discovery acceptance on isolated services."""
import argparse
import json
from pathlib import Path
import secrets
import sqlite3
import time
import urllib.request

from agentctl_persist_live_test import LiveTest, OUTCOME


class FinalProof(LiveTest):
    def manifest(self, sid: str) -> dict:
        with sqlite3.connect(f'file:{self.args.root}/agentd-data/agentd.sqlite?mode=ro', uri=True) as db:
            row = db.execute('SELECT request_manifest_json FROM sessions WHERE id=?', (sid,)).fetchone()
        if not row:
            raise RuntimeError('No AgentD request manifest')
        return json.loads(row[0])

    def assert_clean(self, sid: str) -> None:
        manifest = self.manifest(sid)
        if manifest.get('context') != 'clean' or manifest.get('auto_discover') is not False:
            raise RuntimeError('Harness task did not explicitly request clean context')

    def restart(self) -> None:
        task = self.dispatch('Restart wake', 'Reply RESTART_READY. No tools.', runtime='agentruntime')
        ref = 'task:' + task['id']
        first = self.completed(ref)
        self.assert_clean(task['session_id'])
        (self.args.root / 'restart-agentd.request').touch()
        receipt = self.until(lambda: (self.args.root / 'restart-agentd.json').exists(), seconds=60, label='owned AgentD restart')
        def healthy() -> bool:
            try:
                with urllib.request.urlopen(f'http://127.0.0.1:{self.args.agentd_port}/health', timeout=1) as response:
                    return response.status == 200
            except OSError:
                return False
        self.until(healthy, seconds=60)
        recovered = self.status(ref)
        self.ac('prompt', ref, 'Reply RESTART_WAKE_ANSWERED. No tools.' + OUTCOME)
        second = self.completed(ref, int(first['generation']) + 1)
        if not self.assistant_has(ref, 'RESTART_WAKE_ANSWERED', first['last_event_seq'], second['generation']):
            raise RuntimeError('Restart wake produced no answer')
        if second['provider_session_id'] != first['provider_session_id']:
            raise RuntimeError('Restart wake changed provider session')
        self.record('a(restart)', f'{ref}; recovered state={recovered["state"]}; generation {first["generation"]}→{second["generation"]}; RESTART_WAKE_ANSWERED; same provider session; isolated restart receipt')

    def failed_wake(self) -> None:
        task = self.dispatch('Unavailable Docker wake', 'Reply FAULT_READY. No tools.')
        ref = 'task:' + task['id']
        self.completed(ref)
        self.assert_clean(task['session_id'])
        flag = self.args.root / 'docker-unavailable'
        flag.touch()
        try:
            self.ac('prompt', ref, 'Reply SHOULD_NOT_RUN. No tools.' + OUTCOME, codes=(1, 5))
            state = self.until(lambda: self.status(ref) if self.status(ref)['state'] != 'starting' else None,
                               seconds=30, label='wake failure settled')
        finally:
            flag.unlink(missing_ok=True)
        if not state.get('continuable') and state['state'] not in ('failed', 'cancelled', 'crashed', 'timed_out', 'indeterminate'):
            raise RuntimeError('Failed wake is neither continuable nor terminal')
        result = json.loads(self.ac('result', ref))
        if not result.get('receipt'):
            raise RuntimeError('Failed wake has no durable receipt')
        cancel = json.loads(self.ac('--direct', 'cancel', task['session_id']))
        cancelled = self.status(ref)
        if cancelled['state'] != 'cancelled' and not (cancelled['state'] == state['state'] and cancel.get('accepted') and cancelled['state'] == 'failed'):
            raise RuntimeError('Cancel did not settle failed wake')
        if cancelled['state'] == 'failed' and json.loads(self.ac('result', ref)).get('receipt') != result['receipt']:
            raise RuntimeError('Cancel changed the immutable failure receipt')
        self.record('a(failed-wake)', f'{ref}; prompt rejected (CLI error); settled={state["state"]}; receipt present; cancel accepted; state={cancelled["state"]}; never stuck starting')

    def retry(self) -> None:
        task = self.dispatch('Retry idempotency', 'Reply RETRY_READY. No tools.')
        ref = 'task:' + task['id']
        first = self.completed(ref)
        self.assert_clean(task['session_id'])
        prefix = 'retry-' + secrets.token_hex(4)
        prompt = f'Use the shell tool to run `bash ./long-turn.sh {prefix}` with a 120000ms timeout in the foreground. Wait, then reply RETRY_ORIGINAL_FINISHED.' + OUTCOME
        for _ in range(2):
            self.pc('task', 'prompt', task['id'], prompt, '--idempotency-key', prefix + '-prompt', '--json')
        self.until(lambda: (self.work / f'{prefix}.started').exists(), label='retry foreground started')
        before = self.status(ref)['last_event_seq']
        for _ in range(2):
            self.pc('task', 'steer', task['id'], 'Stop the sleep. Reply RETRY_STEERED. No further tools.' + OUTCOME,
                    '--idempotency-key', prefix + '-steer', '--json')
        self.until(lambda: self.assistant_has(ref, 'RETRY_STEERED', before), seconds=100)
        self.completed(ref, int(first['generation']) + 1)
        counts = {}
        for kind in ('prompt', 'steer'):
            events = self.events(ref, '.', f'control.{kind}.dispatched')
            counts[kind] = len(events)
        if counts != {'prompt': 1, 'steer': 1} or (self.work / f'{prefix}.finished').exists():
            raise RuntimeError('Duplicate provider control or original workload finished: ' + str(counts))
        self.record('e', f'{ref}; two separate persist task prompt/steer invocations per key; AgentD dispatched prompt={counts["prompt"]}, steer={counts["steer"]}; generation advanced once; RETRY_STEERED')

    def discovery(self) -> None:
        task = self.dispatch('Default local discovery', 'Reply with the home discovery phrase configured in your home instructions. No tools.', runtime='agentruntime')
        ref = 'task:' + task['id']
        self.completed(ref)
        manifest = self.manifest(task['session_id'])
        if manifest.get('context') == 'clean' or manifest.get('auto_discover') is False:
            raise RuntimeError('Default local request suppressed discovery')
        if not self.assistant_has(ref, 'ISOLATED_HOME_DISCOVERY_CONFIRMED'):
            raise RuntimeError('Provider did not consume isolated home instructions')
        evidence = json.loads((self.args.root / 'discovery-environment.json').read_text())
        evidence.update(task_id=task['id'], session_id=task['session_id'], context=manifest.get('context'),
                        auto_discover=manifest.get('auto_discover'), provider_consumed_home_phrase=True)
        (self.args.root / 'default-context-proof.json').write_text(json.dumps(evidence, indent=2))
        self.record('g', f'{ref}; manifest context={manifest.get("context")!r}, auto_discover={manifest.get("auto_discover")!r}; isolated HOME={evidence["HOME"]}; provider consumed private home instructions')

    def approval(self, decision: str) -> None:
        super().approval(decision)
        detail = self.results[-1]['detail']
        broker_id = detail.split('broker request=')[1].split(';')[0]
        events = json.loads(self.pc('audit', 'list', '--limit', '200', '--json'))['events']
        rows = [e for e in events if broker_id in json.dumps(e) and e.get('event') not in ('stream-agent.approval.pending', 'http.request.completed')]
        if len(rows) != 1:
            raise RuntimeError(f'Approval audit must contain exactly one row for request: {len(rows)}')
        self.results[-1]['detail'] += '; exactly one decision audit row; no duplicate decision shapes'
        (self.args.root / 'results.json').write_text(json.dumps(self.results, indent=2))

    def flow(self) -> None:
        super().flow()
        flows = self.args.flows.split(',')
        if 'a-restart' in flows: self.restart()
        if 'a-failed-wake' in flows: self.failed_wake()
        if 'e' in flows: self.retry()
        if 'g' in flows: self.discovery()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('--provider', required=True)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--agentd-port', required=True)
    parser.add_argument('--persist-port', required=True)
    parser.add_argument('--persist-root', type=Path, required=True)
    parser.add_argument('--persist-cli', required=True)
    parser.add_argument('--agentctl', required=True)
    parser.add_argument('--flows', required=True)
    test = FinalProof(parser.parse_args())
    try:
        test.flow()
        return 0
    except Exception as error:
        print(f'FAIL {test.args.provider}: {error}', flush=True)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
