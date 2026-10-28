# Phase 12 — measured cloud reliability

Run: `lab-20260928T210911Z`, September 28 Eastern / September 29 UTC, 2026.
This extends the existing private AWS lab; no additional EC2 node, load balancer,
public endpoint or database was provisioned for monitoring.

## What changed

- `/metrics` exposes queue/running counts, terminal counters, start/completion
  histograms, SLO numerator/denominator counts and worker heartbeat age.
- Migration 006 adds a heartbeat table only. Workers commit a heartbeat every
  five seconds and emit structured job-ID/attempt/outcome logs.
- Private Prometheus and Grafana run on the existing node. The checked-in
  dashboard has four panels; rules cover queue growth, errors, scrape failure
  and recent p95 completion latency. Application alert email routing is not set up.
- A guarded, standard-library Python driver produces real HTTPS load and
  repeatable cloud drills. It defaults to a plan, refuses other contexts and
  near-expiry runs, serializes drills, and owns its private monitoring tunnels.

The image deployed for API, worker and migration is pinned to
`sha256:3f50f9feb395fb0afa59c53ee66870a971a6ac72eb22b23a9e5d4b8fc75ffd2d`.
Prometheus is pinned at 3.15.0 and Grafana at 13.2.2 by multi-platform digest in
`platform_tools/observability.py`.

## Live results

The initial complete run passed all seven checks. Raw evidence is preserved,
including failed test-tool runs; they are not silently replaced by passing runs.

| Check | Observed result |
| --- | --- |
| 100-job burst, 8 client threads | 100 succeeded; zero request errors or unfinished jobs; p95 completion 453 ms, p99 464 ms |
| Worker interrupted after claim, before completion commit | Recovered in 33.252 s; attempt 1 expired, attempt 2 succeeded |
| PostgreSQL Pod restart | Recovery and fresh-job completion in 13.446 s; same PVC volume; prior keyed result preserved |
| 45 jobs with 1.5 s completion-write delay | Queue alert fired, jobs drained and queue alert resolved |
| API deployment with nonexistent executable | Unready new Pod observed; two old replicas remained available; runner restored the original template in 6.499 s |
| 12 invalid campaign jobs | Each failed permanently after one attempt; error-rate alert fired; a subsequent valid job completed |
| Persistent completion timeout | Three real leases expired; job became terminal `failed` with `lease_expired` and three attempt-history records |

Final serialized steady-load run, **01:45:44–01:46:47 UTC**: 600 fresh keyed jobs
scheduled at 10 requests/s for approximately 60 seconds, eight client threads,
one worker. All 600 were accepted and succeeded, with no request errors or
unfinished work. p95 submission latency was **12.45 ms**; p95 enqueue-to-start
was **956.06 ms**; p95 enqueue-to-completion was **959.67 ms**, p99 **998.32 ms**.
All 600 met both timing thresholds. The 63.577 s wall measurement includes
fetching every final job; 9.437 observed completions/s is not maximum capacity.
Maximum client scheduling delay was 6.26 ms. Report: `report-rel_ea50609f11e0.json`.

The explicit slow-write/latency-alert rerun at **01:42:32–01:44:25 UTC** also
passed. `JobQueueGrowing` and `JobCompletionLatencyHigh` were both observed
**firing**, not just configured. The queue alert subsequently resolved and all
45 affected jobs succeeded. Report: `report-rel_88cb2915eee5.json`.

All exports were SHA-256 verified after transfer from the private operator;
the full six-report history is retained, including the two failed tool runs.
Grafana's authenticated API confirmed the provisioned dashboard and four panels.
At final inspection, all six application/monitoring Pods were Ready, the scrape
target was up, the queue was empty, and no injected SQL fixture table remained.

## Environment and measurement limits

One `t3.medium` EKS node (2 vCPU, 4 GiB class), one `t3.micro` in-VPC operator,
two API replicas, one built-in worker and one PostgreSQL Pod on 10 GiB encrypted
gp3 storage. Application pods share the node with networking/storage controllers
and monitoring. This is not a multi-node availability test. Client traffic
traverses trusted HTTPS, the private ALB, API and hostname-verified DB TLS.

The burst's 102.6 completions/s over about 0.975 s is a **sample observation**,
not sustained capacity. CPU credit behavior, long-duration storage growth,
cross-region clients and saturation were not benchmarked. The load driver is
custom Python, not k6/vegeta; individual accepted jobs and request errors are
retained so the measurement can be audited.

The two experimental SLO targets are 99% starting within 5 s and 99.9% reaching
a terminal state within 60 s. Neither is a production commitment. Terminal
failure counts as a terminal state; success/failure counters must be considered
separately. Intentional failure injection is included in the lifetime metrics,
so the overall chaos window can miss an SLO even when normal traffic passes.

At **01:51:14 UTC**, after all jobs were old enough to evaluate, the complete
lab population contained **1,553 jobs: 1,540 succeeded and 13 deliberately
failed**, with zero queued/running jobs. The start SLI was **1,428/1,553 =
91.951%**; terminal-within-60 s was **1,537/1,553 = 98.970%**. Both miss the
99% / 99.9% targets across the intentionally faulted window. The final normal
600-job cohort met both targets; that is not evidence of a month-long SLO.
All four application alert rules were inactive at final inspection. Queue,
failure-rate and latency alert firing were observed during the relevant drills;
the scrape-failure rule is configured but its live firing was not asserted.

## Important distinctions from the original proposal

