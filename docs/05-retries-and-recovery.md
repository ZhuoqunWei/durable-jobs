# Milestone 5: bounded retries, crash recovery and attempt history

Implemented September 15, 2026. This is delivery mode: the milestone is built
and tested, with the decisions below available for a code walkthrough. It adds
no framework, dependency, cloud service or untrusted-code execution.

Later the same day, [milestone 6](06-failure-harness.md) added the named
failure-testing harness. References below to that being next describe this
milestone's original scope. September 16 added a
[CLI-only script runner](07-script-runner.md), not script jobs in this queue.

## What it does now

A worker can die after committing its claim. Another worker can later notice
the expired lease, record that abandoned attempt, wait through backoff and
claim the same logical job with a new token. Three committed claims are allowed
per new job by default, including claims interrupted by crashes.

Successful jobs finish. Invalid campaign data fails immediately. Failures that
are explicitly retryable can enter `retry_waiting` before another claim:

```text
queued → running → succeeded
            ├──→ failed                     permanent failure or budget exhausted
            └──→ retry_waiting → running    retryable failure, budget remaining
```

An expired lease follows the failure branch only when recovery runs. A job
does not change state merely because time passes. At least one worker must
keep polling; there is no independent recovery daemon.

## Decision 1: separate one job from its execution attempts

Four identifiers/concepts have different responsibilities:

| Concept | Responsibility |
| --- | --- |
| Job ID | The logical unit of work; remains the same across retries |
| Submission idempotency key | Deduplicates matching POST requests; it is not worker authority |
| Attempt number | Counts committed claims, including crashes before useful work starts |
| Lease token | Authorizes completion of one current, unexpired claim; replaced on every new claim |

`jobs` stores current state and the final successful result. `job_attempts`
stores the history: number, status, start, finish, lease expiry and safe failure
code. A failed attempt can belong to a job that later succeeds. Therefore
attempt status `failed` does not necessarily mean job status `failed`.

Attempt states are `running`, `succeeded`, `failed` and `lease_expired`.
`retry_waiting` is only a job state; waiting does not consume an attempt.

Both tables are changed in the same transaction. A committed running job must
have its running attempt record, and a completion cannot update just one side.
The database permits only one running attempt per job. Ended attempts clear
their token, and public history never returns tokens.

Alternative: keep only an integer attempt count. That would support a budget,
but would not explain whether attempt one timed out, crashed or returned an
invalid campaign. A small ledger makes these failures inspectable without
building a separate logging platform.

## Decision 2: keep retry policy in plain Go

The domain policy lives in `internal/jobs/retry.go`, separate from SQL and the
worker. Its function signature is:

```go
func AfterFailure(code string, attempts, maxAttempts int) (State, time.Duration)
```

Completion in `internal/database/execution.go` reads the attempt count and
budget from the locked database row, then invokes it:

```go
next, delay := outcome.Status, time.Duration(0)
if outcome.Status == jobs.Failed {
    next, delay = jobs.AfterFailure(outcome.FailureCode, attempts, maxAttempts)
}
```

The processor reports what happened. It cannot directly choose `retry_waiting`
or grant itself another attempt. The store applies the policy to authoritative
database state, rather than trusting a stale claim object's counters.

| Failure code | Policy |
| --- | --- |
| `invalid_campaign` | Terminal: another run will not repair the input |
| `unsupported_job_type` | Terminal: this processor cannot handle it |
| `processing_failed` | Terminal: unknown failures are not assumed transient |
| `processing_timeout` | Retry if budget remains |
| `transient_failure` | Retry if budget remains; processor must explicitly classify it |
| `lease_expired` | Recovery records abandonment; retry if budget remains |

A processor error wrapping `jobs.ErrTransient` maps to `transient_failure`.
Raw error strings are not stored as public failure codes. The built-in campaign
transform is pure and normally fast; transient behavior is exercised using an
injected test processor, not artificial fault flags in production code.

