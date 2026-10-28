# Milestone 4: one claim, one built-in migration, one committed outcome

Historical walkthrough of the September 10, 2026 milestone. At that point the
worker was single-attempt, without crash recovery. September 15's
[retry/recovery milestone](05-retries-and-recovery.md) supersedes the lifecycle,
claim SQL and limitations below; read it for current behavior and upgrade steps.
The campaign rules and ownership principles here still apply. A later
[CLI-only script runner](07-script-runner.md) is separate from this worker.
AI orchestration remains unimplemented.

## What you can demonstrate

Submit a synthetic campaign, run `make worker-once`, then retrieve its result.

```json
{"type":"migrate_campaign","payload":{"campaign_id":"campaign_42","category":"Pets","created_at":"08/31/2026"}}
```

The completed job has `status: "succeeded"`, `attempts: 1` and this `result`:

```json
{"campaign_id":"campaign_42","category_id":"animals","created_date":"2026-08-31","schema_version":1}
```

Input is retained unchanged. A nonexistent calendar date such as `02/29/2026`
produces `status: "failed"` and `failure_code: "invalid_campaign"`, without a
result. Neither output is written to an external campaign service or table.
Result and completion status are committed together in the job row.

Run `make migrate` before the updated API or worker. `make worker-once` claims
one available queued job, not necessarily the job you last submitted.
`make worker` polls until Ctrl-C; two worker processes can safely compete.
If the queue is empty, `worker-once` exits successfully without claiming work.
Its successful exit means an iteration completed, not that a campaign passed
validation: use GET to distinguish succeeded from failed.

## Decision 1: domain rules are independent of infrastructure

The first domain module is `internal/campaign`, whose entry point is:

```go
func Migrate(payload []byte) (MigratedCampaign, error)
```

It knows about a legacy campaign and a version-1 migrated campaign. It knows
nothing about PostgreSQL, HTTP or Docker. The worker adapter calls it and turns
its return value into a processing outcome. This is a small start on DDD, not a
claim that renaming folders implements a complete domain model.

Current synthetic contract:

- Exactly the fields `campaign_id`, `category`, `created_at`; unknown fields fail.
- ID: 1–128 ASCII letters/digits/underscores/hyphens, starting with a letter/digit.
- Categories: `Pets` → `animals`, `Medical` → `medical`, `Education` → `education`.
- Dates: exact `MM/DD/YYYY` or `YYYY-MM-DD`, valid calendar dates, years 0001–9999.
- Output uses a date, not an invented timestamp or timezone conversion.
- The taxonomy is made up for the demo. No employer data or taxonomy is used.

An envelope may pass POST validation but fail these domain rules in the worker.
The existing submission parser/JSONB duplicate-key semantics have not changed.

Why not put this logic in the HTTP handler? A worker, CLI or later sandbox
adapter should use the same domain rules without pretending to be an HTTP request.
Why not introduce microservices? Separate deployment adds no value to these rules.

## Decision 2: a job has a constrained lifecycle

```text
queued → running → succeeded
                 ↘ failed
```

`jobs.State.CanTransitionTo` describes the allowed transitions. `Outcome.Validate`
rejects impossible results: success requires a bounded JSON object and no failure
code; failure requires an approved code and no result. The SQL store uses atomic
state predicates, and migration 003 adds matching row-shape constraints.

Each job currently has zero attempts while queued and one once claimed. A
separate attempt-history table and retry states belong to the recovery milestone,
when there will actually be more than one attempt to model.

## Decision 3: claim with a short database transaction

From `internal/database/execution.go`:

```sql
SELECT id FROM jobs WHERE status = 'queued'
ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED
```

This is the candidate-selection part of the claim's UPDATE statement. The UPDATE
sets running state, an attempt count, a generated claim token and a lease expiry.
The transaction commits before processing begins. A locked candidate is skipped,
so one worker does not wait for another worker's row. Ordering is best-effort
oldest available, not strict FIFO when earlier rows are locked.

