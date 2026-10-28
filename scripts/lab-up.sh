#!/usr/bin/env bash
set -euo pipefail
umask 077
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"
cluster=durable-jobs-clear
lab_dir="$repo_root/.local/phase10"
kubeconfig="$lab_dir/kubeconfig"
kind_bin="$repo_root/bin/kind"
export KIND_EXPERIMENTAL_PROVIDER=docker
test -x "$kind_bin" || { echo 'Run make lab-tools first.' >&2; exit 1; }
docker info >/dev/null
kubectl version --client >/dev/null
mkdir -p "$lab_dir"
if "$kind_bin" get clusters | grep -Fxq "$cluster"; then
  test -s "$kubeconfig" || { echo 'Existing cluster has no repository-local kubeconfig; refusing adoption.' >&2; exit 1; }
else
  test ! -e "$kubeconfig" || { echo 'Stale kubeconfig exists; inspect it before recreating the lab.' >&2; exit 1; }
  "$kind_bin" create cluster --name "$cluster" --kubeconfig "$kubeconfig" \
    --config deploy/kind.yaml --wait 120s \
    --image kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0
fi
report_dir="$(mktemp -d "$lab_dir/build.XXXXXXXX")"
docker build --target runtime -t durable-jobs:phase10 . 2>&1 | tee "$report_dir/app-build.log"
docker build -t durable-jobs-probe:phase10 platform_tools 2>&1 | tee "$report_dir/probe-build.log"
docker image inspect durable-jobs:phase10 durable-jobs-probe:phase10 > "$report_dir/images.json"
"$kind_bin" load docker-image --name "$cluster" durable-jobs:phase10 durable-jobs-probe:phase10
python3 platform_tools/lab.py deploy --context kind-durable-jobs-clear \
  --namespace durable-jobs-lab --kubeconfig "$kubeconfig" | tee "$report_dir/deploy.json"
echo "Lab retained for learning. Build evidence: $report_dir"
