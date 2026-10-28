"""Deployment metadata preflight. Offline by default; explicit inspect is read-only.

No Terraform, Kubernetes, login, provisioning, image publishing or secret reads.
Successful metadata checks are neither deployment authorization nor runtime proof.
"""

import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


ROOT = Path(__file__).resolve().parents[1]
TEMPLATE = ROOT / "infra/aws/preflight.example.json"
ADDONS = {
    "pod_identity_agent": ("eks-pod-identity-agent", None),
    "vpc_cni": ("vpc-cni", "aws-node"),
    "kube_proxy": ("kube-proxy", None),
    "coredns": ("coredns", None),
    "ebs_csi": ("aws-ebs-csi-driver", "ebs-csi-controller-sa"),
}
VERSION = r"v[0-9]+\.[0-9]+\.[0-9]+-eksbuild\.[0-9]+"
PUBLIC_BLOCK = ("BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets")
MANUAL = {
    "authorization": "Owner approval of account, region, cost estimate, duration and teardown; a report never grants permission.",
    "state_locking": "Configure encrypted remote backend and lockfile IAM; verify locking/recovery separately. Bucket metadata is insufficient.",
    "private_access": "Verify approved operator role and private network access; an IAM role or security group alone is not an access path.",
    "runtime_image": "Verify linux/amd64 image content, provenance and smoke test; registry existence does not prove architecture or working code.",
    "secrets_and_trust": "Prepare four out-of-band DB Secrets, matching DB TLS/SAN, client CA trust, certificate rotation and data retention.",
    "addon_review": "Review actual schema/defaults, IAM policies, agent/CNI host networking, DNS and tolerations; no automatic version selection.",
    "live_verification": "After approval: verify bootstrap, storage, migration, private HTTPS/DNS and a completed synthetic job; record evidence.",
}
OPS = {
    ("sts", "get-caller-identity"): set(),
    ("eks", "describe-cluster-versions"): {"cluster-versions"},
    ("eks", "describe-addon-versions"): {"addon-name", "kubernetes-version"},
    ("eks", "describe-addon-configuration"): {"addon-name", "addon-version"},
    ("s3api", "get-bucket-location"): {"bucket", "expected-bucket-owner"},
    ("s3api", "get-bucket-versioning"): {"bucket", "expected-bucket-owner"},
    ("s3api", "get-bucket-encryption"): {"bucket", "expected-bucket-owner"},
    ("s3api", "get-public-access-block"): {"bucket", "expected-bucket-owner"},
    ("ecr", "describe-images"): {"registry-id", "repository-name", "image-ids"},
    ("acm", "describe-certificate"): {"certificate-arn"},
    ("iam", "get-role"): {"role-name"},
}


class PreflightError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise PreflightError(message)


def matches(pattern, value):
    return isinstance(value, str) and re.fullmatch(pattern, value) is not None


def validate_config(config, live=False):
    keys = {"account_id", "region", "profile", "cluster_name", "kubernetes_version",
            "addon_versions", "state_bucket", "image", "certificate_arn"}
    require(isinstance(config, dict) and set(config) == keys, "Use the exact non-secret preflight template fields")
    patterns = {"account_id": r"[0-9]{12}", "region": r"us-(east|west)-[12]",
                "profile": r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}",
                "state_bucket": r"[a-z0-9][a-z0-9-]{1,61}[a-z0-9]"}
    for field, pattern in patterns.items():
        require(config[field] is None or matches(pattern, config[field]), f"Invalid {field}; use null if unselected")
    require(matches(r"[a-z][a-z0-9-]{1,30}[a-z0-9]", config["cluster_name"]), "Invalid cluster name")
    require(config["kubernetes_version"] == "1.35", "Review code/tests before changing Kubernetes 1.35")
    pins = config["addon_versions"]
    require(isinstance(pins, dict) and set(pins) == set(ADDONS), "All five add-on fields are required")
    require(all(v is None or matches(VERSION, v) for v in pins.values()), "Use exact add-on builds or null")
    account, region = config["account_id"], config["region"]
    for field, pattern in {
        "image": rf"{account}\.dkr\.ecr\.{region}\.amazonaws\.com/durable-jobs@sha256:[a-f0-9]{{64}}",
        "certificate_arn": rf"arn:aws:acm:{region}:{account}:certificate/[a-f0-9]{{8}}-(?:[a-f0-9]{{4}}-){{3}}[a-f0-9]{{12}}",
    }.items():
        require(config[field] is None or (account is not None and region is not None and matches(pattern, config[field])),
                f"Invalid {field}; require the selected account/region and exact resource, or null")
    if live:
        require(all(config[k] is not None for k in ("account_id", "region", "profile")),
                "Inspect requires an explicitly selected account, region and trusted AWS CLI profile")


