import contextlib
import copy
from datetime import datetime, timedelta, timezone
import io
import json
import os
import subprocess
import unittest
from unittest.mock import patch

import aws_preflight as preflight


NOW = datetime(2026, 9, 28, 12, tzinfo=timezone.utc)
ACCOUNT = "111122223333"
VERSION = "v0.0.0-eksbuild.1"  # Shape-only fixture, never a deployment pin.
CONFIG = {
    "account_id": ACCOUNT, "region": "us-west-2", "profile": "fixture-lab",
    "cluster_name": "durable-jobs-lab", "kubernetes_version": "1.35",
    "addon_versions": {k: VERSION for k in preflight.ADDONS},
    "state_bucket": "fixture-durable-jobs-state",
    "image": f"{ACCOUNT}.dkr.ecr.us-west-2.amazonaws.com/durable-jobs@sha256:" + "a" * 64,
    "certificate_arn": f"arn:aws:acm:us-west-2:{ACCOUNT}:certificate/00000000-0000-0000-0000-000000000001",
}


class FakeAWS:
    def __init__(self):
        self.calls = []
        self.change = lambda service, operation, data: data

    def read(self, service, operation, **params):
        self.calls.append((service, operation, params))
        if operation == "get-caller-identity":
            data = {"Account": ACCOUNT, "Arn": f"arn:aws:sts::{ACCOUNT}:assumed-role/fixture-role/fixture-session"}
        elif operation == "describe-cluster-versions":
            data = {"clusterVersions": [{"clusterVersion": "1.35", "versionStatus": "STANDARD_SUPPORT",
                                        "endOfStandardSupportDate": (NOW + timedelta(days=100)).isoformat(),
                                        "defaultPlatformVersion": "eks.1"}]}
        elif operation == "describe-addon-versions":
            data = {"addons": [{"addonName": params["addon-name"], "owner": "aws", "addonVersions": [
                {"addonVersion": VERSION, "architecture": ["amd64"], "computeTypes": ["ec2"],
                 "requiresConfiguration": False,
                 "compatibilities": [{"clusterVersion": "1.35", "platformVersions": ["*"]}]}]}]}
        elif operation == "describe-addon-configuration":
            service_account = next(sa for name, sa in preflight.ADDONS.values() if name == params["addon-name"])
            data = {"addonName": params["addon-name"], "addonVersion": VERSION,
                    "configurationSchema": '{"type":"object"}',
                    "podIdentityConfiguration": [{"serviceAccount": service_account}] if service_account else []}
        elif operation == "get-bucket-location":
            data = {"LocationConstraint": "us-west-2"}
        elif operation == "get-bucket-versioning":
            data = {"Status": "Enabled"}
        elif operation == "get-bucket-encryption":
            data = {"ServerSideEncryptionConfiguration": {"Rules": [{"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}}]}}
        elif operation == "get-public-access-block":
            data = {"PublicAccessBlockConfiguration": dict.fromkeys(preflight.PUBLIC_BLOCK, True)}
        elif operation == "describe-images":
            data = {"imageDetails": [{"registryId": ACCOUNT, "repositoryName": "durable-jobs", "imageDigest": "sha256:" + "a" * 64}]}
        elif operation == "describe-certificate":
            data = {"Certificate": {"CertificateArn": CONFIG["certificate_arn"], "Status": "ISSUED",
                                    "SubjectAlternativeNames": ["jobs.durable-jobs-lab.internal"],
                                    "NotBefore": (NOW - timedelta(days=1)).isoformat(),
                                    "NotAfter": (NOW + timedelta(days=30)).isoformat()}}
        elif operation == "get-role":
            data = {"Role": {"Arn": f"arn:aws:iam::{ACCOUNT}:role/aws-service-role/elasticloadbalancing.amazonaws.com/AWSServiceRoleForElasticLoadBalancing"}}
        else:
            raise AssertionError(f"Unexpected operation {operation}")
        return self.change(service, operation, data)


class PreflightTests(unittest.TestCase):
    def test_default_is_offline_and_reports_missing_inputs(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), patch.object(preflight, "AWSCLI") as client:
            self.assertEqual(preflight.main([]), 0)
        client.assert_not_called()
        result = json.loads(output.getvalue())
        self.assertEqual(result["mode"], "offline")
        self.assertFalse(result["deployment_ready"])
        self.assertIn("account_id", result["missing_inputs"])
        self.assertIn("addon_versions.vpc_cni", result["missing_inputs"])
        self.assertTrue(all(c["status"] == "unknown" for c in result["checks"].values()))

    def test_inspect_requires_explicit_complete_identity_before_any_call(self):
        with patch.object(preflight, "AWSCLI") as client, contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(preflight.main(["inspect"]), 2)
            self.assertEqual(preflight.main(["inspect", "--config", str(preflight.TEMPLATE)]), 2)
        client.assert_not_called()

    def test_rejects_unsafe_and_mismatched_config(self):
        for field, value in [("account_id", "123"), ("account_id", True), ("region", "us-gov-west-1"),
                             ("profile", "--endpoint-url"), ("state_bucket", "file:///tmp/state"),
                             ("kubernetes_version", "1.99"), ("cluster_name", "bad-"),
                             ("image", CONFIG["image"].replace(ACCOUNT, "999988887777")),
                             ("image", CONFIG["image"].replace("@sha256:" + "a" * 64, ":latest")),
                             ("certificate_arn", CONFIG["certificate_arn"].replace("us-west-2", "us-east-1"))]:
            with self.subTest(field=field, value=value):
                config = copy.deepcopy(CONFIG)
                config[field] = value
                with self.assertRaises(preflight.PreflightError):
                    preflight.validate_config(config)
        for pins in ({}, {**CONFIG["addon_versions"], "vpc_cni": "latest"}):
            with self.assertRaises(preflight.PreflightError):
                preflight.validate_config({**CONFIG, "addon_versions": pins})
        with self.assertRaises(preflight.PreflightError):
            preflight.validate_config({**CONFIG, "password": "must-not-log"})

    def test_rejects_duplicate_json_keys(self):
        with self.assertRaises(preflight.PreflightError):
            json.loads('{"account_id":"111122223333","account_id":"999988887777"}', object_pairs_hook=preflight.unique_keys)

    def test_success_is_metadata_only_with_manual_gates_remaining(self):
        reader = FakeAWS()
        report = preflight.inspect(CONFIG, reader, NOW)
        self.assertEqual(report["status"], "metadata_checks_passed")
        self.assertEqual(len(report["checks"]), 11)
        self.assertFalse(report["deployment_ready"])
        self.assertIn("authorization", report["manual_prerequisites"])
        self.assertIn("state_locking", report["manual_prerequisites"])
        self.assertEqual(reader.calls[0][:2], ("sts", "get-caller-identity"))
        self.assertTrue(all((s, op) in preflight.OPS for s, op, _ in reader.calls))
        for service, _, params in reader.calls:
            if service == "s3api":
                self.assertEqual(params["expected-bucket-owner"], ACCOUNT)

    def test_wrong_account_or_unapproved_identity_stops_all_other_reads(self):
        for replacement in ({"Account": "999988887777"}, {"Arn": f"arn:aws:iam::{ACCOUNT}:root"},
                            {"Arn": f"arn:aws:iam::{ACCOUNT}:user/fixture"}):
            reader = FakeAWS()
            reader.change = lambda s, op, d: {**d, **replacement}
            report = preflight.inspect(CONFIG, reader, NOW)
            self.assertEqual(report["status"], "blocked")
            self.assertEqual(len(reader.calls), 1)
            self.assertEqual(report["checks"]["state_bucket"]["status"], "unknown")

    def test_unselected_resources_and_versions_are_not_passed_or_auto_selected(self):
        config = {**CONFIG, "state_bucket": None, "image": None, "certificate_arn": None,
                  "addon_versions": dict.fromkeys(preflight.ADDONS)}
        original = copy.deepcopy(config)
        reader = FakeAWS()
        report = preflight.inspect(config, reader, NOW)
        self.assertEqual(report["status"], "incomplete")
        self.assertEqual(report["checks"]["addon.vpc_cni"]["compatible_candidates"], [VERSION])
        self.assertEqual(report["checks"]["addon.vpc_cni"]["status"], "unknown")
        self.assertEqual(config, original)
        self.assertTrue(all(s in ("sts", "eks", "iam") for s, _, _ in reader.calls))
        self.assertFalse(any(op == "describe-addon-configuration" for _, op, _ in reader.calls))

    def test_incompatible_addon_or_required_configuration_cannot_pass(self):
        mutations = {"architecture": ["arm64"], "computeTypes": ["fargate"], "requiresConfiguration": True,
                     "compatibilities": [{"clusterVersion": "1.34", "platformVersions": ["*"]}]}
        for field, value in mutations.items():
            reader = FakeAWS()
            def change(s, op, data):
                if op == "describe-addon-versions":
                    data["addons"][0]["addonVersions"][0][field] = value
                return data
            reader.change = change
            report = preflight.inspect(CONFIG, reader, NOW)
            self.assertEqual(report["checks"]["addon.vpc_cni"]["status"], "fail")
            self.assertFalse(any(op == "describe-addon-configuration" for _, op, _ in reader.calls))

    def test_platform_restrictions_and_missing_pod_identity_support(self):
        for operation in ("describe-addon-versions", "describe-addon-configuration"):
            reader = FakeAWS()
            def change(s, op, data):
                if op == operation == "describe-addon-versions":
                    data["addons"][0]["addonVersions"][0]["compatibilities"][0]["platformVersions"] = ["eks.99"]
                if op == operation == "describe-addon-configuration":
                    data["podIdentityConfiguration"] = []
                return data
            reader.change = change
            report = preflight.inspect(CONFIG, reader, NOW)
            self.assertEqual(report["checks"]["addon.vpc_cni"]["status"], "fail")
            self.assertEqual(report["checks"]["addon.ebs_csi"]["status"], "fail")

    def test_old_support_does_not_trigger_addon_reads(self):
        reader = FakeAWS()
        def change(s, op, data):
            if op == "describe-cluster-versions":
                data["clusterVersions"][0]["versionStatus"] = "EXTENDED_SUPPORT"
            return data
        reader.change = change
        report = preflight.inspect(CONFIG, reader, NOW)
        self.assertEqual(report["checks"]["eks_version"]["status"], "fail")
        self.assertFalse(any(op.startswith("describe-addon") for _, op, _ in reader.calls))

    def test_storage_image_and_certificate_regressions(self):
        mutations = [
            ("get-bucket-location", "state_bucket", lambda d: d.update(LocationConstraint="us-east-1")),
            ("get-bucket-versioning", "state_bucket", lambda d: d.update(Status="Suspended")),
            ("get-bucket-encryption", "state_bucket", lambda d: d["ServerSideEncryptionConfiguration"].update(Rules=[])),
            ("get-public-access-block", "state_bucket", lambda d: d["PublicAccessBlockConfiguration"].update(BlockPublicPolicy=1)),
            ("describe-images", "image", lambda d: d["imageDetails"][0].update(imageDigest="sha256:" + "b" * 64)),
            ("describe-certificate", "certificate", lambda d: d["Certificate"].update(Status="EXPIRED")),
            ("describe-certificate", "certificate", lambda d: d["Certificate"].update(SubjectAlternativeNames=["wrong.internal"])),
            ("describe-certificate", "certificate", lambda d: d["Certificate"].update(NotAfter=NOW.timestamp())),
            ("describe-certificate", "certificate", lambda d: d["Certificate"].update(NotBefore=(NOW + timedelta(days=1)).isoformat())),
            ("get-role", "elb_role", lambda d: d["Role"].update(Arn="arn:aws:iam::999988887777:role/wrong")),
        ]
        for operation, key, mutate in mutations:
            with self.subTest(operation=operation, mutate=mutate):
                reader = FakeAWS()
                def change(s, op, data):
                    if op == operation:
                        mutate(data)
                    return data
                reader.change = change
                report = preflight.inspect(CONFIG, reader, NOW)
                self.assertEqual(report["checks"][key]["status"], "fail")
                self.assertEqual(report["status"], "blocked")

    def test_malformed_responses_are_reported_not_assumed_success(self):
        reader = FakeAWS()
        reader.change = lambda s, op, d: d if op == "get-caller-identity" else {}
        report = preflight.inspect(CONFIG, reader, NOW)
        self.assertEqual(report["status"], "blocked")
        self.assertEqual(report["checks"]["state_bucket"]["status"], "fail")


class AWSCLITests(unittest.TestCase):
    def test_explicit_profile_region_and_safe_environment(self):
        fake = subprocess.CompletedProcess([], 0, '{"Account":"111122223333"}', "")
        with patch.dict(os.environ, {"AWS_ACCESS_KEY_ID": "do-not-copy", "AWS_ENDPOINT_URL": "https://untrusted.invalid", "AWS_CONFIG_FILE": "/tmp/not-approved"}), patch.object(preflight.subprocess, "run", return_value=fake) as run:
            preflight.AWSCLI(CONFIG).read("sts", "get-caller-identity")
        command, = run.call_args.args
        options = run.call_args.kwargs
        self.assertEqual(command[:3], ["aws", "sts", "get-caller-identity"])
        self.assertIn("fixture-lab", command)
        self.assertIn("us-west-2", command)
        self.assertNotIn("--no-paginate", command)
        self.assertLessEqual(options["timeout"], 30)
        self.assertFalse(options.get("shell", False))
        self.assertNotIn("AWS_ACCESS_KEY_ID", options["env"])
        self.assertNotIn("AWS_CONFIG_FILE", options["env"])
        self.assertNotIn("AWS_ENDPOINT_URL", options["env"])
        self.assertEqual(options["env"]["AWS_IGNORE_CONFIGURED_ENDPOINT_URLS"], "true")
        self.assertEqual(options["env"]["AWS_EC2_METADATA_DISABLED"], "true")

    def test_mutations_extra_flags_and_file_inputs_are_rejected_before_subprocess(self):
        for service, operation, params in [("eks", "create-cluster", {}), ("s3api", "delete-bucket", {}),
                                            ("sts", "get-caller-identity", {"endpoint-url": "https://untrusted.invalid"}),
                                            ("iam", "get-role", {"role-name": "file:///tmp/secret"})]:
            with patch.object(preflight.subprocess, "run") as run, self.assertRaises(preflight.PreflightError):
                preflight.AWSCLI(CONFIG).read(service, operation, **params)
            run.assert_not_called()

    def test_errors_and_partial_pages_fail_without_raw_output(self):
        results = [subprocess.CompletedProcess([], 1, "private-stdout", "private-stderr"),
                   subprocess.CompletedProcess([], 0, "private-invalid-json", ""),
                   subprocess.CompletedProcess([], 0, '{"NextToken":"private-token"}', ""),
                   subprocess.CompletedProcess([], 0, '[]', "")]
        for result in results:
            with patch.object(preflight.subprocess, "run", return_value=result), self.assertRaises(preflight.PreflightError) as error:
                preflight.AWSCLI(CONFIG).read("sts", "get-caller-identity")
            self.assertNotIn("private-", str(error.exception))
        with patch.object(preflight.subprocess, "run", side_effect=subprocess.TimeoutExpired("private-command", 30)), self.assertRaises(preflight.PreflightError) as error:
            preflight.AWSCLI(CONFIG).read("sts", "get-caller-identity")
        self.assertNotIn("private-command", str(error.exception))

    def test_global_deadline_prevents_more_calls(self):
        client = preflight.AWSCLI(CONFIG)
        client.deadline = 0
        with patch.object(preflight.subprocess, "run") as run, self.assertRaises(preflight.PreflightError):
            client.read("sts", "get-caller-identity")
        run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
