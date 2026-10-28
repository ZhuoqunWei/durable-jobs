# Phase 11E: separate networking credentials without breaking bootstrap

Date: September 28, 2026

Implemented and checked offline. No AWS resources were created or changed.
This is a **fresh-cluster configuration**, not a migration for an existing EKS
cluster. Networking, credentials and node startup still need a real AWS test.

Follow-up: [Phase 11F](16-deployment-preflight.md) now supplies the offline
checklist and opt-in read-only metadata inspector. Actual deployment inputs and
live inspection remain pending; the new tool does not authorize AWS spending.

## What changed, in plain language

The VPC CNI gives pods network addresses. Previously its AWS permissions were
on every worker machine's IAM role. Now only the cluster's `kube-system/aws-node`
service account is trusted to receive a dedicated CNI role through Pod Identity.
The application does not receive that role.

Simply removing the old permission would not be enough. The earlier ordering
waited for nodes before installing the credential agent. But a node needs the
CNI to initialize networking, and the CNI needs credentials. That creates a
runtime startup trap even if Terraform's configuration is syntactically valid.

The new preparation order is:

```text
private cluster + EKS Auth endpoint / security rule
    → Pod Identity agent add-on
    → CNI add-on + dedicated IAM policy and managed identity association
    → managed node group
    → kube-proxy → CoreDNS → EBS CSI
```

These first two add-ons prepare host-network DaemonSets before compute; their
pods run when nodes arrive. This is not a claim that pods ran without machines.
Review the selected managed builds' host-network, DNS and toleration behavior,
then verify live startup. CoreDNS is a Deployment, so it follows compute.
Disabling unmanaged bootstrap defaults means we must explicitly supply all
three: VPC CNI, kube-proxy and CoreDNS. No Helm installation is added for them.

## Read these small pieces of code

In `infra/aws/network-addons.tf`, EKS owns the CNI credential association:

```hcl
pod_identity_association {
  role_arn        = aws_iam_role.vpc_cni.arn
  service_account = "aws-node"
}
```

The role's trust policy also requires the exact cluster ARN and `kube-system`
namespace; the service-account name alone is not the entire boundary. Default
Pod Identity session tags must remain enabled. No duplicate standalone
association or IRSA role is configured.

In `infra/aws/eks.tf`, the node group's `depends_on` includes both the CNI and
credential-agent add-ons. In `infra/aws/addons.tf`, the agent no longer depends
on the node group. EBS explicitly follows compute and DNS instead.

`infra/aws/iam.tf` now attaches only `AmazonEKSWorkerNodePolicy` and
`AmazonEC2ContainerRegistryReadOnly` to nodes. The IPv4
`AmazonEKS_CNI_Policy` is attached only to the dedicated CNI role. AWS manages
that policy and its broad EC2 permissions: this is identity separation, not
complete cluster-scoped least privilege. Host-network infrastructure and node
administrators remain privileged; IMDS hop limit one is not pod isolation.

## Important replacement warning

`bootstrap_self_managed_addons` is now `false`. With the pinned AWS provider,
changing that field on an existing cluster **forces cluster replacement**.
There is no deployed AWS cluster from this project, so no migration was run.
Do not apply this change to an existing environment or approve a replacement
plan as a shortcut. That requires a separate staged migration: establish add-on
ownership, assign identity, restart/verify actual CNI credential use, then
remove the node's CNI permission. Preserve data and review every planned change.

## Tests and what they do not prove

Run `make infra-check`:

- Pinned Terraform/provider schema and formatting checks in a network-disabled,
  credential-free container; **36 mocked plan cases**, never apply.
- CNI trust, policy attachment and managed association; no CNI permission on
  nodes; explicit managed DNS/network ownership; five required exact version
  pins; rejection of missing, mutable and null versions.
- Terraform emits its actual resource dependency graph. The standard-library
  Python checker verifies **17 required dependency paths** and rejects cycles.
  Its **6 unit tests** cover missing ordering, reversed agent/node ordering,
  transitive dependencies, missing resources and malformed input. These tests
  also run in `make platform-check`; CI's existing infra job runs the checker.

September 28 evidence:

- `.local/phase11/run.1GxtpngO/`: plan results, dependency graph and graph checks.
- `make platform-check`: **33 local/tooling + 24 AWS-profile tests** passed,
  including pinned Helm rendering; profile report
  `.local/phase11b/config.2AglcgfL/`.
- `make check`: Go race tests and vet passed. No Go/SQL changes; the database
  integration and failure-injection suites were not rerun for this milestone.

Static order does not prove managed add-on health, IAM propagation, image pulls,
EC2 permissions, network reachability or successful jobs. No credentials were
read and no live IAM, node or cluster was modified. Existing local data remains.

## Next: prepare the first approved AWS demo

Do not add another framework or wait for dashboards. Finish these prerequisites:

1. Select the AWS account, region, short lab window, budget and teardown policy
   with the owner. Review current charges and the operator's permission scope.
2. Prepare locked/encrypted Terraform state and an approved private operator
   access path. The operator security group alone is not an access machine.
3. Check all **five** builds via AWS `describe-addon-versions` for Kubernetes
   1.35, the approved region and AMD64; inspect `describe-addon-configuration`.
   Confirm CNI supports `aws-node` Pod Identity and inspect agent host-network,
   NotReady tolerations and DNS fallback. Check host IPv6 requirements as well.
   Record exact pins; the `v0.0.0-eksbuild.N` fixtures are deliberately fake.
4. Prepare the approved AMD64 image digest, database Secrets/TLS, ALB certificate
   and client trust; verify the existing ELB service-linked role. Do not log
   secrets or generate real credentials into this repository.
5. After explicit deployment approval, verify agent/CNI readiness and CNI role
   usage without node CNI permissions. Check nodes Ready, Service routing and
   in-cluster DNS before EBS/PVC and application rollout. If bootstrap fails,
   inspect add-on events/logs, identity and endpoint reachability; stop instead
   of blindly restoring broad node permissions or increasing timeouts.
6. Use the existing DB → migration → app stages, install private ingress,
   verify DNS/HTTPS and submit a synthetic job that completes. Capture evidence
   and perform only the approved teardown. Dashboards/incident work follows.

## Fifteen-minute ownership exercise

Read the role trust and node `depends_on` together. Explain why the machine
needs a different role from its networking pod. In a test fixture only, make
the agent depend on nodes and explain the cycle the graph test rejects. Then
answer: why could every offline check pass while the first node still fails?

Defensible description: "I configured separate CNI credentials and tested the
fresh-cluster dependency order; live AWS bootstrap verification is still pending."

## Primary references

- [AWS CNI identity separation and Pod Identity support](https://docs.aws.amazon.com/eks/latest/userguide/cni-iam-role.html)
- [AWS Pod Identity agent setup and private credential access](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html)
- [AWS agent host-network implementation](https://github.com/aws/eks-pod-identity-agent/tree/main/charts/eks-pod-identity-agent)
- [EKS module's before-compute agent/CNI example](https://github.com/terraform-aws-modules/terraform-aws-eks/blob/master/examples/eks-managed-node-group/eks-al2023.tf)
- [Pinned provider's cluster bootstrap replacement semantics](https://github.com/hashicorp/terraform-provider-aws/blob/v6.66.0/website/docs/r/eks_cluster.html.markdown)
- [Terraform resource dependency graph](https://developer.hashicorp.com/terraform/cli/commands/graph)

Upstream examples inform the design; they are not installed dependencies or
proof of the specific managed builds to be selected for our deployment.
