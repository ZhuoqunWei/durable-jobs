# Phase 10: local Kubernetes and Python platform tooling

This is the first implemented part of the CLEAR-focused extension. It adds a
developer-facing deployment and diagnostic path around the existing Go service.
It is a local Kubernetes lab, not AWS/EKS experience or a production deployment.

## Run it

Verified September 28, 2026: 27 Python tests, Go race/vet/lint checks, manifest
rendering and Kubernetes server validation passed. The existing Phase 9
database/container suite also passed. The final nine-scenario run is recorded
in `.local/phase10/evidence-62d3ebcba0264afd924624a94ab4b320.json`; it directly
compares the paused job ID with the recovered job ID. An earlier successful run
is `evidence-a7d6d80cc92347f4b91dfb084864f2a0.json`; the initial rollout-timeout
report is `evidence-c8dedd8821984b7fa16a0732f6ae0c82.json`. These ignored files
are local evidence, not remote CI results. The workflow now includes Python unit
tests but has not run remotely, and does not yet run the kind failure exercise.

Requirements: Docker running, Go, Python 3.10+ on macOS/Linux, Make and kubectl.
The tested client is kubectl 1.34.1; keep a client compatible with the pinned
Kubernetes 1.35.8 server. `make lab-tools` installs kind 0.33.0 into this repo's
ignored `bin/` directory, not a system package directory.

```sh
make platform-check  # Python tests, manifest rendering and shell syntax
make lab-tools
make lab-up          # Creates/reuses ONLY the dedicated local cluster
make lab-status      # Read-only Kubernetes resource summary
make lab-doctor      # In-cluster DNS, TCP and readiness checks; no job submission
make lab-verify      # Fresh synthetic job, completion, replay, conflict and history
make lab-check       # Opt-in failures/restarts; briefly pauses the lab worker
```

Running `python3 platform_tools/lab.py` with no arguments prints a plan and does
nothing. Mutating commands require an explicit context, namespace and kubeconfig;
the Make targets supply the dedicated lab values. Other targets are rejected.

`lab-up` saves build logs and image identities to `.local/phase10/build.<id>/`.
`lab-check` saves a JSON report to `.local/phase10/evidence-<id>.json`, including
expected failures. The whole run passes only if they fail at the expected stage.
Reports show elapsed exercise times, not a measured availability SLO.

## What runs where

- A single kind node, `durable-jobs-clear-control-plane`, runs in Docker.
- Namespace `durable-jobs-lab` contains two API replicas, one worker and one
  PostgreSQL replica. These are not separate failure domains.
- ClusterIP Services provide API routing and database service discovery.
- PostgreSQL uses a StatefulSet and a 1 GiB persistent volume claim.
- A one-off migration Job must succeed before the application rollout starts.
- Short-lived Python diagnostic Jobs run inside the cluster, so they test
  cluster DNS and Service networking rather than a laptop port-forward.

The existing Compose database and its volume are never used. The new lab uses
only synthetic records. Its sample Secret contains intentionally public local
credentials and must never be reused on AWS. Kubernetes Secret encoding is not
encryption. The ordinary application `.env` is not read by the platform scripts.

The Kubernetes API binds to host loopback. No application host port, NodePort,
Ingress, cloud load balancer or external DNS record is created. The default kind
network does not provide a demonstrated NetworkPolicy isolation boundary. The
namespace label/context checks prevent common targeting mistakes; they are not
an authorization system or a hostile multi-tenant security boundary.

## Decision 1: keep the workload in Go, write operations tooling in Python

`platform_tools/lab.py` coordinates Kubernetes; `platform_tools/probe.py` checks
network and application behavior. Both use only Python's standard library.
There is no second queue implementation, third-party framework or frontend.

The API, worker, migrations and domain rules retain the existing static Go image.
The worker explicitly runs `-processor builtin`; it cannot execute Python
catalog jobs here. The existing script sandbox stays a separate local exercise.
No Docker socket is mounted into any application or diagnostic pod.

Python diagnostic containers run without root, with a read-only filesystem,
dropped capabilities, CPU/memory limits and no service-account token. They have
no Kubernetes or AWS credentials. They are trusted project code, not an
arbitrary-code execution service.

## Decision 2: readiness is not job progress

In `deploy/kubernetes/base/app/api.yaml`:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: http
readinessProbe:
  httpGet:
    path: /readyz
    port: http
```

The first asks whether the HTTP process responds. The second checks database
connectivity and governs whether the Service should route to that pod. Restarting
a healthy API just because the database is unavailable would not fix the database.

Neither proves a worker is progressing. The worker currently has no custom
heartbeat probe: Kubernetes reports its process/container state, not queue health.
`verify` fills that gap with an end-to-end synthetic check. A future heartbeat
metric will support continuous monitoring; this command is not a replacement.

## Decision 3: a diagnostic should identify the failing layer

In `platform_tools/probe.py`, each successful stage is recorded before the next:

```python
socket.getaddrinfo(client.host, client.port, type=socket.SOCK_STREAM)
with socket.create_connection((client.host, client.port), timeout=client.remaining()):
    pass
