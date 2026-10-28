"""In-cluster, standard-library diagnostics. No Kubernetes/AWS credentials needed."""

import argparse
import json
import re
import signal
import socket
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class ProbeError(Exception):
    def __init__(self, stage, detail, **evidence):
        super().__init__(detail)
        self.stage = stage
        self.evidence = evidence


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Client:
    def __init__(self, origin, deadline):
        parsed = urllib.parse.urlsplit(origin)
        if (parsed.scheme not in ("http", "https") or not parsed.hostname
                or parsed.username is not None or parsed.password is not None
                or parsed.path not in ("", "/") or parsed.query or parsed.fragment):
            raise ProbeError("configuration", "Expected an HTTP(S) origin without credentials or path")
        try:
            self.port = parsed.port or (443 if parsed.scheme == "https" else 80)
        except ValueError as exc:
            raise ProbeError("configuration", "Invalid port") from exc
        self.host = parsed.hostname
        self.origin = origin.rstrip("/")
        self.deadline = deadline
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def remaining(self):
        left = self.deadline - time.monotonic()
        if left <= 0:
            raise ProbeError("deadline", "Verification deadline exceeded")
        return min(3.0, left)

    def request(self, method, path, expected, body=None, key=None):
        headers = {"Content-Type": "application/json"}
        if key:
            headers["Idempotency-Key"] = key
        req = urllib.request.Request(self.origin + path, method=method, headers=headers,
                                     data=None if body is None else json.dumps(body).encode())
        try:
            response = self.opener.open(req, timeout=self.remaining())
        except urllib.error.HTTPError as exc:
            response = exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            cause = getattr(exc, "reason", exc)
            raise ProbeError("http", f"{method} {path}: {type(cause).__name__}; check endpoints, connection and TLS") from exc
        with response:
            if response.code != expected:
                raise ProbeError("http", f"{method} {path}: HTTP {response.code}, expected {expected}")
            if expected == 409:
                return {}
            data = response.read(131073)
            if len(data) > 131072:
                raise ProbeError("response", "Response exceeded 128 KiB")
            try:
                result = json.loads(data)
            except (ValueError, UnicodeError) as exc:
                raise ProbeError("response", "Response was not valid JSON") from exc
            if not isinstance(result, dict):
                raise ProbeError("response", "Expected a JSON object")
            return result


def require(condition, stage, message):
    if not condition:
        raise ProbeError(stage, message)


def verify_job(client, key, timeout):
    require(re.fullmatch(r"[A-Za-z0-9_.:-]{1,128}", key), "configuration", "Invalid idempotency key")
    payload = {"type": "migrate_campaign", "payload": {
        "campaign_id": "phase10_synthetic", "category": "Pets", "created_at": "08/31/2026"}}
    created = client.request("POST", "/jobs", 202, payload, key)
    job_id = created.get("id", "")
    try:
        require(str(uuid.UUID(job_id)) == job_id, "submission", "Invalid job ID")
    except (ValueError, TypeError, AttributeError) as exc:
        raise ProbeError("submission", "Invalid job ID") from exc
    deadline = time.monotonic() + timeout
    while True:
        # Fresh response every poll: missing fields cannot inherit earlier output.
        job = client.request("GET", f"/jobs/{job_id}", 200)
        require(job.get("id") == job_id, "result", "Retrieved another job")
        if job.get("status") == "succeeded":
            break
        require(job.get("status") in ("queued", "running", "retry_waiting"),
                "job_progress", "Job failed or returned an unknown state")
        if time.monotonic() >= deadline:
            raise ProbeError("job_progress", "API accepted work but it did not finish; inspect worker replicas, logs and database access",
                             job_id=job_id, key=key)
        time.sleep(min(0.2, max(0, deadline - time.monotonic())))
    expected = {"campaign_id": "phase10_synthetic", "category_id": "animals",
                "created_date": "2026-08-31", "schema_version": 1}
    require(job.get("result") == expected and job.get("attempts") == 1
            and bool(job.get("started_at")) and bool(job.get("completed_at"))
            and not job.get("failure_code"), "result", "Wrong output, timestamps or baseline attempt count")
    replay = client.request("POST", "/jobs", 202, payload, key)
    require(all(replay.get(field) == job.get(field) for field in
                ("id", "status", "result", "attempts", "started_at", "completed_at")),
            "idempotency", "Replay did not preserve the completed job")
    changed = {"type": payload["type"], "payload": dict(payload["payload"], category="Medical")}
    client.request("POST", "/jobs", 409, changed, key)
    history = client.request("GET", f"/jobs/{job_id}/attempts", 200)
    attempts = history.get("attempts")
    require(history.get("job_id") == job_id and isinstance(attempts, list)
            and len(attempts) == 1 and isinstance(attempts[0], dict)
            and attempts[0].get("number") == 1 and attempts[0].get("status") == "succeeded"
            and bool(attempts[0].get("finished_at")), "history", "Expected one finished successful attempt")
    return {"job_id": job_id, "key": key, "attempts": 1}


def diagnose(client, mode, key, timeout):
    checks = []
    stage = "dns"
    try:
        socket.getaddrinfo(client.host, client.port, type=socket.SOCK_STREAM)
        checks.append({"stage": "dns", "status": "passed"})
        stage = "tcp"
        with socket.create_connection((client.host, client.port), timeout=client.remaining()):
            pass
        checks.append({"stage": "tcp", "status": "passed"})
        stage = "readiness"
        ready = client.request("GET", "/readyz", 200)
        require(ready == {"status": "ready", "database": "ok"}, "readiness", "Readiness contract did not match")
        checks.append({"stage": "readiness", "status": "passed"})
        result = {}
        if mode == "verify":
            result = verify_job(client, key, timeout)
            checks.append({"stage": "job_completion_replay_conflict_history", "status": "passed"})
        return {"status": "passed", "scope": mode, "checks": checks, **result}
    except (ProbeError, OSError) as exc:
        stage = exc.stage if isinstance(exc, ProbeError) else stage
        hints = {"dns": "Check the Service name, namespace and CoreDNS; this does not prove an AWS DNS fault",
                 "tcp": "DNS resolved but TCP failed; inspect Service endpoints and connectivity"}
        checks.append({"stage": stage, "status": "failed", "detail": hints.get(stage, str(exc))})
        evidence = exc.evidence if isinstance(exc, ProbeError) else {}
        return {"status": "failed", "scope": mode, "checks": checks, **evidence}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("doctor", "verify"), default="doctor")
    parser.add_argument("--url", default="http://api:8085")
    parser.add_argument("--key", default=None)
    parser.add_argument("--timeout", type=int, default=30)
    args = parser.parse_args(argv)
    if not 1 <= args.timeout <= 60:
        parser.error("timeout must be between 1 and 60 seconds")

    def expired(_signum, _frame):
        raise ProbeError("deadline", "Hard process deadline exceeded (including DNS)")

    signal.signal(signal.SIGALRM, expired)
    signal.alarm(args.timeout + 10)
    try:
        client = Client(args.url, time.monotonic() + args.timeout + 8)
        report = diagnose(client, args.mode, args.key or "phase10-" + uuid.uuid4().hex, args.timeout)
    except ProbeError as exc:
        report = {"status": "failed", "checks": [{"stage": exc.stage, "detail": str(exc)}]}
    finally:
        signal.alarm(0)
    print(json.dumps(report, sort_keys=True))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
