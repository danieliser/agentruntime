#!/usr/bin/env python3
"""Drive isolated PERSIST/AgentD acceptance using the two public CLIs."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('--provider', choices=('claude', 'codex'), required=True)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--agentd-port', required=True)
    parser.add_argument('--persist-port', required=True)
    parser.add_argument('--persist-root', type=Path, required=True)
    parser.add_argument('--persist-cli', required=True)
    parser.add_argument('--agentctl', required=True)
    args = parser.parse_args()
    env = dict(os.environ, PERSIST_URL=f"http://127.0.0.1:{args.persist_port}",
               AGENTD_URL=f"http://127.0.0.1:{args.agentd_port}")
    persist = [args.persist_cli, '--dir', str(args.persist_root), 'exec', 'tsx',
               str(args.persist_root / 'src/cli/index.ts')]
    ctl = [args.agentctl, '--json']
    log = args.root / 'cli.log'

    def run(command: list[str], *, timeout: int = 180, codes: tuple[int, ...] = (0,)) -> str:
        result = subprocess.run(command, env=env, text=True, capture_output=True, timeout=timeout)
        with log.open('a') as stream:
            stream.write(json.dumps({'argv': command, 'code': result.returncode,
                                     'out': result.stdout, 'err': result.stderr}) + '\n')
        if result.returncode not in codes:
            raise RuntimeError(f"CLI failed ({result.returncode}); details are in the private run log")
        return result.stdout

    # Verify both command surfaces and the isolated audit store before creating
    # provider work. All task and session changes below cross one of these CLIs.
    run(persist + ['daemon', 'status'])
    run(persist + ['audit', 'list', '--limit', '1'])
    run(ctl + ['ls'])
    token_file = args.root / 'agentd-data' / 'auth.token'
    if not token_file.is_file() or token_file.stat().st_mode & 0o077:
        raise RuntimeError('Isolated AgentD secret token is missing or has unsafe permissions')

    # Exercise the no-lease wake path on both runtimes. A unique prompt gives a
    # cheap, bounded result and the session is terminated after readback.
    for runtime in ('local', 'docker'):
        result = json.loads(run(ctl + ['run', '--provider', args.provider, '--model',
                                       ('claude-haiku-4-5' if args.provider == 'claude' else 'gpt-6-luna'),
                                       '--runtime', runtime, '--cwd', str(args.root),
                                       '--context', 'clean', '--permissions', 'unrestricted',
                                       '--approval-policy', 'never',
                                       f'Reply exactly PERSIST_WAKE_{runtime.upper()}. No tools.']))
        sid = result['session_id']
        run(ctl + ['wait', sid, '--until', 'turn_end,error', '--timeout', '150s'], timeout=170, codes=(10, 11, 12))
        run(ctl + ['grep', sid, f'PERSIST_WAKE_{runtime.upper()}'])
        run(ctl + ['terminate', sid])
        run(ctl + ['wait', sid, '--until', 'done', '--timeout', '30s'], timeout=40)

    # Auto-plan and approval prove PERSIST owns dispatch. Keep the prompt tiny;
    # expose the event stream through the PERSIST CLI and node lifecycle through
    # agentctl's plan-node references.
    proposal = run(persist + ['plan', 'auto',
                              'Create exactly two tiny tasks. A replies A_DONE. B depends on A and replies B_DONE.',
                              '--cwd', str(args.root), '--json'])
    proposal_obj = json.loads(proposal)
    proposed_nodes = proposal_obj.get('tasks', [])
    if (len(proposed_nodes) != 2 or not proposed_nodes[0].get('id')
            or proposed_nodes[0]['id'] not in proposed_nodes[1].get('depends_on', [])):
        raise RuntimeError('Auto planner did not return a two-node dependency DAG')
    proposal_id = proposal_obj.get('proposal_id')
    if not proposal_id:
        raise RuntimeError('PERSIST auto plan did not return proposal_id')
    receipt = json.loads(run(persist + ['plan', 'approve', proposal_id, '--json']))
    plan_id = receipt.get('plan_id')
    if not plan_id:
        raise RuntimeError('PERSIST plan approval did not return plan_id')
    run(persist + ['plan', 'get', plan_id])
    run(ctl + ['plan', 'show', plan_id])
    run(ctl + ['wait', f'plan:{plan_id}', '--until', 'node_started', '--timeout', '5m'], timeout=310, codes=(10,))
    events = run(persist + ['plan', 'watch', plan_id], timeout=300)
    if 'node_started' not in events:
        raise RuntimeError('PERSIST plan stream did not report node_started')
    plan_data = json.loads(run(persist + ['plan', 'get', plan_id]))
    nodes = plan_data.get('nodes', plan_data.get('tasks', []))
    for node in nodes:
        node_id = node.get('task_id') or node.get('id')
        if node_id:
            node_ref = f'plan:{plan_id}/{node_id}'
            run(ctl + ['wait', node_ref, '--until', 'done', '--timeout', '5m'], timeout=310, codes=(10, 13))
            run(ctl + ['since', node_ref])
            run(ctl + ['grep', node_ref, 'DONE'])

    # The PERSIST-owned task path dispatches provider work. Existing AgentD
    # approval handling is then exercised against a live pending request, with
    # a real filesystem effect (approve) and absence check (deny) performed by
    # the provider tools themselves; PERSIST audit is the independent readback.
    for decision, marker in (('approve', 'PERSIST_APPROVED'), ('deny', 'PERSIST_DENIED')):
        audit_before = run(persist + ['audit', 'list', '--limit', '100'])
        task = json.loads(run(persist + ['task', 'dispatch', '--title', f'{marker} approval check',
                                         '--content', f'Use the shell tool to run exactly `touch {marker}.txt` now. This action must request provider approval. If approved, wait for it to finish; if denied, stop without trying another command.',
                                         '--agent', args.provider, '--cwd', str(args.root),
                                         '--runtime', 'docker', '--json']))
        sid = task.get('session_id')
        if not sid:
            raise RuntimeError('PERSIST dispatch returned no AgentD session id')
        run(ctl + ['wait', sid, '--until', 'approval,error,turn_end', '--timeout', '180s'], timeout=190, codes=(10, 11, 12))
        approvals = json.loads(run(ctl + ['approvals', sid])).get('approvals', [])
        if not approvals:
            raise RuntimeError('No live pending AgentD approval was created')
        request_id = json.dumps(approvals[0]['request_id'])
        run(ctl + [decision, sid, request_id])
        run(ctl + ['wait', sid, '--until', 'turn_end,error', '--timeout', '180s'], timeout=190, codes=(10, 11, 12))
        marker_exists = (args.root / f'{marker}.txt').exists()
        if marker_exists != (decision == 'approve'):
            raise RuntimeError(f'Approval decision did not have the expected file effect for {decision}')
        audit = run(persist + ['audit', 'list', '--limit', '100'])
        if audit == audit_before:
            raise RuntimeError('PERSIST audit readback did not change after the approval decision')
        run(persist + ['task', 'get', task['id']])

    # A task with a 60 second foreground sleep provides a deterministic active
    # Docker turn for PERSIST steering, followed by an interrupt on a second run.
    long_prompt = 'Use the shell tool to run `sleep 60` as a foreground command now. Wait for it to finish, then reply LONG_TURN_FINISHED.'
    active = json.loads(run(persist + ['task', 'dispatch', '--title', 'Active Docker steer',
                                       '--content', long_prompt, '--agent', args.provider,
                                       '--cwd', str(args.root), '--runtime', 'docker', '--json']))
    task_id = active['id']
    sid = active['session_id']
    wait_for_tool(ctl, sid, run)
    run(persist + ['task', 'steer', task_id, 'Stop the sleep and reply STEERED.'])
    run(ctl + ['wait', sid, '--until', 'turn_end,error', '--timeout', '120s'], timeout=130, codes=(10, 11, 12))
    active2 = json.loads(run(persist + ['task', 'dispatch', '--title', 'Active Docker interrupt',
                                        '--content', long_prompt, '--agent', args.provider,
                                        '--cwd', str(args.root), '--runtime', 'docker', '--json']))
    wait_for_tool(ctl, active2['session_id'], run)
    run(ctl + ['interrupt', active2['session_id']])
    run(ctl + ['wait', active2['session_id'], '--until', 'turn_end,error', '--timeout', '90s'], timeout=100, codes=(10, 11, 12))
    print(f"PASS isolated PERSIST/AgentD flows ({args.provider}); private evidence: {args.root}")
    return 0


def wait_for_tool(ctl: list[str], session_id: str, run) -> None:
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        events = run(ctl + ['grep', session_id, 'tool.call'], codes=(0,))
        if 'tool.call' in events:
            return
        time.sleep(0.5)
    raise RuntimeError('AgentD did not emit a tool.call for the active Docker workload')


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"FAIL isolated PERSIST/AgentD flows: {exc}", file=sys.stderr)
        raise SystemExit(1)
