"""Reproducible local release demo. Default: plan; --run creates disposable kind."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import time
import uuid

from lab import APP_IMAGE, PROBE_IMAGE, NAMESPACE, ROOT, Lab, LabError
from probe import Client, ProbeError

KIND_IMAGE = "kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"
ARTIFACTS = ROOT / ".local/phase13"
KIND = ROOT / "bin/kind"


def require(condition, message):
    if not condition:
        raise LabError(message)


def local_environment():
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("AWS_", "PG")) and k not in ("DATABASE_URL", "KUBECONFIG")}
    env["KIND_EXPERIMENTAL_PROVIDER"] = "docker"
    return env


def require_local_docker(runner):
    # DOCKER_CONTEXT takes precedence over DOCKER_HOST in the Docker CLI.
    host = None if os.environ.get("DOCKER_CONTEXT") else os.environ.get("DOCKER_HOST")
    if not host:
        context = json.loads(runner(["docker", "context", "inspect"]))
        host = context[0]["Endpoints"]["docker"]["Host"]
    require(host.startswith("unix://"), "Only a local Unix-socket Docker engine is allowed")


def recovery_evidence(before, after, history, replay):
    """Fail closed: a new job, missing evidence or extra execution cannot pass."""
    expected = {"campaign_id": "phase13_crash", "category_id": "animals",
                "created_date": "2026-08-31", "schema_version": 1}
    require(before.get("status") == "running" and before.get("attempts") == 1,
            "No committed first claim was observed before interruption")
    require(after.get("id") == before.get("id") and bool(after.get("id"))
            and after.get("status") == "succeeded" and after.get("attempts") == 2
            and after.get("result") == expected and bool(after.get("completed_at")),
            "The original interrupted job did not succeed on attempt two")
    attempts = history.get("attempts", [])
    require(history.get("job_id") == after["id"] and isinstance(attempts, list) and len(attempts) == 2
            and all(isinstance(a, dict) for a in attempts)
            and [a.get("number") for a in attempts] == [1, 2]
            and [a.get("status") for a in attempts] == ["lease_expired", "succeeded"]
            and all(a.get("finished_at") for a in attempts), "Unexpected attempt history")
    require(all(after.get(k) == replay.get(k) for k in
                ("id", "status", "attempts", "result", "started_at", "completed_at")),
            "Keyed replay changed the recovered result")


class Commands:
    def __init__(self, directory):
        self.directory = directory
        self.number = 0

    def __call__(self, command, *, stdin=None, timeout=30):
        self.number += 1
        # No AWS or application database credentials enter local demo commands.
        env = local_environment()
        command = [str(arg) for arg in command]
        try:
            result = subprocess.run(command, input=stdin, text=True, capture_output=True,
                                    env=env, timeout=timeout, check=False)
        except (OSError, subprocess.TimeoutExpired) as error:
            raise LabError(f"Command unavailable or timed out: {Path(command[0]).name}") from error
        # Synthetic local logs only. Never export kubeconfig, .env or AWS files.
        log = self.directory / f"command-{self.number:03d}.log"
        log.write_text(result.stdout + result.stderr)
        if result.returncode:
            raise LabError(f"{Path(command[0]).name} failed; inspect {log.name}")
        return result.stdout


class DisposableCluster:
    def __init__(self, directory, runner):
        require(directory.parent.resolve() == ARTIFACTS.resolve()
                and re.fullmatch(r"run-[a-f0-9]{12}", directory.name), "Invalid demo directory")
        self.directory, self.run = directory, runner
        self.name = "durable-jobs-demo-" + directory.name[4:]
        self.context = "kind-" + self.name
        self.kubeconfig = directory / "kubeconfig"
        self.node_ids = None

    def nodes(self):
        output = self.run(["docker", "ps", "-aq", "--no-trunc", "--filter",
                           "label=io.x-k8s.kind.cluster=" + self.name])
        return sorted(output.split())

    def create(self):
        require(not self.nodes() and not self.kubeconfig.exists(), "Refusing to adopt an existing cluster")
        require(KIND.is_file(), "Run make lab-tools first")
        require_local_docker(self.run)
        try:
            self.run([str(KIND), "create", "cluster", "--name", self.name,
                      "--kubeconfig", str(self.kubeconfig), "--config", str(ROOT / "deploy/kind.yaml"),
                      "--image", KIND_IMAGE, "--wait", "180s", "--retain"], timeout=360)
        finally:
            # A failed create may still leave a node. Record only this newly
            # reserved random name; never adopt a pre-existing learning cluster.
            self.node_ids = self.nodes()
            (self.directory / "ownership.json").write_text(json.dumps({
                "cluster": self.name, "node_ids": self.node_ids}, indent=2) + "\n")
        require(len(self.node_ids) == 1, "Expected one newly created kind node")

    def guard(self):
        require(self.node_ids is not None and len(self.node_ids) == 1
                and self.nodes() == self.node_ids, "Cluster ownership changed; refusing operation")

    def cleanup(self):
        if self.node_ids is None:
            return {"status": "not_created"}
        current = self.nodes()
        require(current == self.node_ids, "Cluster ownership changed; cleanup refused")
        if current:
            self.run([str(KIND), "delete", "cluster", "--name", self.name,
                      "--kubeconfig", str(self.kubeconfig)], timeout=150)
        require(not self.nodes(), "Demo node still exists after deletion")
        return {"status": "removed", "cluster": self.name, "fresh_absence_check": True}


class DemoLab(Lab):
    """Reuse probe/Job contracts without widening Phase 10's fixed target guard."""
    def __init__(self, cluster, image, probe_image):
        cluster.guard()
        self.cluster, self.runner = cluster, cluster.run
        self.image, self.probe_image = image, probe_image
        self.prefix = ["kubectl", "--kubeconfig", str(cluster.kubeconfig),
                       "--context", cluster.context, "--namespace", NAMESPACE,
                       "--request-timeout=10s"]

    def guard(self, allow_missing_namespace=False):
        self.cluster.guard()
        super().guard(allow_missing_namespace)

    def job(self, container, label, deadline=100):
        container = dict(container)
        container["image"] = {APP_IMAGE: self.image, PROBE_IMAGE: self.probe_image}.get(
            container["image"], container["image"])
        return super().job(container, label, deadline)

    def deploy(self):
        self.guard(allow_missing_namespace=True)
        self.kubectl("apply", "-k", str(ROOT / "deploy/kubernetes/local/infra"))
        self.kubectl("rollout", "status", "statefulset/postgres", "--timeout=180s", timeout=195)
        passed, _, _ = self.job({"image": self.image, "command": ["/migrate"],
                               "envFrom": [{"secretRef": {"name": "database-secret"}}]}, "migrate")
        require(passed, "Migration failed; no application deployment attempted")
        manifests = self.kubectl("kustomize", str(ROOT / "deploy/kubernetes/local/app"))
        require(manifests.count("image: " + APP_IMAGE) == 2, "Unexpected application image references")
        self.kubectl("apply", "-f", "-", stdin=manifests.replace("image: " + APP_IMAGE, "image: " + self.image))
        for name in ("api", "worker"):
            self.kubectl("rollout", "status", "deployment/" + name, "--timeout=180s", timeout=195)

    def sql(self, statement):
        return self.kubectl("exec", "-i", "postgres-0", "--", "psql", "-U", "lab", "-d", "lab",
                            "-At", "-v", "ON_ERROR_STOP=1", stdin=statement)

    def worker_crash(self, directory):
        self.guard()
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        log = (directory / "tunnel.log").open("w")
        tunnel = subprocess.Popen(self.prefix + ["port-forward", "svc/api", f"{port}:8085",
                                  "--address=127.0.0.1"], stdout=log, stderr=log, env=local_environment())
        fault_installed = False
        try:
            client = Client(f"http://127.0.0.1:{port}", time.monotonic() + 120)
            for _ in range(50):
                require(tunnel.poll() is None, "Owned API tunnel exited")
                try:
                    client.request("GET", "/readyz", 200)
                    break
                except ProbeError:
                    time.sleep(0.1)
            else:
                raise LabError("Local API tunnel did not become ready")
            # Delay only this synthetic job's first completion. The 8 s delay
            # exceeds the worker's 3 s storage deadline, preventing an unnoticed
            # successful commit while the harness observes and kills the Pod.
            self.sql("""BEGIN;
            CREATE FUNCTION phase13_delay() RETURNS trigger LANGUAGE plpgsql AS $$
            BEGIN
              IF OLD.status='running' AND NEW.status='succeeded' AND NEW.attempts=1
                AND NEW.payload->>'campaign_id'='phase13_crash' THEN
                PERFORM pg_sleep(8);
              END IF;
              RETURN NEW;
            END $$;
            CREATE TRIGGER phase13_delay BEFORE UPDATE OF status ON jobs
              FOR EACH ROW EXECUTE FUNCTION phase13_delay();
            COMMIT;""")
            fault_installed = True
            pods = json.loads(self.kubectl("get", "pods", "-l", "app=worker", "-o", "json"))["items"]
            require(len(pods) == 1, "Expected one worker before crash")
            old = pods[0]["metadata"]
            key = "phase13-" + uuid.uuid4().hex
            payload = {"type": "migrate_campaign", "payload": {"campaign_id": "phase13_crash",
                       "category": "Pets", "created_at": "08/31/2026"}}
            job = client.request("POST", "/jobs", 202, payload, key)
            path = "/jobs/" + str(uuid.UUID(job["id"]))
            until = time.monotonic() + 10
            while time.monotonic() < until:
                before = client.request("GET", path, 200)
                if before.get("status") == "running":
                    break
                time.sleep(0.1)
            else:
                raise LabError("No running job observed; crash scenario was not exercised")
            started = time.monotonic()
            self.kubectl("delete", "pod", old["name"], "--grace-period=0", "--force", "--wait=false")
            until = time.monotonic() + 75
            while time.monotonic() < until:
                after = client.request("GET", path, 200)
                if after.get("status") in ("succeeded", "failed"):
                    break
                time.sleep(0.25)
            else:
                raise LabError("Interrupted job did not recover within 75 seconds")
            elapsed = time.monotonic() - started
            history = client.request("GET", path + "/attempts", 200)
            replay = client.request("POST", "/jobs", 202, payload, key)
            recovery_evidence(before, after, history, replay)
            new = json.loads(self.kubectl("get", "pods", "-l", "app=worker", "-o", "json"))["items"]
            require(len(new) == 1 and new[0]["metadata"]["uid"] != old["uid"], "No replacement worker observed")
            return {"status": "passed", "before": before, "after": after, "history": history,
                    "replay": replay, "recovery_seconds": round(elapsed, 3),
                    "old_pod_uid": old["uid"], "new_pod_uid": new[0]["metadata"]["uid"],
                    "claim": "One persisted result after retry; not exactly-once external execution"}
        finally:
            try:
                if fault_installed:
                    self.sql("DROP TRIGGER phase13_delay ON jobs; DROP FUNCTION phase13_delay();")
            finally:
                tunnel.terminate()
                try:
                    tunnel.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    tunnel.kill()
                    tunnel.wait(timeout=5)
                log.close()


