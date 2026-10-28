from datetime import datetime, timedelta, timezone
import unittest
from aws_lab_cleanup import owned, validate, reconcile


class Fake:
    def __init__(self, **responses):
        self.responses = responses
        self.writes = []

    def can_paginate(self, operation):
        return False

    def __getattr__(self, method):
        def call(**params):
            if method in self.responses:
                value = self.responses[method]
                if isinstance(value, Exception):
                    raise value
                return value
            if method.startswith(("delete_", "terminate_", "release_", "change_")):
                self.writes.append((method, params))
                return {}
            raise AssertionError("Unexpected call: " + method)
        return call


class CleanupTest(unittest.TestCase):
    def setUp(self):
        self.start = datetime(2026, 9, 28, 22, tzinfo=timezone.utc)
        self.config = {"account": "123456789012", "region": "us-east-1", "cluster": "durable-jobs-lab",
            "run_id": "lab-20260928T220000Z", "started_at": self.start.isoformat(),
            "cleanup_after": (self.start + timedelta(hours=5)).isoformat(),
            "deadline": (self.start + timedelta(hours=6)).isoformat()}
        self.tags = [{"Key": "Project", "Value": "durable-jobs-lab"}, {"Key": "LabRun", "Value": self.config["run_id"]}]
        not_found = RuntimeError("not found")
        not_found.response = {"Error": {"Code": "ResourceNotFoundException"}}
        self.clients = {
            "eks": Fake(describe_cluster=not_found),
            "ec2": Fake(describe_instances={"Reservations": []}, describe_volumes={"Volumes": []},
                        describe_nat_gateways={"NatGateways": []}, describe_vpc_endpoints={"VpcEndpoints": []}, describe_addresses={"Addresses": []}),
            "elbv2": Fake(describe_load_balancers={"LoadBalancers": []}, describe_target_groups={"TargetGroups": []}),
            "route53": Fake(list_hosted_zones={"HostedZones": []})}

    def test_deadline_with_cleanup_buffer_and_wrong_account(self):
        self.assertFalse(validate(self.config, self.config["account"], self.start))
        self.assertTrue(validate(self.config, self.config["account"], self.start + timedelta(hours=5)))
        for update in ({"deadline": (self.start + timedelta(hours=7)).isoformat()}, {"cleanup_after": self.config["deadline"]},
                       {"region": "us-west-2"}, {"account": "999999999999"}):
            with self.assertRaises(ValueError):
                validate({**self.config, **update}, "123456789012", self.start)

    def test_exact_run_ownership(self):
        self.assertTrue(owned(self.tags, self.config))
        self.assertFalse(owned(self.tags[:1], self.config))
        self.assertFalse(owned({"Project": "other", "LabRun": self.config["run_id"]}, self.config))

    def test_absence_not_delete_request_is_completion(self):
        self.assertTrue(reconcile(self.config, self.clients, execute=True)["cost_resources_absent"])
        self.clients["ec2"].responses["describe_volumes"] = {"Volumes": [{"VolumeId": "vol-test", "State": "available", "Tags": self.tags, "Attachments": []}]}
        result = reconcile(self.config, self.clients, execute=True)
        self.assertFalse(result["cost_resources_absent"])
        self.assertEqual(self.clients["ec2"].writes, [("delete_volume", {"VolumeId": "vol-test"})])

    def test_inspection_never_writes_and_attached_volume_not_deleted(self):
        self.clients["ec2"].responses["describe_volumes"] = {"Volumes": [{"VolumeId": "vol-test", "State": "in-use", "Tags": self.tags, "Attachments": [{"InstanceId": "i-test"}]}]}
        reconcile(self.config, self.clients, execute=False)
        reconcile(self.config, self.clients, execute=True)
        self.assertEqual(self.clients["ec2"].writes, [])

    def test_foreign_cluster_and_access_denied_are_not_absence(self):
        self.clients["eks"].responses["describe_cluster"] = {"cluster": {"tags": {"Project": "other"}}}
        with self.assertRaises(ValueError):
            reconcile(self.config, self.clients, execute=True)
        error = RuntimeError("Denied")
        error.response = {"Error": {"Code": "AccessDeniedException"}}
        self.clients["eks"].responses["describe_cluster"] = error
        with self.assertRaises(RuntimeError):
            reconcile(self.config, self.clients, execute=True)

    def test_only_operator_instances_terminated_directly(self):
        self.clients["ec2"].responses["describe_instances"] = {"Reservations": [{"Instances": [
            {"InstanceId": "i-operator", "State": {"Name": "running"}, "Tags": self.tags + [{"Key": "Role", "Value": "lab-operator"}]},
            {"InstanceId": "i-managed-node", "State": {"Name": "running"}, "Tags": self.tags}]}]}
        reconcile(self.config, self.clients, execute=True)
        self.assertEqual(self.clients["ec2"].writes, [("terminate_instances", {"InstanceIds": ["i-operator"]})])

    def test_unowned_volume_refused(self):
        self.clients["ec2"].responses["describe_volumes"] = {"Volumes": [{"VolumeId": "vol-other", "State": "available", "Tags": []}]}
        with self.assertRaises(ValueError):
            reconcile(self.config, self.clients, execute=True)
        self.assertEqual(self.clients["ec2"].writes, [])

    def test_nodegroup_precedes_alb_and_cluster_deletion(self):
        name = self.config["cluster"] + "-workers"
        self.clients["eks"].responses.update(
            describe_cluster={"cluster": {"tags": dict((t["Key"], t["Value"]) for t in self.tags), "status": "ACTIVE"}},
            list_nodegroups={"nodegroups": [name]},
            describe_nodegroup={"nodegroup": {"tags": self.tags, "status": "ACTIVE"}})
        self.clients["elbv2"].responses.update(
            describe_load_balancers={"LoadBalancers": [{"LoadBalancerArn": "arn:test:lb", "Scheme": "internal"}]},
            describe_tags={"TagDescriptions": [{"Tags": self.tags}]})
        result = reconcile(self.config, self.clients, execute=True)
        self.assertFalse(result["cost_resources_absent"])
        self.assertEqual([m for m, _ in self.clients["eks"].writes], ["delete_nodegroup"])
        self.assertEqual(self.clients["elbv2"].writes, [])
        self.clients["eks"].responses.update(list_nodegroups={"nodegroups": []}, list_addons={"addons": []})
        reconcile(self.config, self.clients, execute=True)
        self.assertEqual([m for m, _ in self.clients["eks"].writes], ["delete_nodegroup", "delete_cluster"])
        self.assertEqual([m for m, _ in self.clients["elbv2"].writes], ["delete_load_balancer"])


if __name__ == "__main__":
    unittest.main()
