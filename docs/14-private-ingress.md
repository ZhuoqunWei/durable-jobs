# Phase 11D: internal ALB and private DNS configuration

Date: September 28, 2026

**Implemented offline:** dedicated controller identity, narrowly defined network
paths, a pinned Helm controller profile, HTTPS Ingress and guarded Route53 alias
publication. **Not deployed:** no controller, load balancer, certificate or DNS
record has been created in AWS. No paid cloud resources or existing local data
were changed. This is one configuration milestone, not an operational EKS demo.

## Follow a request

```text
Approved in-VPC caller
  → resolves jobs.durable-jobs-lab.internal in the private Route53 zone
  → HTTPS 443 to internal ALB (caller must trust its certificate)
  → HTTP 8085 to an API pod's VPC IP
  → verified TLS to PostgreSQL → worker processes the durable job
```

DNS finds an address; the ALB routes HTTP requests; the queue and worker process
the job. These are different responsibilities. A successful DNS lookup or
`/readyz` response does not prove that a worker completes a fresh job.

## Ownership: one owner per resource

| Component | Owner |
|---|---|
| VPC, security groups/rules, controller IAM association | Terraform |
| Controller Deployment, ServiceAccount, RBAC, webhook, IngressClass | Pinned Helm chart, in a future approved installation |
| API Ingress intent | Separate rendered application manifest |
| ALB, listener, rules and target groups | AWS Load Balancer Controller |
| Private DNS zone and optional alias | Terraform |
| ACM certificate and caller CA trust | Separately approved operator workflow; not provisioned here |

There is no Terraform `aws_lb` resource and no ExternalDNS controller. Managing
the same ALB through Terraform and Kubernetes would create competing owners.
Controller SG management is disabled; the Terraform rules are authoritative.

## Decisions, using the actual configuration

### Private traffic and TLS termination

The Ingress source in `deploy/kubernetes/aws/ingress/ingress.yaml` includes:

```yaml
alb.ingress.kubernetes.io/scheme: internal
alb.ingress.kubernetes.io/target-type: ip
alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS":443}]'
alb.ingress.kubernetes.io/manage-backend-security-group-rules: "false"
alb.ingress.kubernetes.io/healthcheck-path: /readyz
```

Explicit private subnet and security-group IDs come from the foundation output.
Only the existing operator SG is admitted on 443; the ALB can initiate only TCP
8085 to the node/pod SG. The API Service remains ClusterIP, not NodePort. The
existing VPC CNI gives API pods VPC IPs; separate pod security groups or another
CNI would require a network-rule review.

**TLS ends at the ALB.** Its backend is private HTTP, not end-to-end TLS or mTLS.
The API still has no application authentication; private networking is not user
authorization. This is a synthetic, operator-only lab, not a public service.
The node-mesh rules still permit same-group traffic, so this does not establish
namespace/pod isolation.

Rendering requires an explicit same-account/region ACM certificate ARN. For
the `.internal` hostname, plan a suitable imported/private certificate and a
trusted issuing CA on the caller. No public certificate is requested and no
paid Private CA is created. Check SAN, expiry, key suitability and renewal in
the approved workflow; matching an ARN pattern proves none of these. Imported
ACM certificates need operator-managed renewal. Never use `curl -k` as TLS proof.

Health checks require `/readyz` to return 200, not merely a live process. ALBs
can fail open when all targets are unhealthy, so target health is not a
fail-closed security control. Fresh-job verification remains a separate step.

### A controller role, not permissions on every node

`infra/aws/alb.tf` binds the role to exactly this cluster's
`kube-system/aws-load-balancer-controller` service account via Pod Identity.
The controller still needs Kubernetes API/RBAC access to watch and reconcile;
the application's service account still has no AWS role or API token.

The vendored, SHA-checked v3.5.0 IAM reference is reduced before attachment:

- Keep discovery and required ALB/target/listener operations.
- Remove IAM, SG mutations, DNS, WAF/Shield/Cognito and unused capacity operations.
- Restrict ELB resource ARNs to the approved account/region and application-LB
  forms; strengthen existing cluster-tag presence checks to exact name matches.
- Explicitly deny internet-facing load-balancer creation.

This custom policy needs live IAM/reconciliation validation. Discovery remains
broad, and some listener operations have region/account-wide scope rather than
cluster-tag restrictions. The chart's RBAC is also broader than the watched
application namespace. This is not a production least-privilege claim. The
operator must verify that the ELB service-linked role already exists; the
controller cannot create it. [Phase 11E](15-cni-bootstrap.md) now moves CNI
permissions off nodes in the fresh-cluster configuration; live proof is pending.

The controller has explicit region and VPC settings, avoiding a need to weaken
the node's IMDS hop limit. Service/NLB and Gateway API controllers, shared
IngressGroups, WAF/Shield, certificate creation and backend SG management are
disabled. Only the custom ingress class in `durable-jobs-aws` is watched.

### Publish DNS only after there is a real ALB

`published_alb_arn` defaults to null. The initial foundation plan does not look
up an ALB or publish an alias. After an approved controller installation creates
the ALB, supply its actual ARN in a separately reviewed plan. Terraform reads
the ALB and checks its internal scheme, type, VPC, private subnets, exact SG and
cluster/Ingress ownership tags **before writing the DNS record**.

The private alias uses the returned ALB DNS name and canonical hosted-zone ID,
not guessed regional constants. It refuses to overwrite an unrelated existing
record. Tag/configuration checks do not prove target health or certificate
validity; the later live verification must establish those separately.

## How to inspect and test without AWS

```sh
make infra-check
make platform-check
.local/aws-tools/bin/python platform_tools/aws_ingress.py
```