Do not keep a transaction open while doing the migration. Long-running work
should not hold a database row lock or consume a connection throughout execution.
An in-memory mutex would not coordinate separate worker processes.

Default settings: 30-second lease, 10-second cooperative processing deadline,
3-second storage deadlines, one-second polling interval on empty queues/errors.
PostgreSQL supplies lease timestamps; worker clocks do not decide ownership.

## Decision 4: knowing a job ID is not enough to complete it

After locking the row, completion uses this condition:

```sql
WHERE id = $1 AND status = 'running' AND lease_token::text = $2
  AND lease_expires_at > clock_timestamp()
```

The token identifies the current claim. The expiry check runs in a statement
AFTER acquiring the row lock, so waiting for a lock cannot preserve an obsolete
expiry decision. A stale/expired token, absent job or already-completed job yields
`ErrLeaseLost`. Claim tokens are not included in HTTP responses.

The linearization point is the guarded UPDATE under the row lock. A transaction
may finish committing after the expiry instant; any future reclaim operation
must also lock/recheck the row and will then see its committed terminal state.

This protects the database result, not arbitrary side effects. If a future
processor sends money, emails or external writes, those need independent
idempotency/transactional design. We do not claim exactly-once execution.

## Decision 5: do not invent success when an outcome is uncertain

- Claim commit error: return no usable claim; never execute it.
- Completion commit error: report uncertainty; read the job to reconcile.
- Invalid campaign: persist `invalid_campaign` as a terminal failure.
- Cooperative processor deadline: persist `processing_timeout`, if the claim is still valid.
- Other processor error: persist `processing_failed`, not private error text.
- Shutdown after claiming: cancel processing/storage and leave running state.
- Crash or expired lease: leave running state with its expiry visible. No reclaim yet.

A Go context is cooperative cancellation, not a security boundary. The worker
currently executes only a small trusted pure function on bounded input. It cannot
forcibly stop a malicious/infinite-loop processor. Sandboxed scripts need a real
process isolation and resource-control design in a later milestone.

## Evidence and limits

`make check` runs unit tests, race detection and static checks. `make integration`
creates randomly named test schemas, drops only those owned schemas on cleanup,
and preserves the application jobs and persistent Docker volume.

Tests cover:

- All state transitions; valid/invalid domain data and result shapes.
- Sixteen simultaneous claim calls across two pools: exactly one owner of one job.
- Skipping a locked oldest row; claiming it after its lock is released.
- Invalid/stale tokens, expired leases and immutable terminal completion via the store.
- Expiry during a real observed row-lock wait; cancellation while waiting.
- Competing success/failure completions: only one wins.
- Deferred claim/completion COMMIT failures with no acknowledged partial state.
- Schema upgrades from versions 001 and 002 preserving existing rows/keys.
- Valid and invalid campaigns through the actual worker and SQL store.
- A compiled worker process, a restarted worker process, GET and keyed POST replay.

The process test uses an isolated schema, not the user's queued demo jobs. These
are correctness tests, not throughput benchmarks, strong sandboxing, or proof of
crash recovery. No cloud service, frontend framework or dependency was added.

## What comes next

First: bounded retries, expired-lease reclamation and explicit attempt history.
Prove crash-before-completion and stale-worker races with deterministic tests.
Then: a standalone fixture/failure evaluation harness with real measured reports.
Then: constrained script execution, a threat model and resource-limit tests.
Only after that: optional bounded AI proposal/test/revision orchestration.

Be ready to explain: why is the original idempotency key not enough to protect
worker completion, and what happens if a database commit succeeds but its
acknowledgement is lost?

## Primary references

- [PostgreSQL queue-like use of SKIP LOCKED](https://www.postgresql.org/docs/17/sql-select.html#SQL-FOR-UPDATE-SHARE)
- [PostgreSQL Read Committed semantics](https://www.postgresql.org/docs/17/transaction-iso.html#XACT-READ-COMMITTED)
