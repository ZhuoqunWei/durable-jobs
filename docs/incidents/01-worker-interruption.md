# Controlled incident: worker lost before completion

Date: September 29, 2026 UTC. Environment: disposable private EKS lab.
This was deliberate failure injection, not a customer outage.

## Impact and detection

One synthetic job had a committed running lease when its worker Pod was forcibly
deleted. The job temporarily remained `running`; no successful result had been
committed. The driver detected recovery using the job API and persisted attempt
history, not merely a replacement Pod becoming Ready.

## Timeline

- **01:29:36.540 UTC:** scenario began; a scoped completion-write delay made the
  interruption reproducible for the very fast built-in campaign processor.
- The job reached `running`, then the original worker Pod was forcibly deleted.
- The test removed the scoped delay. The replacement worker could not reclaim
  the job until its original 30-second lease expired.
- Recovery marked the original attempt `lease_expired`; the normal retry delay
  elapsed; attempt two completed successfully.
- **33.252 seconds after the deletion request:** completion, attempt history and
  replacement-Pod checks had all passed. This measurement includes observation
  overhead, not just server-side processing time.

## Diagnosis and recovery

The worker disappeared after owning a lease but before a completion transaction
was committed. This is an ambiguous execution outcome, not a reason to return a
fabricated success. Recovery is performed by polling workers, not a hidden
queue daemon. The new attempt receives a new lease token. An old attempt cannot
later overwrite the new attempt's stored outcome.

No operator changed the job state to force success. The queue's normal expiry,
backoff and claim path recovered it. Observed evidence: two different Pod UIDs,
two attempt-history entries (`lease_expired`, `succeeded`), one final result.

## What this does not prove

The test kills before completion commit, not necessarily midway through CPU
computation. One stored completion does not guarantee one invocation or
exactly-once external side effects. The current processor is a pure synthetic
transformation. Payment/email integrations would require separate idempotency
or an outbox/effect protocol.

## Follow-ups

- Keep the timeout/lease invariant explicit and test stale-token completion.
- Before supporting long jobs, design lease renewal and cancellation together.
- A worker heartbeat is useful for detection but is not proof of job progress.
- Reproduce this locally and explain why immediate reclaim would be unsafe.

Evidence: `.local/phase12/report-rel_82e0c068ebf8.json`, scenario `worker-crash`.
The full raw report remains private; the failure runner is checked in.