ready = client.request("GET", "/readyz", 200)
```

If DNS fails, do not call it a worker problem. If DNS succeeds but TCP fails,
inspect service endpoints/connectivity. A closed port does not prove a firewall
rule is the cause. If readiness passes but a fresh job stays queued, investigate
worker replicas, logs and database access. Reports offer hypotheses, not invented
root causes, and never automatically change security rules or routing.

The Python HTTP client refuses redirects/proxies, checks status and JSON shape,
limits response size, and bounds request time. A process deadline also bounds
DNS lookup. The Kubernetes Job has a further deadline; host subprocess calls
have timeouts. A failed command or malformed report cannot become a green check.

## Decision 4: fresh keys detect stopped workers

Normal verification generates a new idempotency key for each invocation:

```python
key = "phase10-" + uuid.uuid4().hex
```

Reusing a fixed key could retrieve yesterday's completed job while today's
worker is stopped. Explicit key reuse is reserved for persistence/replay tests.
Verification checks exact transformed output, timestamps, a single attempt for
the fault-free fixture, an identical replay, a 409 for changed input and matching
attempt history. None of this claims exactly-once execution under failures.

## Failure exercises

`make lab-check` records nine scenarios:

1. A fresh job completes with the expected result and history.
2. A deliberately nonexistent `.invalid` name fails at DNS.
3. The real API hostname with a closed port fails at TCP after DNS succeeds.
4. With zero workers, API connectivity/readiness still succeeds.
5. A fresh job then fails the progress deadline, correctly rejecting false health.
6. Restoring the worker completes that same keyed queued job.
7. API rolling restart preserves the original baseline job/result/attempt.
8. Worker rolling restart preserves the same baseline.
9. PostgreSQL pod restart reattaches storage and preserves the same baseline.

The test restores the worker in a `finally` block; forced termination or loss of
the cluster API can prevent that. In that case use `make lab-up` to restore the
declared replica count. The test refuses customized initial worker counts.
This is not a worker kill mid-execution experiment; that remains covered by the
existing database/process tests, with a Kubernetes-specific version still future.

## Data retention and rollback limits

The cluster, PVC and synthetic jobs remain for the next learning session. They
consume local Docker resources but create no AWS charges. Completed diagnostic
and migration Jobs expire after ten minutes; their JSON evidence is saved before
expiration. No persistent database data is automatically deleted.

The volume survives a database pod restart, not deletion of the kind cluster,
loss of its Docker storage or a laptop failure. This is neither backup nor HA.
Do not delete the cluster or its PVC unless you intend to discard its lab data.

API/worker Deployments use rolling updates with `maxUnavailable: 0` and retain
three revisions. This is not automatic rollback. Database migrations are not
rolled back by restoring an application image. Mixed-version schema compatibility
must be reviewed before future schema changes; an unsafe migration must not be
treated as a routine zero-downtime rollout. Local images use explicit loaded tags;
the deployment command restarts pods after loading a new image. Future remote
deployments should use immutable image digests.

API pods also have a five-second minimum ready period and a five-second
kubelet-managed pre-stop delay, leaving time for Service endpoint changes before
Go's graceful shutdown. These are intended to reduce rollout transition races;
they do not prove zero downtime. The initial exercise recorded an HTTP timeout
after an API rollout; the exact cause was not conclusively isolated. Subsequent
runs passed with the drain period. Keep the failed report as well as those runs.

## What remains for CLEAR

- Phase 9 remote CI/GHCR/merge protection activation (no GitHub remote yet).
- Terraform-managed VPC, route tables, security groups and EKS.
- Private ALB / Route 53 integration and read-only boto3 network diagnostics.
- Metrics, alerts, Datadog validation, SLOs and deployment rollback exercises.
- A user-tested quickstart and recorded real developer feedback.

Cloud provisioning, credentials and spend require separate approval. Static
Terraform checks and a kind demonstration must not be described as an AWS run.

## Ten-minute learning exercise

Read `probe.py:diagnose`, then `verify_job`, then the API manifest. Predict which
check still passes when workers are stopped. Run the failure exercise and find
the two corresponding reports. Explain why they disagree without saying
"Kubernetes is broken." Then change one diagnostic message and its test yourself.

## Sources

- [kind setup and isolated kubeconfig](https://kind.sigs.k8s.io/docs/user/quick-start/)
- [Kubernetes probe semantics](https://kubernetes.io/docs/concepts/workloads/pods/probes/)
- [StatefulSets and storage retention](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)
- [Kustomize](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/kustomization/)
