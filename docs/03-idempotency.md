# Milestone 2: retry a submission without creating another job

Implemented September 9, 2026. This protects job creation, not worker execution.
At this milestone there was no worker. The subsequent
[worker milestone](04-worker-and-domain.md) adds execution. Keyed replays now
return the existing job's current state and result, not its original queued snapshot.

## The API contract

Send an optional `Idempotency-Key` header with POST /jobs. Use a fresh random UUID
for each logical submission and reuse it only when retrying that submission.
The key is not a password or an authorization mechanism.

| Request | Response | Stored effect |
| --- | --- | --- |
| New key, valid input | 202 + job + Location | One new job |
| Existing key, same input | 202 + original job + same Location | No extra job |
| Existing key, different valid input | 409, no Location | Original job unchanged |
| No key | 202 for each valid submission | Each submission creates a job |
| Empty, repeated or invalid supplied key | 400 | No storage call |
| Cannot confirm commit | 503 | Outcome may be unknown; retry with the SAME key and input |

Keys are case-sensitive and contain 1–128 ASCII letters, digits, `.`, `_`, `:`
or `-`. Values are not lowercased or repaired. The HTTP transport may strip
surrounding header whitespace; the application validates the value it receives.
Comma-joined or repeated headers are rejected rather than choosing one key.

The key is global to this local single-user service. It has no expiration and
remains reserved for the lifetime of the stored job. No deletion API exists.
Restoring an older database backup or removing rows can remove this protection.
Before a multi-user deployment, key uniqueness must include authenticated tenant
identity; otherwise one user's key could collide with another's. Do not expose
this unauthenticated learning API publicly.

## Decision 1: one row, one atomic commit

Migration 002 adds this constraint to jobs:

```sql
ADD CONSTRAINT jobs_idempotency_key_unique UNIQUE (idempotency_key)
```

The key lives beside the immutable type and payload. There is no separate ledger
to update, expire, or reconcile and no second copy of the submitted payload.
Legacy jobs have NULL keys; PostgreSQL's ordinary unique constraint allows
multiple NULLs. Existing unkeyed clients keep their original behavior.

A separate idempotency table would be useful for protecting several endpoints,
retaining keys after jobs are archived, or replaying exact historical responses.
For this one endpoint it adds machinery we don't need yet. If a future worker
needs transformed data, store it separately: do not overwrite the original input
that idempotency compares.

## Decision 2: let the unique constraint arbitrate simultaneous submissions

From internal/database/jobs.go:

```sql
INSERT INTO jobs (type, payload, idempotency_key)
VALUES ($1, $2::jsonb, NULLIF($3, ''))
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id::text, type, payload, status, created_at
```

The winning insert owns the key. A conflicting insert waits for the first
transaction's outcome. If the first commits, the second inserts nothing. If
the first rolls back, the waiting insert can proceed. A Go mutex or in-memory
map would not coordinate two API instances or survive an API restart.

Only a conflict on this specific unique key is handled as a replay. Other
database failures, including a failing commit, are not silently swallowed.

## Decision 3: use a new statement snapshot after a conflict

An INSERT with ON CONFLICT DO NOTHING can lose to a row that was not visible in
the INSERT's original snapshot. A SELECT in the same statement/CTE can therefore
miss the winning row. PostgreSQL documents this behavior under Read Committed.

The transaction explicitly selects Read Committed, even if the session default
is different:

```go
tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
```

After pgx.ErrNoRows from INSERT, a SEPARATE SQL statement gets a fresh snapshot:

```sql
SELECT id::text, type, payload, status, created_at,
       type = $2 AND payload = $3::jsonb
FROM jobs WHERE idempotency_key = $1
```

If the final boolean is false, return ErrIdempotencyConflict, mapped to HTTP 409.
If true, return the existing job, but still finish the transaction before 202.
A no-op ON CONFLICT DO UPDATE could return the row in one statement, but it would
perform an unnecessary update for every replay. We chose the extra read instead.