def run_demo(directory, image=None):
    commands = Commands(directory)
    cluster = DisposableCluster(directory, commands)
    report = {"status": "running", "started_at": datetime.now(timezone.utc).isoformat(),
              "cluster": cluster.name, "checks": [], "scope": "disposable local kind; no AWS"}
    try:
        require_local_docker(commands)
        report["docker"] = commands(["docker", "version", "--format", "{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}"] ).strip()
        report["kind"] = commands([str(KIND), "version"]).strip()
        supplied_image = image is not None
        image = image or "durable-jobs-demo:" + directory.name
        require(re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._:/@-]+", image), "Invalid local image reference")
        if not supplied_image:
            commands(["docker", "build", "--target", "runtime", "-t", image, str(ROOT)], timeout=900)
        report["runtime_image_id"] = commands(["docker", "image", "inspect", image, "--format", "{{.Id}}"]).strip()
        require(re.fullmatch(r"sha256:[a-f0-9]{64}", report["runtime_image_id"]), "Invalid runtime image ID")
        probe_image = "durable-jobs-demo-probe:" + directory.name
        commands(["docker", "build", "-t", probe_image, str(ROOT / "platform_tools")], timeout=300)
        report["probe_image_id"] = commands(["docker", "image", "inspect", probe_image, "--format", "{{.Id}}"]).strip()
        cluster.create()
        commands([str(KIND), "load", "docker-image", "--name", cluster.name, image, probe_image], timeout=180)
        target = DemoLab(cluster, image, probe_image)
        target.deploy()
        baseline = target.probe("verify")
        require(baseline.get("status") == "passed", "Fresh-job verification failed")
        report["checks"].append({"scenario": "fresh_submission_replay_conflict_history", **baseline})
        crash = target.worker_crash(directory)
        report["checks"].append({"scenario": "worker_crash_and_recovery", **crash})
        report["status"] = "passed"
    except (LabError, ProbeError, OSError, ValueError, KeyError, KeyboardInterrupt) as error:
        report.update(status="failed", error=str(error) or type(error).__name__)
    finally:
        try:
            report["cleanup"] = cluster.cleanup()
        except (LabError, OSError, ValueError) as error:
            report.update(status="failed", cleanup={"status": "failed", "error": str(error)})
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        output = directory / "report.json"
        output.write_text(json.dumps(report, indent=2) + "\n")
        (directory / "report.sha256").write_text(hashlib.sha256(output.read_bytes()).hexdigest() + "  report.json\n")
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="store_true", help="Create and remove a new synthetic local cluster")
    parser.add_argument("--image", help="Use an already-built LOCAL image; never pull an unverified tag")
    args = parser.parse_args(argv)
    if not args.run:
        print(json.dumps({"status": "plan_only", "changes": False, "command": "make demo-check",
                          "steps": ["build", "fresh local kind", "migrate", "verify", "kill worker", "verify retry", "remove owned cluster"],
                          "preserves": ["original PostgreSQL volume", "Phase 10 cluster", "all AWS resources", "local reports"]}))
        return 0
    def stop(_signal, _frame):
        raise KeyboardInterrupt("Interrupted; attempting owned-cluster cleanup")
    previous = signal.signal(signal.SIGTERM, stop)
    try:
        directory = ARTIFACTS / ("run-" + uuid.uuid4().hex[:12])
        directory.mkdir(parents=True, mode=0o700, exist_ok=False)
        report = run_demo(directory, args.image)
        print(json.dumps({"status": report["status"], "report": str(directory / "report.json"),
                          "cleanup": report["cleanup"]}))
        return 0 if report["status"] == "passed" else 1
    finally:
        signal.signal(signal.SIGTERM, previous)


if __name__ == "__main__":
    raise SystemExit(main())