The default budget is three attempts: attempt one's retry waits one second;
attempt two's retry waits two seconds; failure on attempt three is terminal.
The policy doubles delays up to one minute for larger stored budgets, which the
schema bounds to 1–10. HTTP callers cannot configure the budget in this version.
There is no jitter yet; coordinated fleet-scale retry bursts are a later concern.

Importantly, a claim consumes budget even if the process dies before executing
the transform. An uncertain claim commit is also not permission to execute:
the worker returns an error, and recovery handles a claim if it actually committed.
Consequently repeated crashes can exhaust a job without useful processing.

## Decision 3: use the database to coordinate processes

Each worker iteration performs two separate, short transactions:

1. Recover at most ten expired claims.
2. Claim one eligible job, then commit before processing begins.

Claim selection now uses this actual SQL from `execution.go`:

```sql
SELECT id::text FROM jobs
WHERE status IN ('queued','retry_waiting') AND available_at <= clock_timestamp()
    AND attempts < max_attempts
ORDER BY available_at, created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED
```

The claim update increments attempts, generates a fresh token and inserts its
attempt record in the same transaction. Job `started_at` retains the first
start; the ledger stores each individual start. PostgreSQL supplies timestamps,
so worker machine clocks do not decide lease ownership.

Recovery in `internal/database/recovery.go` similarly locks expired running
jobs with `FOR UPDATE SKIP LOCKED`. For each selected job it closes the attempt
as `lease_expired`, clears ownership and either schedules a retry or marks the
job failed when the budget is exhausted. Recovery does not run the processor
and does not increment the attempt count.

