SHELL := /bin/sh

-include .env
export DATABASE_URL HTTP_ADDR

STATICCHECK ?= GOTOOLCHAIN=$$(go env GOVERSION) go run honnef.co/go/tools/cmd/staticcheck@v0.8.1
PYTHON ?= python3
LAB_ARGS = --context kind-durable-jobs-clear --namespace durable-jobs-lab --kubeconfig "$(CURDIR)/.local/phase10/kubeconfig"

.PHONY: help db-up db-stop db-status db-shell run worker worker-once migrate build test integration check lint container-check fmt exercise harness harness-list sandbox-image sandbox-demo sandbox-check script-job script-queue-check lab-tools lab-up lab-status lab-doctor lab-verify lab-check platform-check infra-check aws-config-check aws-local-check alb-tools aws-preflight
.PHONY: monitoring-check demo-plan demo-check workflow-check

help:
	@printf '%s\n' \
	  'make db-up        Start the project database and wait for it to be healthy' \
	  'make run          Run the API in this terminal (Ctrl-C stops it)' \
	  'make worker       Process queued synthetic jobs (Ctrl-C stops it)' \
	  'make worker-once  Recover expired claims, process at most one due job, exit' \
	  'make migrate      Apply checked-in SQL migrations once' \
	  'make test         Run unit tests, including submission validation' \
	  'make integration  Test storage and HTTP against an isolated test schema' \
	  'make harness      Run named reliability scenarios and save a JSON report' \
	  'make harness-list List scenarios without running tests or using the database' \
	  'make sandbox-image Build the pinned local Python runner image explicitly' \
	  'make sandbox-demo  Run the synthetic Python transform in a limited container' \
	  'make sandbox-check Run opt-in container isolation and cleanup tests' \
	  'make script-job    Print a pinned synthetic script submission (no execution)' \
	  'make script-queue-check Test HTTP, PostgreSQL, CLI worker and real sandbox together' \
	  'make check        Run tests with the race detector, then go vet' \
	  'make lint         Check Go formatting and run pinned Staticcheck (no edits)' \
	  'make container-check  Build images, run checks and verify an isolated container stack' \
	  'make lab-tools    Install pinned kind into this repository only' \
	  'make lab-up       Build and deploy the separate local Kubernetes lab (retains data)' \
	  'make lab-status   Inspect local Kubernetes resources without changing them' \
	  'make lab-doctor   Run an in-cluster DNS/TCP/readiness diagnostic Job' \
	  'make lab-verify   Submit fresh synthetic work and check completion/replay/history' \
	  'make lab-check    Exercise DNS/port/worker failures and process restarts in the local lab' \
	  'make demo-plan    Preview the disposable release demo without changing anything' \
	  'make demo-check   Build, deploy, interrupt a worker and verify recovery in a NEW disposable local cluster' \
	  'make workflow-check  Validate the GitHub workflow using pinned actionlint' \
	  'make platform-check  Test the Python tooling and render Kubernetes configurations' \
	  'make infra-check  Validate and test Terraform offline in a credential-free container' \
	  'make aws-config-check  Test AWS application/ingress profiles and render the pinned controller chart offline' \
	  'make aws-preflight  Show missing deployment inputs offline; no credentials, AWS calls or deployment' \
	  'make alb-tools    Prepare repository-local Helm and checksum-verified controller chart (no cluster)' \
	  'make aws-local-check  Test its DB roles/TLS and real jobs in disposable local containers' \
	  'make exercise     Run submission-validation tests with verbose output' \
	  'make db-stop      Stop only this project database, retaining its data'

db-up:
	docker compose up -d --wait --wait-timeout 60 postgres

db-stop:
	docker compose stop postgres

db-status:
	docker compose ps

db-shell:
	docker compose exec postgres psql -U "$(POSTGRES_USER)" -d "$(POSTGRES_DB)"

run:
	go run ./cmd/api

worker:
	go run ./cmd/worker