class AWSCLI:
    def __init__(self, config):
        validate_config(config, live=True)
        self.config = config
        self.deadline = time.monotonic() + 180

    def read(self, service, operation, **params):
        require((service, operation) in OPS and set(params) == OPS[service, operation],
                "Unsupported AWS operation or parameters")
        require(all(isinstance(v, str) and v and not v.startswith(("-", "file:", "fileb:")) for v in params.values()),
                "Invalid AWS parameter")
        remaining = self.deadline - time.monotonic()
        require(remaining > 0, "Preflight time budget exhausted; rerun after resolving access")
        command = ["aws", service, operation]
        for key, value in params.items():
            command.extend(["--" + key, value])
        command.extend(["--profile", self.config["profile"], "--region", self.config["region"],
                        "--output", "json", "--no-cli-pager", "--no-cli-auto-prompt",
                        "--cli-connect-timeout", "5", "--cli-read-timeout", "15"])
        # Only the explicitly selected local profile supplies AWS configuration.
        # It may use a trusted credential_process/SSO cache; we never log it in.
        env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
        env.update(AWS_EC2_METADATA_DISABLED="true", AWS_IGNORE_CONFIGURED_ENDPOINT_URLS="true",
                   AWS_PAGER="", AWS_CLI_AUTO_PROMPT="off", AWS_MAX_ATTEMPTS="1")
        try:
            result = subprocess.run(command, capture_output=True, text=True, check=False,
                                    timeout=min(30, remaining), env=env)
            require(result.returncode == 0, "AWS read failed; check CLI/profile permissions, resource existence and connectivity")
            data = json.loads(result.stdout)
            require(isinstance(data, dict) and not data.get("nextToken") and not data.get("NextToken"),
                    "Incomplete or unexpected AWS response")
            return data
        except (OSError, subprocess.TimeoutExpired, ValueError) as error:
            # Never echo raw stderr/stdout, credential-provider output or tokens.
            raise PreflightError("AWS CLI unavailable, timed out or returned invalid JSON") from error


def timestamp(value):
    if type(value) in (int, float):
        return datetime.fromtimestamp(value, timezone.utc)
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(parsed.tzinfo is not None, "AWS timestamp has no timezone")
    return parsed


def initial_report(config, mode, now):
    ids = ["identity", "eks_version"] + ["addon." + k for k in ADDONS] + ["state_bucket", "image", "certificate", "elb_role"]
    return {"mode": mode, "checked_at": now.isoformat(), "status": "not_checked", "deployment_ready": False,
            "config_sha256": hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest(),
            "checks": {key: {"status": "unknown", "message": "Not inspected"} for key in ids},
            "manual_prerequisites": MANUAL.copy()}


