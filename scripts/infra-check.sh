#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_root"
mkdir -p .local/phase11
report_dir="$(mktemp -d "$project_root/.local/phase11/run.XXXXXXXX")"
image_tag="durable-jobs-infra-check:$(basename "$report_dir" | tr '[:upper:]' '[:lower:]')"
python_bin="${PYTHON:-python3}"

printf 'Offline AWS configuration checks. Evidence: %s\n' "$report_dir"
docker build --tag "$image_tag" infra/aws 2>&1 | tee "$report_dir/build.log"

# No network, AWS environment, home mount or Docker socket reaches Terraform.
# Use the host user's unprivileged ID so reports remain readable on Linux CI.
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges --user "$(id -u):$(id -g)" \
  --pids-limit 128 --memory 2g --cpus 2 \
  --tmpfs /tmp:rw,nosuid,nodev,size=64m \
  --tmpfs /work:rw,exec,nosuid,nodev,size=2g,mode=1777 \
  --mount "type=bind,src=$report_dir,dst=/reports" \
  "$image_tag" 2>&1 | tee "$report_dir/check.log"
# Host Python only reads the static graph; it never loads a provider or AWS SDK.
"$python_bin" -m unittest discover -s platform_tools -p 'test_infra_graph.py' -v 2>&1 | tee "$report_dir/graph-tests.log"
"$python_bin" platform_tools/infra_graph.py "$report_dir/dependencies.dot" 2>&1 | tee "$report_dir/graph-check.log"
printf 'PASS: mocked plans and static dependency checks only. No AWS deployment. Evidence: %s\n' "$report_dir"
