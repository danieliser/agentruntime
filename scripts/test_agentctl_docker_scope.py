import unittest
from agentctl_docker_scope import scoped_args


class DockerScopeTest(unittest.TestCase):
    def test_listings_never_include_other_daemons(self) -> None:
        self.assertEqual(scoped_args(["ps", "-aq", "--filter", "label=agentruntime.session_id"], "scope-1"),
                         ["ps", "--filter", "label=agentctl.live_test=scope-1", "-aq", "--filter", "label=agentruntime.session_id"])

    def test_spawn_labels_owned_container_and_pins_base_images(self) -> None:
        self.assertEqual(scoped_args(["run", "--rm", "agentruntime-agent-codex:latest"], "scope-1"),
                         ["run", "--label", "agentctl.live_test=scope-1", "--rm", "agentruntime-agent-codex:2.3.3-persist.10"])

    def test_network_and_proxy_names_are_isolated_but_dns_alias_is_stable(self) -> None:
        self.assertEqual(scoped_args(['network','connect','--alias','agentruntime-proxy','agentruntime-policy-v1','agentruntime-proxy'], 'scope-1'),
                         ['network','connect','--alias','agentruntime-proxy','agentruntime-policy-v1-scope-1','agentruntime-proxy-scope-1'])
        run = scoped_args(['run','--name','agentruntime-proxy','--network','agentruntime-agents','agentruntime-proxy:latest'], 'scope-1')
        self.assertIn('agentruntime-proxy-scope-1', run)
        self.assertIn('agentruntime-agents-scope-1', run)
        self.assertIn('--network-alias', run)

    def test_readiness_image_selection_uses_same_pinned_images(self) -> None:
        self.assertEqual(scoped_args(["image", "inspect", "agentruntime-proxy:latest"], "scope-1"),
                         ["image", "inspect", "agentruntime-proxy:2.3.3-persist.10"])


if __name__ == "__main__":
    unittest.main()