There is no concurrent deletion or input mutation path in this milestone. If
one is added, re-evaluate the read-after-conflict and retention guarantees.

## Decision 4: compare JSON meaning instead of raw bytes or a Go float64 map

The comparison includes the parsed type and the entire JSONB payload, not unknown
top-level fields that the envelope parser already ignores. In this contract:

- Object key order, JSON whitespace and equivalent Unicode escapes don't matter.
- JSON numbers 1 and 1.0 compare equal; the string "1" is different.
- Array order matters, as do missing properties versus explicit null.
- PostgreSQL compares large numbers without first converting them to float64.
- The parser's existing duplicate-key behavior remains; JSONB retains the last
  duplicate object value. This is not a strict duplicate-key-rejecting API.

We don't store a hash. With a 64 KiB request limit and the original input already
in the row, direct comparison keeps canonicalization and collision handling out
of the design. This is a simplicity choice, not a performance benchmark result.

## Decision 5: distinguish lost acknowledgements from failed transactions

If the database commits but the response gets lost, retrying the same key/input
returns the committed job. If the database rolls back, that key is not reserved
and a later retry can create the job. A 503 still doesn't prove rollback.

Never generate a new key merely because a request timed out. Never add a key on
a retry of an originally unkeyed request and assume it will deduplicate the first
request: that first job had no key to match.

Replays return the same job identity, not a cached historical HTTP response. When
workers eventually update status, a replay can show the current status. This is
idempotent creation, not an exactly-once execution guarantee.

## Try it

From the repository folder, with the database and API running:

```sh
curl -i -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: synthetic-demo-001' \
  --data-binary @examples/migration-job.json http://127.0.0.1:8085/jobs
```

Run that exact command again: 202 and the same ID. Change the payload but keep the
key: 409. Choose a new key only when you intend a new logical submission.

Apply migration 002 with `make migrate` before running the updated API. Migration
001 is unchanged. Neither migration deletes jobs or resets the volume.

## Tests and what they demonstrate

Run `make check` and `make integration`.

- Header validation and 400/409/503 response handling.
- Equivalent JSON replays, changed input conflicts, case-sensitive keys, exact
  large-number comparisons and unkeyed/new-key behavior.
- Sixteen concurrent calls across two handlers with independent pools produce
  one row. Competing payloads produce eight 202s and eight 409s, with one row.
- Reopening the pool and reconstructing the handler preserves key deduplication.
- Invalid JSONB and a deliberately failed COMMIT do not reserve the key.
- Deterministic overlap: the test observes a real PostgreSQL lock wait before
  committing/rolling back the owner, or canceling and retrying the waiter.
- A session configured as Repeatable Read still works because Create explicitly
  requests Read Committed.
- Upgrading a version-001 database preserves its existing jobs unchanged.

The integration tests create and clean up only their own random schemas. They
do not clear application jobs or remove the Docker volume.

Live verification on September 9, 2026 also exercised the running API over HTTP:
two identical keyed requests returned 202 and the same job, changed input returned
409, and another identical request returned that same job after a full API-process
restart. The application database contains the preserved original unkeyed demo
job plus one new keyed synthetic demo job; both remain queued.

## Discussion

The core interview explanation is: "I make key ownership and job creation atomic
using a database unique constraint. Concurrent callers either create the job or
read the existing one and compare input. Retrying after an unknown commit outcome
doesn't create another job when the key and original input are preserved."

Next milestone: a worker with explicit states, atomic claims, leases and crash
recovery. Submission deduplication does not solve repeated worker side effects.

## Primary references

- [PostgreSQL Read Committed and ON CONFLICT visibility](https://www.postgresql.org/docs/17/transaction-iso.html#XACT-READ-COMMITTED)
- [PostgreSQL INSERT and conflict targets](https://www.postgresql.org/docs/17/sql-insert.html)
- [PostgreSQL JSONB semantics](https://www.postgresql.org/docs/17/datatype-json.html)
