# Lab teardown stalled on a dependent EKS permission

Date: September 29, 2026 UTC. Scope: the approved, synthetic Durable Jobs AWS lab.

## Impact and detection

The independent cleanup started after 02:11:58 UTC. By 02:26:05, read-only
inventory checks showed that worker/operator instances, volumes, the load
balancer, NAT and the other scoped network cost resources were absent, but the
EKS control plane remained billable. The cleanup-error alarm was ALARM.

## Diagnosis

CloudWatch logs showed `AccessDeniedException` during `DeleteAddon`: the cleanup
role lacked `eks:DeletePodIdentityAssociation` for an add-on's identity binding.
Permission to delete an add-on was therefore insufficient for this configured
add-on lifecycle. An earlier transient target-group dependency error had cleared
and was not the remaining blocker. Verifier read permissions were working.

## Response and evidence

- Reported the missing permission and requested owner approval; the read-only
  heartbeat did not silently broaden the cleanup role's permissions.
- After approval, verified the account, role, cluster/run tags, unchanged cleanup
  code hash/configuration and the actual identity association list.
- Added a separately named, validated inline policy granting only
  `eks:DeletePodIdentityAssociation` on the three exact associations in this lab,
  scoped to the region and expiring at 00:00 UTC September 30. No wildcard account
  or cluster grant, new credentials, compute, budget or timer changes.
- At 02:52:12, the existing scheduled function successfully requested add-on
  removal. That request alone was not treated as teardown completion.
- At **03:01:32.745927 UTC**, fresh resource inventory checks confirmed all ten
  scoped categories empty, before the 03:11:58 deadline. Both alarms were OK.

Private evidence: `.local/aws-live/cleanup-identity-repair-policy.json`, its
validation/confirmation reports, `cleanup-after-identity-repair-logs.json`, and
`.local/phase12/cleanup-status.json`. Do not publish raw operational directories.

## Follow-ups and limits

Before any future authorized cloud session, include add-on identity dependencies
in the generated teardown policy and validate the full lifecycle; the exact-ID
repair here is for this past run, not a reusable future setup. Keep fresh absence
checks and an independent cleanup deadline. IAM simulation/configuration review
alone is not proof that asynchronous deletion works.

S3/ECR/log and monitoring artifacts were retained and may still incur small
charges. This incident closed the scoped lab compute/network/storage teardown,
not every account-wide AWS charge. Original local database data was preserved.

Reference: [AWS EKS actions and resource types](https://docs.aws.amazon.com/service-authorization/latest/reference/list_eks.html).
