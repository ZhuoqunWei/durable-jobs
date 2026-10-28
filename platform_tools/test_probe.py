import copy
import json
import socket
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import MagicMock, patch

import probe


JOB = {"id": "67d91e08-080c-44da-96af-477ba3799050", "status": "succeeded", "attempts": 1,
       "started_at": "2026-09-28T10:00:00Z", "completed_at": "2026-09-28T10:00:01Z",
       "result": {"campaign_id": "phase10_synthetic", "category_id": "animals",
                  "created_date": "2026-08-31", "schema_version": 1}}


class FakeClient:
    host, port = "api", 8085

    def __init__(self):
        self.job = copy.deepcopy(JOB)
        self.replay = copy.deepcopy(JOB)
        self.history = {"job_id": JOB["id"], "attempts": [
            {"number": 1, "status": "succeeded", "finished_at": JOB["completed_at"]}]}
        self.posts = 0

    def remaining(self):
        return 1

    def request(self, method, path, expected, body=None, key=None):
        if path == "/readyz":
            return {"status": "ready", "database": "ok"}
        if path.endswith("/attempts"):
            return self.history
        if method == "POST":
            self.posts += 1
            return {"id": JOB["id"]} if self.posts == 1 else self.replay
        return self.job


class VerificationTests(unittest.TestCase):
    def test_full_contract(self):
        result = probe.verify_job(FakeClient(), "test-key", 1)
        self.assertEqual(result["job_id"], JOB["id"])

    def test_bad_results_fail_closed(self):
        cases = {"wrong-result": {"result": {}}, "multiple-attempts": {"attempts": 2},
                 "missing-time": {"completed_at": None}, "wrong-job": {"id": "other"},
                 "failed-job": {"status": "failed"}, "unknown-state": {"status": "alien"},
                 "stalled-worker": {"status": "queued"}, "failure-code": {"failure_code": "oops"}}
        for name, change in cases.items():
            with self.subTest(name=name):
                client = FakeClient()
                client.job.update(change)
                with self.assertRaises(probe.ProbeError):
                    probe.verify_job(client, "key", 0)

    def test_missing_result_does_not_inherit(self):
        client = FakeClient()
        del client.job["result"]
        with self.assertRaises(probe.ProbeError):
            probe.verify_job(client, "key", 1)

    @patch("probe.socket.create_connection")
    @patch("probe.socket.getaddrinfo", return_value=[1])
    def test_stalled_job_report_keeps_recovery_identity(self, _dns, _tcp):
        client = FakeClient()
        client.job["status"] = "queued"
        report = probe.diagnose(client, "verify", "recovery-key", 0)
        self.assertEqual(report["status"], "failed")
        self.assertEqual(report["job_id"], JOB["id"])
        self.assertEqual(report["key"], "recovery-key")

    def test_invalid_history_and_replay(self):
        for field in ("history", "replay"):
            client = FakeClient()
            setattr(client, field, {})
            with self.assertRaises(probe.ProbeError):
                probe.verify_job(client, "key", 1)

    def test_invalid_key_never_submits(self):
        client = FakeClient()
        with self.assertRaises(probe.ProbeError):
            probe.verify_job(client, "bad key", 1)
        self.assertEqual(client.posts, 0)

    @patch("probe.socket.create_connection")
    @patch("probe.socket.getaddrinfo", return_value=[1])
    def test_doctor_is_not_job_verification(self, _dns, _tcp):
        client = FakeClient()
        report = probe.diagnose(client, "doctor", "unused", 1)
        self.assertEqual(report["status"], "passed")
        self.assertEqual(report["scope"], "doctor")
        self.assertNotIn("job_id", report)
        self.assertEqual(client.posts, 0)

    @patch("probe.socket.getaddrinfo", side_effect=socket.gaierror())
    def test_dns_failure(self, _dns):
        report = probe.diagnose(FakeClient(), "doctor", "unused", 1)
        self.assertEqual(report["checks"][-1]["stage"], "dns")
        self.assertEqual(report["status"], "failed")

    @patch("probe.socket.getaddrinfo", return_value=[1])
    @patch("probe.socket.create_connection", side_effect=ConnectionRefusedError())
    def test_tcp_failure_after_dns_success(self, _tcp, _dns):
        report = probe.diagnose(FakeClient(), "doctor", "unused", 1)
        self.assertEqual(report["checks"][0]["status"], "passed")
        self.assertEqual(report["checks"][-1]["stage"], "tcp")

    @patch("probe.socket.getaddrinfo", return_value=[1])
    @patch("probe.socket.create_connection")
    def test_wrong_readiness_response(self, _tcp, _dns):
        client = FakeClient()
        client.request = MagicMock(return_value={"status": "ok"})
        report = probe.diagnose(client, "doctor", "unused", 1)
        self.assertEqual(report["checks"][-1]["stage"], "readiness")


class HTTPTests(unittest.TestCase):
    def test_invalid_origins(self):
        for address in ("file:///x", "http://x/a", "http://a:b@x", "http://x?a=b", "http://x#frag", "http://x:bad"):
            with self.subTest(address=address), self.assertRaises(probe.ProbeError):
                probe.Client(address, time.monotonic() + 1)

    def test_expired_deadline(self):
        with self.assertRaises(probe.ProbeError):
            probe.Client("http://localhost", 0).remaining()

    def test_http_contract_and_redirect_refusal(self):
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                status, body = {"/ok": (200, b'{"ok":true}'), "/bad": (200, b'bad'),
                                "/array": (200, b'[]'), "/large": (200, b'x' * 131073),
                                "/redirect": (302, b''), "/down": (503, b'{}')}[self.path]
                self.send_response(status)
                if status == 302:
                    self.send_header("Location", "/ok")
                self.end_headers()
                self.wfile.write(body)
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            client = probe.Client(f"http://127.0.0.1:{server.server_port}", time.monotonic() + 5)
            self.assertEqual(client.request("GET", "/ok", 200), {"ok": True})
            for path in ("/bad", "/array", "/large", "/redirect", "/down"):
                with self.subTest(path=path), self.assertRaises(probe.ProbeError):
                    client.request("GET", path, 200)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
