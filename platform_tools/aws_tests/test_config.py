import contextlib
import copy
import io
import json
import pathlib
import subprocess
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import aws_config as config

ACCOUNT = "111122223333"
REGION = "us-west-2"
IMAGE = f"{ACCOUNT}.dkr.ecr.{REGION}.amazonaws.com/durable-jobs@sha256:" + "a" * 64


class ConfigurationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = {stage: config.render_overlay(stage) for stage in config.STAGES}

    def bundle(self, image=IMAGE):
        return config.build_bundle(image, ACCOUNT, REGION,
                                   renderer=lambda stage: copy.deepcopy(self.source[stage]))

    def test_valid_render_has_three_separate_stages_and_no_secrets(self):
        bundle = self.bundle()
        config.validate_bundle(bundle, IMAGE, ACCOUNT, REGION)
        self.assertEqual([len(bundle[s]) for s in config.STAGES], [9, 1, 3])
        self.assertFalse(any(r["kind"] == "Secret" for stage in bundle.values() for r in stage))
        self.assertNotIn("local_lab_only", json.dumps(bundle))
        self.assertNotIn("phase10", json.dumps(bundle))

    def test_image_account_region_and_digest_required(self):
        for image, account, region in [
            (IMAGE.replace("@sha256:" + "a" * 64, ":latest"), ACCOUNT, REGION),
            (IMAGE.replace(ACCOUNT, "999988887777"), ACCOUNT, REGION),
            (IMAGE.replace(REGION, "us-east-1"), ACCOUNT, REGION),
            (IMAGE, "bad", REGION), (IMAGE, ACCOUNT, "us-gov-west-1"),
            (config.PLACEHOLDER, ACCOUNT, REGION),
        ]:
            with self.subTest(image=image, account=account, region=region), self.assertRaises(config.ConfigError):
                config.check_image(image, account, region)

    def test_render_never_calls_cluster_or_aws(self):
        fake = subprocess.CompletedProcess([], 0, "kind: Namespace\nmetadata: {name: fixture}\n", "")
        with patch.object(config.subprocess, "run", return_value=fake) as run:
            config.render_overlay("infra")
        self.assertEqual(run.call_args.args[0], ["kubectl", "kustomize", str(config.OVERLAYS / "infra")])

    def test_unknown_stage_cannot_choose_arbitrary_path(self):
        with patch.object(config.subprocess, "run") as run, self.assertRaises(config.ConfigError):
            config.render_overlay("../../local/infra")
        run.assert_not_called()

    def test_renderer_failure_does_not_echo_raw_output(self):
        fake = subprocess.CompletedProcess([], 1, "", "password=do-not-echo")
        with patch.object(config.subprocess, "run", return_value=fake), self.assertRaises(config.ConfigError) as err:
            config.render_overlay("infra")
        self.assertNotIn("do-not-echo", str(err.exception))

    def test_default_is_plan_only(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output), patch.object(config, "render_overlay") as render:
            self.assertEqual(config.main([]), 0)
        render.assert_not_called()
        self.assertFalse(json.loads(output.getvalue())["deployment_ready"])

    def test_release_names_are_repeatable_and_change_with_image(self):
        first, repeated = self.bundle(), self.bundle()
        changed = self.bundle(IMAGE[:-1] + "b")
        self.assertEqual(first, repeated)
        self.assertNotEqual(first["migrate"][0]["metadata"]["name"], changed["migrate"][0]["metadata"]["name"])
        self.assertEqual(config.resource(first, "infra", "StatefulSet", "postgres")["spec"]["template"],
                         config.resource(changed, "infra", "StatefulSet", "postgres")["spec"]["template"])

    def test_migration_and_app_cannot_silently_use_different_images(self):
        bundle = self.bundle()
        config.pod(bundle["migrate"][0])["containers"][0]["image"] = IMAGE[:-1] + "b"
        with self.assertRaises(config.ConfigError):
            config.validate_bundle(bundle, IMAGE, ACCOUNT, REGION)

    def test_rejects_security_and_storage_regressions(self):
        # Mutations exercise the validator, not just a snapshot of valid YAML.
        def app(b):
            return config.resource(b, "app", "Deployment", "api")

        mutations = {
            "public-service": lambda b: config.resource(b, "app", "Service", "api")["spec"].update(type="LoadBalancer"),
            "service-ingress-override": lambda b: config.resource(b, "app", "Service", "api")["metadata"].setdefault("annotations", {}).update({"alb.ingress.kubernetes.io/backend-protocol": "HTTPS"}),
            "node-port": lambda b: config.resource(b, "app", "Service", "api")["spec"]["ports"][0].update(nodePort=30085),
            "delete-storage": lambda b: config.resource(b, "infra", "StorageClass", "durable-jobs-gp3-retain").update(reclaimPolicy="Delete"),
            "immediate-binding": lambda b: config.resource(b, "infra", "StorageClass", "durable-jobs-gp3-retain").update(volumeBindingMode="Immediate"),
            "unencrypted": lambda b: config.resource(b, "infra", "StorageClass", "durable-jobs-gp3-retain")["parameters"].update(encrypted="false"),
            "local-storage": lambda b: config.resource(b, "infra", "StatefulSet", "postgres")["spec"]["volumeClaimTemplates"][0]["spec"].update(storageClassName="standard"),
            "delete-pvc": lambda b: config.resource(b, "infra", "StatefulSet", "postgres")["spec"]["persistentVolumeClaimRetentionPolicy"].update(whenDeleted="Delete"),
            "plaintext-db": lambda b: config.resource(b, "infra", "ConfigMap", "database-client-config")["data"].update(DATABASE_URL=config.DATABASE_URL.replace("verify-full", "disable")),
            "password-in-config": lambda b: config.resource(b, "infra", "ConfigMap", "database-config")["data"].update(POSTGRES_PASSWORD="synthetic-bad-value"),
            "account-token": lambda b: config.pod(app(b)).update(automountServiceAccountToken=True),
            "host-network": lambda b: config.pod(app(b)).update(hostNetwork=True),
            "root": lambda b: config.pod(app(b))["securityContext"].update(runAsNonRoot=False),
            "privileged": lambda b: config.pod(app(b))["containers"][0]["securityContext"].update(privileged=True),
            "wrong-arch": lambda b: config.pod(app(b))["nodeSelector"].update({"kubernetes.io/arch": "arm64"}),
            "never-pull": lambda b: config.pod(app(b))["containers"][0].update(imagePullPolicy="Never"),
            "whole-secret": lambda b: config.pod(app(b))["containers"][0]["envFrom"].append({"secretRef": {"name": "postgres-admin"}}),
            "wrong-namespace": lambda b: app(b)["metadata"].update(namespace="durable-jobs-lab"),
            "migration-retries": lambda b: b["migrate"][0]["spec"].update(backoffLimit=6),
            "secret-resource": lambda b: b["infra"].append({"kind": "Secret", "metadata": {"name": "oops"}}),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                bundle = self.bundle()
                mutate(bundle)
                with self.assertRaises(config.ConfigError):
                    config.validate_bundle(bundle, IMAGE, ACCOUNT, REGION)

    def test_raw_overlay_cannot_be_mistaken_for_release(self):
        with self.assertRaises(config.ConfigError):
            config.validate_bundle(copy.deepcopy(self.source), IMAGE, ACCOUNT, REGION)

    def test_local_overlay_keeps_original_secret_and_storage(self):
        result = subprocess.run(["kubectl", "kustomize", str(config.ROOT / "deploy/kubernetes/local/infra")],
                                capture_output=True, text=True, check=True, timeout=30)
        docs = list(config.yaml.safe_load_all(result.stdout))
        self.assertTrue(any(r["kind"] == "Secret" and r["metadata"]["namespace"] == "durable-jobs-lab" for r in docs))
        db = next(r for r in docs if r["kind"] == "StatefulSet")
        self.assertNotIn("storageClassName", db["spec"]["volumeClaimTemplates"][0]["spec"])


if __name__ == "__main__":
    unittest.main()
