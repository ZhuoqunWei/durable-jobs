"""Check bootstrap ordering in Terraform's simplified resource DOT graph.

This is deliberately not a general DOT parser or an AWS client. Terraform runs
in the offline container; this checker only reads its non-secret graph artifact.
An edge A -> B means A depends on B, not that A starts first.
"""

import argparse
from graphlib import CycleError, TopologicalSorter
from pathlib import Path
import re


EDGE = re.compile(r'"([A-Za-z0-9_.]+)"\s*->\s*"([A-Za-z0-9_.]+)";')
NODE = re.compile(r'"([A-Za-z0-9_.]+)"\s+\[label="[A-Za-z0-9_.]+"\];')

# These are reachability assertions: Terraform may remove redundant direct edges.
REQUIRED_PATHS = (
    ("aws_eks_node_group.lab", "aws_eks_addon.pod_identity_agent"),
    ("aws_eks_node_group.lab", "aws_eks_addon.vpc_cni"),
    ("aws_eks_node_group.lab", "aws_iam_role_policy_attachment.nodes"),
    ("aws_eks_node_group.lab", "aws_route.private_egress"),
    ("aws_eks_addon.pod_identity_agent", "aws_vpc_endpoint.eks_auth"),
    ("aws_eks_addon.pod_identity_agent", "aws_vpc_security_group_ingress_rule.eks_auth"),
    ("aws_eks_addon.vpc_cni", "aws_eks_addon.pod_identity_agent"),
    ("aws_eks_addon.vpc_cni", "aws_iam_role_policy_attachment.vpc_cni"),
    ("aws_iam_role_policy_attachment.vpc_cni", "aws_iam_role.vpc_cni"),
    ("aws_iam_role.vpc_cni", "aws_eks_cluster.lab"),
    ("aws_eks_cluster.lab", "terraform_data.deployment_gate"),
    ("aws_eks_addon.kube_proxy", "aws_eks_node_group.lab"),
    ("aws_eks_addon.coredns", "aws_eks_node_group.lab"),
    ("aws_eks_addon.coredns", "aws_eks_addon.kube_proxy"),
    ("aws_eks_addon.ebs_csi", "aws_eks_node_group.lab"),
    ("aws_eks_addon.ebs_csi", "aws_eks_addon.coredns"),
    ("aws_eks_addon.ebs_csi", "aws_iam_role_policy_attachment.ebs_csi"),
)


def parse_graph(text):
    """Fail closed if the pinned CLI's simple, flat-resource format changes."""
    lines = text.strip().splitlines()
    if not lines or lines[0].strip() != "digraph G {" or lines[-1].strip() != "}":
        raise ValueError("Expected a complete Terraform resource graph")
    graph, edges = {}, []
    for raw in lines[1:-1]:
        line = raw.strip()
        if not line or line in ('rankdir = "RL";', 'node [shape = rect, fontname = "sans-serif"];'):
            continue
        if match := NODE.fullmatch(line):
            graph.setdefault(match[1], set())
        elif match := EDGE.fullmatch(line):
            edges.append((match[1], match[2]))
        else:
            raise ValueError(f"Unsupported resource graph line: {line}")
    if not graph:
        raise ValueError("Empty resource graph")
    for dependent, prerequisite in edges:
        if dependent not in graph or prerequisite not in graph:
            raise ValueError("Dependency references an undeclared resource")
        graph[dependent].add(prerequisite)
    return graph


def prerequisites(graph, resource):
    pending, seen = list(graph[resource]), set()
    while pending:
        dependency = pending.pop()
        if dependency not in seen:
            seen.add(dependency)
            pending.extend(graph[dependency])
    return seen


def validate_bootstrap(graph):
    try:
        tuple(TopologicalSorter(graph).static_order())
    except CycleError as error:
        raise ValueError("Bootstrap dependency cycle") from error
    for dependent, prerequisite in REQUIRED_PATHS:
        if dependent not in graph or prerequisite not in graph:
            raise ValueError(f"Missing bootstrap resource: {dependent} or {prerequisite}")
        if prerequisite not in prerequisites(graph, dependent):
            raise ValueError(f"{dependent} must depend on {prerequisite}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("graph", type=Path)
    args = parser.parse_args()
    try:
        graph = parse_graph(args.graph.read_text(encoding="utf-8"))
        validate_bootstrap(graph)
    except (OSError, ValueError) as error:
        parser.exit(1, f"FAIL: {error}\n")
    print(f"PASS: {len(REQUIRED_PATHS)} bootstrap dependency paths; no cycles. "
          "Static ordering only, not live AWS startup proof.")


if __name__ == "__main__":
    main()
