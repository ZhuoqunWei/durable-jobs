import contextlib
import copy
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

import release_demo as demo
from lab import LabError


class ReleaseDemoTests(unittest.TestCase):
    def test_default_is_plan_without_external_calls(self):
        with patch.object(demo, "run_demo") as run, contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(demo.main([]), 0)
        run.assert_not_called()
        self.assertFalse(json.loads(out.getvalue())["changes"])

    def cluster(self, runner=None):
        return demo.DisposableCluster(demo.ARTIFACTS / "run-012345abcdef", runner or Mock(return_value=""))

    def test_other_directories_rejected(self):
        for path in (Path("/tmp/run-012345abcdef"), demo.ARTIFACTS / "run-other", demo.ROOT):
            with self.subTest(path=path), self.assertRaises(LabError):
                demo.DisposableCluster(path, Mock())

    def test_remote_docker_rejected_before_builds(self):
        with patch.dict(demo.os.environ, {"DOCKER_HOST": "tcp://remote.example:2375", "DOCKER_CONTEXT": ""}):
            runner = Mock()
            with self.assertRaises(LabError):
                demo.require_local_docker(runner)
            runner.assert_not_called()

    def test_explicit_context_takes_precedence_over_host(self):
        runner = Mock(return_value=json.dumps([{"Endpoints": {"docker": {"Host": "ssh://remote.example"}}}]))
        with patch.dict(demo.os.environ, {"DOCKER_HOST": "unix:///local", "DOCKER_CONTEXT": "remote"}):
            with self.assertRaises(LabError):
                demo.require_local_docker(runner)
            runner.assert_called_once()

    def test_existing_cluster_is_never_adopted(self):
        runner = Mock(return_value="preexisting-node")
        with self.assertRaises(LabError):
            self.cluster(runner).create()
        self.assertEqual(runner.call_count, 1)

    def test_changed_node_identity_prevents_mutation_and_cleanup(self):
        for operation in ("guard", "cleanup"):
            runner = Mock(return_value="replacement-node")
            cluster = self.cluster(runner)
            cluster.node_ids = ["original-node"]
            with self.assertRaises(LabError):
                getattr(cluster, operation)()
            self.assertNotIn("delete", str(runner.call_args_list))

    def test_uncreated_cluster_is_not_deleted(self):
        cluster = self.cluster()
        self.assertEqual(cluster.cleanup()["status"], "not_created")
        cluster.run.assert_not_called()

    def test_cleanup_requires_fresh_absence(self):
        for remaining in ("", "node-id"):
            runner = Mock(side_effect=["node-id", "", remaining])
            cluster = self.cluster(runner)
            cluster.node_ids = ["node-id"]
            if remaining:
                with self.assertRaises(LabError):
                    cluster.cleanup()
            else:
                self.assertTrue(cluster.cleanup()["fresh_absence_check"])
            delete = runner.call_args_list[1].args[0]
            self.assertEqual(delete[1:5], ["delete", "cluster", "--name", cluster.name])
            self.assertIn(str(cluster.kubeconfig), delete)

    def test_target_guard_keeps_loopback_restriction(self):
        cluster = self.cluster()
        cluster.node_ids = ["node-id"]
        cluster.run = Mock(side_effect=["node-id", "node-id", json.dumps({"clusters": [
            {"cluster": {"server": "https://production.example"}}]})])
        target = demo.DemoLab(cluster, "app:run", "probe:run")
        with self.assertRaises(LabError):
            target.guard()
        self.assertIn(str(cluster.kubeconfig), target.prefix)
        self.assertIn(cluster.context, target.prefix)

    def test_subprocess_does_not_inherit_application_or_aws_credentials(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(demo.os.environ, {
            "AWS_SECRET_ACCESS_KEY": "sensitive", "DATABASE_URL": "sensitive", "PGPASSWORD": "sensitive",
            "KUBECONFIG": "production"}), patch.object(demo.subprocess, "run", return_value=
                subprocess.CompletedProcess([], 0, "ok", "")) as run:
            demo.Commands(Path(directory))(["test-command"])
            env = run.call_args.kwargs["env"]
            self.assertNotIn("AWS_SECRET_ACCESS_KEY", env)
            self.assertNotIn("DATABASE_URL", env)
            self.assertNotIn("PGPASSWORD", env)
            self.assertNotIn("KUBECONFIG", env)
            self.assertEqual(env["KIND_EXPERIMENTAL_PROVIDER"], "docker")

    def evidence(self):
        before = {"id": "job-id", "status": "running", "attempts": 1}
        after = {"id": "job-id", "status": "succeeded", "attempts": 2, "completed_at": "now",
                 "result": {"campaign_id": "phase13_crash", "category_id": "animals",
                            "created_date": "2026-08-31", "schema_version": 1}}
        history = {"job_id": "job-id", "attempts": [
            {"number": 1, "status": "lease_expired", "finished_at": "then"},
            {"number": 2, "status": "succeeded", "finished_at": "now"}]}
        return before, after, history, copy.deepcopy(after)

    def test_valid_recovery_evidence(self):
        demo.recovery_evidence(*self.evidence())

    def test_wrong_job_missing_history_and_extra_attempts_fail(self):
        for defect in ("new-job", "no-claim", "extra-attempt", "empty-history", "null-history", "wrong-replay", "wrong-output"):
            before, after, history, replay = self.evidence()
            if defect == "new-job": after["id"] = "another-job"
            if defect == "no-claim": before["status"] = "queued"
            if defect == "extra-attempt": after["attempts"] = 3
            if defect == "empty-history": history["attempts"] = []
            if defect == "null-history": history["attempts"] = None
            if defect == "wrong-replay": replay["completed_at"] = "later"
            if defect == "wrong-output": after["result"] = {}
            with self.subTest(defect=defect), self.assertRaises(LabError):
                demo.recovery_evidence(before, after, history, replay)

    def test_cleanup_failure_cannot_report_success(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(demo, "Commands", return_value=Mock(side_effect=LabError("build failed"))), patch.object(demo, "DisposableCluster") as constructor:
            constructor.return_value.name = "test"
            constructor.return_value.cleanup.side_effect = LabError("cleanup failed")
            report = demo.run_demo(Path(directory))
            self.assertEqual(report["status"], "failed")
            self.assertEqual(report["cleanup"]["status"], "failed")
            self.assertTrue((Path(directory) / "report.json").is_file())


if __name__ == "__main__":
    unittest.main()
