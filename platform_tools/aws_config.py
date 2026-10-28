"""Render/check AWS-specific manifests offline. There is deliberately no deploy command."""

import argparse
import hashlib
import json
import pathlib
import re
import subprocess
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
OVERLAYS = ROOT / "deploy/kubernetes/aws"
NAMESPACE = "durable-jobs-aws"
STAGES = ("infra", "migrate", "app")
PLACEHOLDER = "example.invalid/durable-jobs:unconfigured"
POSTGRES_IMAGE = "postgres:17.11@sha256:f4c66b820c6f974249089d3d16d86a3698eae11e8746eb6644b2271031e91232"
DATABASE_URL = ("postgres://jobs_owner@postgres.durable-jobs-aws.svc.cluster.local:5432/"
                "durable_jobs?sslmode=verify-full&sslrootcert=/etc/database-ca/ca.crt")
PREREQUISITES = [
    "Explicit approval of AWS account, region, cost, lab duration and teardown",
    "Approved in-VPC operator access and encrypted/locked Terraform state",
    "Review EKS add-on versions, then install/verify the Terraform EBS CSI and Pod Identity configuration; this renderer does not install them",
    "fresh-cluster networking bootstrap and dedicated CNI credentials verified live before workload rollout (docs/15)",
    "Private ingress is a separate aws_ingress.py stage: controller identity, trusted ACM certificate, internal ALB and verified DNS publication",
    "Published/verified linux/amd64 or multi-architecture image at the supplied digest",
    "Four out-of-band Kubernetes Secrets: postgres-admin, database-credentials, postgres-tls, database-ca",
    "Certificate SAN for postgres.durable-jobs-aws.svc.cluster.local and rotation/backup plan",
    "Wait for database readiness, then successful migration, then application rollout and fresh-job verification",
]


class ConfigError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise ConfigError(message)


def check_image(image, account_id, region):
    require(bool(re.fullmatch(r"[0-9]{12}", account_id)), "Expected a 12-digit account ID")
    require(bool(re.fullmatch(r"us-(east|west)-[12]", region)), "Expected a commercial US region")
    prefix = f"{account_id}.dkr.ecr.{region}.amazonaws.com/"
    require(bool(re.fullmatch(re.escape(prefix) + r"durable-jobs@sha256:[a-f0-9]{64}", image)),
            "Use a digest-pinned durable-jobs ECR image in the explicitly selected account and region")


