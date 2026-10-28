# Cloud guardrails: first live bootstrap

Date: September 28, 2026. Historical bootstrap milestone: at its completion,
the Go API, workers and database were not yet running in AWS. The subsequent
[live deployment](18-live-aws-lab.md) now records successful private EKS job
verification and the full-lab expiry configuration. Remaining-gate statements
below describe the earlier bootstrap checkpoint, not current deployment status.

## Verified so far

- A $50 **new-spending** allowance is authorized, separately from the previous
  monthly account bill. A custom-window cost budget covers September 28 00:00
  UTC through September 30 00:00 UTC. Five actual-cost email notifications are
  configured at amounts greater than $5, $10, $25, $40 and $50. The AWS API
  confirmed all five subscriptions; actual email delivery has not been tested.
- That budget includes **all account spending during the short window**, not
  only project tags. This conservatively includes other same-day costs and
  avoids relying on delayed tag activation. Credits/refunds do not hide gross
  usage. Alerts are delayed notifications, **not a hard cap or shutdown**. AWS
  deletes the custom budget at its end; a later session needs a reviewed window
  and the remaining allowance, not an automatically renewed $50 permission.
- A dedicated S3 state bucket has versioning, AES256 default encryption, all
  four public-access blocks, bucket-owner-enforced ownership and TLS-only access.
  It is retained independently from disposable compute. Storage/request charges
  are possible; it is not claimed to be free.
- The `durable-jobs-lab` CLI profile assumes a dedicated operator role. Only the
  already-authorized same-account IAM user is trusted. No access key was created
  or changed. The role can inspect selected metadata and access two exact state
  keys; it can delete their lockfiles, **not their state objects**. It has no
  general infrastructure provisioning, IAM administration or PassRole permission.
- A real Terraform 1.16.4 probe wrote encrypted, versioned synthetic remote
  state using that role. An intentionally occupied lock caused Terraform to
  reject a plan; removing only that owned probe lock restored a no-change plan.
  The probe has no AWS provider resources. No application Terraform state or
  original local database data was modified.

The backend template is `infra/aws/backend.s3.tf.example`. The non-secret,
account-specific backend configuration is kept under `.local/aws-bootstrap/`.
It is for a future isolated live deployment directory. The existing offline
Terraform checks remain credential-free and never connect to this bucket.
Only the default Terraform workspace is supported by the narrow state policy.

## Cleanup canary: the precise boundary

`platform_tools/aws_cleanup_canary.py` is a small Lambda handler, **not a full
EKS teardown function**. An AWS one-time schedule invokes it to release one newly
allocated, unused Elastic IP after its short expiry. It checks:

1. The caller account, region and exact allocation ID.
2. Matching Project, Purpose and unique LabRun tags.
3. That the address is not associated with an instance or network interface.
4. That the approved expiry has passed.

IAM also permits release only of that exact tagged address. The scheduling role
can invoke only that function; its trust is scoped to the dedicated schedule
group and account. The schedule deletes itself after completion. The function
has no EKS/NAT/database delete permissions. Logs retain seven days of evidence.
This tests laptop-independent scheduling and a real scoped deletion, not the
dependency ordering or coverage of a six-hour lab teardown.

**Live result:** at approximately 20:55:57 UTC, the scheduled Lambda logged the
release request. A separate EC2 read confirmed the exact allocation no longer
existed, and the one-time schedule had deleted itself. No local invocation of
the function was used. The test address is released and is not recoverable as
a reserved address; no data was stored on it.

The one-off Lambda and both canary IAM roles were then removed. CloudWatch logs
retain seven days of evidence; the local report is preserved. Private evidence
is in `.local/aws-bootstrap/canary-evidence.json`, alongside state and budget
reports. The complete six-hour **cluster** lab timer remains **unarmed**.

Verification completed: `make check`, `make platform-check` (59 primary Python
tests plus 24 AWS configuration tests), and `make infra-check` (36 mocked plans,
six graph tests and 17 dependency checks). The state probe and scheduled IP
cleanup additionally ran against real AWS. Neither is live EKS recovery proof.

## Next bounded milestone, before paid cluster creation

1. Implement and test full teardown coverage for the exact lab: node group,
   controller-created load balancer, retained synthetic database volume, cluster,
   NAT/EIP, endpoints and private DNS. Preserve state/reports, retry dependency
   failures, and verify absence rather than merely accepting delete requests.
2. Establish the approved private operator network path and a separate scoped
   infrastructure provisioning role. Do not make the cluster endpoint public
   or turn the operator into an administrator to bypass these requirements.
3. Build/publish the AMD64 application image, prepare private TLS trust and
   out-of-band database secrets, review add-on defaults and a real plan.
4. Arm the six-hour cleanup **before** creating paid lab infrastructure, then
   deploy and demonstrate a submitted synthetic job completing through private
   HTTPS. Record results and clean up within the agreed session.

No extra frameworks or application features are necessary for this milestone.

## What to explain in an interview

The alert tells you spending has happened; the timer initiates cleanup. Neither
alone guarantees a hard dollar cap. State locking prevents two infrastructure
writers from racing; versioning provides recovery history. A narrow cleanup role
limits the damage if its code or scheduling is wrong.

The ownership check in the actual handler is intentionally easy to read:

```python
if any(tags.get(key) != value for key, value in expected.items()):
    raise ValueError("Ownership tags mismatch; refusing cleanup")
```

Ten added offline tests cover spending-window semantics, state-delete scope,
trust, early/wrong-account cleanup, ownership mismatch, in-use resources and
idempotent not-found handling. They run in the default `make platform-check`
suite. Passing these tests is not live EKS recovery evidence.

References: [AWS budget API](https://docs.aws.amazon.com/cli/latest/reference/budgets/create-budget.html),
[Terraform S3 state/lock permissions](https://developer.hashicorp.com/terraform/language/backend/s3),
[schedule auto-deletion](https://docs.aws.amazon.com/scheduler/latest/UserGuide/managing-schedule-delete.html),
[scheduler trust scoping](https://docs.aws.amazon.com/scheduler/latest/UserGuide/cross-service-confused-deputy-prevention.html).
