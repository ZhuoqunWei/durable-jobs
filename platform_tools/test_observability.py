import json
from pathlib import Path
import unittest

from observability import dashboard, render


class ObservabilityTests(unittest.TestCase):
    def test_checked_in_dashboard_and_manifest_match_generator(self):
        root=Path(__file__).resolve().parents[1]
        self.assertEqual(json.loads((root/'deploy/observability/dashboard.json').read_text()),dashboard())
        self.assertEqual(json.loads((root/'deploy/observability/kubernetes.json').read_text()),render(root))

    def test_private_bounded_nonroot(self):
        resources = render(Path(__file__).resolve().parents[1])["items"]
        self.assertFalse(any(x["kind"] in ("Ingress", "Secret") for x in resources))
        for item in resources:
            if item["kind"] == "Service":
                self.assertEqual(item["spec"]["type"], "ClusterIP")
            if item["kind"] == "Deployment":
                pod = item["spec"]["template"]["spec"]
                self.assertFalse(pod["automountServiceAccountToken"])
                self.assertTrue(pod["securityContext"]["runAsNonRoot"])
                self.assertIn("@sha256:", pod["containers"][0]["image"])
                self.assertIn("limits", pod["containers"][0]["resources"])
        self.assertEqual(len(dashboard()["panels"]), 4)
        self.assertNotIn("anonymous", json.dumps(resources))

    def test_one_shared_database_scrape(self):
        resources = render(Path(__file__).resolve().parents[1])["items"]
        config = resources[0]["data"]["prometheus.yml"]
        self.assertEqual(config.count("targets:"), 1)
        self.assertIn("api:8085", config)


if __name__ == "__main__":
    unittest.main()
