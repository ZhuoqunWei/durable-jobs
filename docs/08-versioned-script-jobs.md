# Milestone 8: versioned scripts in the durable queue

The API now accepts `run_script` alongside `migrate_campaign`. The same durable
claim, retry, recovery, fencing and history machinery applies to both. The
production CLI worker dispatches built-in jobs to Go and catalog script jobs to
the constrained local Docker runner. No new Go dependency or cloud service.

## What to run first

For a repeatable end-to-end check that does **not** process application jobs:

```sh
make script-queue-check
```

Prerequisites: `.env`, the running project PostgreSQL container, and the image
from `make sandbox-image`. The image is already built on the development Mac.
The check starts its own loopback HTTP server, random database schema, and
freshly compiled worker processes. It uses all six synthetic fixtures, then
removes its own schema. No separate API or polling worker is required.

The first fixture deliberately makes runner initialization fail before any
container starts. Attempt one records `transient_failure`. After the actual
one-second backoff, a fresh CLI worker completes attempt two with the same pins.
The other valid fixtures succeed once; invalid fixtures fail once. It also
checks duplicate POSTs, conflicting image IDs, API/store restart, worker restart,
ordered attempt history, invalid metadata, and refusal to substitute an image.
This is not a script-container crash/orphan-recovery test or a benchmark.

## The job contract

Run `make script-job` to print a complete synthetic request with the current
trusted image's actual immutable ID. It performs read-only runner preflight;
it does not submit a job or execute Python. The shape is:

```json
{
  "type": "run_script",
  "payload": {
    "script": "campaign_migration",
    "version": "1",
    "source_sha256": "5242e0048e191a58c3480685f892f17dcebf3cca6437eeb820cd4b84264d0d68",
    "image_id": "sha256:<the 64-hex ID printed by make script-job>",
    "input": {
      "campaign_id": "campaign_42",
      "category": "Pets",
      "created_at": "08/31/2026"
    }
  }
}
```

The illustrative image placeholder is not valid submission input. Use the
generated request. Only `campaign_migration` version `1` is registered so far.
Source, file paths, tags such as `latest`, unknown versions, unknown fields,
case aliases and duplicate metadata keys are rejected before persistence.
The API never needs Docker access to accept this contract.

The complete HTTP body remains limited to 64 KiB. PostgreSQL renders JSONB with
extra whitespace, so the internal parser allows a bounded 128 KiB representation
and compacts input back to at most 64 KiB. Numeric exponent expansion can exceed
that compact limit; the store checks the returned JSONB before COMMIT and rejects
it with 400 rather than accepting a job the runner cannot read.

To run the application manually, stop older API/workers, apply migration 005
with `make migrate`, start `make run`, submit a generated request with a stable
Idempotency-Key, then run `make worker`. **The worker processes any eligible job,
including the two older queued demo jobs.** Use the isolated check above when
you want to preserve those jobs. Never pipe a failed generator's partial output
blindly into a POST; inspect/save its successful JSON output first.

## Decision 1: version identity is more than a friendly name

Read [scripts/catalog.go](../internal/scripts/catalog.go). The reviewed source
is embedded from `internal/scripts/campaign_v1.py` when Go builds the binary.
`Resolve` checks the requested name, version and hash against the manifest and
computes SHA-256 of the actual embedded bytes. Version 1 is immutable; a behavior
change should get a new file, version and hash while retaining old versions.
Editing `examples/transform-campaign.py` changes only standalone experiments.

Read [worker/scripts.go](../internal/worker/scripts.go):

```go
if runner.ImageID() != payload.ImageID {
    return fail(jobs.ScriptVersionUnavailable)
}
result := runner.Run(ctx, sandbox.Request{Script: source, Input: payload.Input})
```

The runner resolves only the operator's trusted local tag and executes its
captured immutable ID. The client cannot select an arbitrary local image by
having its ID accepted syntactically. If the operator rebuilds/retags the image
to a different ID, an older queued job fails rather than silently changing
runtime. There is no runtime archive or multi-image registry in this milestone.
Preserve compatible binaries/images if you need old versions to keep running.
These pins prevent substitution; they do not prove deterministic Python behavior
or bit-identical results across different machines.

