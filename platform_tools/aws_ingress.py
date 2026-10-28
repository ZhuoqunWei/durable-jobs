"""Offline internal ALB profile. No AWS, Kubernetes or Helm install calls."""

import argparse
import copy
import json
import pathlib
import re
import sys

import yaml

from aws_config import ConfigError, NAMESPACE, ROOT, require

SOURCE = ROOT / "deploy/kubernetes/aws/ingress"
CHART_VERSION = "3.5.0"
CHART_SHA256 = "45051f634b33e10baccb3354d0681b7de787c60445e599fa276e0c9aedd4ccd5"
IMAGE_REPOSITORY = "public.ecr.aws/eks/aws-load-balancer-controller"
IMAGE_TAG = "v3.5.0@sha256:298acdff5a571731276aaea3d5cc450a264e4ad710a5bddf3e518f68a3f9f6cb"
CONTROLLER_IMAGE = f"{IMAGE_REPOSITORY}:{IMAGE_TAG}"
INGRESS_CLASS = "durable-jobs-internal"
SERVICE_ACCOUNT = "aws-load-balancer-controller"
FEATURE_GATES = {key: False for key in (
    "EnableServiceController", "ALBGatewayAPI", "NLBGatewayAPI",
    "GlobalAcceleratorController", "EnableCertificateManagement", "LBCapacityReservation",
)}
PREREQUISITES = [
    "Approved account, costs, private operator access and foundation deployment",
    "Healthy Pod Identity agent and dedicated ALB controller association",
    "Operator-reviewed ELB service-linked role already exists; controller cannot create IAM roles",
    "Compatible pinned controller/chart, reviewed RBAC and healthy webhook",
    "Existing ACM certificate matching the private hostname; caller trusts its issuing CA",
    "Healthy application and migration; no app authentication or end-to-end API TLS is claimed",
    "Controller-created internal ALB verified before explicitly publishing its Route53 alias",
]


def check_config(config, certificate):
    require(set(config) == {"account_id", "region", "cluster_name", "vpc_id", "private_subnet_ids",
                            "security_group_id", "hostname"}, "Expected the ingress_config Terraform output only")
    require(bool(re.fullmatch(r"[0-9]{12}", config["account_id"])), "Invalid account")
    require(bool(re.fullmatch(r"us-(east|west)-[12]", config["region"])), "Invalid region")
    require(bool(re.fullmatch(r"[a-z][a-z0-9-]{1,30}[a-z0-9]", config["cluster_name"])), "Invalid cluster/hostname label")
    require(config["hostname"] == f"jobs.{config['cluster_name']}.internal", "Unexpected private hostname")
    require(bool(re.fullmatch(r"vpc-[a-f0-9]{17}", config["vpc_id"])), "Invalid VPC")
    require(bool(re.fullmatch(r"sg-[a-f0-9]{17}", config["security_group_id"])), "Invalid ALB security group")
    subnets = config["private_subnet_ids"]
    require(isinstance(subnets, list) and len(subnets) == 2 and len(set(subnets)) == 2
            and all(re.fullmatch(r"subnet-[a-f0-9]{17}", s) for s in subnets), "Two distinct private subnet IDs required")
    prefix = f"arn:aws:acm:{config['region']}:{config['account_id']}:certificate/"
    require(bool(re.fullmatch(re.escape(prefix) + r"[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}", certificate)),
            "Use one explicit ACM certificate ARN from the selected account and region")


def expected_annotations(config, certificate):
    values = {
        "scheme": "internal", "target-type": "ip", "ip-address-type": "ipv4",
        "subnets": ",".join(config["private_subnet_ids"]),
        "security-groups": config["security_group_id"],
        "manage-backend-security-group-rules": "false", "listen-ports": '[{"HTTPS":443}]',
        "certificate-arn": certificate, "ssl-policy": "ELBSecurityPolicy-TLS13-1-2-2021-06",
        "backend-protocol": "HTTP", "healthcheck-protocol": "HTTP", "healthcheck-port": "traffic-port",
        "healthcheck-path": "/readyz", "success-codes": "200",
    }
    return {f"alb.ingress.kubernetes.io/{key}": value for key, value in values.items()}


