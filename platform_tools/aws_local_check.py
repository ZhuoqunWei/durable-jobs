"""Exercise the AWS profile's DB/TLS contract in a disposable local Docker stack.

This is not Kubernetes/EBS/AWS validation. All passwords and certificates are
fresh synthetic test fixtures. Existing databases, volumes and clusters are unused.
"""

import copy
import json
import os
import pathlib
import secrets
import subprocess
import sys
import tempfile
import uuid

import aws_config as config

DB_HOST = "postgres.durable-jobs-aws.svc.cluster.local"
FIXTURE_IMAGE = "111122223333.dkr.ecr.us-west-2.amazonaws.com/durable-jobs@sha256:" + "a" * 64


class CheckFailed(Exception):
    pass


def run(command, *, log, env=None, timeout=180):
    result = subprocess.run(command, capture_output=True, text=True, timeout=timeout,
                            check=False, env=env)
    log.write_text(result.stdout + result.stderr)
    return result


def cleanup_stack(compose, report, environment):
    # Failed log collection must never prevent cleanup of this disposable stack.
    try:
        run(compose + ["logs", "--no-color"], log=report / "services.log", env=environment, timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        pass
    try:
        cleanup = run(compose + ["down", "--timeout", "10"], log=report / "cleanup.log", env=environment, timeout=60)
        return cleanup.returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


def main():
    root = config.ROOT
    reports = root / ".local/phase11b"
    reports.mkdir(parents=True, exist_ok=True)
    report = pathlib.Path(tempfile.mkdtemp(prefix="runtime.", dir=reports))
    project = "dj-aws-check-" + uuid.uuid4().hex[:12]
    runtime_image = "durable-jobs-aws-check:" + project
    checks_image = "durable-jobs-aws-checks:" + project
    print(f"Disposable local TLS/DB check. Evidence: {report}", flush=True)
    summary = {"scope": "local_containers_only", "aws_verified": False,
               "status": "failed", "checks": [], "compose_project": project}
    compose = ["docker", "compose", "--env-file", "/dev/null", "-p", project,
               "-f", str(report / "compose.json")]
    # Passwords remain outside the generated Compose file and process arguments.
    environment = dict(os.environ)
    environment["DJ_TEST_ADMIN_PASSWORD"] = secrets.token_urlsafe(24)
    environment["DJ_TEST_APP_PASSWORD"] = "test:'/@?# " + secrets.token_urlsafe(24)
    started = False

    def command(args, name, *, contains=None, fails=False, timeout=180):
        result = run(args, log=report / (name + ".log"), env=environment, timeout=timeout)
        output = result.stdout + result.stderr
        if (result.returncode == 0) == fails or (contains and contains not in output):
            raise CheckFailed(name)
        summary["checks"].append({"name": name, "status": "passed", "expected_failure": fails})
        print(f"PASS: {name}", flush=True)
        return result.stdout

    try:
        existing = command(["docker", "ps", "-aq", "--filter", f"label=com.docker.compose.project={project}"], "unique_project")
        if existing.strip():
            raise CheckFailed("Unexpected existing project; refusing reuse")
        bundle = config.build_bundle(FIXTURE_IMAGE, "111122223333", "us-west-2")
        database = config.pod(config.resource(bundle, "infra", "StatefulSet", "postgres"))["containers"][0]
        bootstrap = config.resource(bundle, "infra", "ConfigMap", "database-bootstrap")["data"]
        fixtures = report / "fixtures"
        fixtures.mkdir(mode=0o755)
        # Parent report remains 0700. Only the dedicated DB receives this mount.
        for name, text in bootstrap.items():
            (fixtures / name).write_text(text)
            (fixtures / name).chmod(0o644)
        for stem in ("tls", "wrong-ca"):
            command(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                     "-keyout", str(fixtures / f"{stem}.key"), "-out", str(fixtures / f"{stem}.crt"),
                     "-subj", "/CN=" + DB_HOST, "-addext", "subjectAltName=DNS:" + DB_HOST], "certificate_" + stem)
            # Test-only key is copied into a private container tmpfs with mode 0600.
            # This is not a production certificate-generation or provisioning tool.
            (fixtures / f"{stem}.key").chmod(0o644)
            (fixtures / f"{stem}.crt").chmod(0o644)

        for target, image in (("checks", checks_image), ("runtime", runtime_image)):
            command(["docker", "build", "--target", target, "-t", image, str(root)], "build_" + target, timeout=600)

        ca_mounts = [f"{fixtures / 'tls.crt'}:/etc/database-ca/ca.crt:ro",
                     f"{fixtures / 'wrong-ca.crt'}:/etc/database-ca/wrong.crt:ro"]
        client_env = {"DATABASE_URL": config.DATABASE_URL, "PGPASSWORD": "${DJ_TEST_APP_PASSWORD:?test fixture required}"}
        client = {"image": runtime_image, "read_only": True, "cap_drop": ["ALL"],
                  "security_opt": ["no-new-privileges:true"], "pids_limit": 64,
                  "mem_limit": "128m", "cpus": 1, "volumes": ca_mounts, "environment": client_env}
        postgres_env = config.resource(bundle, "infra", "ConfigMap", "database-config")["data"].copy()
        postgres_env.update(POSTGRES_PASSWORD="${DJ_TEST_ADMIN_PASSWORD:?test fixture required}",
                            APP_PASSWORD="${DJ_TEST_APP_PASSWORD:?test fixture required}",
                            PGPASSWORD="${DJ_TEST_APP_PASSWORD:?test fixture required}")
        services = {
            "postgres": {"image": database["image"], "user": "999:999", "read_only": True,
                "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"],
                "pids_limit": 128, "mem_limit": "512m", "cpus": 1,
                "environment": postgres_env,
                "networks": {"default": {"aliases": [DB_HOST, "wrong-db.internal"]}},
                "volumes": [f"{fixtures}:/fixtures:ro",
                    f"{fixtures / '10-role.sql'}:/docker-entrypoint-initdb.d/10-role.sql:ro",
                    f"{fixtures / 'pg_hba.conf'}:/etc/postgres-bootstrap/pg_hba.conf:ro"] + ca_mounts,
                "tmpfs": ["/var/lib/postgresql/data:uid=999,gid=999,mode=0700", "/var/run/postgresql:uid=999,gid=999",
                          "/tmp:uid=999,gid=999", "/etc/postgres-tls:uid=999,gid=999,mode=0700"],
                "entrypoint": ["/bin/sh", "-ec", "cp /fixtures/tls.crt /etc/postgres-tls/tls.crt; "
                    "cp /fixtures/tls.key /etc/postgres-tls/tls.key; chmod 600 /etc/postgres-tls/tls.key; "
                    'exec docker-entrypoint.sh "$@"', "test-bootstrap"],
                "command": database["args"],
                "healthcheck": {"test": ["CMD", "pg_isready", "-U", "postgres", "-d", "durable_jobs"],
                                "interval": "1s", "timeout": "3s", "retries": 40}},
            "migrate": {**copy.deepcopy(client), "entrypoint": ["/migrate"]},
            "api": {**copy.deepcopy(client), "healthcheck": {
                "test": ["CMD", "/smoke", "-url", "http://127.0.0.1:8085", "-ready-only", "-timeout", "3s"],
                "interval": "1s", "timeout": "4s", "retries": 30}},
            "worker": {**copy.deepcopy(client), "entrypoint": ["/worker"], "command": ["-processor", "builtin"]},
            "smoke": {**copy.deepcopy(client), "entrypoint": ["/smoke"],
                      "command": ["-url", "http://api:8085", "-timeout", "30s"]},
            "checks": {"image": checks_image, "environment": client_env, "volumes": ca_mounts,
                       "command": ["make", "check", "integration"]},
        }
        (report / "compose.json").write_text(json.dumps({"services": services, "networks": {"default": {"internal": True}}}, indent=2))
        started = True  # Cleanup owns only this freshly generated Compose project.
        command(compose + ["up", "-d", "--wait", "--wait-timeout", "60", "postgres"], "database_bootstrap")
        command(compose + ["run", "--rm", "migrate"], "verified_tls_migration")
        command(compose + ["run", "--rm", "checks"], "race_vet_database_integration", timeout=300)
        command(compose + ["up", "-d", "--wait", "--wait-timeout", "60", "api", "worker"], "application_start")
        smoke = command(compose + ["run", "--rm", "smoke"], "job_completion_replay")
        result = json.loads(smoke)
        if result.get("status") != "passed" or not result.get("job_id"):
            raise CheckFailed("Missing successful job result")
        summary["job_result"] = result
        command(compose + ["exec", "-T", "postgres", "psql", config.DATABASE_URL, "-Atc",
                "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()"], "connection_is_tls", contains="t\n")
        command(compose + ["exec", "-T", "postgres", "psql", config.DATABASE_URL, "-Atc",
                "SELECT NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole FROM pg_roles WHERE rolname=current_user"],
                "application_is_not_superuser", contains="t\n")
        for name, url, evidence in [
            ("reject_plaintext", config.DATABASE_URL.replace("verify-full", "disable"), "no encryption"),
            ("reject_wrong_hostname", config.DATABASE_URL.replace(DB_HOST, "wrong-db.internal"), "not wrong-db.internal"),
            ("reject_wrong_ca", config.DATABASE_URL.replace("/ca.crt", "/wrong.crt"), "unknown authority"),
        ]:
            command(compose + ["run", "--rm", "-e", "DATABASE_URL=" + url, "migrate"], name, fails=True, contains=evidence)
        environment["PGPASSWORD"] = "deliberately-wrong-test-password"
        command(compose + ["run", "--rm", "-e", "PGPASSWORD", "migrate"], "reject_wrong_password",
                fails=True, contains="password authentication failed")
        command(compose + ["exec", "-T", "postgres", "psql", config.DATABASE_URL, "-v", "ON_ERROR_STOP=1", "-c",
                "CREATE ROLE must_not_be_created"], "reject_role_creation", fails=True, contains="permission denied")
        summary["status"] = "passed"
    except (CheckFailed, config.ConfigError, OSError, ValueError, subprocess.TimeoutExpired) as exc:
        # No raw subprocess output or credential values in the summary.
        summary["error_type"] = type(exc).__name__
        print("Check failed; inspect the synthetic test logs in the evidence directory.", file=sys.stderr)
    finally:
        if started:
            if not cleanup_stack(compose, report, environment):
                summary["status"] = "failed"
                summary["cleanup_failed"] = True
        (report / "result.json").write_text(json.dumps(summary, indent=2))
    print(f"Local runtime check: {summary['status']}. AWS/EBS not tested. Evidence: {report}", flush=True)
    return 0 if summary["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
