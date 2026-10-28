# Phase 9 Packaging and CI

This extends Durable Jobs without changing its queue, lease, retry or completion
rules. It packages the service and verifies the real API/worker path in a fresh
environment. Kubernetes and observability are later milestones.

## Run the demonstration

From this repository, with Docker running:

```sh
make container-check
```

Docker with Compose and Make are the only host requirements for this command.
The compiler, race detector's C toolchain, and linter run in a checks container.
The first run downloads pinned dependencies and images.

The command builds images, starts disposable Postgres, runs formatting/lint/vet,
unit and integration tests with the race detector, applies migrations, starts
API/worker, and verifies an actual synthetic job. It then restarts API/worker
and confirms the same completed job and all attempt data remain unchanged.
It does not restart Postgres; that is a later failure-injection scenario.

## Isolation and cleanup

`compose.phase9.yaml` is deliberately separate from `compose.yaml`. It has no
host ports, bind mounts, Docker socket or named database volume. Its Postgres
uses temporary memory-backed storage. A random project name isolates each run.
The script never starts the existing application database or reads its rows.

On completion or failure it collects logs, then removes only its own containers
and network. Its disposable test data is lost; existing application volumes are
untouched. It never uses `down --volumes` or pruning. Built images and caches
remain. SIGKILL or a Docker outage can prevent cleanup; inspect the exact project
name in the report rather than running a global cleanup command.

Reports are in the ignored `.local/phase9/run.<random>/` directory:

- `checks-build.log`, `runtime-build.log`, `checks.log`: builds and test output.
- `image.json`, `run.txt`: image identity, configured user and Docker platform.
- `smoke-before.json`, `smoke-after.json`: job ID, attempt count and checks.
- `restart.txt`: comparison of all job and attempt rows across process restarts.
- `services.log`, `containers.json`, `cleanup.log`, `exit-code.txt`: operational evidence.

This command deliberately skips opt-in tests requiring the host Docker daemon.
The existing sandbox unit tests still run, but real Python isolation/script-queue
tests remain separate: `make sandbox-check` and `make script-queue-check`.

## Decision 1 Build tools do not belong in the runtime

Read `Dockerfile` first. Its named stages have distinct jobs:

```text
source + Go compiler
  checks  -> lint, vet, race and integration tests
  build   -> compile api, worker, migrate and smoke
  runtime -> static executables + CA certificates, no compiler or shell
```

This instruction produces the executables:

```dockerfile
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate ./cmd/smoke
```

`CGO_ENABLED=0` avoids a runtime C-library dependency. Race tests run separately
with the C toolchain enabled. The final `scratch` image contains only these
executables and explicitly copied CA certificates. `USER 65532:65532` runs
them without root privileges.

One image contains four executable roles. The default entrypoint is `/api`;
Compose selects `/worker`, `/migrate` or `/smoke` when appropriate. This keeps
all roles on the same source version without three nearly identical builds.

The Go and Postgres base images use version + multi-platform digest pins.
Staticcheck is pinned to v0.8.1. Host lint uses the project's selected compiler:
a linter built with an older Go toolchain may not understand the newer standard
library. Updating these pins requires a reviewed change and another test run.
`.dockerignore` is an allowlist: `.env`, Git metadata, reports and unrelated
host files never enter the build context.

## Decision 2 Container networking is explicit

The host API defaults to loopback. The image sets:

```dockerfile
ENV HTTP_ADDR=0.0.0.0:8085
```

That lets other containers reach the API; it does not publish a host port.
Inside this Compose network, `api:8085` means the API and `postgres:5432` means
the database. `localhost` inside a worker means that worker, not Postgres.

Compose waits for Postgres readiness, runs migrations, then starts API/worker.
The API health check runs `/smoke -ready-only`, which only GETs `/readyz` and
never creates a job. Runtime roles have read-only filesystems, dropped Linux
capabilities and `no-new-privileges`; these are restrictions, not a claim of
hostile multi-tenant isolation.

The container worker uses `/worker -processor builtin`, processing only
`migrate_campaign`. Existing local commands default to `versioned` and retain
reviewed Python catalog jobs. A script submitted to this lab is terminally
rejected as `unsupported_job_type`; it is not executed elsewhere. Do not share
this lab queue with script workers. No Docker daemon access is provided.

## Decision 3 Readiness is not evidence of job completion

Read `cmd/smoke/main.go`. The smoke command checks:

1. API readiness.
2. A synthetic POST returns a job ID.
3. GET eventually returns the exact transformed output, timestamps and one
   attempt for this fault-free fixture.
4. An identical keyed POST returns the same completed job.
5. Changed input under the same key returns 409.
6. Attempt history contains one committed successful attempt.

Requests and the whole run have deadlines. Tests simulate failure, stuck jobs,
wrong output, missing history and other invalid responses to prevent false
success. The fixed `phase9-smoke-v1` key is for the disposable database only.
Do not point the full smoke command at an application database; it creates work.

One attempt in this baseline is not an exactly-once execution guarantee.
Crashes may cause repeated computation; fenced completion protects the stored
result. The restart check verifies process-restart persistence, not host-failure
durability of this temporary database.

## Decision 4 PR checks and publishing have different permissions

`.github/workflows/ci.yaml` runs `make container-check` for PRs and pushes to
`main`. `Phase 9 checks` has read-only repository permission. `set -o pipefail`
ensures logging with `tee` cannot hide a failed command.

After a successful main-branch push, CI saves the exact tested runtime image.
A separate job loads that artifact and publishes
`ghcr.io/<owner>/<repository>:sha-<git-sha>`. Only this trusted-main job has
`packages: write`. PRs never log in or publish. Actions use resolved commit
SHA pins; checkout does not persist its token.

A SHA tag identifies a commit but is not registry-enforced immutability. Future
deployments should use the resulting digest. CI builds its native Linux
architecture; this is not a tested multi-architecture release.

## GitHub activation is still pending

The repository currently has no GitHub remote. No repository or image has been
published. The workflow has been checked locally, not observed running remotely.
After the owner chooses a repository and authorizes publication:

1. Push the source/workflow and observe a real successful CI and GHCR run.
2. Protect `main`: require PRs, the `Phase 9 checks` status, up-to-date branches,
   and disallow bypasses where the account permits.
3. Open a deliberately failing PR and verify that GitHub blocks its merge.

Until then, do not claim that remote CI or merge blocking is active. Existing
GHCR packages may require explicitly granting this workflow access.

## Your ten-minute walkthrough

Start with `Dockerfile`, then `compose.phase9.yaml`, then `cmd/smoke/main.go`.
Predict before running:

- Why can the checks image use a compiler while the runtime has none?
- What would happen if the worker connected to `localhost:5432`?
- Which check fails if the API accepts work but no worker is running?
- Why should a repeated keyed request after restart return the old job?

Use the two smoke reports to explain a successful run. Do not memorize YAML.
Before Kubernetes, be able to explain the image, the network address and the
evidence that distinguishes acceptance from completion.

## References

- [Docker multi-stage builds](https://docs.docker.com/build/building/multi-stage/)
- [Staticcheck](https://staticcheck.dev/docs/)
- [GitHub image publishing](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images)
- [GitHub protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