On success, `job.result` contains `output`, `script`, `version`, `source_sha256`,
`image_id`, and `cleanup_confirmed: true`. The combined result must fit 64 KiB;
oversized output plus metadata fails rather than being truncated. Arbitrary
stderr, exceptions, source text, lease tokens and container names are not stored
in public results/history. Failed jobs retain their original pins in payload.

## Decision 2: retry infrastructure problems, not a broken script

Read `VersionedProcessor.Process` and [jobs/retry.go](../internal/jobs/retry.go).

- Runner preflight, create, attach or state-inspection failure with confirmed
  cleanup: existing `transient_failure` policy, at most three attempts.
- Script exception, invalid output or other script failure: `script_failed`,
  terminal. Script runtime/memory/output limits: `script_limit_exceeded`, terminal.
- Unavailable/mismatched version, source or runtime identity:
  `script_version_unavailable`, terminal, no substitute execution.
- Overall worker deadline with confirmed cleanup: `processing_timeout`, bounded
  retry. This is distinct from the runner's own three-second execution limit.
- Unconfirmed cleanup: `sandbox_cleanup_failed`, terminal when its fenced write
  succeeds, and a fatal signal to stop that worker process.

Unknown failures remain terminal by default. Clean cancellation leaves a running
claim to recover later; it does not acknowledge successful processing.

## Decision 3: reserve time for cleanup and stop on uncertainty

The CLI uses `ScriptConfig`: 60-second lease, 30-second overall processing
deadline (including runner preflight), up to six seconds of independent cleanup
grace, and three-second storage calls. The constructor requires:

```go
lease > processing + cleanupGrace + 2*storageTimeout
```

The older `DefaultConfig` remains 30/10 seconds for trusted-only unit/harness
cases. The script runner's execution phase is still limited to three seconds.
These are deadline budgets, not a hard wall-clock guarantee during host/daemon
suspension. No lease heartbeat has been added; fencing still rejects expired
completions after reacquiring the database lock.

`RunOnce` gives `ErrStopWorker` priority over cancellation/deadline errors. It
tries to record `sandbox_cleanup_failed` under a fresh bounded storage context,
then returns the fatal error even if that write fails. `Run` and `-once` propagate
it, so the process exits nonzero instead of claiming more jobs.

If that terminal database write fails or loses its lease, the job can remain
running and another worker can eventually recover it. This is not a global
circuit breaker: stop other workers too and inspect sandbox-labeled containers
before restarting after a cleanup alarm. Do not auto-prune unrelated resources.
SIGKILL, machine crashes and daemon failures can still leave orphan containers;
there is no durable container ownership ledger/reaper. Queue lease recovery does
not prove those containers were removed. The fixed reviewed catalog reduces
exposure; public arbitrary-code execution remains explicitly out of scope.

## Verify and explain

```sh
make check
make integration
make script-queue-check
make sandbox-check
make harness
make build
```

The ten-scenario harness is unchanged; the combined Docker/queue test is opt-in
and separate. SQL tests additionally prove migration 005 preserves every old
job/history field and every new failure code stays terminal. Unit tests cover
hash mismatch, image mismatch, bounded results, the cleanup/deadline race and
fatal cleanup even when recording its outcome fails.

Three useful interview/learning questions:

1. Why can retrying `latest` change a job's meaning even with the same job ID?
2. Why does killing the Docker attach client not prove the container stopped?
3. Why must the worker stop after unconfirmed cleanup even when SQL is down?

The next portfolio polish is a concise demo narrative and evidence summary.
Authentication, multi-tenant admission limits, managed isolation, orphan reaping,
lease renewal and optional AI proposal/revision are not implemented.

Verified September 16, 2026: all six commands above passed. The six queued script
fixtures, actual transient backoff/retry, restart/replay and image-refusal checks
passed. Migration 005 and the API refresh preserved the two existing application
jobs byte-for-byte across the full-row checksum, with zero attempts. No polling
application worker, owned test schema or sandbox container remained afterward.
