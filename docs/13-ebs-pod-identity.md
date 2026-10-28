# Phase 11C: storage-driver identity and private credentials

Date: September 28, 2026

Follow-up: [Phase 11D](14-private-ingress.md) now supplies offline-tested internal
ingress/controller and guarded private DNS configuration. No AWS deployment.
Follow-up: [Phase 11E](15-cni-bootstrap.md) separates CNI identity and updates
the fresh-cluster order: agent/CNI before nodes, DNS/storage after nodes.

**Implemented as configuration:** managed EKS Pod Identity and EBS CSI add-ons,
a dedicated EBS controller role, and a private EKS Auth interface endpoint.
**Not deployed:** no AWS resources, credentials, remote images or charges were
created. Add-on compatibility, live IAM access and EBS recovery are not proven.

This connects the missing infrastructure pieces for Phase 11B's
`durable-jobs-gp3-retain` StorageClass. That manifest requests a disk; the CSI
driver is the component that actually asks AWS to create and attach it.

## Trace one disk request

```text
Postgres pod + PVC → StorageClass → EBS CSI controller → EC2 API → EBS volume
                                        ↑ temporary AWS credentials
                              Pod Identity agent on the node
                                        ↓ node-role authentication
                              private EKS Auth endpoint
```

The API and worker still have no AWS role. They access PostgreSQL over its
verified-TLS connection, not EBS directly. A Kubernetes service account selects
the controller identity; an IAM role determines its AWS permissions. Neither
is the same as the PostgreSQL user.

## Three decisions to understand

### 1. Separate the storage role from the node and application

In `infra/aws/addons.tf`, the EBS add-on owns this association:

```hcl
pod_identity_association {
  role_arn        = aws_iam_role.ebs_csi.arn
  service_account = "ebs-csi-controller-sa"
}
```

The role trusts `pods.eks.amazonaws.com` for `sts:AssumeRole` and
`sts:TagSession`, but requires these request tags:

```hcl
"aws:RequestTag/eks-cluster-arn"             = aws_eks_cluster.lab.arn
"aws:RequestTag/kubernetes-namespace"       = "kube-system"
"aws:RequestTag/kubernetes-service-account" = "ebs-csi-controller-sa"
```

Keep Pod Identity session tags enabled. A different cluster, namespace or
service account must not satisfy this trust policy. Do not create a second
standalone association or a parallel Helm/IRSA installation for the same driver.
The agent uses the node role; its existing `AmazonEKSWorkerNodePolicy` supplies
`eks-auth:AssumeRoleForPodIdentity`.

The role uses AWS's `AmazonEBSCSIDriverPolicyV2`, with the canonical ARN
`arn:aws:iam::aws:policy/AmazonEBSCSIDriverPolicyV2`. This is **identity separation,
not complete resource-level least privilege**: the managed policy includes
account/region-wide discovery and CSI-tag-conditioned volume/snapshot actions,
not ownership restricted to this particular cluster. AWS can update that policy.
Review its current contents before deployment; customer-managed policy or a
permissions boundary would require separate design and real validation.

At this milestone the CNI retained bootstrap permissions on the node role.
[Phase 11E](15-cni-bootstrap.md) now separates them in the fresh-cluster
configuration; it is not an in-place permission-removal migration. The
CSI node component is privileged infrastructure in `kube-system`, not a pod in
the application's restricted namespace.

### 2. Give credentials a private path, without claiming all traffic is private

`infra/aws/pod-identity-network.tf` defines an interface endpoint for
`com.amazonaws.${var.region}.eks-auth` in the two private subnets. Private DNS
maps the normal service hostname to endpoint addresses. Its security group
accepts TCP 443 from the lab node group only; the endpoint policy permits only
the node IAM role and `eks-auth:AssumeRoleForPodIdentity`.

These controls do different jobs: the security group admits network connections;
the endpoint policy constrains authenticated AWS requests. The IAM role trust
then constrains which pod identity can obtain the storage role.

This is **not** the private Kubernetes API endpoint, an EC2 API endpoint or an
ALB. Image pulls and the driver's EC2 calls still use the existing NAT path.
The endpoint adds per-AZ hourly/data charges on top of existing lab costs.

### 3. Require real version selection, and fail on ownership conflicts

`addon_versions` deliberately defaults to empty strings. Plans reject empty,
`latest`, null or incomplete builds. Tests use synthetic `v0.0.0-eksbuild.N`
values to test wiring only, never as deployment recommendations.

Both add-ons use `resolve_conflicts_on_create/update = "NONE"`. Existing
installations must be inspected and reconciled deliberately, not overwritten.
The original storage milestone ordered nodes before the agent. Phase 11E
supersedes that ordering: endpoint → agent/CNI → nodes → DNS → EBS, avoiding a
CNI credential bootstrap dependency on already-ready nodes. The dependency
graph is not proof that credentials and volume operations work at runtime.

