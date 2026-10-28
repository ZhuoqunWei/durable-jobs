# Phase 11B: prepare the application for AWS

Date: September 28, 2026

**Implemented:** a separate AWS Kustomize profile, offline release renderer,
configuration rejection tests, and a disposable local PostgreSQL/TLS/job test.
**Not implemented in this phase:** AWS deployment, EBS CSI installation/permissions, cloud
secrets provisioning, an internal ALB, or private operator access. No AWS
resources, images in remote registries, or real credentials were created.

The original Go application, local Kubernetes manifests and existing database
were not changed. This is one deployment-profile milestone, not a cloud launch.

Follow-up: [Phase 11C](13-ebs-pod-identity.md) now prepares EBS CSI/Pod Identity
roles, add-ons and private credentials networking. It remains offline-tested
configuration, not a running driver or a verified cloud storage path.

## The three stages

1. **Infra:** namespace, non-privileged service account, configuration, private
   database Service, PostgreSQL StatefulSet and encrypted gp3 StorageClass.
2. **Migrate:** a bounded `/migrate` Job using the same application image as the
   API/worker. Give it time to succeed before proceeding; do not restart failures
   indefinitely or mix it into application startup.
3. **App:** two API replicas and one built-in worker. The API Service remains
   ClusterIP-only. No public endpoint, NodePort, ingress or load balancer exists.

Separate files make this order visible. **There is no deployment automation
yet**, so rendering does not enforce a live migration/rollout gate. A future
approved deployment workflow must wait for database readiness and migration
success before applying the app, then verify an actual fresh job.

## Important decisions, with code

### Keep secrets out of rendered files

Clients receive a public connection configuration and one Secret key:

```yaml
env:
  - name: PGPASSWORD
    valueFrom:
      secretKeyRef:
        name: database-credentials
        key: password
```

The Go pgx driver reads `PGPASSWORD` separately. Its URL fixes the database host,
user, `sslmode=verify-full` and CA path. This avoids placing passwords in URLs or
needing to URL-escape punctuation. The local integration check actually uses a
synthetic password with punctuation and successfully migrates/processes work.

The PostgreSQL server requires encrypted, SCRAM-authenticated TCP connections.
Its certificate must include the SAN
`postgres.durable-jobs-aws.svc.cluster.local`; clients mount only its trusted CA,
not the server's private key. Missing required Secrets stop pods from starting.

These four Secrets must be provisioned **outside Git and Terraform state**, in
the AWS namespace, using an explicitly approved process:

| Secret | Required keys | Consumer |
| --- | --- | --- |
| `postgres-admin` | `password` | PostgreSQL bootstrap only |
| `database-credentials` | `password` | DB creates `jobs_owner`; API, worker and migration authenticate as it |
| `postgres-tls` | `tls.crt`, `tls.key` | PostgreSQL server only |
| `database-ca` | `ca.crt` | API, worker and migration |

No real passwords/certificates or secret-generation deployment command are
included. Kubernetes Secret references alone do not implement a secret manager,
rotation, audit policy or least-privilege RBAC. Those remain deployment prerequisites.

`jobs_owner` cannot create roles/databases and is not a PostgreSQL superuser.
It is the lab's schema owner, shared by migration and runtime clients. This is
deliberately **not production-level role separation**: a follow-up should give
runtime clients DML-only access and keep DDL permissions with migrations.

The initialization SQL runs only on an empty database directory. Changing a
Secret does not update an existing PostgreSQL role's password. Rotation requires
an approved SQL/password change and controlled client restart. Existing data
must never be deleted just to rerun initialization.

### Treat storage as data, not a disposable container

```yaml
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
  encrypted: 'true'
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
```

The 10 GiB claim requests encrypted EBS. Waiting for the first consumer lets
provisioning follow the pod's scheduling topology. Both the StatefulSet's PVC
policy and StorageClass retain data instead of automatically deleting it.

This **requires an EBS CSI driver with its own IAM permissions**, which Phase
11C now configures but has not deployed. Without a working driver and permissions,
a real PVC will remain pending.
Do not bypass it with the kind StorageClass or a node's local directory.

One database and one zonal EBS volume are not multi-AZ availability, a backup or
a restore procedure. A pod can return on a compatible node in the volume's AZ;
loss of that AZ is not solved. Retained storage can keep incurring charges after
workloads are removed, so teardown must explicitly account for it. Never use
an indiscriminate namespace/volume deletion as a cleanup shortcut.

### Use one immutable application release

The renderer requires an ECR reference shaped like:

```text
<approved-account>.dkr.ecr.<approved-region>.amazonaws.com/durable-jobs@sha256:<digest>
```

