import contextlib
import io
import json
import subprocess
import unittest
from unittest.mock import Mock, patch

import lab


class LabTests(unittest.TestCase):
    def target(self, runner=None):
        return lab.Lab(lab.CONTEXT, lab.NAMESPACE, lab.KUBECONFIG, runner=runner or Mock())

    def test_default_is_read_only_plan(self):
        with patch("lab.subprocess.run") as run, contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(lab.main([]), 0)
        run.assert_not_called()
        self.assertFalse(json.loads(output.getvalue())["changes"])

    def test_explicit_target_required(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            lab.main(["deploy"])

    def test_other_targets_rejected(self):
        for target in (("production", lab.NAMESPACE, lab.KUBECONFIG),
                       (lab.CONTEXT, "default", lab.KUBECONFIG),
                       (lab.CONTEXT, lab.NAMESPACE, "/tmp/other")):
            with self.subTest(target=target), self.assertRaises(lab.LabError):
                lab.Lab(*target)

    def test_all_commands_are_scoped(self):
        runner = Mock(return_value="{}")
        self.target(runner).kubectl("get", "pods")
        command = runner.call_args.args[0]
        self.assertIn(str(lab.KUBECONFIG), command)
        self.assertIn(lab.CONTEXT, command)
        self.assertIn(lab.NAMESPACE, command)

    def test_nonlocal_api_and_unmarked_namespace_rejected(self):
        for server, labels in (("https://cloud.example", {}), ("https://127.0.0.1:5555", {})):
            runner = Mock(side_effect=[json.dumps({"clusters": [{"cluster": {"server": server}}]}),
                                       json.dumps({"metadata": {"labels": labels}})])
            with self.assertRaises(lab.LabError):
                self.target(runner).guard()

    def test_missing_namespace_only_allowed_for_initial_deploy(self):
        response = json.dumps({"clusters": [{"cluster": {"server": "https://127.0.0.1:5555"}}]})
        self.target(Mock(side_effect=[response, ""])).guard(allow_missing_namespace=True)
        with self.assertRaises(lab.LabError):
            self.target(Mock(side_effect=[response, ""])).guard()

    def test_failed_migration_stops_application_rollout(self):
        target = self.target()
        target.guard = Mock()
        target.job = Mock(return_value=(False, "", "failed-migration"))
        with self.assertRaises(lab.LabError):
            target.deploy()
        calls = str(target.runner.call_args_list)
        self.assertNotIn("local/app", calls)
        self.assertNotIn("restart", calls)

    def test_missing_or_inconsistent_probe_output_fails(self):
        for passed, text in ((True, "{}"), (True, "bad"), (True, '{"status":"failed"}'),
                             (False, '{"status":"passed"}')):
            target = self.target()
            target.guard = Mock()
            target.job = Mock(return_value=(passed, text, "job-id"))
            with self.subTest(text=text), self.assertRaises(lab.LabError):
                target.probe("doctor")

    def test_arbitrary_probe_targets_rejected(self):
        target = self.target()
        target.guard = Mock()
        target.job = Mock()
        with self.assertRaises(lab.LabError):
            target.probe("doctor", target="metadata.internal")
        target.job.assert_not_called()

    def test_subprocess_timeout_is_bounded(self):
        with patch("lab.subprocess.run", side_effect=subprocess.TimeoutExpired("kubectl", 1)):
            with self.assertRaises(lab.LabError):
                lab.execute(["kubectl"], timeout=1)

    def test_subprocess_failure_does_not_echo_secrets(self):
        with patch("lab.subprocess.run", return_value=subprocess.CompletedProcess("kubectl", 1, "", "SECRET")):
            with self.assertRaises(lab.LabError) as error:
                lab.execute(["kubectl", "apply"])
        self.assertNotIn("SECRET", str(error.exception))


if __name__ == "__main__":
    unittest.main()
