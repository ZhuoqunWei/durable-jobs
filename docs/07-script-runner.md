# Milestone 7: a constrained local Python runner

Original milestone scope: a CLI-only transformation runner, separate from the
queue. The subsequent [versioned-script milestone](08-versioned-script-jobs.md)
connects reviewed catalog scripts to API/worker processing. This document covers
the runner itself. No arbitrary script-upload endpoint, cloud deployment or AI loop.

## Threat model, before implementation

Protect the developer's files, credentials, network and machine resources from
ordinary buggy or deliberately abusive transformation scripts. Treat script
source, its input, stdout and stderr as untrusted. Trust the local operator,
checked-in bootstrap, pinned base image, Docker CLI/daemon and Linux kernel.

The chosen boundary is a new local Docker container per invocation. It is not
Python `exec` by itself, an import blacklist, or a Go context deadline alone.
The Python bootstrap provides a protocol; it is not a security boundary.

Required controls:

- Refuse non-Unix Docker endpoints; pin all subsequent calls to the resolved
  local endpoint. Refuse unsupported daemon security/resource prerequisites.
- Use a trusted prebuilt image resolved to its immutable local image ID. Never
  automatically pull an arbitrary image while running a script.
- No host bind mounts, Docker socket, application environment or volumes.
  `network=none`: no usable external network links or routes. The kernel may
  still expose inactive tunnel devices inside the isolated network namespace.
- Non-root UID/GID, all Linux capabilities dropped, no-new-privileges, default
  seccomp, private cgroup namespace and no shared IPC namespace.
- Read-only root filesystem; only a small, ephemeral `/tmp` is writable.
- Bound script/input/output sizes, CPU, memory, swap, process count and runtime.
  Disable container logging so flooding stdout does not fill the daemon's disk.
- On timeout, cancellation, output overflow or normal exit, explicitly remove
  only the exact container owned by this invocation. Killing the attaching
  Docker client is not sufficient to stop the container.
- Validate output on the host. Never trust a script's claim that it obeyed the
  protocol. Return safe failure codes, not raw tracebacks or command output.

Non-goals and residual risk:

- Containers share the daemon's Linux kernel (inside Docker Desktop's VM on
  this Mac). Kernel/runtime escapes and side channels are outside this demo's
  guarantee. Do not offer this as a hostile multi-tenant execution service.
- Limits are per container, not admission control across many runner processes.
- An operator can weaken the host or replace trusted code/images. This runner
  does not defend against a malicious Docker daemon or host administrator.
- A killed host runner, stopped daemon or machine crash can interrupt cleanup.
  Orphan containers may remain. Never sweep unrelated containers or volumes.
- Syntactically valid JSON is not proof of correct business behavior. Scripts
  can fabricate output; campaign fixtures and domain validation remain needed.
- Arbitrary reads/writes inside the isolated image and `/tmp` are possible.
  Isolation from the host is not a claim that Python modules are restricted.

This local design keeps the Go/PostgreSQL project independent of Cloudflare.
Adopting a managed sandbox or stronger VM boundary would be a separate decision.