It rejects tags such as `latest`, mismatched account/region and the checked-in
`example.invalid` placeholder. All three application processes share the same
digest, and pods target Linux AMD64 to match the current Terraform node group.
This verifies reference format/consistency—not that an image exists, is trusted,
or contains AMD64 binaries. Those must be checked before deployment.

ECR matches the node group's existing registry-read policy. Registry creation,
push permissions and publication are not implemented or authorized here. The
older Phase 9 GHCR publication workflow remains separate and unchanged.

The migration Job name includes a hash of the rendered release. An image change
gets a new Job identity. Application configuration changes also change pod
annotations so a future apply can trigger rollout. An application-image-only
change **does not restart PostgreSQL**. Secret value changes are not hashed;
they require a deliberately coordinated restart/rotation.

## Commands that do not touch AWS

From the project directory:

```sh
make aws-config-check
make aws-local-check
```

The first installs pinned PyYAML into `.local/aws-tools`, renders with local
`kubectl kustomize`, and runs policy/regression tests. No kubeconfig, cluster
discovery or AWS command is used. Package installation may access the internet.
`make platform-check` includes these configuration tests.

The second builds local images and creates an isolated Docker network and a
**temporary, memory-backed database**, with no host ports or existing volumes.
It derives the DB URL, PostgreSQL arguments and bootstrap/auth files from the
AWS profile. Fresh one-day certificates and synthetic passwords are generated
solely for this test. They are not AWS credentials or reusable deployment secrets.
Clients get CA certificate files; only the DB receives the server-key fixture.

It tests:

- Initialization and migrations through a hostname/CA-verified TLS connection.
- Go race/vet and real-database integration tests in the disposable database.
- Successful processing, correct output, keyed replay, conflicting-input
  rejection and attempt history through the real API and worker.
- Actual TLS use and a non-superuser application identity.
- Rejection of plaintext, wrong hostname, wrong CA, wrong password and role creation.

Temporary containers/network and their in-memory synthetic database are removed
afterward, including on ordinary test failure. Reports/images remain. Failed log
collection cannot skip cleanup; failed cleanup makes the run fail. Host reports
are private and ignored by Git. CI retains result/log files, **not private-key
fixtures**. Neither test creates or resets the user's existing database.

To see the prerequisites without changing anything:

```sh
.local/aws-tools/bin/python platform_tools/aws_config.py
```

For a future approved image, `render` requires explicit `--stage infra|migrate|app`,
`--account-id`, `--region` and `--image`. It prints a Kubernetes JSON List for
that one stage, not an apply command. There is intentionally no `deploy` action.
Do not directly apply the source Kustomize folders: their image is an invalid
placeholder, and no account/secrets/migration gate has been satisfied.

## Evidence and limits

Verified locally September 28, 2026:

- All 14 AWS configuration/helper tests pass, including 19 security/storage
  mutation cases and cleanup-failure checks.
- Final disposable TLS run: `.local/phase11b/runtime.uysh6nh_/result.json`;
  the earlier run `.local/phase11b/runtime.81f0wrw1/result.json` also passed.
- Final synthetic completed job: `3ee9558b-351b-412b-b620-82ffb8750b1f`, one recorded
  successful attempt and all six API smoke checks passed.
- Host Go race/vet/lint and the existing 27 Python regression tests pass.
- All 12 Phase 11A Terraform tests still pass; CI syntax validation passes.

The local runtime uses native Linux ARM64 images on this laptop; the AWS profile
selects AMD64. It does not establish EKS scheduling, Kubernetes Secret file
ownership/admission behavior, EBS binding/recovery, cluster networking or AMD64
image compatibility. Kustomize/policy checks are not server-side admission tests.
Opt-in Docker script-sandbox integration is not rerun: this profile runs the
built-in worker, without a Docker socket or arbitrary script execution.

## Next boundary

Verify Phase 11C's EBS CSI and Phase 11E's CNI/Pod Identity configuration; finish the approved image/secret/access
workflow before a cloud run; then add internal ALB/private DNS and fresh-job
verification inside the VPC. Do not weaken the existing local-only CLI's guards
to make it target AWS. There is still no authentication on the application API,
no demonstrated pod-level network isolation, no backup/restore or live cloud
failure-recovery proof, and no authorization to provision paid resources.

For the learning walkthrough, explain these three questions first:

1. Why can the URL live in a ConfigMap but the password cannot?
2. What happens to the data when a database pod disappears versus an AZ failing?
3. Why must a failed migration stop an application rollout?

## Primary references

- [EBS CSI driver and required IAM permissions](https://docs.aws.amazon.com/eks/latest/userguide/ebs-csi.html)
- [StorageClasses: binding and reclaim policies](https://kubernetes.io/docs/concepts/storage/storage-classes/)
- [StatefulSet storage retention](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)
- [PostgreSQL TLS configuration and server-key permissions](https://www.postgresql.org/docs/17/ssl-tcp.html)
