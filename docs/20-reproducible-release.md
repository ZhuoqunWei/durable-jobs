> Historical checkpoint: this report describes the September 29 local validation. See the root README and GitHub Actions for publication and hosted-CI status. Image publication is now opt-in through a manual workflow input.

# Phase 13 — reproducible release and interview practice

This is a local release-verification milestone. It does not deploy another AWS
lab, publish a repository, or claim remotely activated CI/CD. GitHub activation
requires the owner's repository/visibility choice and real remote runs.

## Observed validation

September 29, 2026 UTC:

- First run `run-f7414360edf6`: passed 02:53:45–02:55:36; recovery **31.716 s**.
- Clean source-copy run `run-49c22cb0e87e`: passed 02:59:27–03:01:13; recovery
  **31.589 s**. The source copy contained no `.env`, `.local`, `.git`, existing
  kubeconfig or database. It installed pinned kind and built its images anew.
- Both runs checked fresh job/replay/conflict/history, interrupted a committed
  worker claim, observed the same job's second successful attempt and verified
  their temporary node was absent after cleanup.
- Both reports and command logs are retained under `.local/phase13/` in the
  original repository. The original PostgreSQL volume and Phase 10 kind cluster
  were checked present afterward. Docker caches remain; this was not a cold-cache
  machine benchmark or a second person's independent reproduction.
- Thirteen new regression tests cover target guards, credential-environment
  isolation, recovery evidence and cleanup failure. Full Python platform tests,
  Go race/vet/integration tests, manifest/Prometheus checks, Staticcheck and
  pinned actionlint passed locally; `.local/phase13/validation.log` records the
  final run (90 primary Python tests and 24 AWS configuration tests). The opt-in
  real Docker script-sandbox suite was not rerun. Hosted GitHub execution remains
  unverified.

## Run from a fresh source checkout

Prerequisites: Docker running on a local Unix socket, Go, Python 3.9+, Make and
kubectl. No `.env`, AWS login, existing database, Helm or Terraform installation
is needed for this demo. First-run image/tool downloads need Internet access.

```sh
make lab-tools
make demo-plan
make demo-check
```

`demo-plan` makes no changes. `demo-check` builds the runtime and diagnostic
images, creates a uniquely named `durable-jobs-demo-*` kind cluster with its own
kubeconfig, migrates a fresh synthetic database, and deploys the existing local
manifests. It checks:

1. Fresh HTTP submission, correct output, keyed replay, conflicting-input 409,
   and completed attempt history.
2. A committed running job followed by forced worker-Pod interruption.
3. Recovery of the **same job ID** on attempt two; history must contain
   `lease_expired` then `succeeded`, and replay must preserve its result.
4. Removal of only the newly created cluster, followed by a fresh node-absence
   check. Cleanup failure makes the entire command fail.

Reports, image IDs, report checksums and command logs remain under the printed
`.local/phase13/run-*/` path. The temporary cluster's synthetic database is
disposable and removed with its node. Original application storage and the
`durable-jobs-clear` learning cluster are neither used nor removed. Images remain
cached locally. Do not publish the whole `.local` directory or its kubeconfigs.

An existing random cluster name is never adopted. Cleanup checks the exact
Docker node ID recorded at creation; changed ownership causes a refusal, not
a broad delete. If interrupted with SIGTERM/Control-C, cleanup is attempted.
A machine crash or SIGKILL can bypass cleanup: inspect `ownership.json` and
the exact node before manually removing anything. There is no global prune.

## Why a temporary completion delay?

The built-in campaign transformation is too fast to reliably interrupt by
guessing a delay. In the disposable database only, a trigger delays the first
completion of `phase13_crash` for eight seconds. This exceeds the worker's
three-second storage deadline. The harness observes the committed claim, kills
the worker Pod, and waits for the real 30-second lease to expire. It does not
shorten timestamps or write a fake successful result. The trigger is removed
in `finally`; attempt two is not delayed even if cleanup has not happened yet.

This demonstrates interrupted-claim recovery and fenced persisted completion,
not exactly-once execution of arbitrary external side effects. The workload is
synthetic and has no payment/email side effects. Forced Pod deletion alone is
not proof of instant process termination; final job and attempt evidence are
required, and the completion delay prevents a first-attempt successful commit.

## CI prepared here; remote enforcement is separate

The workflow now runs the disposable recovery demo against the exact image
artifact produced by the container checks. It does not rebuild a different
runtime image for release. The aggregate **Required checks** job fails if any
dependency failed, was cancelled, or was skipped. Main-branch image publication
depends on that gate and the infrastructure checks. PR jobs have no package-write
permission and no AWS credentials. Only the trusted main publication job can
write to GHCR. Kubeconfig is excluded from uploaded evidence.

GitHub documents why dependent required checks need an explicit `always()` gate:
[troubleshooting required status checks](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks).

Activation checklist, after the owner chooses the destination:

- Review only intended source files; exclude local reports, credentials and state.
- Configure the chosen remote and publish with explicit owner authorization.
- Observe an actual PR run including the local Kubernetes recovery job.
- Configure branch protection/rules to require **Required checks**, if supported
  by that repository/account; verify with a deliberately failing test PR.
- Confirm the failed PR cannot merge, then revert its deliberate failure.
- Observe a successful main run and verify the published commit-SHA image digest.

No workflow file can by itself prove merge blocking, hosted-run success, GHCR
publication or merge-to-AWS continuous deployment. Those remain unverified.
