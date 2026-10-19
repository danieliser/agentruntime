from pathlib import Path
import unittest
from agentctl_observability_bootstrap import isolated_env

class BootstrapTest(unittest.TestCase):
    def test_env_uses_supported_port_and_private_token(self) -> None:
        env = isolated_env(Path('/isolated'), 18091, {'AGENTD_URL': 'http://localhost:38093', 'AGENTD_TOKEN_FILE': '/live', 'NODE_OPTIONS':'--require=/live/extension.cjs'})
        self.assertEqual(env['AGENTD_URL'], 'http://127.0.0.1:18091')
        self.assertEqual(env['AGENTD_TOKEN_FILE'], '/isolated/agentd-data/auth.token')
        self.assertEqual(env['AGENTCTL_CURSOR_DIR'], '/isolated/cursors')
        self.assertNotIn('NODE_OPTIONS', env)

    def test_live_ports_rejected(self) -> None:
        for port in (38093, 0, 65536):
            with self.assertRaisesRegex(RuntimeError, 'forbidden'):
                isolated_env(Path('/isolated'), port, {})


class ConnectionEvidenceTest(unittest.TestCase):
    def test_forbidden_destination_detection(self) -> None:
        from agentctl_observability_bootstrap import forbidden_connections
        self.assertTrue(forbidden_connections('p12\nn127.0.0.1:59112->127.0.0.1:38093\n'))
        self.assertFalse(forbidden_connections('p12\nn127.0.0.1:59112->127.0.0.1:19001\n'))

if __name__ == '__main__': unittest.main()

class RestartTest(unittest.TestCase):
    def test_restart_only_replaces_the_supplied_owned_process(self) -> None:
        from unittest.mock import Mock, patch
        from agentctl_observability_bootstrap import restart_owned_agentd
        old = Mock()
        with patch('agentctl_observability_bootstrap.os.killpg') as kill, patch('agentctl_observability_bootstrap.subprocess.Popen') as launch:
            result = restart_owned_agentd(old, ['isolated-agentd'], Path('/private/work'), {}, 'log')
            kill.assert_called_once_with(old.pid, __import__('signal').SIGTERM)
            old.wait.assert_called_once_with(timeout=30)
            launch.assert_called_once_with(['isolated-agentd'], cwd=Path('/private/work'), env={}, stdout='log', stderr=-2, start_new_session=True)
            self.assertIs(result, launch.return_value)

class CredentialSelectionTest(unittest.TestCase):
    def test_private_home_uses_exact_native_keychain_account(self) -> None:
        from unittest.mock import Mock, patch
        from agentctl_observability_bootstrap import private_claude_credentials
        with patch('agentctl_observability_bootstrap.getpass.getuser', return_value='native-user'), patch('agentctl_observability_bootstrap.subprocess.run', return_value=Mock(stdout='fixture\n')) as run:
            self.assertEqual(private_claude_credentials(), 'fixture')
            run.assert_called_once_with(['security', 'find-generic-password', '-a', 'native-user', '-s', 'Claude Code-credentials', '-w'], capture_output=True, text=True, check=True, timeout=10)
