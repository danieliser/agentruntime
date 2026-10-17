import unittest
from typing import Any, Callable
from unittest.mock import MagicMock, Mock

from agentctl_live_test import LiveTest


class ActiveSteeringEvidenceTest(unittest.TestCase):
    def test_natural_workload_completion_cannot_pass_steering(self) -> None:
        test = self.fixture(workload_finished=True)
        with self.assertRaisesRegex(RuntimeError, "original steering workload completed"):
            test.flow()
        test.approvals_flow.assert_not_called()
        self.assertFalse(any(call.args[0] == "steer" for call in test.record.call_args_list))

    def test_cancelled_workload_and_replacement_output_pass_steering(self) -> None:
        test = self.fixture(workload_finished=False)
        with self.assertRaisesRegex(RuntimeError, "reached approvals"):
            test.flow()
        self.assertTrue(any(call.args[:2] == ("steer", True) for call in test.record.call_args_list))
        test.assistant_has.assert_any_call("session", "STEERED_ORBIT_9532", 20)

    def test_output_before_steer_does_not_qualify(self) -> None:
        test = self.fixture(workload_finished=False)
        test.assistant_has.side_effect = lambda sid, marker, after=0: after < 20
        with self.assertRaisesRegex(RuntimeError, "condition did not become true"):
            test.flow()
        test.approvals_flow.assert_not_called()

    @staticmethod
    def fixture(workload_finished: bool) -> LiveTest:
        # Exercise the actual acceptance flow without a daemon, Docker or API.
        test = object.__new__(LiveTest)
        test.work = MagicMock()
        test.work.__truediv__.side_effect = lambda name: Mock(
            exists=Mock(return_value=workload_finished if name == "steer.finished" else True))
        test.run = Mock(return_value="session")
        test.wait = Mock(return_value=Mock(returncode=10))
        test.value = Mock(return_value={"turn": "active"})
        test.ctl = Mock(return_value=Mock(stdout="events"))
        test.record = Mock()
        test.tail = Mock(side_effect=[10, 20])
        test.tool_started = Mock(return_value=True)
        test.assistant_has = Mock(return_value=True)
        test.events = Mock(return_value=[{"sequence": 21, "type": "turn.completed"}])

        def until(check: Callable[[], Any], **kwargs: Any) -> Any:
            result = check()
            if not result:
                raise RuntimeError("condition did not become true before deadline")
            return result

        test.until = until
        test.approvals_flow = Mock(side_effect=RuntimeError("reached approvals"))
        return test


if __name__ == "__main__":
    unittest.main()