def validate_profile(profile, config, certificate):
    check_config(config, certificate)
    require(set(profile) == {"controller-values", "ingress"}, "Unexpected profile stages")
    ingress = profile["ingress"]
    expected = {
        "apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
        "metadata": {"name": "api", "namespace": NAMESPACE,
                     "annotations": expected_annotations(config, certificate)},
        "spec": {"ingressClassName": INGRESS_CLASS, "rules": [{"host": config["hostname"], "http": {"paths": [{
            "path": "/", "pathType": "Prefix", "backend": {"service": {"name": "api", "port": {"number": 8085}}}
        }]}}]},
    }
    require(json.dumps(ingress, sort_keys=True) == json.dumps(expected, sort_keys=True),
            "Ingress must retain private HTTPS, reviewed SG/subnets, readiness and exact backend")
    values = profile["controller-values"]
    required = {
        "clusterName": config["cluster_name"], "region": config["region"], "vpcId": config["vpc_id"],
        "replicaCount": 2, "image": {"repository": IMAGE_REPOSITORY, "tag": IMAGE_TAG},
        "serviceAccount": {"create": True, "name": SERVICE_ACCOUNT, "annotations": {}, "automountServiceAccountToken": True},
        "rbac": {"create": True}, "ingressClass": INGRESS_CLASS, "createIngressClassResource": True,
        "ingressClassParams": {"create": False}, "watchNamespace": NAMESPACE,
        "disableIngressClassAnnotation": True, "disableIngressGroupNameAnnotation": True,
        "defaultTargetType": "ip", "defaultLoadBalancerScheme": "internal",
        "enableBackendSecurityGroup": False, "enableManageBackendSecurityGroupRules": False,
        "enableServiceMutatorWebhook": False, "enablePodReadinessGateInject": False,
        "enableShield": False, "enableWaf": False, "enableWafv2": False,
        "enableCertManager": False, "keepTLSSecret": True,
        "tolerateNonExistentBackendService": False, "tolerateNonExistentBackendAction": False,
        "controllerConfig": {"featureGates": FEATURE_GATES}, "hostNetwork": False,
        "nodeSelector": {"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux"},
        "podSecurityContext": {"fsGroup": 65534, "seccompProfile": {"type": "RuntimeDefault"}},
        "securityContext": {"runAsNonRoot": True, "readOnlyRootFilesystem": True,
                            "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
        "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}, "limits": {"cpu": "500m", "memory": "256Mi"}},
    }
    # Python considers 0 == False, but Helm's kindIs bool does not. Preserve JSON
    # types so a numeric zero cannot silently fall back to an unsafe chart default.
    require(json.dumps(values, sort_keys=True) == json.dumps(required, sort_keys=True),
            "Controller values differ from the bounded, separately identified profile")


def build_profile(config, certificate):
    check_config(config, certificate)
    profile = {stage: yaml.safe_load((SOURCE / f"{stage}.yaml").read_text())
               for stage in ("controller-values", "ingress")}
    values = profile["controller-values"]
    values.update(clusterName=config["cluster_name"], region=config["region"], vpcId=config["vpc_id"])
    ingress = profile["ingress"]
    # Replace only deployment-specific placeholders; validate the rest unchanged.
    for key in ("subnets", "security-groups", "certificate-arn"):
        annotation = "alb.ingress.kubernetes.io/" + key
        require(ingress["metadata"]["annotations"][annotation] == "unconfigured", "Unexpected configured source")
        ingress["metadata"]["annotations"][annotation] = expected_annotations(config, certificate)[annotation]
    ingress["spec"]["rules"][0]["host"] = config["hostname"]
    validate_profile(profile, config, certificate)
    return copy.deepcopy(profile)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "render"), nargs="?", default="plan")
    parser.add_argument("--stage", choices=("controller-values", "ingress"))
    parser.add_argument("--config", type=pathlib.Path)
    parser.add_argument("--certificate-arn")
    args = parser.parse_args(argv)
    if args.command == "plan":
        print(json.dumps({"status": "configuration_only", "deployment_ready": False,
                          "chart_version": CHART_VERSION, "chart_sha256": CHART_SHA256,
                          "prerequisites": PREREQUISITES}, indent=2))
        return 0
    if not all((args.stage, args.config, args.certificate_arn)):
        parser.error("Rendering requires --stage, --config and --certificate-arn; no deployment is performed")
    try:
        config = json.loads(args.config.read_text())
        profile = build_profile(config, args.certificate_arn)
    except (ConfigError, OSError, ValueError, KeyError, TypeError, IndexError, yaml.YAMLError):
        print("Ingress configuration rejected. Review the non-secret foundation output and certificate ARN.", file=sys.stderr)
        return 1
    print(json.dumps(profile[args.stage], indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
