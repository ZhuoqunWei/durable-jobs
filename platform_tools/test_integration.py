import unittest
from unittest.mock import Mock

import integration
from lab import LabError


class FailureExerciseTests(unittest.TestCase):
    def target(self):
        target = Mock()
        target.kubectl.return_value = '{"spec":{"replicas":1}}'
        return target

    def test_worker_restored_when_verification_breaks(self):
        target = self.target()
        target.probe.side_effect = [
            {"status": "passed", "key": "baseline", "job_id": "id", "attempts": 1},
            {"status": "failed", "checks": [{"stage": "dns"}]},
            {"status": "failed", "checks": [{"stage": "tcp"}]},
            LabError("injected failure after worker pause")]
        with self.assertRaises(LabError):
            integration.run(target, [])
        calls = [call.args for call in target.kubectl.call_args_list]
        self.assertIn(("scale", "deployment/worker", "--replicas=0"), calls)
        self.assertIn(("scale", "deployment/worker", "--replicas=1"), calls)

    def test_customized_worker_count_is_not_changed(self):
        target = self.target()
        target.kubectl.return_value = '{"spec":{"replicas":3}}'
        with self.assertRaises(LabError):
            integration.run(target, [])
        self.assertEqual(target.kubectl.call_count, 1)

    def test_wrong_failure_stage_cannot_pass(self):
        with self.assertRaises(LabError):
            integration.expect({"status": "failed", "checks": [{"stage": "tcp"}]}, "failed", "dns")


if __name__ == "__main__":
    unittest.main()
