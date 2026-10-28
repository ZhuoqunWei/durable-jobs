import pathlib
import subprocess
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import aws_local_check as runtime


class CleanupTests(unittest.TestCase):
    def test_failed_log_capture_still_cleans_up(self):
        effects = [subprocess.TimeoutExpired("logs", 30), subprocess.CompletedProcess([], 0)]
        command = ["docker", "compose", "-p", "synthetic-owned-project"]
        with patch.object(runtime, "run", side_effect=effects) as run:
            self.assertTrue(runtime.cleanup_stack(command, pathlib.Path("/unused"), {}))
        self.assertEqual(run.call_args.args[0], command + ["down", "--timeout", "10"])
        self.assertNotIn("--volumes", run.call_args.args[0])

    def test_failed_cleanup_is_not_reported_as_success(self):
        effects = [subprocess.CompletedProcess([], 0), subprocess.CompletedProcess([], 1)]
        with patch.object(runtime, "run", side_effect=effects):
            self.assertFalse(runtime.cleanup_stack(["compose"], pathlib.Path("/unused"), {}))

    def test_timed_out_cleanup_is_not_reported_as_success(self):
        effects = [subprocess.CompletedProcess([], 0), subprocess.TimeoutExpired("down", 60)]
        with patch.object(runtime, "run", side_effect=effects):
            self.assertFalse(runtime.cleanup_stack(["compose"], pathlib.Path("/unused"), {}))


if __name__ == "__main__":
    unittest.main()
