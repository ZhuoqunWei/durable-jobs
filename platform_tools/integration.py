"""Opt-in failure exercises for ONLY the dedicated synthetic local cluster."""

import json
import time
import uuid

from lab import CONTEXT, KUBECONFIG, NAMESPACE, ROOT, Lab, LabError


def expect(report, status, stage=None):
    if report.get("status") != status:
        raise LabError(f"Expected {status}, observed {report}")
    if stage and report.get("checks", [{}])[-1].get("stage") != stage:
        raise LabError(f"Expected failure at {stage}, observed {report}")


def run(target, results):
    target.guard()
    original = json.loads(target.kubectl("get", "deployment", "worker", "-o", "json"))["spec"]["replicas"]
    if original != 1:
        raise LabError("Failure exercise requires the default one-worker lab; refusing to change a customized replica count")

    baseline = target.probe("verify")
    results.append({"scenario": "baseline", "report": baseline})
    expect(baseline, "passed")
    for name, stage in (("missing-dns", "dns"), ("closed-port", "tcp")):
        report = target.probe("doctor", target=name)
        results.append({"scenario": name, "report": report})
        expect(report, "failed", stage)

    waiting_key = "phase10-paused-" + uuid.uuid4().hex
    try:
        target.kubectl("scale", "deployment/worker", "--replicas=0")
        target.kubectl("wait", "--for=delete", "pod", "-l", "app=worker", "--timeout=60s", timeout=75)
        ready = target.probe("doctor")
        results.append({"scenario": "api-ready-without-worker", "report": ready})
        expect(ready, "passed")
        stalled = target.probe("verify", key=waiting_key, timeout=2)
        results.append({"scenario": "fresh-work-without-worker", "report": stalled})
        expect(stalled, "failed", "job_progress")
    finally:
        target.kubectl("scale", "deployment/worker", "--replicas=1")
        target.kubectl("rollout", "status", "deployment/worker", "--timeout=120s", timeout=135)

    resumed = target.probe("verify", key=waiting_key)
    results.append({"scenario": "worker-restored", "report": resumed})
    expect(resumed, "passed")
    if not stalled.get("job_id") or resumed.get("job_id") != stalled["job_id"]:
        raise LabError("Worker restoration did not complete the previously accepted job")
    # Do not alter storage or delete any volume. Only restart lab processes.
    for resource in ("deployment/api", "deployment/worker", "statefulset/postgres"):
        start = time.monotonic()
        target.kubectl("rollout", "restart", resource)
        target.kubectl("rollout", "status", resource, "--timeout=120s", timeout=135)
        target.kubectl("rollout", "status", "deployment/api", "--timeout=120s", timeout=135)
        replay = target.probe("verify", key=baseline["key"])
        results.append({"scenario": resource + "-restart", "report": replay,
                        "elapsed_seconds": round(time.monotonic() - start, 3)})
        expect(replay, "passed")
        if replay["job_id"] != baseline["job_id"] or replay["attempts"] != baseline["attempts"]:
            raise LabError("Restart did not preserve the baseline job and attempt count")


def main():
    report = {"status": "running", "context": CONTEXT, "namespace": NAMESPACE, "scenarios": []}
    destination = ROOT / ".local/phase10" / ("evidence-" + uuid.uuid4().hex + ".json")
    destination.parent.mkdir(parents=True, exist_ok=True)
    try:
        run(Lab(CONTEXT, NAMESPACE, KUBECONFIG), report["scenarios"])
        report["status"] = "passed"
    except (LabError, ValueError, KeyError) as exc:
        report.update(status="failed", error=str(exc))
    finally:
        destination.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"status": report["status"], "report": str(destination)}))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
