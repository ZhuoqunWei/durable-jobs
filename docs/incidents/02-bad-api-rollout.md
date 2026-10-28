# Controlled incident: a crashing API release

Date: September 29, 2026 UTC. Environment: disposable private EKS lab.
This was a deliberately invalid executable, not a discovered production bug.

## Impact and detection

The test changed the new API Pod's command to `/not-a-real-binary`. Kubernetes
could not start it. The driver observed its container failure state and checked
the Deployment replica counts while making HTTPS readiness requests.

The two previously healthy replicas remained available. This demonstrates the
rolling-update guard in this single-node lab; it does not establish zero outage
for every possible deployment or node failure.

## Timeline

- **01:32:24.367 UTC:** the scenario began and saved the known-good Pod template.
- Kubernetes created one updated surge replica. Its invalid executable prevented
  readiness; the prior two replicas were not removed.
- The driver verified the failure, then automatically replaced the entire Pod
  template with the saved version and waited for rollout completion.
- **6.499 seconds from the initial patch request:** detection, restoration and
  rollout confirmation had completed. HTTPS readiness checks during the drill
  succeeded.

## Diagnosis and fix

This was an application launch failure, not DNS, ALB, PostgreSQL or image-pull
failure. `maxUnavailable: 0` prevents intentionally making old replicas
unavailable until replacements pass readiness. `maxSurge: 1` reserves an extra
Pod during the transition. Spare scheduling capacity is necessary.

The test restores the exact template with a JSON replace operation. A partial
merge can accidentally retain the invalid `command` field even after restoring
the old image. A regression test verifies that restoration still runs if an
intermediate probe fails.

## Limits and follow-ups

- This is automatic rollback **inside the test driver**, not a deployed GitHub
  merge-to-cluster CD pipeline or Kubernetes performing rollback on its own.
- A backwards-incompatible schema migration may make application rollback
  unsafe; use expand/contract changes and migration compatibility checks.
- Keep readiness separate from liveness and exercise the full job smoke test
  before promoting a release.
- Keep enough node capacity for the surge replica; a Pending Pod is different
  from a crashing executable and requires a different diagnosis.

Evidence: `.local/phase12/report-rel_82e0c068ebf8.json`, scenario `bad-deploy`.
No database volume, job record or service endpoint was deleted for this drill.