def render_overlay(stage):
    require(stage in STAGES, "Unknown deployment stage")
    try:
        result = subprocess.run(["kubectl", "kustomize", str(OVERLAYS / stage)],
                                capture_output=True, text=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise ConfigError("Local manifest rendering is unavailable or timed out") from exc
    require(result.returncode == 0, "Local manifest rendering failed; no cluster command was run")
    try:
        resources = list(yaml.safe_load_all(result.stdout))
    except yaml.YAMLError as exc:
        raise ConfigError("Rendered manifests are invalid YAML") from exc
    require(bool(resources) and all(isinstance(item, dict) for item in resources), "Empty or invalid overlay")
    return resources


def resource(bundle, stage, kind, name):
    matches = [item for item in bundle[stage] if item["kind"] == kind and item["metadata"]["name"] == name]
    require(len(matches) == 1, f"Expected exactly one {kind}/{name} in {stage}")
    return matches[0]


def pod(resource):
    return resource["spec"]["template"]["spec"]


def secret_env(name, secret, key="password"):
    return {"name": name, "valueFrom": {"secretKeyRef": {"name": secret, "key": key}}}


def validate_bundle(bundle, image, account_id, region):
    """Check the supported profile, not live cluster state or every Kubernetes policy."""
    check_image(image, account_id, region)
    require(set(bundle) == set(STAGES), "All three separate stages are required")
    expected = {
        "infra": {("Namespace", NAMESPACE), ("StorageClass", "durable-jobs-gp3-retain"),
                  ("ServiceAccount", "durable-jobs"), ("Service", "postgres"),
                  ("StatefulSet", "postgres"), ("ConfigMap", "database-config"),
                  ("ConfigMap", "database-client-config"), ("ConfigMap", "database-bootstrap"),
                  ("ConfigMap", "app-config")},
        "app": {("Service", "api"), ("Deployment", "api"), ("Deployment", "worker")},
    }
    for stage in ("infra", "app"):
        actual = [(item["kind"], item["metadata"]["name"]) for item in bundle[stage]]
        require(set(actual) == expected[stage] and len(actual) == len(expected[stage]),
                "Unexpected or duplicated resources; Secrets, RBAC and public ingress are not part of this bundle")
    require(len(bundle["migrate"]) == 1 and bundle["migrate"][0]["kind"] == "Job", "Migration must be a separate Job")
    migration = bundle["migrate"][0]
    require(bool(re.fullmatch(r"migrate(?:-[a-f0-9]{16})?", migration["metadata"]["name"])), "Unexpected migration name")

    for stage in STAGES:
        for item in bundle[stage]:
            metadata = item["metadata"]
            if item["kind"] in ("Namespace", "StorageClass"):
                require("namespace" not in metadata, "Cluster-scoped object must not have a namespace")
            else:
                require(metadata.get("namespace") == NAMESPACE, "Only the separate AWS namespace is allowed")
            if item["kind"] == "Service":
                require(set(metadata.get("annotations", {})) <= {"durable-jobs.dev/release"},
                        "Service annotations must not override the separate reviewed ingress configuration")
                spec = item["spec"]
                require(spec.get("type", "ClusterIP") == "ClusterIP" and not spec.get("externalIPs")
                        and not spec.get("loadBalancerIP") and not any(p.get("nodePort") for p in spec["ports"]),
                        "Services must remain cluster-private")

    namespace = resource(bundle, "infra", "Namespace", NAMESPACE)
    require(namespace["metadata"]["labels"].get("pod-security.kubernetes.io/enforce") == "restricted",
            "Restricted pod security is required")
    service_account = resource(bundle, "infra", "ServiceAccount", "durable-jobs")
    require(service_account.get("automountServiceAccountToken") is False
            and set(service_account["metadata"].get("annotations", {})) <= {"durable-jobs.dev/release"},
            "Workload identity must have no implicit API/AWS access")

    storage = resource(bundle, "infra", "StorageClass", "durable-jobs-gp3-retain")
    require(storage["provisioner"] == "ebs.csi.aws.com" and storage["reclaimPolicy"] == "Retain"
            and storage["volumeBindingMode"] == "WaitForFirstConsumer"
            and storage["parameters"].get("encrypted") == "true" and storage["parameters"].get("type") == "gp3",
            "Require encrypted gp3 EBS, topology-aware binding and retained data")
    database = resource(bundle, "infra", "StatefulSet", "postgres")
    require(database["spec"]["replicas"] == 1, "This lab supports one database, not replicated PostgreSQL")
    require(database["spec"]["persistentVolumeClaimRetentionPolicy"] == {"whenDeleted": "Retain", "whenScaled": "Retain"},
            "Database PVCs must be retained")
    claims = database["spec"]["volumeClaimTemplates"]
    require(len(claims) == 1 and claims[0]["metadata"]["name"] == "data"
            and claims[0]["spec"].get("storageClassName") == "durable-jobs-gp3-retain"
            and claims[0]["spec"]["accessModes"] == ["ReadWriteOnce"]
            and claims[0]["spec"]["resources"]["requests"]["storage"] == "10Gi", "Unexpected database storage profile")

    client = resource(bundle, "infra", "ConfigMap", "database-client-config")["data"]
    require(client == {"DATABASE_URL": DATABASE_URL}, "Database clients must verify TLS, with no password in the URL")
    database_config = resource(bundle, "infra", "ConfigMap", "database-config")["data"]
    require(database_config == {"POSTGRES_USER": "postgres", "POSTGRES_DB": "durable_jobs",
            "PGDATA": "/var/lib/postgresql/data/pgdata", "POSTGRES_INITDB_ARGS": "--auth-host=scram-sha-256"},
            "Database settings must not contain passwords or local-lab defaults")
    require(resource(bundle, "infra", "ConfigMap", "app-config")["data"] == {"HTTP_ADDR": "0.0.0.0:8085"},
            "Unexpected application settings")
    bootstrap = resource(bundle, "infra", "ConfigMap", "database-bootstrap")["data"]
    require(bootstrap == {name: (OVERLAYS / "infra" / name).read_text() for name in ("10-role.sql", "pg_hba.conf")},
            "Database bootstrap/auth configuration differs from the reviewed source")

    workloads = [database, migration] + [resource(bundle, "app", "Deployment", name) for name in ("api", "worker")]
    for workload in workloads:
        spec = pod(workload)
        require(spec.get("serviceAccountName") == "durable-jobs" and spec.get("automountServiceAccountToken") is False,
                "Workloads must not inherit a service account token")
        require(spec.get("nodeSelector") == {"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux"},
                "Workload architecture must match the current EKS node group")
        require(not any(spec.get(key) for key in ("hostNetwork", "hostPID", "hostIPC", "initContainers")),
                "Unexpected host access or init container")
        require(spec["securityContext"].get("runAsNonRoot") is True
                and spec["securityContext"].get("seccompProfile") == {"type": "RuntimeDefault"}, "Non-root/seccomp required")
        require(not any("hostPath" in v for v in spec.get("volumes", [])), "Host paths are forbidden")
        require(len(spec["containers"]) == 1, "Only the reviewed container is supported")
        container = spec["containers"][0]
        security = container["securityContext"]
        require(security.get("readOnlyRootFilesystem") is True and security.get("allowPrivilegeEscalation") is False
                and not security.get("privileged") and security.get("capabilities") == {"drop": ["ALL"]}, "Container isolation weakened")
        require(container.get("resources", {}).get("requests") and container.get("resources", {}).get("limits"), "Resource budgets required")
        require(container.get("imagePullPolicy") == "IfNotPresent", "AWS images cannot use the local Never pull policy")
        require(all(set(entry) == {"configMapRef"} for entry in container.get("envFrom", [])), "Never import a whole Secret")
        if workload is database:
            require(container["image"] == POSTGRES_IMAGE, "Use the pinned PostgreSQL image")
            require(container["env"] == [secret_env("POSTGRES_PASSWORD", "postgres-admin"),
                                          secret_env("APP_PASSWORD", "database-credentials")], "DB passwords must be separate Secret references")
            require(container["args"] == ["postgres", "-c", "ssl=on", "-c", "ssl_cert_file=/etc/postgres-tls/tls.crt",
                    "-c", "ssl_key_file=/etc/postgres-tls/tls.key", "-c", "ssl_min_protocol_version=TLSv1.2",
                    "-c", "hba_file=/etc/postgres-bootstrap/pg_hba.conf", "-c", "password_encryption=scram-sha-256"],
                    "Database must enable TLS and use the reviewed authentication rules")
            tls = next(v for v in spec["volumes"] if v["name"] == "tls")
            require(tls["secret"] == {"secretName": "postgres-tls", "defaultMode": 0o440}, "TLS key must be a restricted Secret mount")
            require(any(v["name"] == "data" and v["mountPath"] == "/var/lib/postgresql/data" for v in container["volumeMounts"]),
                    "PostgreSQL must actually mount the data claim")
        else:
            require(container["image"] == image, "API, worker and migration must share the reviewed image digest")
            require(container.get("env") == [secret_env("PGPASSWORD", "database-credentials")], "Only the application password may enter clients")
            require(spec.get("volumes") == [{"name": "database-ca", "secret": {"secretName": "database-ca"}}]
                    and container.get("volumeMounts") == [{"name": "database-ca", "mountPath": "/etc/database-ca", "readOnly": True}],
                    "Clients must mount only their database CA, not the server private key")
            config_names = {entry["configMapRef"]["name"] for entry in container["envFrom"]}
            require(config_names == ({"database-client-config", "app-config"} if container["name"] == "api"
                                     else {"database-client-config"}), "Unexpected client configuration source")

    require(migration["spec"]["backoffLimit"] == 0 and migration["spec"]["activeDeadlineSeconds"] == 120
            and pod(migration)["restartPolicy"] == "Never" and pod(migration)["containers"][0]["command"] == ["/migrate"],
            "Migration must be bounded and explicit, not an application startup side effect")
    worker = resource(bundle, "app", "Deployment", "worker")
    require(pod(worker)["containers"][0]["command"] == ["/worker", "-processor", "builtin"], "AWS worker must not invoke the Docker sandbox")
    api = resource(bundle, "app", "Deployment", "api")
    require(api["spec"]["strategy"]["rollingUpdate"]["maxUnavailable"] == 0, "Keep API rolling-update availability setting")
    api_container = pod(api)["containers"][0]
    require(api_container["readinessProbe"]["httpGet"]["path"] == "/readyz"
            and api_container["livenessProbe"]["httpGet"]["path"] == "/healthz", "Keep readiness and liveness distinct")


def build_bundle(image, account_id, region, renderer=render_overlay):
    check_image(image, account_id, region)
    bundle = {stage: renderer(stage) for stage in STAGES}
    for stage in ("migrate", "app"):
        for item in bundle[stage]:
            if item["kind"] in ("Deployment", "Job"):
                container = pod(item)["containers"][0]
                require(container["image"] == PLACEHOLDER, "Unreviewed application image in source overlay")
                container["image"] = image
    validate_bundle(bundle, image, account_id, region)
    revision = hashlib.sha256(json.dumps(bundle, sort_keys=True).encode()).hexdigest()
    # An app-image change should not restart the database. Hash only its own
    # pod/config here; Secret values remain deliberately unavailable to this tool.
    database_revision = hashlib.sha256(json.dumps([
        pod(resource(bundle, "infra", "StatefulSet", "postgres")),
        resource(bundle, "infra", "ConfigMap", "database-config")["data"],
        resource(bundle, "infra", "ConfigMap", "database-bootstrap")["data"],
    ], sort_keys=True).encode()).hexdigest()
    bundle["migrate"][0]["metadata"]["name"] = "migrate-" + revision[:16]
    for stage in STAGES:
        for item in bundle[stage]:
            item["metadata"].setdefault("annotations", {})["durable-jobs.dev/release"] = revision
            if item["kind"] in ("Deployment", "StatefulSet", "Job"):
                pod_revision = database_revision if item["kind"] == "StatefulSet" else revision
                item["spec"]["template"].setdefault("metadata", {}).setdefault("annotations", {})["durable-jobs.dev/release"] = pod_revision
    # ServiceAccount carries only a release marker, never an IAM-role annotation.
    validate_bundle(bundle, image, account_id, region)
    return bundle


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "render"), nargs="?", default="plan")
    parser.add_argument("--stage", choices=STAGES)
    parser.add_argument("--image")
    parser.add_argument("--account-id")
    parser.add_argument("--region")
    args = parser.parse_args(argv)
    if args.command == "plan":
        print(json.dumps({"status": "configuration_only", "deployment_ready": False,
                          "stages": list(STAGES), "prerequisites": PREREQUISITES}, indent=2))
        return 0
    if not all((args.stage, args.image, args.account_id, args.region)):
        parser.error("Rendering requires --stage, --image, --account-id and --region; nothing is deployed")
    try:
        bundle = build_bundle(args.image, args.account_id, args.region)
    except (ConfigError, KeyError, TypeError, ValueError, StopIteration, OSError):
        # Never echo raw inputs/manifests; this command is not a secret inspector.
        print("Configuration rejected. Check the reviewed AWS overlay and explicit image/account/region.", file=sys.stderr)
        return 1
    print(json.dumps({"apiVersion": "v1", "kind": "List", "items": bundle[args.stage]}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
