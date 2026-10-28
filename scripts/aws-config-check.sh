#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_root"

# A project-local environment; never install Python dependencies globally.
if [[ ! -x .local/aws-tools/bin/python ]]; then
  "${PYTHON:-python3}" -m venv .local/aws-tools
fi
if ! .local/aws-tools/bin/python -c 'import yaml; assert yaml.__version__ == "6.0.3"' >/dev/null 2>&1; then
  .local/aws-tools/bin/python -m pip install --disable-pip-version-check -r platform_tools/requirements-aws.txt
fi
mkdir -p .local/phase11b
report_dir="$(mktemp -d "$project_root/.local/phase11b/config.XXXXXXXX")"
bash scripts/alb-tools.sh 2>&1 | tee "$report_dir/ingress-tools.log"
.local/aws-tools/bin/python -m unittest discover -s platform_tools/aws_tests -p 'test_*.py' -v 2>&1 | tee "$report_dir/tests.log"
.local/aws-tools/bin/python platform_tools/aws_config.py > "$report_dir/plan.json"
.local/aws-tools/bin/python platform_tools/aws_ingress.py > "$report_dir/ingress-plan.json"
.local/aws-tools/bin/python platform_tools/aws_preflight.py plan > "$report_dir/preflight-plan.json"
printf 'PASS: offline AWS application configuration only. Evidence: %s\n' "$report_dir"
