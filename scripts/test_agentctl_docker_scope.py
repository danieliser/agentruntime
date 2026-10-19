import unittest
from unittest.mock import patch
from agentctl_docker_scope import scoped_args


@patch.dict("os.environ", {"AGENTCTL_LIVE_IMAGE_TAG":"test-base"})
class DockerScopeTest(unittest.TestCase):
    def test_listings_never_include_other_daemons(self) -> None:
        self.assertEqual(scoped_args(["ps", "-aq", "--filter", "label=agentruntime.session_id"], "scope-1"),
                         ["ps", "--filter", "label=agentctl.live_test=scope-1", "-aq", "--filter", "label=agentruntime.session_id"])

    def test_spawn_labels_owned_container_and_pins_base_images(self) -> None:
        self.assertEqual(scoped_args(["run", "--rm", "agentruntime-agent-codex:latest"], "scope-1"),
                         ["run", "--label", "agentctl.live_test=scope-1", "--rm", "agentruntime-agent-codex:test-base"])

    def test_network_and_proxy_names_are_isolated_but_dns_alias_is_stable(self) -> None:
        self.assertEqual(scoped_args(['network','connect','--alias','agentruntime-proxy','agentruntime-policy-v1','agentruntime-proxy'], 'scope-1'),
                         ['network','connect','--alias','agentruntime-proxy','agentruntime-policy-v1-scope-1','agentruntime-proxy-scope-1'])
        run = scoped_args(['run','--name','agentruntime-proxy','--network','agentruntime-agents','agentruntime-proxy:latest'], 'scope-1')
        self.assertIn('agentruntime-proxy-scope-1', run)
        self.assertIn('agentruntime-agents-scope-1', run)
        self.assertIn('--network-alias', run)

    def test_readiness_image_selection_uses_same_pinned_images(self) -> None:
        self.assertEqual(scoped_args(["image", "inspect", "agentruntime-proxy:latest"], "scope-1"),
                         ["image", "inspect", "agentruntime-proxy:test-base"])


if __name__ == "__main__":
    unittest.main()

class VolumeScopeTest(unittest.TestCase):
    def test_created_volume_has_test_ownership_label(self) -> None:
        self.assertEqual(scoped_args(['volume', 'create', 'test-volume'], 'scope-1'),
                         ['volume', 'create', '--label', 'agentctl.live_test=scope-1', 'test-volume'])

class DefaultImagePinTest(unittest.TestCase):
    def test_default_uses_existing_generic_release_tag(self) -> None:
        with patch.dict('os.environ', {}, clear=True):
            for name in ('agentruntime-agent', 'agentruntime-agent-claude', 'agentruntime-agent-codex', 'agentruntime-proxy'):
                self.assertEqual(scoped_args(['image', 'inspect', name + ':latest'], 'scope-1'),
                                 ['image', 'inspect', name + ':2.3.3'])
