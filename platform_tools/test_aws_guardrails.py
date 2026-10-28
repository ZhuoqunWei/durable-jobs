from copy import deepcopy
from datetime import datetime, timedelta, timezone
import unittest

import aws_guardrails as guards
from aws_cleanup_canary import cleanup

ACCOUNT = "123456789012"
BUCKET = f"durable-jobs-state-{ACCOUNT}-us-east-1"
NOW = datetime(2026, 9, 28, 22, tzinfo=timezone.utc)


class GuardrailsTest(unittest.TestCase):
    def test_budget_separates_short_window_from_month(self):
        result = guards.budget_request(ACCOUNT, "owner@example.com", NOW)
        budget = result["Budget"]
        self.assertEqual(budget["BudgetLimit"], {"Amount": "50", "Unit": "USD"})
        self.assertEqual(budget["TimeUnit"], "CUSTOM")
        self.assertEqual(budget["TimePeriod"], {"Start": "2026-09-28T00:00:00+00:00", "End": "2026-09-30T00:00:00+00:00"})
        self.assertFalse(budget["CostTypes"]["IncludeCredit"])
        self.assertNotIn("CostFilters", budget)
        self.assertEqual([n["Notification"]["Threshold"] for n in result["NotificationsWithSubscribers"]], [5, 10, 25, 40, 50])
        self.assertTrue(all(n["Notification"]["ThresholdType"] == "ABSOLUTE_VALUE" for n in result["NotificationsWithSubscribers"]))

    def test_rejects_invalid_configuration(self):
        for args in [("bad", "us-east-1", BUCKET), (ACCOUNT, "us-west-2", BUCKET), (ACCOUNT, "us-east-1", "unrelated")]:
            with self.assertRaises(ValueError):
                guards.validate(*args)
        with self.assertRaises(ValueError):
            guards.budget_request(ACCOUNT, "invalid", NOW)
        with self.assertRaises(ValueError):
            guards.budget_request(ACCOUNT, "owner@example.com", NOW.replace(tzinfo=None))

    def test_state_delete_permissions_only_cover_exact_locks(self):
        statements = guards.operator_policy(ACCOUNT, "us-east-1", BUCKET)["Statement"]
        deletes = [s for s in statements if "s3:DeleteObject" in s["Action"]]
        self.assertEqual(len(deletes), 1)
        self.assertEqual(deletes[0]["Resource"], [f"arn:aws:s3:::{BUCKET}/{k}.tflock" for k in guards.STATE_KEYS])
        self.assertFalse(any("iam:PassRole" in s["Action"] for s in statements))
        self.assertFalse(any(s["Action"] == "*" for s in statements))

    def test_trust_is_single_same_account_user(self):
        arn = f"arn:aws:iam::{ACCOUNT}:user/lab-owner"
        self.assertEqual(guards.operator_trust(ACCOUNT, arn)["Statement"][0]["Principal"], {"AWS": arn})
        for bad in ("*", f"arn:aws:iam::{ACCOUNT}:root", "arn:aws:iam::999999999999:user/other"):
            with self.assertRaises(ValueError):
                guards.operator_trust(ACCOUNT, bad)

    def test_bucket_policy_and_backend_do_not_contain_credentials(self):
        self.assertEqual(guards.bucket_policy(BUCKET)["Statement"][0]["Effect"], "Deny")
        self.assertTrue(all(guards.PUBLIC_BLOCK.values()))
        backend = guards.backend_config(ACCOUNT, BUCKET)
        self.assertTrue(backend["encrypt"] and backend["use_lockfile"])
        self.assertEqual(backend["allowed_account_ids"], [ACCOUNT])
        self.assertNotIn("access_key", backend)


class FakeEC2:
    def __init__(self, address):
        self.address = address
        self.released = []
        self.error = None

    def describe_addresses(self, **params):
        if self.error:
            raise self.error
        return {"Addresses": [self.address]}

    def release_address(self, **params):
        self.released.append(params["AllocationId"])


class CanaryTest(unittest.TestCase):
    def setUp(self):
        self.config = {"account": ACCOUNT, "region": "us-east-1", "allocation_id": "eipalloc-0123456789abcdef0",
                       "run_id": "canary-20260928T215800Z", "not_before": "2026-09-28T21:59:00Z"}
        self.address = {"AllocationId": self.config["allocation_id"], "Domain": "vpc", "Tags": [
            {"Key": "Project", "Value": "durable-jobs-lab"}, {"Key": "Purpose", "Value": "cleanup-canary"},
            {"Key": "LabRun", "Value": self.config["run_id"]}]}
        self.ec2 = FakeEC2(self.address)

    def test_releases_exact_unused_tagged_address_after_expiry(self):
        self.assertEqual(cleanup(self.config, ACCOUNT, self.ec2, NOW)["status"], "release_requested")
        self.assertEqual(self.ec2.released, [self.config["allocation_id"]])

    def test_wrong_account_or_early_invocation_never_deletes(self):
        for account, now in [("999999999999", NOW), (ACCOUNT, NOW - timedelta(hours=1))]:
            with self.assertRaises(ValueError):
                cleanup(self.config, account, self.ec2, now)
        self.assertEqual(self.ec2.released, [])

    def test_foreign_missing_tags_or_in_use_address_never_deleted(self):
        for field, value in [("Tags", []), ("AssociationId", "eipassoc-123"), ("NetworkInterfaceId", "eni-123"),
                             ("InstanceId", "i-123"), ("Domain", "standard"), ("AllocationId", "eipalloc-11111111111111111")]:
            ec2 = FakeEC2({**deepcopy(self.address), field: value})
            with self.assertRaises(ValueError):
                cleanup(self.config, ACCOUNT, ec2, NOW)
            self.assertEqual(ec2.released, [])
        foreign = deepcopy(self.address)
        foreign["Tags"][2]["Value"] = "canary-other"
        with self.assertRaises(ValueError):
            cleanup(self.config, ACCOUNT, FakeEC2(foreign), NOW)

    def test_only_not_found_is_idempotent_success(self):
        for code in ["InvalidAllocationID.NotFound", "AccessDenied", "AuthFailure"]:
            error = RuntimeError("AWS failure")
            error.response = {"Error": {"Code": code}}
            self.ec2.error = error
            if code == "InvalidAllocationID.NotFound":
                self.assertEqual(cleanup(self.config, ACCOUNT, self.ec2, NOW)["status"], "already_absent")
            else:
                with self.assertRaises(RuntimeError):
                    cleanup(self.config, ACCOUNT, self.ec2, NOW)
        self.assertEqual(self.ec2.released, [])

    def test_rejects_ambiguous_or_malformed_target(self):
        for change in ({"allocation_id": "*"}, {"region": "us-west-2"}, {"run_id": "other"},
                       {"not_before": "2026-09-28T21:59:00"}, {"extra": "field"}):
            with self.assertRaises(ValueError):
                cleanup({**self.config, **change}, ACCOUNT, self.ec2, NOW)
        self.assertEqual(self.ec2.released, [])


if __name__ == "__main__":
    unittest.main()
