#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_root"
mkdir -p bin .local/alb-tools

# Repository-local tool only; never installs a release or reads cluster state.
if [[ ! -x bin/helm ]] || [[ "$(go version -m bin/helm | awk '$1 == "mod" && $2 == "helm.sh/helm/v3" {print $3}')" != 'v3.22.0' ]]; then
  GOBIN="$project_root/bin" GOTOOLCHAIN=go1.27.1 go install helm.sh/helm/v3/cmd/helm@v3.22.0
fi
chart="$project_root/.local/alb-tools/aws-load-balancer-controller-3.5.0.tgz"
expected='45051f634b33e10baccb3354d0681b7de787c60445e599fa276e0c9aedd4ccd5'
if [[ ! -f "$chart" ]]; then
  download="$(mktemp "$project_root/.local/alb-tools/download.XXXXXXXX")"
  curl --fail --silent --show-error --location --max-time 60 \
    https://aws.github.io/eks-charts/aws-load-balancer-controller-3.5.0.tgz --output "$download"
  [[ "$(shasum -a 256 "$download" | cut -d ' ' -f 1)" == "$expected" ]] || { printf 'Chart checksum mismatch.\n' >&2; exit 1; }
  mv "$download" "$chart"
fi
[[ "$(shasum -a 256 "$chart" | cut -d ' ' -f 1)" == "$expected" ]] || { printf 'Cached chart checksum mismatch.\n' >&2; exit 1; }
printf 'Verified repository-local Helm v3.22.0 and controller chart 3.5.0. No installation performed.\n'