The final command prints a non-deployable plan and prerequisites. `make
platform-check` includes the ingress tests; `make aws-config-check` runs only
the AWS profile tests. `make alb-tools` downloads/builds public dependencies
inside this repo: Helm module v3.22.0 and chart 3.5.0 with an upstream-index
SHA-256 check. The controller image is pinned to a verified multi-architecture
manifest digest. These downloads do not install a cluster release.

Tests run actual `helm template` with a temporary empty home, no inherited AWS
environment and `/dev/null` kubeconfig. They inspect the resulting Deployment,
service account, class, CRDs, feature flags and image. Helm's synthetic webhook
certificate stays in memory; rendered private keys are not saved in CI evidence.
Terraform's mocked plans additionally run in a network-disabled container.

Verified locally on September 28, 2026:

- Terraform format/schema checks and **31 mocked plan cases** passed. Final
  evidence: `.local/phase11/run.1QgydKTf/check.log` and `results.xml`.
- **27 local-platform + 24 AWS-profile tests** passed, including real Helm
  rendering and rejection cases for public ingress, routing overrides, identity
  changes and incorrectly typed booleans. Final AWS evidence:
  `.local/phase11b/config.YZYB8EFA/`.
- Go race/unit checks, vet, Staticcheck and shell syntax checks passed. No Go
  or SQL changed; live database/container integration was not rerun this phase.
- No cluster mutations, authenticated AWS API calls, image publication or data
  deletion. Public dependencies were fetched; cloud acceptance remains untested.

The renderer supports separate `controller-values` and `ingress` stages:

```sh
.local/aws-tools/bin/python platform_tools/aws_ingress.py render \
  --stage ingress --config /path/to/reviewed-foundation-ingress.json \
  --certificate-arn 'arn:aws:acm:REGION:ACCOUNT:certificate/REVIEWED-UUID'
```

The input file must contain the non-secret value of Terraform's `ingress_config`
output, not a whole state file. The example intentionally cannot pass as real
configuration. Rendering checks syntax and consistency, not whether supplied
IDs actually exist in the correct VPC. There is deliberately no deploy command.
Keep the original three workload stages and the local-only CLI unchanged.

## Future approved deployment order and missing proof

1. Complete account/budget/state/private-access review and review Phase 11E's
   fresh-cluster bootstrap. Select compatible EKS add-on versions; verify the existing ELB
   service-linked role. Review controller IAM, pinned versions and chart RBAC.
2. Apply the reviewed foundation with `published_alb_arn = null`; verify Pod
   Identity and EBS. Produce the approved application image and four Secrets.
3. Render reviewed controller values, install the pinned chart from an
   authorized private runner, and verify controller/webhook readiness. Chart
   upgrades need a deliberate CRD and webhook-certificate review; `helm template`
   output must not be blindly reapplied on every change.
4. Deploy workload infra → successful migration → API/worker. Then apply the
   separately reviewed Ingress with the trusted ACM certificate.
5. Verify internal ALB, listener/certificate, target health and ownership. Supply
   its actual ARN for the separate DNS plan. Verify private resolution and TLS
   from the authorized operator and rejection from a disallowed network source.
6. Submit synthetic work through the private hostname and verify completion,
   idempotent replay and history. The existing local verifier intentionally
   refuses non-local targets; it has not been weakened to run against AWS.
7. Separately authorize failure/recovery exercises and teardown. Remove the
   alias before deleting the Ingress; let the controller clean up its ALB and
   finalizers before removing the controller or its role. Inventory retained
   EBS/PVCs; do not use an indiscriminate `terraform destroy` shortcut.

None of these cloud operations were performed. Real AWS reachability, IAM,
certificate trust, storage binding/recovery, ALB reconciliation and rollback
remain unproven. GitHub publishing and merge protection remain inactive.

## Interview relevance and your next exercise

This maps directly to CLEAR's AWS networking, Kubernetes, Terraform and Python
requirements. The honest claim is **configured and offline-tested**, not
"operated production infrastructure" or "deployed to EKS."

Spend 15 minutes tracing the request above through `ingress.yaml`, `alb.tf` and
`aws_ingress.py`. Explain: why a private DNS record does not create a route; why
the controller needs AWS permissions but the API does not; and why a healthy
ALB does not prove workers are processing jobs. Then change one bad input in a
test and explain which safeguard rejects it.

Follow-up: [Phase 11E](15-cni-bootstrap.md) implements CNI identity and bootstrap
ordering offline. Next: finish cloud-readiness prerequisites and the AWS
diagnostic/verification path. Actual cloud
execution still requires your account, budget and explicit approval. Avoid
adding another framework while these claims remain unverified.

## Primary references reviewed

- [Controller installation and identity](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/deploy/installation/)
- [Ingress settings and security-group ownership](https://kubernetes-sigs.github.io/aws-load-balancer-controller/latest/guide/ingress/annotations/)
- [Pinned controller chart index](https://aws.github.io/eks-charts/index.yaml)
- [Pinned v3.5.0 IAM reference](https://github.com/kubernetes-sigs/aws-load-balancer-controller/blob/v3.5.0/docs/install/iam_policy.json)
- [ELB IAM scheme restrictions](https://docs.aws.amazon.com/elasticloadbalancing/latest/userguide/security_iam_service-with-iam.html)
- [Route53 aliases for load balancers](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/routing-to-elb-load-balancer.html)
- [ALB health checks and fail-open behavior](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/target-group-health-checks.html)
- [Imported ACM certificates and renewal](https://docs.aws.amazon.com/acm/latest/userguide/import-certificate.html)
