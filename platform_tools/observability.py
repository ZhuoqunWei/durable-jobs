"""Render private, bounded, ephemeral lab monitoring; no cluster calls."""
import argparse
import json
from pathlib import Path

NAMESPACE = "durable-jobs-aws"
PROMETHEUS = "prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e"
GRAFANA = "grafana/grafana:13.2.2@sha256:ac461fb352abc50da10a51c7d02462e9c05488f11f53f14b3ad79a8145f638a0"


def dashboard():
    definitions = [
        ("Queue depth", ["durable_jobs_queue_depth", "durable_jobs_running"]),
        ("Lifetime p95 latency (seconds)", [
            'histogram_quantile(0.95, durable_jobs_start_latency_seconds_bucket)',
            'histogram_quantile(0.95, durable_jobs_completion_latency_seconds_bucket)']),
        ("Persisted terminal jobs", ['durable_jobs_terminal_total']),
        ("Worker heartbeat age / active workers", ['durable_jobs_worker_heartbeat_age_seconds', 'durable_jobs_active_workers']),
    ]
    panels = []
    for i, (title, expressions) in enumerate(definitions):
        panels.append({"id": i + 1, "title": title, "type": "timeseries",
            "datasource": {"type": "prometheus", "uid": "lab-prometheus"},
            "gridPos": {"x": (i % 2) * 12, "y": (i // 2) * 8, "w": 12, "h": 8},
            "targets": [{"refId": chr(65+j), "expr": expr} for j, expr in enumerate(expressions)],
            "fieldConfig": {"defaults": {}, "overrides": []}, "options": {}})
    return {"uid": "durable-jobs-lab", "title": "Durable Jobs — reliability lab",
        "schemaVersion": 39, "version": 1, "editable": False, "refresh": "5s",
        "time": {"from": "now-30m", "to": "now"}, "panels": panels}


def render(root):
    def resource(kind, name, **fields):
        return {"apiVersion": "apps/v1" if kind == "Deployment" else "v1", "kind": kind,
            "metadata": {"name": name, "namespace": NAMESPACE, "labels": {"part-of": "durable-jobs-reliability"}}, **fields}
    monitoring = Path(root) / "deploy/observability"
    configs = [resource("ConfigMap", "prometheus-config", data={
        name: (monitoring / name).read_text() for name in ("prometheus.yml", "rules.yml")}),
        resource("ConfigMap", "grafana-provisioning", data={
            "datasources.yml": "apiVersion: 1\ndatasources:\n  - name: Lab Prometheus\n    uid: lab-prometheus\n    type: prometheus\n    access: proxy\n    url: http://prometheus:9090\n    isDefault: true\n    editable: false\n",
            "dashboards.yml": "apiVersion: 1\nproviders:\n  - name: lab\n    type: file\n    disableDeletion: true\n    options:\n      path: /etc/grafana/dashboards\n",
            "dashboard.json": json.dumps(dashboard())})]
    for name, image, port, uid in [("prometheus", PROMETHEUS, 9090, 65534), ("grafana", GRAFANA, 3000, 472)]:
        mounts = [{"name": "data", "mountPath": "/prometheus" if name == "prometheus" else "/var/lib/grafana"},
                  {"name": "tmp", "mountPath": "/tmp"}]
        volumes = [{"name": "data", "emptyDir": {"sizeLimit": "256Mi"}}, {"name": "tmp", "emptyDir": {"sizeLimit": "32Mi"}}]
        container = {"name": name, "image": image, "ports": [{"containerPort": port, "name": "http"}],
            "resources": {"requests": {"cpu": "50m", "memory": "128Mi"}, "limits": {"cpu": "300m", "memory": "256Mi"}},
            "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
            "readinessProbe": {"httpGet": {"path": "/-/ready" if name == "prometheus" else "/api/health", "port": port}, "periodSeconds": 5},
            "livenessProbe": {"httpGet": {"path": "/-/healthy" if name == "prometheus" else "/api/health", "port": port}, "initialDelaySeconds": 30, "periodSeconds": 10},
            "volumeMounts": mounts}
        if name == "prometheus":
            container["args"] = ["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus", "--storage.tsdb.retention.time=6h", "--storage.tsdb.retention.size=200MB"]
            volumes.append({"name": "config", "configMap": {"name": "prometheus-config"}})
            mounts.append({"name": "config", "mountPath": "/etc/prometheus", "readOnly": True})
        else:
            container["env"] = [{"name": "GF_SECURITY_ADMIN_PASSWORD", "valueFrom": {"secretKeyRef": {"name": "grafana-admin", "key": "password"}}},
                {"name": "GF_ANALYTICS_REPORTING_ENABLED", "value": "false"}, {"name": "GF_ANALYTICS_CHECK_FOR_UPDATES", "value": "false"},
                {"name": "GF_USERS_ALLOW_SIGN_UP", "value": "false"}]
            volumes.append({"name": "config", "configMap": {"name": "grafana-provisioning"}})
            for key, path in [("datasources.yml", "/etc/grafana/provisioning/datasources/lab.yml"), ("dashboards.yml", "/etc/grafana/provisioning/dashboards/lab.yml"), ("dashboard.json", "/etc/grafana/dashboards/lab.json")]:
                mounts.append({"name": "config", "mountPath": path, "subPath": key, "readOnly": True})
        configs.append(resource("Deployment", name, spec={"replicas": 1, "strategy": {"type": "Recreate"},
            "selector": {"matchLabels": {"app": name}}, "template": {"metadata": {"labels": {"app": name}}, "spec": {
                "automountServiceAccountToken": False, "nodeSelector": {"kubernetes.io/arch": "amd64"},
                "securityContext": {"runAsNonRoot": True, "runAsUser": uid, "runAsGroup": uid, "fsGroup": uid, "seccompProfile": {"type": "RuntimeDefault"}},
                "containers": [container], "volumes": volumes}}}))
        configs.append(resource("Service", name, spec={"type": "ClusterIP", "selector": {"app": name}, "ports": [{"port": port, "targetPort": "http"}]}))
    return {"apiVersion": "v1", "kind": "List", "items": configs}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    Path(args.output).write_text(json.dumps(render(Path(__file__).resolve().parents[1]), indent=2)+"\n")
