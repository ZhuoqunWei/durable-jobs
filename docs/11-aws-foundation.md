# Phase 11A: an offline-tested AWS foundation

Current bootstrap follow-up: [Phase 11E](15-cni-bootstrap.md) now supplies
separate CNI credentials and explicit managed networking/DNS add-ons. This is
fresh-cluster configuration, not an in-place migration or live AWS proof.

Follow-up: [Phase 11C](13-ebs-pod-identity.md) adds storage-driver/Pod Identity
configuration and a private EKS Auth endpoint. The scope and evidence below
describe the original foundation milestone; no AWS deployment has occurred.

Date: September 28, 2026

Scope: Terraform source for a private AWS network and EKS cluster, plus repeatable
configuration tests. **No AWS resources have been created. Durable Jobs is still
running only in the separate local lab.** Do not call this a deployed EKS service.

Follow-up: [Phase 11B](12-aws-application-profile.md) now supplies the AWS workload
profile, a StorageClass request and local TLS/DB verification. The driver/IAM,
cloud secrets, private access and actual deployment prerequisites below still apply.

## What this adds, in plain language

The Go code answers **what happens to a job**. This Terraform configuration
answers **where the service could run, and which network paths would exist**.
It does not change the queue, submission API, retries or existing database.

- A dedicated VPC (the lab's network), divided across two Availability Zones.
- Two public subnets for outbound gateways and two private subnets for workers.
- An EKS control plane with its Kubernetes API reachable only privately.
- One managed EC2 worker initially, with a configured upper size of two.
- Explicit security groups, node roles and a named administrator role.
- A private Route 53 DNS zone, with **no application address yet**.
- Seven-day control-plane log retention, including audit logs.
- A credential-free Docker check and GitHub CI job for this configuration.

Terraform 1.16.4 and AWS provider 6.66.0 are pinned. The official Terraform image
is digest-pinned; the provider lock includes Linux AMD64 and ARM64 checksums.
These are reviewed versions, not an instruction to auto-upgrade silently.

## Run the safe check

From the project directory, run:

```sh
make infra-check
```

Docker is required; a host Terraform installation and AWS credentials are not.
The build downloads public, checksum-verified dependencies. Its context is
allowlisted: no `.env`, real `.tfvars`, state, home directory or AWS credentials.
The actual format/validation/test container then runs with **network disabled**,
a read-only image, no capabilities, no Docker socket and an ephemeral work area.
Only its new report directory is mounted writable. No authenticated AWS command
is part of this workflow.

Reports live under `.local/phase11/run.*/`:

- `build.log`: image/provider installation result.
- `check.log`: format, validation and mocked test results.
- `versions.json`: exact Terraform/provider versions.
- `results.xml`: individual test results, including expected rejection cases.

A failing command returns nonzero. Earlier failed reports are retained rather
than replaced. The test container is removed automatically; the locally built
image/cache and reports remain. The workflow is prepared for GitHub, but there
is still no configured remote, remote CI run or enforced branch protection.

Verified locally on September 28, 2026 (Linux ARM64 container): formatting and
schema validation passed; all **12 mocked plan cases passed, none skipped**.
Evidence: `.local/phase11/run.U4S7GvgJ/`. Existing Go race/vet checks, 27 Python
tests, Kustomize rendering, shell syntax and GitHub workflow validation also
passed. The AMD64 provider is checksum-locked but the future GitHub runner has
not executed these checks. No database migration or cloud integration test was
needed or run for this configuration-only milestone.

## Three decisions worth understanding

### 1. Private does not mean disconnected

In `infra/aws/eks.tf`:

```hcl
endpoint_private_access = true
endpoint_public_access  = false
```

This is the **Kubernetes administration endpoint**, not the Durable Jobs API.
Workers can reach it privately. An ordinary laptop cannot connect to it just
because it has the correct IAM role. A future operator needs both an authorized
private network path and the approved role. This milestone creates an operator
security group, not a runner, VPN, bastion or session.

Private nodes still need outbound access for registries and AWS APIs. Their
default route points to a NAT gateway; the gateway sits in a public subnet whose
route points to the internet gateway. This outbound path does not create an
unsolicited inbound path to workers. A route decides **where packets go**;
a security group decides **which traffic is allowed**.

### 2. A cheaper lab is not a highly available platform

In `infra/aws/network.tf`, each private route selects its NAT:

```hcl
nat_gateway_id = aws_nat_gateway.lab[var.nat_mode == "per_az" ? each.key : var.availability_zones[0]].id
```

`single` uses one gateway, so the second AZ shares its failure domain and may
incur cross-AZ transfer. `per_az` creates one gateway in each AZ and routes each
private subnet through its own. Both cost money; neither is selected for a real
account yet. Two AZs do not make the whole app highly available: the initial
node count is one, and this step does not deploy or replicate PostgreSQL.

`max_size = 2` is only a node-group setting, not a spending cap, an autoscaler,
or a guarantee that EKS rolling updates cannot temporarily add extra capacity.

### 3. A passing mock is configuration evidence, not cloud evidence

In `infra/aws/tests/foundation.tftest.hcl`:

```hcl
mock_provider "aws" {
  override_during = plan
  # Fictional account and computed resource values are supplied below.
}
```

Every test uses `command = plan`. The tests assert route choices, private
endpoints, subnet placement, security-group references, metadata settings,
encrypted disks, access configuration and deployment guard failures. Related
subnets/security groups/NAT gateways use distinct fake IDs so accidental swaps
can fail assertions. Seven rejection tests accompany five positive cases.

These tests do **not** prove real IAM permissions, AZ availability, node joining,
DNS reachability, image pulls, workload success, a functioning ALB, or actual
failure recovery. Mocks do not execute the provider's real AWS operations.

## Security and operational limits—read before any deployment

- The API lacks authentication. Keep the future workload private; networking
  alone does not authorize individual application users.
- The launch template supplies only the explicit node security group. EKS
  therefore does not automatically attach its default primary cluster group
  to these nodes. Nodes allow same-group traffic, control-plane kubelet/webhook
  ports, and outbound HTTPS. This is not namespace/pod isolation.
- EKS still creates a primary control-plane security group with AWS-managed
  defaults. The additional group does not subtract its permissions. Do not
  describe this as fully least-privilege control-plane egress.
- Outbound TCP 443 permits arbitrary destinations, not an allowlist of AWS
  APIs. Restricting domains/services needs a separate design. Security groups
  do not filter the Amazon-provided DNS resolver.
- No SSH key, SSH ingress or public worker IP is configured. IMDSv2 is required,
  with hop limit one. This is not a complete container escape/credential boundary.
- The original bootstrap node role included the CNI policy. Phase 11E now puts
  it on a dedicated CNI role; Phases 11C/11D supply storage/ALB controller roles.
  Do not put their permissions on the node role. The application has no AWS role.
- The explicit existing operator role receives **cluster administrator** access.
  It must be selected deliberately. Its trust/assumption permissions are outside
  this configuration. There is no implicit cluster-creator administrator.
- Phase 11E replaces unmanaged networking/DNS bootstrap with explicitly managed
  add-ons. Their five pins still need real compatibility review; pinning
  Terraform does not pin every AMI/add-on.
- Version 1.35 and STANDARD support were selected for this checkpoint. Recheck
  support dates before deployment. STANDARD is not a promise of indefinite
  version freezing; AWS may upgrade a cluster after its support window.
- The worker root disks are encrypted and disposable; they are not database
  storage or backups. No EBS CSI driver, StorageClass or durable AWS database is
  provisioned yet.
- Logs have bounded retention, but logging, EKS, EC2, EBS, NAT, public IPv4,
  Route 53 and data transfer can all incur charges. No price or free-tier claim.
- The approval variable/account checks reduce mistakes, not malicious use.
  Real IAM controls and explicit user authorization remain necessary.
- Real state would contain infrastructure details. No remote backend is
  configured yet; choose encrypted, locked state storage and recovery procedures
  before applying. State/plan/real tfvars files are ignored locally, not made safe
  merely by `.gitignore`. Never commit them or credentials.

## What remains before an actual AWS run

1. Approve the exact account, region, AZs, IAM identities, current cost estimate,
   maximum lab duration, data policy and separately authorized teardown plan.
   A billing alert is not a hard spending limit.
2. Review the real plan from a suitably scoped environment; check account,
   address overlap, quotas, EKS version support and private operator access.
3. Set up locked/encrypted state and an approved in-VPC management/probe runner.
   Do not open the Kubernetes endpoint to the world to work around access.
4. Add pod-scoped identities, the storage decision/driver and secrets management.
   Local kind credentials/storage assumptions must not be copied to AWS.
5. Build/publish an explicitly approved AMD64 or multi-architecture application
   image. A laptop's ARM64-only image cannot run on these x86 nodes.
6. Add the AWS Load Balancer Controller, its role and an **internal** ingress.
   The controller owns the ALB/targets; Terraform must not create a competing
   ALB. Add the private DNS alias only after the real load balancer exists.
7. Deploy migrations and the application through an AWS-specific overlay, then
   run the existing synthetic job/replay checks from inside the VPC. The current
   Python lab CLI intentionally refuses nonlocal contexts; do not disable that
   safeguard to reuse it. Add a separately scoped AWS workflow.
8. Record actual failures/recovery and remove only the approved disposable
   resources afterward. This document does not authorize that work automatically.

No `apply`, AWS credential setup or automatic cloud deployment is added to Make
or CI. The next no-spend learning exercise is to trace one private route and
predict what breaks if its NAT target or API access setting is changed.

## Interview boundary

Accurate now: "Added Terraform configuration for a private
two-AZ EKS lab, with credential-free tests for routing, access boundaries and
deployment guards."

Not yet accurate: "Deployed Durable Jobs on AWS," "operated a highly available
EKS platform," "implemented ALB routing," or "proved cloud recovery."

## Primary references

- [Terraform provider mocking and plan-time overrides](https://developer.hashicorp.com/terraform/language/tests/mocking)
- [EKS launch-template security groups and metadata](https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html)
- [EKS default security groups and required traffic](https://docs.aws.amazon.com/eks/latest/userguide/sec-group-reqs.html)
- [Private EKS cluster access and outbound dependencies](https://docs.aws.amazon.com/eks/latest/userguide/private-clusters.html)
- [EKS access entries](https://docs.aws.amazon.com/eks/latest/userguide/access-entries.html)
- [EKS versions in standard support](https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions-standard.html)
