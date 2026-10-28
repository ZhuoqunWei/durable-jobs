# Milestone 6: make the reliability claims repeatable

Implemented September 15, 2026, after the [guided tour](00-guided-tour.md).
No production behavior, dependencies or database migrations changed in this
milestone. The harness is a repository-local command, not a server or AI agent.

Historical scope note: September 16's [script-runner milestone](07-script-runner.md)
added a separate constrained Python CLI and real-container test suite. The named
queue scenarios in this document remain unchanged.

## Run it

From the repository root, with Docker and the project PostgreSQL running:

```sh
make harness-list
make harness
```

The harness uses the project's `DATABASE_URL` from the Makefile's `.env` setup.
The API and application worker do not need to run. Each check creates its own
randomly named test schema, applies the existing migrations there and cleans
up that schema. It never processes the existing application jobs.

To run only the two walkthrough examples:

```sh
make harness HARNESS_ARGS='-scenario campaign-fixtures,crash-recovery'
```

To print JSON without saving a report:

```sh
make -s harness HARNESS_ARGS='-json -report-dir ""'
```

`make build` also produces `bin/harness`. It still requires the repository,
Go toolchain and database: run it from this folder with `DATABASE_URL` exported.
It is not a self-contained binary containing the test suite.

## Decision 1: reuse the assertions, not the worker implementation

`internal/harness/scenarios.go` maps ten readable scenario IDs to exact test
names and expected behavior. Examples include campaign fixtures, duplicate
submissions, a transient retry, budget exhaustion, process-crash recovery,
stale-worker rejection and atomic attempt-history updates.

The harness runs those tests using `go test -json -race -count=1`. The explicit
test names allow a missing/renamed test to be detected. Disabling cached results
ensures each run actually executes the checks. We reuse the database integration
tests so there is one place to maintain the assertions, not a second imitation
of the queue that could pass while the real queue fails.

The report reader consumes Go's structured test events, not a search for the
word PASS in terminal text. The event format is documented in
[Go's test2json reference](https://pkg.go.dev/cmd/test2json); the test flags are
described in the [Go command reference](https://pkg.go.dev/cmd/go#hdr-Test_packages).

## Decision 2: distinguish “passed” from “not tested”

`internal/harness/report.go` requires a successful package completion and an
explicit passing event for every selected scenario. It retains child subtest
outcomes too. The core rule is:

```go
// A skipped child makes its parent incomplete even if Go passes it.
if e.Action == "fail" {
    r.Status = "fail"
} else if e.Action == "skip" && r.Status != "fail" {
    r.Status = "skip"
}
```

Overall status is `pass`, `fail` or `incomplete`. Per-scenario results can also
be `skip` or `missing`. A failed check or failed Go process makes the run fail.
A missing test, skipped child, absent package completion or interrupted stream
cannot become a pass. The CLI exits nonzero unless the run passed and output
was written successfully.

This matters when PostgreSQL is unavailable: a green compilation is not proof
of durability. The harness requires a database URL before starting and was also
checked against a deliberately closed local port; that invocation failed.

## Decision 3: make expected and observed data visible

Open `internal/database/testdata/campaign-fixtures.json`. Six synthetic cases
cover three valid migrations, an invalid calendar date, an unknown category and
an unexpected field. For example:

```json
{
  "name": "invalid_calendar_date",
  "payload": {
    "campaign_id": "fixture_bad_date",
    "category": "Pets",
    "created_at": "02/29/2026"
  },
  "status": "failed",
  "failure_code": "invalid_campaign"
}
```

`TestCampaignFixtureMatrixIntegration` runs each fixture through the actual HTTP
handler, SQL store, worker and campaign processor. HTTP requests are dispatched
in-process using Go's test recorder; this fixture test does not exercise TCP.
Each fixture asserts acceptance before execution, the exact final output,
identity/timestamps, agreement with public attempt history, no public token and
no extra processing of terminal work.

For the invalid date, a successful test records:

```json
{
  "fixture": "invalid_calendar_date",
  "expected": {"status":"failed","attempts":1,"failure_code":"invalid_campaign"},
  "observed": {"status":"failed","attempts":1,"failure_code":"invalid_campaign"}
}
```

The harness scenario is PASS because the job failed in the expected way.
Do not confuse the test's outcome with the job's outcome.

These structured observations are stored under the fixture scenario's
`evidence`. Other scenarios report their actual test/subtest verdicts against
the registry's expected assertions; the harness does not invent job snapshots
for tests that do not emit them.

## Reports and safety boundaries

Every normal invocation creates a distinct JSON file under `.local/harness/`,
which Git ignores. Report files have private permissions and do not overwrite
previous runs. They contain the command, selected scenarios, expected behavior,
observed verdicts, fixture evidence and elapsed seconds. They do not include
database URLs, environment dumps or arbitrary test/build output. Only explicit
synthetic fixture observations are retained; inspect `make integration` locally
for detailed failure diagnostics.

The harness validates that PostgreSQL hosts and fallbacks are loopback hosts.
Remote hosts and Unix sockets are refused. It clears inherited test-helper and
query-option overrides in the child environment. This is a local-demo guard,
not a security sandbox or permission boundary for hostile repository code.

The process-crash scenario kills exactly the owned test worker it just started.
It waits for real lease expiry/backoff and uses fresh CLI worker processes to
finish the same job. Some other scenarios advance timestamps only inside their
isolated schemas to cover boundary conditions quickly. There are no artificial
fault switches in the production worker.

The selected test suite has a 90-second timeout and the harness a two-minute
outer deadline. Graceful test completion cleans up owned schemas. A forced kill
or timeout can interrupt cleanup and leave an isolated test schema behind.
Inspect exact ownership before removing one; never reset the application
database or delete schemas by a broad name pattern.

## What was verified, and what this does not claim

The initial full run passed all ten named scenarios and six fixture cases.
`make check`, `make integration` and `make build` also passed. The report parser
has regressions for missing tests, skipped children, failed packages, malformed
events, truncated streams, private report files and omission of raw output.
Both existing application jobs remained queued and unchanged.

Durations are measured test runtimes, not job latency percentiles, throughput
benchmarks or production service-level guarantees. The full integration suite
still contains additional checks not selected by this demo harness.

Still unimplemented: sandboxed transformations, evaluation of arbitrary/AI
generated scripts, load testing, deployment and authentication. The next
bounded implementation milestone is a constrained script runner with a written
threat model; adding a harness alone does not make untrusted execution safe.