## Validation and limits

Run `make infra-check`. The pinned Terraform/provider validation and mocked
plans run without network access, host credentials or real AWS state. Cases
check the exact role trust, dedicated policy attachment, version pins, owned
association, private endpoint/subnets, SG paths and endpoint policy. Negative
cases reject missing/mutable/malformed pins and retain the account/approval
guards. The AWS manifest tests still check the workload's no-IAM service account,
encrypted gp3 storage and retained PVC policy.

These checks do **not** demonstrate that AWS accepts the selected versions or
policies, a controller starts, a PVC binds, or data survives a node replacement.
Formatting and successful mocked plans must not be described as an EKS deployment.

Verified locally on September 28, 2026:

- `make infra-check`: formatting/schema validation and **20 mocked plan cases**
  passed. Evidence: `.local/phase11/run.KF3PKaA6/check.log` and `results.xml`.
- `make platform-check`: **27 local-platform + 14 AWS-profile tests** passed,
  including the unchanged local manifests. AWS profile evidence:
  `.local/phase11b/config.6I4Eyw7r/`.
- `make check`: Go unit/race checks and vet passed. Live database/container
  integration was not rerun; no Go, SQL, database or workload manifests changed.
- No cluster mutations, authenticated AWS calls, publication or data deletion.

## Checklist for a future, separately approved cloud run

1. Agree on account, region, cost/duration/teardown, encrypted locked Terraform
   state and private operator access. No AWS commands below were run this phase.
2. Use `aws eks describe-addon-versions` for each of `eks-pod-identity-agent` and
   `aws-ebs-csi-driver`, with explicit `--region` and `--kubernetes-version 1.35`.
   Select exact compatible AMD64 builds; record the responses and date. For
   each selected build, use `aws eks describe-addon-configuration` to review
   its schema and Pod Identity service-account requirements. Verify the agent
   host's IPv6-loopback requirements; IPv4 VPC alone does not imply disabled IPv6
   on the host. Do not copy synthetic test pins into deployment inputs.
3. Review current IAM policy contents, endpoint support and existing add-on /
   association ownership. Review Phase 11E's fresh-cluster CNI identity and
   bootstrap order, plus the broader node/ECR and security-group compromises.
4. Only after approval, apply the reviewed infrastructure plan and verify both
   add-ons are ACTIVE, the agent is ready on each node, and the EBS controller
   has the intended association. Read events/logs for credential failures;
   do not fix them by granting storage permissions to all nodes.
5. Supply the approved image and four out-of-band Secrets. Apply the three
   workload stages with readiness/migration gates and a fresh-job check.
   Record the PVC/PV, volume AZ, encryption and actual successful job result.
6. Separately test same-AZ pod/node recovery with synthetic data and an approved
   disruption plan. Record timestamps and prove the original result survives.
   A single zonal volume is not multi-AZ availability. Retention is not backup;
   no snapshot controller, backup/restore procedure or custom KMS key is added.
7. Inventory retained EBS volumes before any separately authorized teardown.
   Removing an add-on does not erase a retained volume or stop its charges.

## Next bounded milestone

Prepare internal ALB/controller identity and private DNS integration offline,
with one owner for each resource. Keep application authentication, private
operator access, approved cloud execution and real failure evidence explicit
as unfinished work. Do not change the local-only CLI into a cloud deploy tool.

## Five-minute learning exercise

Start in `infra/aws/addons.tf`, then follow `pod-identity-network.tf`:

- Explain why the API service account cannot use the EBS role.
- If the agent cannot reach EKS Auth, which network and identity checks differ?
- If credentials work but EC2 times out, why might NAT matter?
- Why can a pending PVC be a driver/permission/topology failure, not a Go bug?

Answer these before claiming ownership in an interview. A truthful description:
"I prepared and offline-tested a dedicated Pod Identity role and private
credential path for an EKS storage driver; live cloud recovery is still pending."

## Primary references reviewed

- [AWS EBS CSI driver requirements](https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html)
- [Pod Identity trust policies](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-role.html)
- [Pod Identity session tags](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-abac.html)
- [Pod Identity agent prerequisites](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html)
- [EKS private interface endpoints](https://docs.aws.amazon.com/eks/latest/userguide/vpc-interface-endpoints.html)
- [Canonical EBS CSI V2 policy ARN and permissions](https://docs.aws.amazon.com/aws-managed-policy/latest/reference/AmazonEBSCSIDriverPolicyV2.html)
- [Pinned AWS provider 6.66.0 add-on schema](https://github.com/hashicorp/terraform-provider-aws/blob/v6.66.0/website/docs/r/eks_addon.html.markdown)
