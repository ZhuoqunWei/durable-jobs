# Phase 11F: deployment preflight without deploying

Date: September 28, 2026

Implemented: a standard-library Python checker and offline tests. **No AWS
application deployment has been performed.** On September 28, authorized read-only CLI and
Chrome inspection confirmed account access, regional version/add-on availability
and initial network prerequisites. This was a preliminary inspection, not a
successful run of every preflight check. The checked-in template remains
unconfigured. Missing inputs are not evidence that resources do not exist.

## Current deployment handoff

The owner selected US East (N. Virginia), a six-hour first lab, a budget below
$50 in **new project spending**, an alert recipient, and deletion of the new
cloud synthetic database at cleanup while preserving local data and reports.
The existing monthly bill is separate. The owner also separately authorized
retiring the old delivery application's paid services; that shutdown is recorded
in the private `.local/aws-shutdown/SHUTDOWN-REPORT.md`. It does not authorize
changing other unrelated resources.

The [live guardrails milestone](17-cloud-guardrails.md) now records the spending
alerts, retained state bucket, assumed operator role and synthetic state-lock
test. No EKS cluster or application database has been created. The six-hour
cluster lab clock has not started; a cleanup canary is not full teardown.

Private account details, the alert recipient, preliminary compatible add-on
candidates and outstanding prerequisites are recorded in the ignored local file
`.local/aws-session-2026-09-28.json`. Do not commit that file. The new
`durable-jobs-lab` profile assumes the dedicated operator role. The preflight's
assumed-role guard remains unchanged. The bootstrap identity is not the daily
operator role. Private operator network access, image, TLS/secrets, provisioner
permissions and complete cluster cleanup still need setup.

## Start here

```sh
make aws-preflight
```

This reads the checked-in non-secret template and prints a JSON checklist.
It makes no AWS calls, reads no credentials and changes no files or resources.
Every uninspected resource is `unknown`, never implicitly green.

Account/region, additional budget and the short lab duration have been selected.
Do not send access keys
or passwords in chat. Cost and teardown
approval are separate from permission to inspect metadata. Read-only API calls
can still appear in AWS audit logs and may have request charges.

## Optional read-only inspection, after selecting the target

Create a local copy of `infra/aws/preflight.example.json` under ignored `.local/`.
Use an existing trusted AWS CLI v2 assumed-role profile; the tool neither
configures an account nor logs in. Fill account, region and profile first.
Keep unprepared resources and unselected versions `null`. Unknown keys,
duplicate keys, mismatched account/region references and mutable image tags
are rejected before inspection. Only the lab's simple hyphenated bucket names,
Kubernetes 1.35 and same-account/same-region resources are supported.

Once inspection is authorized, the explicit command is:

```sh
python3 platform_tools/aws_preflight.py inspect --config .local/aws-preflight.json
```

There is deliberately no `apply`, `deploy`, `login` or `publish` command.
Default `plan` and every automated test remain offline. AWS metadata inspection
is not added to CI. The existing CI Python suite runs the new fake-client tests.

## Eleven checks, with clear limits

| Check | What can pass | What it does not establish |
| --- | --- | --- |
| Identity | STS reports selected account and an assumed-role session | Deployment permission or appropriate IAM scope |
| EKS version | Selected minor is in standard support; default platform identified | Working cluster or node bootstrap |
| Five add-ons | Exact build supports minor/platform/AMD64/EC2; no required custom config; schema retrieved; CNI/EBS advertise required Pod Identity service accounts | Healthy pods, reviewed defaults or working IAM |
| State bucket | Expected owner, selected region, versioning, default encryption and all four bucket public-access blocks | Active Terraform backend, lock access, TLS-only bucket policy, existing-object encryption or recovery |
| Image | Exact digest exists in selected account's `durable-jobs` ECR repository | AMD64 contents, provenance, executable code or successful smoke test |
| Certificate | Selected certificate is issued, has the exact private-hostname SAN, valid now and has more than 48 hours remaining | Client CA trust, correct renewal or successful ALB TLS |
| ELB role | Expected existing service-linked role ARN | Controller IAM correctness or successful ALB creation |

If an add-on pin is null, the report lists compatible candidates without
choosing one or changing Terraform inputs. Candidate ordering is lexical, not
a recommendation or newest-version ranking. Selected builds produce a schema
fingerprint; schemas/defaults still require review using the AWS configuration
API. Test fixtures use fake `v0.0.0-eksbuild.N` builds, never recommendations.

