# First live AWS lab

Date: September 28, 2026. Status: **deployed and end-to-end verified** in us-east-1.
This is the first actual cloud application run, not just a mocked plan or an
infrastructure-only deployment. Scheduled teardown has **not yet been observed**.

The private endpoint is `https://jobs.durable-jobs-lab.internal`. It is reachable
from the approved in-VPC operator, not from an ordinary laptop browser. There
is no frontend, public endpoint or application authentication.

## Deployment choices

- US East (N. Virginia), one `t3.medium` managed EKS worker, one temporary
  `t3.micro` SSM operator, one NAT gateway. The VPC and internal load balancer
  span two AZs; one worker and one PostgreSQL instance are **not highly available**.
- Kubernetes 1.35, managed networking/storage add-ons at explicitly checked
  builds. No Auto Mode, managed database, public cluster endpoint or SSH port.
- Static non-root Go image built for Linux/AMD64, published to private ECR and
  referenced by digest. API, migration and worker share that exact image.
- API TLS terminates at an internal ALB. Its issuing CA is trusted explicitly by
  the verifier, not added globally to the laptop. ALB-to-API traffic is private
  HTTP; PostgreSQL clients independently use hostname-verified TLS.
- Synthetic data only. Database passwords and certificate keys stay in a
  private encrypted deployment bundle and Kubernetes Secrets, never Terraform
  variables, source control or command output. Remote bundle integrity is checked.
- A dedicated role operates Kubernetes. The private operator host may assume
  it; the API/worker service account has no AWS role or automatic API token.
- The deployment role is separate from the operator. Its temporary network
  provisioning actions are regional, not fully resource-level ABAC. Exact
  project IAM role names are allowlisted. A reviewed fresh-resource plan avoids
  reusing or modifying the old delivery application's VPC. This is a lab
  trade-off, **not a production least-privilege reference**.

## Independent expiry

The first live clock started at **21:11:58 UTC on September 28**. Cleanup begins
at **02:11:58 UTC on September 29** (10:11 p.m. Eastern on September 28), leaving
one hour before the six-hour deadline, **03:11:58 UTC** (11:11 p.m. Eastern).

An EventBridge rule invokes `platform_tools/aws_lab_cleanup.py` every five
minutes. Before expiry it does nothing. Afterwards it retries until separately
observed cost-bearing compute/database/network resources are absent. It runs
outside the lab VPC, so removing NAT or the operator cannot stop its access.

- Every resource needs this project's **Project + unique LabRun** ownership.
- Managed nodes are deleted through EKS, not killed underneath an autoscaler.
- The controller waits for the node group to disappear before deleting its ALB.
- Attached volumes are not forcibly deleted. The synthetic PostgreSQL volume
  is deleted only after becoming detached/available; this deletion is authorized.
- NAT, its released address, paid endpoints and the exact new private DNS zone
  are included, including partially completed infrastructure deployments.
- Delete-request acceptance is not completion. Remaining resources are checked
  again on the next invocation. Errors propagate instead of being called success.

This is a best-effort failure-aware timer, **not a guaranteed dollar cap**.
CloudWatch alarms cover function errors and resources remaining beyond the
deadline. The SNS email subscription was confirmed later that evening; no
test alert or end-to-end alarm delivery is claimed.

The cleanup deliberately retains state/history, images, logs, IAM definitions
and free VPC scaffolding. Small storage/monitoring charges may remain; a final
reviewed Terraform teardown can remove retained scaffolding. It never touches
the original local database volume or the old application's saved backup.

## Verification checklist

- [x] Real metadata preflight: identity, support/version compatibility, image,
  certificate, state bucket and ELB service role.
- [x] 37 mocked Terraform plans, dependency checks and local code/tooling tests.
- [x] Real state-lock collision test from the preceding guardrails milestone.
- [x] Separate scheduled IP cleanup canary from the preceding milestone.
- [x] Independent lab expiry function/rule armed and read-only invocation tested.
- [x] Private SSM operator online; Linux tools and downloaded bundle checksums verified.
- [x] EKS node and all five managed add-ons healthy.
- [x] Persistent PostgreSQL ready, then migration completed, then API/worker ready.
- [x] Controller creates an internal ALB with the expected subnets/security group/tags.
- [x] Private DNS alias published only after validating the actual ALB.
- [x] Trusted HTTPS verifies completion, idempotent replay, conflict and attempt history.
- [ ] Scheduled full-lab cleanup actually observed (future event; never infer from setup).

