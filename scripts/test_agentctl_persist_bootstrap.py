import os
from pathlib import Path
import tempfile
import unittest
from agentctl_persist_bootstrap import resolve_node, isolated_env

class BootstrapTest(unittest.TestCase):
    def test_pin_wins_over_path_and_default(self) -> None:
        with tempfile.TemporaryDirectory(dir=Path(__file__).resolve().parents[1] / '.agentctl-live-test') as tmp:
            root = Path(tmp)
            (root / '.node-version').write_text('25\n')
            node = root / 'nvm/versions/node/v25.9.0/bin/node'
            node.parent.mkdir(parents=True)
            node.write_text('#!/bin/sh\necho v25.9.0\n')
            node.chmod(0o700)
            self.assertEqual(resolve_node(root, {'NVM_DIR': str(root / 'nvm'), 'PATH': '/usr/bin'}), str(node))

    def test_missing_pin_fails_without_falling_back(self) -> None:
        with tempfile.TemporaryDirectory(dir=Path(__file__).resolve().parents[1] / '.agentctl-live-test') as tmp:
            root = Path(tmp)
            (root / '.nvmrc').write_text('999')
            with self.assertRaisesRegex(RuntimeError, 'Node 999'):
                resolve_node(root, {'NVM_DIR': str(root / 'nvm'), 'PATH': '/usr/bin'})

    def test_env_uses_supported_port_and_private_token(self) -> None:
        env = isolated_env(Path('/isolated'), 18803, 18091, {'PERSIST_AUTH_TOKEN': 'live', 'PERSIST_V3_TOKEN_FILE': '/live', 'PERSIST_HTTP__PORT': '8803'})
        self.assertNotIn('PERSIST_AUTH_TOKEN', env)
        self.assertEqual(env['PERSIST_HTTP__PORT'], '18803')
        self.assertEqual(env['PERSIST_AGENTRUNTIME__CLEAN_CONTEXT'], 'true')
        self.assertEqual(env['PERSIST_TOKEN_FILE'], '/isolated/persist-data/initial_token.txt')
        self.assertEqual(env['PERSIST_V3_TOKEN_FILE'], env['PERSIST_TOKEN_FILE'])

    def test_live_ports_rejected(self) -> None:
        for p, a in [(8803, 18091), (18803, 38093)]:
            with self.assertRaisesRegex(RuntimeError, 'forbidden'):
                isolated_env(Path('/isolated'), p, a, {})


class ConnectionEvidenceTest(unittest.TestCase):
    def test_forbidden_destination_detection(self) -> None:
        from agentctl_persist_bootstrap import forbidden_connections
        self.assertTrue(forbidden_connections('p12\nn127.0.0.1:59112->127.0.0.1:8803\n'))
        self.assertTrue(forbidden_connections('p12\nn127.0.0.1:59112->127.0.0.1:38093\n'))
        self.assertFalse(forbidden_connections('p12\nn127.0.0.1:59112->127.0.0.1:18803\n'))

if __name__ == '__main__': unittest.main()

class RestartTest(unittest.TestCase):
    def test_restart_only_replaces_the_supplied_owned_process(self) -> None:
        from unittest.mock import Mock, patch
        from agentctl_persist_bootstrap import restart_owned_agentd
        old = Mock()
        with patch('agentctl_persist_bootstrap.os.killpg') as kill, patch('agentctl_persist_bootstrap.subprocess.Popen') as launch:
            result = restart_owned_agentd(old, ['isolated-agentd'], Path('/private/work'), {}, 'log')
            kill.assert_called_once_with(old.pid, __import__('signal').SIGTERM)
            old.wait.assert_called_once_with(timeout=30)
            launch.assert_called_once_with(['isolated-agentd'], cwd=Path('/private/work'), env={}, stdout='log', stderr=-2, start_new_session=True)
            self.assertIs(result, launch.return_value)