The certificate check intentionally requires an exact SAN, not a wildcard.
The 48-hour margin is a lab guardrail, not a suitable production renewal policy.
The bucket check is conservative: it requires bucket-level public-access blocks
even if an account/organization policy already supplies stricter protection.

## Why the safety checks matter

In `platform_tools/aws_preflight.py`, the first check is the account identity:

```python
if run("identity", identity) is None:
    report["status"] = "blocked"
    return report
```

That means a mistaken profile stops the inspection before it reads other
account resources. The subprocess adapter permits only eleven specific
read-only AWS operations and their known parameter names. There is no shell
interpolation or generic command executor. Each call has a 30-second ceiling,
with a 180-second overall budget and no CLI retries. CLI auto-pagination stays
enabled; leftover continuation tokens are rejected instead of treating a
partial response as complete.

The CLI receives explicit profile/region. Ambient `AWS_*` variables are removed,
configured endpoint overrides are disabled, and EC2 metadata credential lookup
is disabled. The selected normal local profile may use its trusted SSO cache
or credential process, so inspect only with a profile you trust. Custom
credential-file environment overrides and unusual credential sources are
outside this small tool's supported setup.

Errors, permission denials, missing metadata and timeouts cannot become a pass.
Raw CLI output, IAM policies, schema contents and credentials are not printed.
Reports contain sanitized results, a UTC timestamp and a config fingerprint;
rerun after changes rather than reusing stale reports.

Even a complete success reports `metadata_checks_passed` with
`deployment_ready: false`. The remaining manual checks cannot be ticked off
using a boolean in this file. Runtime evidence and spending approval are
different from static/metadata checks.

Exit codes: offline `plan` returns 0 when it successfully prints the checklist;
`inspect` returns 0 only when all metadata checks pass, 1 for incomplete/failed
checks, and 2 for rejected inputs. Exit 0 is never deployment authorization.

## Validation performed

- **16 focused fake-client/subprocess tests**: no default AWS access, wrong
  account early stop, input validation, version/platform/architecture/identity
  incompatibility, missing resources, storage/TLS regressions, secret-safe
  failures, pagination handling, operation allowlisting and time budgets.
- `make platform-check`: **49 local/tooling + 24 AWS-profile tests** passed.
- `make check`: Go race tests and vet passed.
- `make aws-preflight`: printed the real offline missing-input report.
- Offline profile evidence: `.local/phase11b/config.orWETx6j/preflight-plan.json`.

No Go/SQL/Terraform infrastructure changes in this milestone. Live DB recovery,
AWS APIs and cloud bootstrap were not exercised. Existing local data was untouched.

## Next actual deployment steps

1. Resolve the account/profile, region, budget, lab duration and retained-data
   teardown choices with the owner; estimate charges before provisioning.
2. Run the authorized metadata inspection and review exact compatible builds.
3. Prepare the actual state backend/locking, private access path, tested image,
   certificates and out-of-band Secrets. This tool does not create them.
4. Review the real plan, authorize the short-lived lab, then follow Phase 11E's
   bootstrap and DB → migration → app → private ingress → completed-job checks.

No dashboard or additional framework is a prerequisite for this first demo.
For a short learning exercise, change the fake STS account in the test and trace
why only one AWS read is recorded. Then explain why image existence is not a
successful deployment.

## Primary references

- [EKS add-on versions and compatibility](https://docs.aws.amazon.com/cli/latest/reference/eks/describe-addon-versions.html)
- [EKS configuration schemas and Pod Identity metadata](https://docs.aws.amazon.com/cli/latest/reference/eks/describe-addon-configuration.html)
- [EKS version support metadata](https://docs.aws.amazon.com/cli/latest/reference/eks/describe-cluster-versions.html)
- [AWS identity inspection](https://docs.aws.amazon.com/cli/latest/reference/sts/get-caller-identity.html)
- [S3 bucket public-access settings and their limits](https://docs.aws.amazon.com/cli/latest/reference/s3api/get-public-access-block.html)
- [Terraform S3 backend encryption and locking configuration](https://developer.hashicorp.com/terraform/language/backend/s3)
- [ECR image metadata](https://docs.aws.amazon.com/cli/latest/reference/ecr/describe-images.html)
- [ACM certificate metadata](https://docs.aws.amazon.com/cli/latest/reference/acm/describe-certificate.html)
- [AWS CLI endpoint override controls](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-endpoints.html)