References for the controls and their limitations:
[Docker runtime options](https://docs.docker.com/engine/containers/run/),
[Docker security model](https://docs.docker.com/engine/security/), and
[Python isolated mode](https://docs.python.org/3.13/using/cmdline.html#cmdoption-I).

## Implemented and verified September 16, 2026

The runner and the controls above are implemented. The local demo passed, along
with 23 real-container cases, the default unit/race/static checks, the database
integration suite and the existing ten-scenario reliability harness. No Go
dependencies, database migrations or production-worker changes were required.

## Run the demo

From the repository root, with Docker Desktop running:

```sh
make sandbox-image  # Explicit setup; the image is already built on this machine.
make sandbox-demo
make sandbox-check
```

The image build can download the pinned official Python base image. Execution
does not pull images or use container networking. The build context contains
only the Dockerfile and bootstrap, not `.env`, the repository or job data.

To run another small synthetic transformation:

```sh
go run ./cmd/sandbox -script examples/transform-campaign.py -input examples/campaign-input.json
```

The file must define a function returning a JSON-serializable object:

```python
def transform(record):
    return {"campaign_id": record["campaign_id"], "category_id": "animals"}
```

That tiny example illustrates the interface only; it does not validate categories.
`examples/transform-campaign.py` contains the fuller synthetic campaign example,
and its output matches all six existing campaign fixtures. Invalid campaign
fixtures produce `script_failed` here; the standalone runner does not turn Python
exceptions into the queue's domain-specific `invalid_campaign` code.

The CLI returns a JSON outcome containing `status`, bounded `output` on success,
a safe `failure_code` on failure, the immutable image ID, script SHA-256,
container name, elapsed milliseconds and `cleanup_confirmed`. Exit status is
zero only for a successful result with confirmed cleanup. Raw stderr, source
code, input and exception messages are not copied into failure responses.

## Fixed limits and defaults

| Resource | Limit |
| --- | --- |
| Python source | 16 KiB of valid UTF-8 |
| Input | One valid JSON object, at most 64 KiB |
| Stdout / accepted JSON output | 64 KiB total stdout, one JSON object |
| Stderr | 4 KiB; never returned as raw diagnostics |
| Start/attach execution phase | 3 seconds, including container startup/attach overhead |
| Memory and total memory+swap | 64 MiB each; no extra swap allowance |
| CPU quota | Half of one CPU |
| Processes | 32 total, including init and Python |
| Writable scratch | 8 MiB `/tmp`, noexec/nosuid/nodev |
| Open files / core dumps | 64 per process / disabled |
| Docker control and cleanup | Five-second deadlines; cleanup uses an independent context |

The execution deadline is not a promise of a three-second total CLI duration:
preflight, creation and cleanup take additional time. A hung daemon can prevent
confirmed cleanup; the runner then fails explicitly. Limits cannot be disabled
through script input or CLI flags.

The container runs as UID/GID 65534. It has no ports, host mounts or image-declared
volumes. Client proxy settings are explicitly cleared before container creation
so proxy credentials cannot be injected. The trusted image is looked up once
and all create calls use its immutable ID, not a tag that can move mid-run.

## Read the Go code in this order

1. `cmd/sandbox/main.go`: read bounded files, prepare a runner, print an outcome.
2. `internal/sandbox/types.go`: request/object validation and result contract.
3. `internal/sandbox/docker.go`: local-daemon prerequisites, image verification,
   command execution and bounded output collectors.
4. `internal/sandbox/runner.go`: create, attach, inspect, validate and clean up.
5. `sandbox/runner.py`: the container-side JSON protocol adapter.

Source and input travel through stdin, not a shell command or host-mounted
file. The host uses an argument array with `exec.CommandContext`; no `sh -c`
interpolation is involved. The bootstrap executes Python inside the container,
calls `transform`, serializes an object, and catches ordinary script failures.
The host independently validates the bytes it receives—even a script that
bypasses the bootstrap with `os.write` or `os._exit` must satisfy that boundary.

The input encoder disables HTML escaping. Otherwise a valid 64 KiB object
containing many `<` characters can expand far beyond the bootstrap's bounded
envelope size. A real-container regression covers maximum-size source and input.

## Why cleanup is part of success

The important code is the deferred cleanup in `Runner.Run`:

```go
defer func() {
    result.CleanupConfirmed = r.cleanup(result.Container, owner)
    if !result.CleanupConfirmed {
        result.Status, result.FailureCode, result.Output = "failed", "cleanup_failed", nil
    }
}()
```

It is installed before `docker create`, because an uncertain create response
may still leave a container behind. Cleanup looks for the exact random name
and both invocation-specific ownership labels, requires one valid container
ID, removes only that ID and confirms its absence. No broad cleanup/prune runs.
It also runs after successful execution so writable scratch and child processes
do not persist between calls.

If a failure reports `cleanup_failed`, keep its container name. Inspect that
exact container's name, labels and ID before any manual removal. Do not use
`docker system prune`, reset the PostgreSQL volume or delete all containers
with a loosely matching prefix. Killing the host CLI with SIGKILL or stopping
Docker can bypass the cleanup path entirely; orphan detection is not automated.

## Two implementation bugs the tests caught

First, a copied bootstrap's parent directory was not traversable by the
non-root user. Creating that directory explicitly in the Dockerfile fixed the
demo without changing its UID or making the root filesystem writable.

Second, embedding `bytes.Buffer` in a bounded output writer promoted methods
such as `ReadFrom` and `WriteString`. Those methods let callers bypass the
bounded `Write` method. The collector now contains a private buffer instead:

```go
type boundedWriter struct {
    buffer   bytes.Buffer
    limit    int
    exceeded bool
    cancel   context.CancelFunc
}
```

Unit tests reject those bypass interfaces; real stdout/stderr flood tests prove
that exceeding a stream's cap stops execution and removes the container.

## Verification and remaining boundaries

`make sandbox-check` is opt-in because it creates real disposable containers.
Before executing probes, tests inspect the daemon's actual container settings.
They verify non-root identity, zero effective capabilities, no-new-privileges,
seccomp enforcement, missing host files/environment, no usable external network,
read-only root, bounded writable scratch, process limits, actual OOM handling,
infinite-loop termination, stdout/stderr bounds, output validation and cleanup
after cancellation. They also exercise the six campaign fixtures and input-size
boundaries. CPU quota configuration is checked; no throughput guarantee is claimed.

Unit tests additionally cover missing security/resource prerequisites, nonlocal
Docker endpoints, images with unwanted volumes, uncertain create acknowledgements,
cleanup failures, ambiguous deletion targets and canceled callers. Default
`make check` runs these unit tests but skips the real-container cases.

The original reliability report still evaluates the queue; `make sandbox-check`
is a separate suite. The subsequent [versioned-script milestone](08-versioned-script-jobs.md)
implements the catalog contract and queue failure policy, tested separately by
`make script-queue-check`. A stronger VM
boundary, aggregate admission control and security review would be needed before
considering public or hostile multi-tenant execution. AI orchestration remains
optional and unimplemented.