def inspect(config, reader, now=None):
    validate_config(config, live=True)
    now = now or datetime.now(timezone.utc)
    report = initial_report(config, "read_only_aws", now)
    checks = report["checks"]

    def run(key, operation):
        try:
            evidence = operation()
            checks[key] = {"status": "pass", "message": "Metadata check passed", **(evidence or {})}
            return evidence
        except PreflightError as error:
            checks[key] = {"status": "fail", "message": str(error)}
        except (KeyError, TypeError, ValueError, AttributeError, OverflowError):
            checks[key] = {"status": "fail", "message": "Unexpected or incomplete AWS metadata"}
        return None

    def identity():
        data = reader.read("sts", "get-caller-identity")
        require(data.get("Account") == config["account_id"], "Wrong AWS account; stopped before other reads")
        require(matches(rf"arn:aws:sts::{config['account_id']}:assumed-role/[A-Za-z0-9+=,.@_-]+/[A-Za-z0-9+=,.@_-]+", data.get("Arn")),
                "Use an assumed-role profile; root and IAM-user identities are not supported")
        return {"account_id": config["account_id"]}

    if run("identity", identity) is None:
        report["status"] = "blocked"
        return report

    def eks_version():
        data = reader.read("eks", "describe-cluster-versions", **{"cluster-versions": config["kubernetes_version"]})
        versions = [v for v in data["clusterVersions"] if v["clusterVersion"] == config["kubernetes_version"]]
        require(len(versions) == 1, "Kubernetes version not uniquely available in selected region")
        version = versions[0]
        require(version["versionStatus"] == "STANDARD_SUPPORT" and timestamp(version["endOfStandardSupportDate"]) > now,
                "Kubernetes version is not currently in standard support")
        require(matches(r"eks\.[0-9]+", version["defaultPlatformVersion"]), "Unknown EKS platform version")
        return {"platform_version": version["defaultPlatformVersion"]}

    platform = run("eks_version", eks_version)
    if platform:
        for key, (name, service_account) in ADDONS.items():
            def addon():
                data = reader.read("eks", "describe-addon-versions", **{
                    "addon-name": name, "kubernetes-version": config["kubernetes_version"]})
                candidates = sorted({v["addonVersion"] for item in data["addons"]
                                     if item["addonName"] == name and item["owner"] == "aws"
                                     for v in item["addonVersions"]
                                     if matches(VERSION, v["addonVersion"]) and v["requiresConfiguration"] is False and "amd64" in v["architecture"]
                                     and "ec2" in v["computeTypes"] and any(
                                         c["clusterVersion"] == config["kubernetes_version"] and
                                         ("*" in c["platformVersions"] or platform["platform_version"] in c["platformVersions"])
                                         for c in v["compatibilities"])})
                pin = config["addon_versions"][key]
                if pin is None:
                    return {"status": "unknown", "message": "Choose and review an exact build; candidates are not recommendations",
                            "compatible_candidates": candidates}
                require(pin in candidates, "Selected add-on build does not match Kubernetes/platform/AMD64/EC2 metadata")
                metadata = reader.read("eks", "describe-addon-configuration", **{"addon-name": name, "addon-version": pin})
                require(metadata["addonName"] == name and metadata["addonVersion"] == pin, "Add-on schema identity mismatch")
                schema = json.loads(metadata["configurationSchema"])
                require(isinstance(schema, dict) and bool(schema), "Missing add-on configuration schema")
                if service_account:
                    require(any(p["serviceAccount"] == service_account for p in metadata["podIdentityConfiguration"]),
                            "Selected add-on build does not advertise the required Pod Identity service account")
                # Fingerprint, don't dump raw schemas or claim defaults were reviewed.
                return {"version": pin, "schema_sha256": hashlib.sha256(json.dumps(schema, sort_keys=True).encode()).hexdigest()}
            run("addon." + key, addon)

    def bucket():
        params = {"bucket": config["state_bucket"], "expected-bucket-owner": config["account_id"]}
        location = reader.read("s3api", "get-bucket-location", **params)["LocationConstraint"] or "us-east-1"
        require(location == config["region"], "State bucket must be in selected lab region")
        require(reader.read("s3api", "get-bucket-versioning", **params).get("Status") == "Enabled", "State bucket versioning must be enabled")
        encryption = reader.read("s3api", "get-bucket-encryption", **params)["ServerSideEncryptionConfiguration"]["Rules"]
        require(bool(encryption) and all(r["ApplyServerSideEncryptionByDefault"]["SSEAlgorithm"] in ("AES256", "aws:kms", "aws:kms:dsse") for r in encryption),
                "State bucket requires default server-side encryption")
        block = reader.read("s3api", "get-public-access-block", **params)["PublicAccessBlockConfiguration"]
        require(all(block.get(k) is True for k in PUBLIC_BLOCK), "All four bucket-level public-access blocks are required")
        return {"scope": "Bucket settings only; not backend activation, object encryption, IAM or lock verification"}

    def image():
        digest = config["image"].split("@", 1)[1]
        data = reader.read("ecr", "describe-images", **{"registry-id": config["account_id"],
                           "repository-name": "durable-jobs", "image-ids": "imageDigest=" + digest})
        require(len(data["imageDetails"]) == 1, "Expected one published image digest")
        detail = data["imageDetails"][0]
        require(detail["imageDigest"] == digest and detail["registryId"] == config["account_id"] and detail["repositoryName"] == "durable-jobs",
                "Registry image identity mismatch")
        return {"scope": "Digest exists; architecture, provenance and runtime remain unverified"}

    def certificate():
        cert = reader.read("acm", "describe-certificate", **{"certificate-arn": config["certificate_arn"]})["Certificate"]
        require(cert["CertificateArn"] == config["certificate_arn"] and cert["Status"] == "ISSUED", "Certificate is not the selected issued certificate")
        # Deliberately require the exact SAN for this lab; no wildcard matcher.
        require(f"jobs.{config['cluster_name']}.internal" in cert["SubjectAlternativeNames"], "Certificate needs the exact private hostname SAN")
        require(timestamp(cert["NotBefore"]) <= now and timestamp(cert["NotAfter"]) > now + timedelta(hours=48),
                "Certificate is not valid now with at least 48 hours remaining")
        return {"scope": "Certificate metadata only; client CA trust and ALB TLS still unverified"}

    for key, field, check in (("state_bucket", "state_bucket", bucket), ("image", "image", image), ("certificate", "certificate_arn", certificate)):
        if config[field] is not None:
            run(key, check)
        else:
            checks[key]["message"] = "Resource not selected or prepared"

    def elb_role():
        role = reader.read("iam", "get-role", **{"role-name": "AWSServiceRoleForElasticLoadBalancing"})["Role"]
        expected = f"arn:aws:iam::{config['account_id']}:role/aws-service-role/elasticloadbalancing.amazonaws.com/AWSServiceRoleForElasticLoadBalancing"
        require(role["Arn"] == expected, "Expected existing ELB service-linked role in selected account")
        return {"scope": "Role existence only; no role or policies created"}
    run("elb_role", elb_role)
    statuses = {c["status"] for c in checks.values()}
    report["status"] = "blocked" if "fail" in statuses else "incomplete" if "unknown" in statuses else "metadata_checks_passed"
    return report


