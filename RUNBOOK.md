# Durable Jobs reliability lab

This is a disposable **private** EKS demonstration, not production. All jobs are
synthetic. Never use the failure runner against another cluster or the original
local PostgreSQL volume. The six-hour AWS cleanup deadline is independent of the
runner and must not be extended for a test.

## Deploy and access

The foundation and first deployment are documented in
[the live lab report](docs/18-live-aws-lab.md). Deploy the API/worker image by digest,
run all pending database migrations before the rolling update, and smoke-test a
new keyed submission, completion, replay, conflict, and attempt history. Do not
recreate the database Secret or PVC when updating application code.

`python3 platform_tools/observability.py --output /tmp/durable-jobs-monitoring.json`
renders private monitoring resources without contacting AWS. Installation uses
the same namespace, a separately generated `grafana-admin` Secret, and ClusterIP
Services only. There is no public dashboard or monitoring Ingress. Prometheus and
Grafana data are ephemeral; export evidence before cleanup.
The equivalent generated manifest is checked in at
`deploy/observability/kubernetes.json`; a regression test detects drift between
the generator, dashboard JSON and checked-in manifest.

On the private operator, with the approved kubeconfig/profile selected:

```sh
kubectl -n durable-jobs-aws port-forward svc/grafana 13000:3000 --address=127.0.0.1
kubectl -n durable-jobs-aws port-forward svc/prometheus 19090:9090 --address=127.0.0.1
```

These bind on the **operator**, not your laptop. Use an authenticated SSM tunnel
for laptop access; do not open a public security-group port. Grafana requires its
random admin password. Never place that password in screenshots, reports or Git.

## Detection and response

Lab thresholds are deliberately short: scrape/evaluate every 5 s and require
15 s of sustained alert conditions. Production thresholds need real traffic data.

| Signal | Meaning | First response |
| --- | --- | --- |
| `JobQueueGrowing` | Depth >5 and increasing over 1 minute | Check worker heartbeat, worker logs, database readiness and completion latency. Restore service before adding workers. |
| `JobFailureRateHigh` | >5% of recent terminal jobs failed | Inspect safe failure codes and attempt history. Invalid campaign data is permanent; do not retry blindly. |
| `JobMetricsUnavailable` | API metrics scrape failed | Check API readiness and database connectivity. A healthy process is not proof that its database is ready. |
| `JobCompletionLatencyHigh` | Recent p95 enqueue-to-terminal latency >5 s | Check queue growth and database timing; distinguish time waiting in the queue from time spent processing. |
| Old/absent heartbeat | No recent committed worker heartbeat | Inspect worker Pod and DB connectivity. A heartbeat proves liveness, not useful progress. |

Prometheus evaluates these rules. **No Alertmanager/email routing is configured
for application alerts.** AWS budget/cleanup email alerts are a separate channel.

## SLO interpretation

Targets: 99% of accepted jobs start within 5 s; 99.9% reach a terminal state within
60 s. Both are experimental lab targets, not claims of achieved production SLOs.

The API exposes `*_good` and `*_eligible` counts from database timestamps. An
accepted job becomes eligible once it is at least 5 s / 60 s old. Unfinished jobs
remain in the denominator. A zero denominator produces no ratio, not 100%.
Completion includes both success and terminal failure: inspect the separate
failure counter, too. Intentional chaos will consume the error budget.

The histogram and counters are database-lifetime aggregates, not process-local
counters. One logical API Service scrape avoids double-counting its two replicas.
Histogram p95 is approximate; the load report computes exact nearest-rank sample
percentiles from persisted timestamps. The SQL scans fit this small lab; a large
production queue needs incremental aggregation and retention.

## Reproduce a failure

On the prepared private operator (not the developer database):

```sh
python3 /opt/durable-jobs/reliability/aws_reliability.py
python3 /opt/durable-jobs/reliability/aws_reliability.py --execute --scenario all
```

Use `--scenario load`, `sustained-load`, `worker-crash`, `database-restart`, `slow-database`,
`bad-deploy`, `poison-job`, or `retry-exhaustion` for a single drill. The runner
refuses an incorrect context/namespace or insufficient time before cleanup.
Each drill records expected assertions and measured evidence in a JSON report.

- Worker kill: a committed claim loses its worker before completion. Wait for
  the real 30 s lease and 1 s retry delay; expect two attempts but one stored result.
- DB restart: delete only `postgres-0`, retain its PVC, and verify both prior
  keyed output and fresh completion after recovery. Workers use bounded storage
  calls and a fixed 1 s poll delay, **not exponential database backoff**.
- Slow DB: a trigger delays completion for matching synthetic campaign IDs.
  Expect backlog alert firing, draining, then resolution. This is SQL latency,
  **not Toxiproxy/network latency**.
- Bad deployment: a nonexistent API executable creates an unready surge Pod.
  `maxUnavailable: 0` keeps the prior replicas available. The runner restores
  the exact saved template automatically. This is not proof of GitHub CD rollback.
- Poison payload: permanent validation failure stops at one attempt, remains
  queryable, and does not prevent the next valid job from completing.
- Retry exhaustion: persistent completion timeouts expire three leases; expect
  terminal `lease_expired` and three history entries. Failed rows act as retained
  dead letters; a dedicated DLQ listing/redrive API is not implemented.

SQL fixtures expire after 5 minutes and are removed in `finally`. If the runner
is killed externally, inspect the exact lab fixture before removing it:

```sql
DROP TRIGGER IF EXISTS lab_reliability_delay ON jobs;
DROP FUNCTION IF EXISTS lab_reliability_delay();
DROP TABLE IF EXISTS lab_reliability_faults;
```

These are only the injected fixture objects, not application tables. For an
interrupted bad rollout, restore `/opt/durable-jobs/reliability/bad-deploy-original-template.json`
or select the verified good image/template explicitly and wait for rollout.
Do not delete a PVC to repair application readiness.

## Evidence and cleanup

Preserve JSON reports, safe logs, dashboard JSON and Prometheus queries locally.
Check the independent cleanup report and actual AWS resource absence; a
successful delete request alone is not completed teardown. ECR/S3/log artifacts
may retain small storage charges after compute/network removal. Preserve the
original `durable-jobs_postgres-data` local Docker volume in all cases.

## Local validation

`make check`, `make integration` (isolated schemas in the existing local DB),
`make platform-check`, and `make monitoring-check`. The checked-in CI invokes
the Go/container checks, Python regressions and Prometheus rule tests; remote
workflow execution and branch protection require separate GitHub setup.

References: [Prometheus alerting rules](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/),
[histograms](https://prometheus.io/docs/practices/histograms/),
[Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/).

## Disposable release demo

For a fresh local deployment and worker-crash/recovery check, use `make lab-tools`
once, then `make demo-plan` and `make demo-check`. See
[Phase 13](docs/20-reproducible-release.md) for prerequisites, evidence locations,
owned-cluster cleanup and the separate GitHub activation checklist. This command
never reuses the Phase 10 cluster or the original application database.