worker-once:
	go run ./cmd/worker -once

migrate:
	go run ./cmd/migrate

build:
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker
	go build -o bin/harness ./cmd/harness
	go build -o bin/sandbox ./cmd/sandbox
	go build -o bin/migrate ./cmd/migrate
	go build -o bin/smoke ./cmd/smoke

sandbox-image:
	go run ./cmd/sandbox -build-image

sandbox-demo:
	go run ./cmd/sandbox -script examples/transform-campaign.py -input examples/campaign-input.json

sandbox-check:
	SANDBOX_INTEGRATION=1 go test -race -count=1 -v -timeout=120s ./internal/sandbox

script-job:
	@go run ./cmd/sandbox -job -input examples/campaign-input.json

script-queue-check:
	@test -n "$(DATABASE_URL)" || (printf 'Create .env from .env.example first.\n'; exit 1)
	@TEST_DATABASE_URL="$(DATABASE_URL)" SANDBOX_INTEGRATION=1 go test -race -count=1 -v -timeout=120s -run '^TestScriptQueue' ./internal/database

harness:
	go run ./cmd/harness $(HARNESS_ARGS)

harness-list:
	go run ./cmd/harness -list

test:
	go test -count=1 ./...

integration:
	@test -n "$(DATABASE_URL)" || (printf 'Create .env from .env.example first.\n'; exit 1)
	@TEST_DATABASE_URL="$(DATABASE_URL)" go test -race -count=1 -v ./internal/database

check:
	go test -race -count=1 ./...
	go vet ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; printf 'Run make fmt and review the formatting changes.\n'; exit 1)
	$(STATICCHECK) ./...

container-check:
	bash scripts/container-check.sh

lab-tools:
	GOBIN="$(CURDIR)/bin" GOTOOLCHAIN=$$(go env GOVERSION) go install sigs.k8s.io/kind@v0.33.0

lab-up:
	bash scripts/lab-up.sh

lab-status:
	$(PYTHON) platform_tools/lab.py status $(LAB_ARGS)

lab-doctor:
	$(PYTHON) platform_tools/lab.py doctor $(LAB_ARGS)

lab-verify:
	$(PYTHON) platform_tools/lab.py verify $(LAB_ARGS)

lab-check:
	$(PYTHON) platform_tools/integration.py

demo-plan:
	$(PYTHON) platform_tools/release_demo.py

demo-check:
	$(PYTHON) platform_tools/release_demo.py --run

workflow-check:
	GOTOOLCHAIN=$$(go env GOVERSION) go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.11 -shellcheck= .github/workflows/ci.yaml

platform-check:
	$(PYTHON) -m unittest discover -s platform_tools -p 'test_*.py' -v
	kubectl kustomize deploy/kubernetes/local/infra >/dev/null
	kubectl kustomize deploy/kubernetes/local/app >/dev/null
	bash -n scripts/lab-up.sh
	$(MAKE) aws-config-check
	$(MAKE) monitoring-check

monitoring-check:
	docker run --rm --entrypoint promtool -w /etc/prometheus -v "$(CURDIR)/deploy/observability:/etc/prometheus:ro" prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e check config /etc/prometheus/prometheus.yml
	docker run --rm --entrypoint promtool -w /etc/prometheus -v "$(CURDIR)/deploy/observability:/etc/prometheus:ro" prom/prometheus:v3.15.0@sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e test rules rules.test.yml

infra-check:
	PYTHON="$(PYTHON)" bash scripts/infra-check.sh

aws-config-check:
	bash scripts/aws-config-check.sh

aws-preflight:
	$(PYTHON) platform_tools/aws_preflight.py plan

alb-tools:
	bash scripts/alb-tools.sh

aws-local-check: aws-config-check
	.local/aws-tools/bin/python platform_tools/aws_local_check.py

fmt:
	gofmt -w cmd internal

exercise:
	go test -count=1 -v ./internal/jobs
