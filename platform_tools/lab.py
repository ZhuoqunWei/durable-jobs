"""Local-only platform CLI. Default command prints a plan; it changes nothing."""

import argparse
import json
import pathlib
import subprocess
import time
import urllib.parse
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]
CONTEXT = "kind-durable-jobs-clear"
NAMESPACE = "durable-jobs-lab"
KUBECONFIG = ROOT / ".local/phase10/kubeconfig"
APP_IMAGE = "durable-jobs:phase10"
PROBE_IMAGE = "durable-jobs-probe:phase10"


class LabError(Exception):
    pass


def execute(command, *, stdin=None, timeout=30):
    try:
        result = subprocess.run(command, input=stdin, capture_output=True,
                                text=True, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise LabError(f"Command unavailable or timed out: {command[0]}") from exc
    if result.returncode:
        # No raw Kubernetes output here: an apply error can echo Secret data.
        raise LabError(f"Command failed (exit {result.returncode}): {' '.join(command[:1])}; inspect the lab with status")
    return result.stdout


class Lab:
    def __init__(self, context, namespace, kubeconfig, runner=execute):
        if (context != CONTEXT or namespace != NAMESPACE
                or pathlib.Path(kubeconfig).resolve() != KUBECONFIG.resolve()):
            raise LabError("Only the dedicated local kind context, namespace and kubeconfig are allowed")
        self.runner = runner
        self.prefix = ["kubectl", "--kubeconfig", str(KUBECONFIG), "--context", context,
                       "--namespace", namespace, "--request-timeout=10s"]

    def kubectl(self, *args, stdin=None, timeout=30):
        return self.runner(self.prefix + list(args), stdin=stdin, timeout=timeout)

    def guard(self, allow_missing_namespace=False):
        config = json.loads(self.kubectl("config", "view", "--minify", "-o", "json"))
        clusters = config.get("clusters", [])
        if len(clusters) != 1:
            raise LabError("Expected exactly one local cluster")
        server = urllib.parse.urlsplit(clusters[0]["cluster"]["server"])
        if server.scheme != "https" or server.hostname != "127.0.0.1":
            raise LabError("Refusing a non-loopback Kubernetes API")
        raw = self.kubectl("get", "namespace", NAMESPACE, "--ignore-not-found", "-o", "json")
        if not raw.strip():
            if allow_missing_namespace:
                return
            raise LabError("Lab namespace missing; run make lab-up")
        labels = json.loads(raw).get("metadata", {}).get("labels", {})
        if labels.get("durable-jobs.dev/lab") != "phase10":
            raise LabError("Refusing an unmarked namespace")

    def status(self):
        data = json.loads(self.kubectl("get", "deployments,statefulsets,pods,services,pvc",
                                       "-o", "json"))
        # Summaries exclude environment values and Secret objects.
        return {"status": "inspected", "resources": [
            {"kind": item["kind"], "name": item["metadata"]["name"],
             "desired_replicas": item.get("spec", {}).get("replicas"),
             "ready_replicas": item.get("status", {}).get("readyReplicas"),
             "pod_ready": next((c.get("status") == "True" for c in
                                item.get("status", {}).get("conditions", [])
                                if c.get("type") == "Ready"), None),
             "phase": item.get("status", {}).get("phase")}
            for item in data.get("items", [])]}

    def job(self, container, label, deadline=100):
        name = f"{label}-{uuid.uuid4().hex[:12]}"
        container = dict(container)
        container.update({"name": "task", "imagePullPolicy": "Never",
                          "resources": {"requests": {"cpu": "25m", "memory": "32Mi"},
                                        "limits": {"cpu": "500m", "memory": "128Mi"}},
                          "securityContext": {"allowPrivilegeEscalation": False,
                                              "readOnlyRootFilesystem": True,
                                              "capabilities": {"drop": ["ALL"]}}})
        manifest = {"apiVersion": "batch/v1", "kind": "Job",
                    "metadata": {"name": name, "namespace": NAMESPACE},
                    "spec": {"backoffLimit": 0, "activeDeadlineSeconds": deadline,
                             "ttlSecondsAfterFinished": 600,
                             "template": {"spec": {"restartPolicy": "Never",
                                 "automountServiceAccountToken": False,
                                 "securityContext": {"runAsNonRoot": True, "runAsUser": 65532,
                                     "runAsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
                                 "containers": [container]}}}}
        self.kubectl("create", "-f", "-", stdin=json.dumps(manifest))
        end = time.monotonic() + deadline + 30
        while time.monotonic() < end:
            job = json.loads(self.kubectl("get", "job", name, "-o", "json"))
            status = job.get("status", {})
            failed = status.get("failed", 0) > 0 or any(
                c.get("type") == "Failed" and c.get("status") == "True"
                for c in status.get("conditions", []))
            if status.get("succeeded", 0) or failed:
                logs = self.kubectl("logs", "job/" + name, "--tail=20")
                return not failed, logs, name
            time.sleep(1)
        raise LabError(f"Timed out waiting for job {name}; inspect pods and image availability")

    def deploy(self):
        self.guard(allow_missing_namespace=True)
        self.kubectl("apply", "-k", str(ROOT / "deploy/kubernetes/local/infra"))
        self.kubectl("rollout", "status", "statefulset/postgres", "--timeout=150s", timeout=165)
        passed, _, name = self.job({"image": APP_IMAGE, "command": ["/migrate"],
            "envFrom": [{"secretRef": {"name": "database-secret"}}]}, "migrate")
        if not passed:
            raise LabError(f"Migration {name} failed; application rollout was not started")
        self.kubectl("apply", "-k", str(ROOT / "deploy/kubernetes/local/app"))
        # Local tags are explicitly loaded by kind. Restart ensures a newly
        # loaded image is used even when the checked-in lab tag stays constant.
        self.kubectl("rollout", "restart", "deployment/api", "deployment/worker")
        for role in ("api", "worker"):
            self.kubectl("rollout", "status", "deployment/" + role, "--timeout=150s", timeout=165)
        return {"status": "deployed", "note": "Run verify; rollout alone does not prove job completion"}

    def probe(self, mode, key=None, target="api", timeout=30):
        self.guard()
        # Fault exercises cannot be repurposed to probe arbitrary services.
        targets = {"api": "http://api:8085", "missing-dns": "http://missing.invalid:8085",
                   "closed-port": "http://api:1"}
        if target not in targets or (mode == "verify" and target != "api"):
            raise LabError("Unsupported diagnostic target")
        args = ["--mode", mode, "--url", targets[target], "--timeout", str(timeout)]
        if key:
            args += ["--key", key]
        passed, logs, name = self.job({"image": PROBE_IMAGE, "args": args}, mode)
        try:
            report = json.loads(logs)
        except ValueError as exc:
            raise LabError(f"Diagnostic job {name} returned no valid report") from exc
        if not isinstance(report, dict) or report.get("status") not in ("passed", "failed"):
            raise LabError(f"Diagnostic job {name} returned an invalid report")
        if passed != (report["status"] == "passed"):
            raise LabError(f"Diagnostic job {name} exit status disagreed with its report")
        return {**report, "kubernetes_job": name}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "deploy", "status", "doctor", "verify"), nargs="?", default="plan")
    parser.add_argument("--context")
    parser.add_argument("--namespace")
    parser.add_argument("--kubeconfig")
    parser.add_argument("--key", help="Explicit key for replay checks; normally a fresh key is generated")
    parser.add_argument("--target", choices=("api", "missing-dns", "closed-port"), default="api")
    parser.add_argument("--timeout", type=int, default=30)
    args = parser.parse_args(argv)
    if args.command == "plan":
        print(json.dumps({"status": "plan_only", "changes": False,
            "steps": ["make lab-tools", "make lab-up", "make lab-status", "make lab-doctor", "make lab-verify"],
            "scope": "Dedicated local kind cluster only; no AWS or existing database access"}))
        return 0
    if not args.context or not args.namespace or not args.kubeconfig or not 1 <= args.timeout <= 60:
        parser.error("Explicit context, namespace, kubeconfig and a timeout of 1-60 seconds are required")
    try:
        lab = Lab(args.context, args.namespace, args.kubeconfig)
        if args.command == "deploy":
            report = lab.deploy()
        elif args.command == "status":
            lab.guard()
            report = lab.status()
        else:
            report = lab.probe(args.command, args.key, args.target, args.timeout)
    except (LabError, ValueError, KeyError) as exc:
        report = {"status": "failed", "error": str(exc)}
    print(json.dumps(report, sort_keys=True))
    return 1 if report["status"] == "failed" else 0


if __name__ == "__main__":
    raise SystemExit(main())
