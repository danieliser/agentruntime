import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from agentctl_persist_live_test import LiveTest

class PlanStreamReadinessTest(unittest.TestCase):
    def test_slow_cli_startup_does_not_fail_durable_replay(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).resolve().parents[1] / '.agentctl-live-test') as directory:
            from types import SimpleNamespace
            test = object.__new__(LiveTest)
            test.args = SimpleNamespace(root=Path(directory))
            test.log = Path(directory) / 'commands.jsonl'
            test.secrets = []
            code = 'import json,time;time.sleep(4.2);[print(json.dumps({"type":t}),flush=True) for t in ["node_started","node_completed","plan_status_changed"]];time.sleep(60)'
            test.persist = [sys.executable, '-c', code]
            test.watch_plan('plan-fixture')
            self.assertIn('node_completed', (Path(directory) / 'plan-watch.jsonl').read_text())

if __name__ == '__main__': unittest.main()