def unique_keys(pairs):
    data = {}
    for key, value in pairs:
        require(key not in data, "Duplicate configuration key")
        data[key] = value
    return data


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "inspect"), default="plan", nargs="?")
    parser.add_argument("--config", type=Path)
    args = parser.parse_args(argv)
    try:
        require(args.command != "inspect" or args.config is not None, "Inspect requires your explicit --config file")
        source = args.config or TEMPLATE
        require(source.stat().st_size <= 16384, "Configuration too large")
        config = json.loads(source.read_text(), object_pairs_hook=unique_keys)
        validate_config(config, live=args.command == "inspect")
        if args.command == "plan":
            report = initial_report(config, "offline", datetime.now(timezone.utc))
            report["missing_inputs"] = [k for k, v in config.items() if v is None] + [
                "addon_versions." + k for k, v in config["addon_versions"].items() if v is None]
        else:
            report = inspect(config, AWSCLI(config))
    except (PreflightError, OSError, ValueError, TypeError):
        print("Preflight input rejected. Use the non-secret template and explicit account/region/profile for inspect.", file=sys.stderr)
        return 2
    print(json.dumps(report, indent=2))
    # Offline exit 0 means the plan was printed, not that deployment is ready.
    return 0 if args.command == "plan" or report["status"] == "metadata_checks_passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