Every path locks the job before its attempt row. Batch size bounds transaction
work; skipping locked rows lets concurrent workers handle other jobs. A busy
row can be recovered on a later poll. Ordering is best-effort availability, not
strict FIFO. PostgreSQL documents this queue-oriented use of
[SKIP LOCKED](https://www.postgresql.org/docs/17/sql-select.html).

These transactions use Read Committed with `synchronous_commit = on`. Locks and
conditional updates coordinate ownership across processes; an in-memory mutex
would coordinate only one process. See PostgreSQL's
[transaction isolation documentation](https://www.postgresql.org/docs/17/transaction-iso.html).

## Decision 4: a late worker must not overwrite newer work

A worker may pause beyond its lease and resume after another worker takes over.
Completion first locks the job, then evaluates this condition in a fresh SQL
statement:

```sql
WHERE id = $1 AND status = 'running' AND lease_token::text = $2
    AND lease_expires_at > clock_timestamp()
```

Checking expiry after acquiring the lock matters: time spent waiting for that
lock can invalidate the lease. Both a matching token and a live lease are
required. The old token cannot complete during retry waiting or after a new
claim; knowing the job ID or submission key is insufficient.

This protects the job result in PostgreSQL. It is not exactly-once execution
and would not undo an email or payment already sent by an old worker. The
current processor has no external side effects. Any future side-effecting
adapter needs its own idempotency/fencing or transactional-outbox design.

## Inspecting the result

Run the API and use the ID of a job already submitted:

```sh
curl http://127.0.0.1:8085/jobs/REPLACE_WITH_JOB_ID
curl http://127.0.0.1:8085/jobs/REPLACE_WITH_JOB_ID/attempts
```

Important job fields:

- `attempts`: committed claims so far; `max_attempts`: the stored budget.
- `available_at`: earliest eligibility for the next claim, not a scheduling guarantee.
- `started_at`: first attempt's start, preserved across retries.
- `failure_code`: last attempt's reason while waiting; cleared when claimed again.
- `completed_at`: present only for terminal jobs; `result`: only on success.

History is ordered by attempt number. An example, with illustrative timestamps:

```json
{
  "job_id": "00000000-0000-0000-0000-000000000001",
  "attempts": [
    {
      "number": 1,
      "status": "lease_expired",
      "started_at": "2026-09-15T12:00:00Z",
      "finished_at": "2026-09-15T12:00:31Z",
      "lease_expires_at": "2026-09-15T12:00:30Z",
      "failure_code": "lease_expired"
    },
    {
      "number": 2,
      "status": "succeeded",
      "started_at": "2026-09-15T12:00:32Z",
      "finished_at": "2026-09-15T12:00:32.1Z",
      "lease_expires_at": "2026-09-15T12:01:02Z"
    }
  ]
}
```

For expired attempts, `finished_at` is when recovery records the outcome, not
the unknowable instant a process crashed. A queued job returns an empty array.
Invalid IDs return 400, missing jobs 404 and storage failures 503. Separate GET
requests can observe different instants while a worker is progressing; they
are not one combined snapshot.

Keyed POST replay still returns the same job's current state, including retry
waiting or final output. Do not create a new key to retry the same submission.

## Migration and local operation

For an existing installation, stop older API/worker processes first. Then:

```sh
make db-up
make migrate
make run
```

Migration `004_retry_and_attempts.sql` adds the budget, availability, retry
constraints, indexes and ledger. It backfills the single known attempt for
pre-existing running/terminal jobs. Previous terminal rows did not retain lease
expiry, so their imported history omits it rather than inventing a timestamp.
Old terminal failures remain terminal even if their code is now retryable.

This is not a rolling upgrade with old workers: those binaries do not maintain
the ledger. Restart updated API/worker binaries only after migration. Migration
004 was applied to the application database; both pre-existing queued demo jobs
and a checksum of all their pre-existing fields were preserved.

In another terminal, processing is explicit:

```sh
make worker-once
# Or keep polling until Ctrl-C:
make worker
```

These commands can process any eligible job, including the existing demo jobs;
they are not scoped to your latest POST. A one-shot run may recover a claim and
exit before its backoff ends. `RunOnce` returning `false, nil` means no job was
claimed, not that recovery made no changes. Use continued polling for eventual
retries. A successful command exit is not proof of a successful migration—read
the job's status/result.

Defaults remain a 30-second lease, 10-second cooperative processing timeout,
3-second storage timeout and one-second polling delay on no work/errors.

## What was verified

Run `make check integration build`. Database tests create isolated synthetic
schemas; they do not claim application jobs.

- Policy tests: retry classification, invalid budgets, backoff and exhaustion.
- State/HTTP tests: reject impossible outcomes; return ordered, token-free history.
- Recovery tests: stale tokens rejected before and after re-claim; repeated
  crashes stop at the budget; permanent failures do not retry.
- Concurrency tests: independent pools recover without duplicate ownership;
  locked rows are skipped; recovery races safely with completion.
- Transaction tests: injected deferred commit failures roll back job and ledger
  changes together for claim, finish and recovery.
- Upgrade tests: schema 003 rows survive migration 004, repeat migration is
  harmless, old running work can finish and terminal work is not reopened.
- Real crash test: start an owned test-worker subprocess, wait for its claim
  to commit, kill that exact process, wait for actual database lease expiry,
  then run fresh compiled CLI workers. They recover, wait through real backoff
  and complete the same job on attempt two. No fake timestamp update is used
  for that process-crash scenario.

Other database tests advance timestamps only inside their isolated schemas to
exercise more cases quickly. The production processor has no test-only controls.

## Limits and the next milestone

There is no lease renewal, sandbox, authentication, admission/rate control,
standalone evaluation harness or AI orchestration. A Go context cannot forcibly
stop arbitrary code; the processor must cooperate with cancellation. This
service is local-only, and retries do not guarantee eventual success.

The next bounded milestone is a repeatable fixture/failure harness: run named
synthetic scenarios and report expected versus observed behavior. After that,
a constrained script runner needs an explicit threat model before untrusted
code is allowed. Those remain separate from the completed retry milestone.

For a walkthrough, begin with one question: **if worker A resumes after worker B
has already completed attempt two, which exact SQL predicate rejects A?** Trace
`Worker.RunOnce` → `JobStore.Finish` → `recordAttemptEnd` to answer it.
