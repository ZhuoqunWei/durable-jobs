import contextlib
import copy
import hashlib
import io
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import aws_config
import aws_ingress as ingress

CONFIG = {
    "account_id": "111122223333", "region": "us-west-2", "cluster_name": "durable-jobs-lab",
    "vpc_id": "vpc-00000000000000001", "security_group_id": "sg-00000000000000005",
    "private_subnet_ids": ["subnet-00000000000000003", "subnet-00000000000000004"],
    "hostname": "jobs.durable-jobs-lab.internal",
}
CERTIFICATE = "arn:aws:acm:us-west-2:111122223333:certificate/00000000-0000-0000-0000-000000000001"


class IngressTests(unittest.TestCase):
    def profile(self):
        return ingress.build_profile(copy.deepcopy(CONFIG), CERTIFICATE)

    def test_default_plan_reads_no_credentials_or_cluster(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), patch.object(pathlib.Path, "read_text") as read, patch.object(subprocess, "run") as run:
            self.assertEqual(ingress.main([]), 0)
        read.assert_not_called()
        run.assert_not_called()
        self.assertFalse(json.loads(output.getvalue())["deployment_ready"])

    def test_cli_renders_only_selected_stage_without_subprocess(self):
        with tempfile.TemporaryDirectory() as directory:
            config = pathlib.Path(directory) / "config.json"
            config.write_text(json.dumps(CONFIG))
            for stage in ("controller-values", "ingress"):
                with contextlib.redirect_stdout(io.StringIO()) as output, patch.object(subprocess, "run") as run:
                    self.assertEqual(ingress.main(["render", "--stage", stage, "--config", str(config),
                                                  "--certificate-arn", CERTIFICATE]), 0)
                run.assert_not_called()
                self.assertEqual(json.loads(output.getvalue()), self.profile()[stage])

    def test_valid_profile_and_application_port_contract(self):
        profile = self.profile()
        ingress.validate_profile(profile, CONFIG, CERTIFICATE)
        app = aws_config.render_overlay("app")
        service = next(r for r in app if r["kind"] == "Service" and r["metadata"]["name"] == "api")
        backend = profile["ingress"]["spec"]["rules"][0]["http"]["paths"][0]["backend"]["service"]
        self.assertEqual(backend["port"]["number"], service["spec"]["ports"][0]["port"])
        self.assertEqual(service["spec"]["type"], "ClusterIP")
        self.assertFalse(any(k.startswith("alb.ingress.kubernetes.io/") for k in service["metadata"].get("annotations", {})))

    def test_invalid_config_rejected(self):
        changes = [
            {"account_id": "bad"}, {"region": "cn-north-1"}, {"cluster_name": "{{ injected }}"},
            {"cluster_name": "lab-", "hostname": "jobs.lab-.internal"},
            {"vpc_id": "default"}, {"security_group_id": "0.0.0.0/0"},
            {"hostname": "public.example.com"}, {"private_subnet_ids": CONFIG["private_subnet_ids"][:1]},
            {"private_subnet_ids": [CONFIG["private_subnet_ids"][0]] * 2},
            {"private_subnet_ids": ["subnet-x", "subnet-y"]}, {"unexpected_secret": "never-print"},
        ]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(aws_config.ConfigError):
                ingress.build_profile({**CONFIG, **change}, CERTIFICATE)

    def test_certificate_requires_same_account_region_and_exact_arn(self):
        for certificate in ("", "unconfigured", CERTIFICATE.replace("us-west-2", "us-east-1"),
                            CERTIFICATE.replace("111122223333", "999988887777"), CERTIFICATE + "," + CERTIFICATE):
            with self.subTest(certificate=certificate), self.assertRaises(aws_config.ConfigError):
                ingress.build_profile(CONFIG, certificate)

    def test_ingress_security_mutations_rejected(self):
        changes = {
            "scheme": "internet-facing", "target-type": "instance", "ip-address-type": "dualstack",
            "subnets": "subnet-00000000000000001,subnet-00000000000000002",
            "security-groups": "sg-00000000000000009", "manage-backend-security-group-rules": "true",
            "listen-ports": '[{"HTTP":80}]', "certificate-arn": "", "ssl-policy": "ELBSecurityPolicy-2016-08",
            "healthcheck-path": "/healthz", "success-codes": "200-499", "group.name": "shared",
            "actions.override": "{}",
        }
        for key, value in changes.items():
            profile = self.profile()
            profile["ingress"]["metadata"]["annotations"]["alb.ingress.kubernetes.io/" + key] = value
            with self.subTest(key=key), self.assertRaises(aws_config.ConfigError):
                ingress.validate_profile(profile, CONFIG, CERTIFICATE)

    def test_wrong_routing_rejected(self):
        for mutate in (
            lambda i: i["metadata"].update(namespace="default"),
            lambda i: i["spec"].update(ingressClassName="alb"),
            lambda i: i["spec"]["rules"][0].update(host="*.internal"),
            lambda i: i["spec"]["rules"][0]["http"]["paths"][0]["backend"]["service"].update(name="postgres"),
        ):
            profile = self.profile()
            mutate(profile["ingress"])
            with self.assertRaises(aws_config.ConfigError):
                ingress.validate_profile(profile, CONFIG, CERTIFICATE)

    def test_controller_scope_and_identity_mutations_rejected(self):
        changes = [
            {"enableBackendSecurityGroup": True}, {"enableManageBackendSecurityGroupRules": True},
            {"enableServiceMutatorWebhook": True}, {"watchNamespace": ""}, {"region": ""}, {"vpcId": ""},
            {"hostNetwork": True}, {"enableWaf": True}, {"enableShield": True},
            {"enableBackendSecurityGroup": 0}, {"enableWaf": "false"},
            {"image": {"repository": ingress.IMAGE_REPOSITORY, "tag": "latest"}},
            {"serviceAccount": {"name": "durable-jobs"}}, {"env": {"AWS_ACCESS_KEY_ID": "never-print"}},
            {"controllerConfig": {"featureGates": {"EnableServiceController": True}}},
        ]
        for change in changes:
            profile = self.profile()
            profile["controller-values"].update(change)
            with self.subTest(change=change), self.assertRaises(aws_config.ConfigError):
                ingress.validate_profile(profile, CONFIG, CERTIFICATE)

    def test_bad_input_does_not_echo_raw_config(self):
        with tempfile.TemporaryDirectory() as directory:
            config = pathlib.Path(directory) / "bad.json"
            config.write_text('{"password":"never-print"}')
            with contextlib.redirect_stderr(io.StringIO()) as error:
                result = ingress.main(["render", "--stage", "ingress", "--config", str(config), "--certificate-arn", CERTIFICATE])
            self.assertEqual(result, 1)
            self.assertNotIn("never-print", error.getvalue())

    def test_actual_pinned_helm_chart_render(self):
        # Dependencies are mandatory, prepared by aws-config-check. Never skip
        # a missing renderer and pretend the controller chart was verified.
        chart = ingress.ROOT / ".local/alb-tools/aws-load-balancer-controller-3.5.0.tgz"
        helm = ingress.ROOT / "bin/helm"
        self.assertTrue(chart.is_file() and helm.is_file(), "Run make alb-tools first")
        self.assertEqual(hashlib.sha256(chart.read_bytes()).hexdigest(), ingress.CHART_SHA256)
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([
                str(helm), "template", "aws-load-balancer-controller", str(chart), "--namespace", "kube-system",
                "--kube-version", "1.35.0", "--include-crds", "--kubeconfig", "/dev/null", "--values", "-",
            ], input=json.dumps(self.profile()["controller-values"]), text=True, capture_output=True, timeout=60,
                env={"PATH": os.defpath, "HOME": directory})
        self.assertEqual(result.returncode, 0, "Local Helm template failed; no cluster calls were authorized")
        documents = [r for r in yaml.safe_load_all(result.stdout) if r]
        # The upstream chart wraps its IngressClass in a Kubernetes List.
        resources = [item for document in documents for item in
                     (document["items"] if document["kind"] == "List" else [document])]
        deployment = next(r for r in resources if r["kind"] == "Deployment")
        pod = deployment["spec"]["template"]["spec"]
        controller = pod["containers"][0]
        self.assertEqual(deployment["metadata"]["namespace"], "kube-system")
        self.assertEqual(pod["serviceAccountName"], ingress.SERVICE_ACCOUNT)
        self.assertEqual(controller["image"], ingress.CONTROLLER_IMAGE)
        self.assertFalse(pod.get("hostNetwork", False))
        self.assertFalse(controller["securityContext"]["allowPrivilegeEscalation"])
        self.assertTrue(controller["securityContext"]["readOnlyRootFilesystem"])
        for argument in (
            "--cluster-name=durable-jobs-lab", "--aws-region=us-west-2", "--aws-vpc-id=vpc-00000000000000001",
            "--watch-namespace=durable-jobs-aws", "--ingress-class=durable-jobs-internal",
            "--enable-backend-security-group=false", "--enable-manage-backend-security-group-rules=false",
            "--enable-shield=false", "--enable-waf=false", "--enable-wafv2=false",
            "--disable-ingress-group-name-annotation=true", "--disable-ingress-class-annotation=true",
            "--default-target-type=ip", "--default-load-balancer-scheme=internal",
        ):
            self.assertIn(argument, controller["args"])
        gates = next(a for a in controller["args"] if a.startswith("--feature-gates="))
        self.assertEqual(set(gates.split("=", 1)[1].split(",")), {key + "=false" for key in ingress.FEATURE_GATES})
        account = next(r for r in resources if r["kind"] == "ServiceAccount")
        self.assertEqual(account["metadata"]["name"], ingress.SERVICE_ACCOUNT)
        self.assertNotIn("eks.amazonaws.com/role-arn", account["metadata"].get("annotations", {}))
        ingress_class = next(r for r in resources if r["kind"] == "IngressClass")
        self.assertEqual(ingress_class["metadata"]["name"], ingress.INGRESS_CLASS)
        self.assertEqual(ingress_class["spec"]["controller"], "ingress.k8s.aws/alb")
        self.assertTrue(any(r["kind"] == "CustomResourceDefinition" and r["metadata"]["name"] == "targetgroupbindings.elbv2.k8s.aws" for r in resources))
        mutators = [w for r in resources if r["kind"] == "MutatingWebhookConfiguration" for w in r["webhooks"]]
        self.assertFalse(any(w["name"] == "mservice.elbv2.k8s.aws" for w in mutators))
        # Helm generates a local synthetic webhook key. Parse only in memory;
        # never log/persist full rendered resources or include that key in CI artifacts.


if __name__ == "__main__":
    unittest.main()
