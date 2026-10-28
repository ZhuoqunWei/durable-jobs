#!/bin/sh
set -eu

# /work is disposable tmpfs. Tests never see host credentials or state.
cp -R /src/. /work/
cd /work
terraform version -json > /reports/versions.json
terraform fmt -check -recursive -diff
terraform validate -no-color
terraform graph > /reports/dependencies.dot
terraform test -no-color -junit-xml=/reports/results.xml
