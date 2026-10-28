# Milestone 1: accept a job and retrieve it

The implementation is assistant-led, with decisions reviewed together.
At this milestone jobs were stored as queued without a worker. The subsequent
[worker milestone](04-worker-and-domain.md) adds single-attempt execution.

This describes the first milestone. The next milestone is now implemented:
[idempotent submission](03-idempotency.md). Its key/retry contract supersedes
the original retry limitation below for requests that include a key.

## Follow one request

1. POST /jobs requires application/json and reads at most 64 KiB.
2. ParseSubmission checks one UTF-8 JSON object, the supported type, and an object payload.
3. JobStore.Create begins a database transaction and inserts the job.
4. PostgreSQL supplies the UUID, queued status and creation time.
5. Create commits before returning success to the HTTP handler.
6. The handler returns 202, the stored job, and a Location header.
7. GET /jobs/{id} reads that job from PostgreSQL.

## Decision 1: validate separately from HTTP

From internal/jobs/request.go:

    var request SubmitRequest
    if err := json.Unmarshal(body, &request); err != nil {
        return SubmitRequest{}, errors.New("body must contain one valid job-submission object")
    }

The function accepts bytes, not an HTTP request or database connection. We can
test malformed input without starting a server. json.Unmarshal rejects trailing
documents. Unknown fields are ignored in this milestone; Go's normal JSON
duplicate-key/case-matching behavior applies. Strict schema hardening is not yet
a claim of this API.

The envelope is validated now. Campaign category/date transformations belong to
the future worker. An empty object payload is structurally valid at this stage.

## Decision 2: use JSONB for the payload

From internal/database/migrations/001_create_jobs.sql:

    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object')

The database enforces an object even if another caller bypasses HTTP validation.
JSONB preserves JSON meaning, not original whitespace/key order. It also has
stricter numeric/Unicode constraints than Go's JSON parser. These input-related
database rejections are mapped to 400, not mistaken for infrastructure failure.

The job ID has a primary-key index for retrieval. We have not added speculative
queue indexes before implementing the worker's actual queries.

## Decision 3: acknowledge only after COMMIT

From internal/database/jobs.go:

    if err := tx.Commit(ctx); err != nil {
        return jobs.Job{}, fmt.Errorf("commit job: %w", err)
    }
    job.CreatedAt = job.CreatedAt.UTC()
    return job, nil

One INSERT can use an implicit transaction. We deliberately use an explicit one
to make the acknowledgement boundary visible and prepare for later idempotency
records that must commit with the job. This costs extra database round trips.

The transaction sets synchronous_commit to on. Durability still depends on
PostgreSQL fsync/storage behavior and retaining the database volume; it is not
replication, backup, or a promise that hardware never fails.

The deferred rollback has its own short cleanup context so client cancellation
does not prevent transaction cleanup. After a successful commit it is harmless.

## Decision 4: distinguish accepted work from finished work

From internal/httpapi/handler.go:

    w.Header().Set("Location", "/jobs/" + job.ID)
    writeJSON(w, http.StatusAccepted, job)

202 means the job is stored and queued. It does not mean the migration ran.
Processing was not part of this milestone. See the worker walkthrough for the
current execution behavior; acceptance is still not a completion guarantee.
201 would also describe resource creation, but 202 emphasizes asynchronous work.

## Decision 5: timeouts do not imply the write failed

A connection can fail after PostgreSQL commits but before the API confirms it.
Therefore a 503 says creation could not be confirmed and the outcome may be unknown.

In this original milestone, repeated POST requests created separate jobs. The
implemented [next milestone](03-idempotency.md) adds a client-supplied key: repeat
the same key/input to retrieve the original job, or receive 409 if input changes.
Submissions without a key still create separate jobs on each retry.

## Migrations and tests

Run make migrate explicitly before starting this version of the API. SQL files
are embedded, ordered by filename, and recorded in schema_migrations. A
transaction-scoped advisory lock serializes migration commands. Never edit an
already-applied migration; add a new numbered file. This tiny runner supports
transactional SQL only and has no rollback/checksum-management system yet.

make check runs validation and HTTP tests, the race detector, and go vet.
make integration uses the real database but creates a fresh random test schema,
then removes only that schema. It checks migration reruns, storage, constraints,
JSONB rejection, HTTP/server-and-connection-pool restart, and commit failure.

The commit-failure test temporarily adds a deferred uniqueness constraint only
inside its own schema. A duplicate passes INSERT ... RETURNING but fails COMMIT.
The test asserts that the API returns 503, not 202, and only one row remains.

## Questions for our discussion

1. Why is returning an ID before COMMIT risky?
2. What does the 202 response prove, and what does it not prove?
3. Why can retrying a timed-out request create two jobs?
4. What would need to be in the same transaction when we add idempotency?
5. Which failures should be 400, and which should be 503?

## References

- PostgreSQL transactions: https://www.postgresql.org/docs/17/tutorial-transactions.html
- Commit durability: https://www.postgresql.org/docs/17/runtime-config-wal.html
- Go JSON decoding: https://pkg.go.dev/encoding/json
- Go request-size limits: https://pkg.go.dev/net/http#MaxBytesReader