- One persisted completion is not exactly-once external execution. Leases and
  fenced transactions protect stored results, not arbitrary side effects.
- Database restart tests persistence on the same EBS volume, not backup/restore
  after volume loss. Single-node/single-database downtime is expected.
- SQL-trigger delay tests slow database completion writes. Toxiproxy and network
  packet impairment are not implemented.
- The bad-rollout drill automatically restores a saved template. A remotely
  activated GitHub merge-to-cluster CD pipeline is still not implemented.
- Retained terminal failed rows are not a dedicated dead-letter listing/redrive
  service. Invalid data fails once; the separate timeout drill tests retry exhaustion.
- Prometheus alert firing is verified independently from delivery. Only the AWS
  cleanup/budget email subscription was confirmed; application alerts are not emailed.

## Why these implementation choices?

The SLO denominator comes from all accepted jobs old enough to evaluate:

```sql
count(*) FILTER (
    WHERE created_at <= statement_timestamp() - interval '5 seconds'
)
```

Suppose 100 jobs were accepted and one never starts. Counting only finished jobs
would hide that job and falsely suggest 100% success. Here it stays in the
denominator, yielding 99/100. The integration test explicitly leaves a queued
job in the denominator. Database-backed counters also survive an API restart.

The worker crash took roughly 33 seconds because its 30-second lease must expire
before a replacement can reclaim the job, followed by retry delay/polling. A
shorter lease improves recovery but risks overlapping work unless processing
deadlines or renewal are designed carefully. The database fences completion by
the current lease token; it does not infer that a vanished Pod finished its work.

The bad deployment is recoverable because the API uses:

```yaml
rollingUpdate:
  maxUnavailable: 0
  maxSurge: 1
```

Readiness prevents the bad new replica from replacing good replicas. Restoring
the exact Pod template removes the invalid command; a partial merge can leave
bad fields behind. This requires spare node capacity and does not protect
against a backwards-incompatible database migration.

## Test-tool defects found during live verification

1. The operator uses Python 3.9. A Go RFC3339 timestamp with five fractional
   digits failed the original parser. The driver now normalizes 1–9 digits to
   microseconds, with a regression test. The failed run is retained.
2. Two diagnostic runs briefly shared a fixed local Prometheus tunnel. When
   the first finished, the second lost access and its follow-up alert observation
   failed. The driver now rejects concurrent drills using an OS file lock,
   selects a private ephemeral port, and verifies its own forwarding process.
   This was a test-driver failure, not evidence that the service lost jobs.

## Evidence, cleanup and next learning step

Private raw JSON and safe log exports live in `.local/phase12/`. Operational
account details and temporary TLS material remain private in `.local/aws-live/`;
do not publish that directory. `RUNBOOK.md` describes reproducible commands,
alert responses, rollback and limitations. Two controlled incident reports
accompany this milestone.

- [Worker-interruption incident](incidents/01-worker-interruption.md)
- [Bad API rollout incident](incidents/02-bad-api-rollout.md)

The final local validation log is `.local/phase12/local-validation.log`:
`make check integration platform-check lint` passed. This includes Go race/vet
and database integration tests, 77 primary Python tests, 24 AWS configuration
tests, Prometheus config/rule/SLO checks, and Staticcheck. Real Docker sandbox
execution tests remain a separate opt-in suite and were not rerun here.

Six temporary non-secret cloud transfer versions were removed after export;
local code and reports remain. The original local PostgreSQL volume was not
reset. No new node, public endpoint, access key or budget extension was added.

Cleanup was scheduled to begin at **02:11:58 UTC September 29**, with a final
deadline of **03:11:58 UTC**. Fresh read-only AWS absence checks completed at
**03:01:32.745927 UTC September 29 (11:01:32 p.m. Eastern September 28)** and
confirmed zero scoped nodegroups, instances, load balancers, target groups,
database/node volumes, EKS clusters, NAT gateways, VPC endpoints, elastic IPs and
private hosted zones. The result is recorded in `.local/phase12/cleanup-status.json`;
it is based on absence, not accepted delete requests. Both cleanup alarms were OK.

Teardown initially stalled because deleting an EKS managed add-on also needed
`eks:DeletePodIdentityAssociation`. After the owner approved the repair, a new
inline policy granted only that action on this run's three exact association
ARNs, restricted to us-east-1 and expiring September 30 at 00:00 UTC. The scheduled
function then progressed without any code, configuration, timer or budget
extension. At the observation above the EventBridge rule was still enabled;
its existing controller subsequently disabled it, confirmed by a read-only
`DescribeRule` check (`.local/aws-live/cleanup-rule-final.json`). The chat's
cleanup follow-up was then paused after successful absence verification; the
chat remains open.
See the [cleanup incident report](incidents/03-cleanup-permission.md).

Original local PostgreSQL storage and all local evidence were preserved.
Retained S3 state/reports, ECR images, CloudWatch logs and monitoring artifacts
may still incur small charges. This does not establish a zero AWS bill or exact
final project spending.
AWS Budgets still returned a stale September 28 16:49 Eastern update when checked
at the end of this milestone; it is not a final lab bill or a guaranteed $50 cap.

Before using this project in an interview, reproduce one local failure yourself
and explain the difference between job state, attempt history and lease token.
Start by tracing `worker.RunOnce` → `JobStore.Claim` → `JobStore.Finish`, then
predict how a killed worker appears in `GET /jobs/{id}/attempts`.