Private account-specific operational records live in `.local/aws-live/`. Do not
publish that directory: it includes temporary TLS material and deployment Secrets.

## Measured live result

The verifier ran on the private SSM operator. It trusted only the supplied lab
CA for this request path; certificate validation was not disabled. DNS, TCP,
database readiness and the full job checks passed. Synthetic job
`19eb101f-10c8-480a-b1f5-7e031886eec7` finished successfully in one attempt:

- Reusing its idempotency key returned the same completed job and result.
- Reusing the key with different input returned HTTP 409.
- Attempt history contained one finished, successful attempt.

Both API targets were healthy behind the ALB. The database's 10 GiB gp3 volume
was bound, encrypted and tagged for this exact cleanup run. The operator and
worker root disks were also encrypted. All five managed add-ons were ACTIVE
with no reported health issues; all running application/controller pods were ready.

This is functional evidence from one synthetic job, **not** a throughput/SLO,
exactly-once execution, high-availability, backup/restore or failure-recovery claim.
API TLS terminates at the ALB; the backend hop is private HTTP.

The temporary provisioning write policy was removed after DNS publication. All
versions of the cloud transfer bundle containing Secrets were deleted; the local
copy is retained privately. Application Kubernetes Secrets and the encrypted
operator disk remain for the active lab and expire with its teardown. The
cleanup function/rule, exact timer and deployed code hash were rechecked.
IAM simulation allowed all 12 required cleanup delete actions against the actual
resource ARNs with this run's tag context. This is a permissions check, not a
real teardown result or a guarantee against runtime dependency failures.

At 21:41 UTC, AWS Budgets reported $2.202 actual and $8.042 forecast for the
**whole account's two-day budget window**. These delayed values are not the
final incremental cost of this lab. The SNS cleanup-failure email subscription
was initially pending. On the owner's follow-up authorization, its confirmation
message was found in Gmail Spam and the exact project's subscription was
confirmed. AWS subsequently reported `PendingConfirmation=false`. Future alerts
may also be filtered into Spam; no test alert was sent. Private evidence is in
`subscription-confirmed.json` and `subscription-confirmed.png`.

Local checks passed: `make check` (Go race tests and vet), `make platform-check`
(67 primary Python tests plus 24 AWS configuration tests), and `make infra-check`
(37 mocked plans, six graph tests and 17 dependency paths). These checks are
separate from the real AWS verification above.

Evidence index (private, not for publication): `cluster-health.json`,
`cloud-validation.json`, `ssm-install-result.json`, `ssm-status-result.json`,
`ssm-verify-result.json`, `cleanup-inspect-final.json`, `finalization.json`, and
`cleanup-write-iam-simulation.json`, all under `.local/aws-live/`. Terraform
outputs and backend configuration are saved there; versioned state itself is
in the private S3 backend.

Do not blindly rerun the setup scripts: this run has a fixed expiry, its transfer
object has been removed, and its temporary provisioning access has been retired.
A later lab needs a fresh budget/time review, state refresh and scoped access.

## Deployment issues found and corrected

1. The initial node-group creation failed because the deployment role could not
   read the existing AWS-managed node-group role. Added `iam:GetRole` for three
   exact service-linked role ARNs, refreshed the plan and resumed from remote
   state. The cluster and VPC were not recreated.
2. The EBS managed add-on failed with a duplicate `AWS_REGION` environment key.
   EKS already supplies it. Removed the extra setting, added an offline regression
   assertion and replaced **only the failed add-on**, before any application
   database existed. No user database or volume was removed.

These are deployment/debugging observations, not controlled recovery exercises.
The subsequent failure-injection milestone is now documented with measurements
in [Phase 12](19-cloud-reliability.md). The initial deployment observations above
remain distinct from those controlled drills.

## Small code detail worth understanding

The EBS driver adds ownership tags to every database volume it provisions:

```hcl
controller = {
  extraVolumeTags = { Project = var.name, LabRun = var.lab_run_id }
}
```

Why this matters: deleting a Kubernetes StatefulSet does not delete a retained
EBS volume. The independent cleanup process must be able to identify exactly
which detached volume is authorized for removal, even after Kubernetes is gone.
