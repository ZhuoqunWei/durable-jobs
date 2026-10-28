#!/usr/bin/env bash
# Own only this invocation's named Compose project. Never touch compose.yaml,
# its persistent database, .env, or any other Docker project.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"
docker info >/dev/null
docker compose version >/dev/null
mkdir -p .local/phase9
report_dir=$(mktemp -d "$repo_root/.local/phase9/run.XXXXXXXX")
project="dj-phase9-$(basename "$report_dir" | tr '[:upper:].' '[:lower:]-')"
export PHASE9_IMAGE=${PHASE9_IMAGE:-"durable-jobs-phase9:$project"}
export PHASE9_CHECKS_IMAGE="durable-jobs-phase9-checks:$project"
compose=(docker compose --env-file /dev/null -p "$project" -f "$repo_root/compose.phase9.yaml")

# Refuse unexpected reuse before installing cleanup handlers.
if [[ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$project")" ]]; then
  printf 'Refusing to reuse existing Compose project %s\n' "$project" >&2
  exit 1
fi

finish() {
  code=$?
  trap - EXIT INT TERM
  set +e
  "${compose[@]}" logs --no-color >"$report_dir/services.log" 2>&1
  "${compose[@]}" ps -a --format json >"$report_dir/containers.json" 2>&1
  "${compose[@]}" down --timeout 10 >"$report_dir/cleanup.log" 2>&1
  cleanup_code=$?
  if [[ $cleanup_code -ne 0 ]]; then
    printf 'Cleanup failed; inspect %s\n' "$report_dir/cleanup.log" >&2
    code=1
  fi
  printf '%s\n' "$code" >"$report_dir/exit-code.txt"
  printf 'Phase 9 exit=%s; evidence: %s\n' "$code" "$report_dir"
  exit "$code"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'Disposable project: %s\nRuntime image: %s\n' "$project" "$PHASE9_IMAGE" | tee "$report_dir/run.txt"
docker version --format '{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}' >>"$report_dir/run.txt"
docker build --target checks -t "$PHASE9_CHECKS_IMAGE" . 2>&1 | tee "$report_dir/checks-build.log"
docker build --target runtime -t "$PHASE9_IMAGE" . 2>&1 | tee "$report_dir/runtime-build.log"
docker image inspect "$PHASE9_IMAGE" >"$report_dir/image.json"
image_user=$(docker image inspect --format '{{.Config.User}}' "$PHASE9_IMAGE")
[[ "$image_user" == '65532:65532' ]] || { printf 'Runtime image is not the expected non-root user\n' >&2; exit 1; }

"${compose[@]}" up -d --wait --wait-timeout 60 postgres
"${compose[@]}" run --rm --no-deps checks 2>&1 | tee "$report_dir/checks.log"
"${compose[@]}" up -d --wait --wait-timeout 60 api worker
"${compose[@]}" run --rm --no-deps smoke | tee "$report_dir/smoke-before.json"

# Compare all job/attempt data across API + worker process restarts. Only this
# newly created synthetic database is queried; the application DB is untouched.
snapshot_sql="SELECT md5(jsonb_build_object('jobs', (SELECT jsonb_agg(j ORDER BY id) FROM jobs j), 'attempts', (SELECT jsonb_agg(a ORDER BY job_id, number) FROM job_attempts a))::text);"
before=$("${compose[@]}" exec -T postgres psql -U phase9 -d phase9 -At -v ON_ERROR_STOP=1 -c "$snapshot_sql")
"${compose[@]}" restart api worker
# Compose waits on API readiness; smoke itself has a bounded processing deadline.
"${compose[@]}" up -d --wait --wait-timeout 60 api worker
"${compose[@]}" run --rm --no-deps smoke | tee "$report_dir/smoke-after.json"
after=$("${compose[@]}" exec -T postgres psql -U phase9 -d phase9 -At -v ON_ERROR_STOP=1 -c "$snapshot_sql")
[[ -n "$before" && "$before" == "$after" ]] || { printf 'Persisted job/attempt state changed after restart/replay\n' >&2; exit 1; }
printf 'PASS: full job/attempt snapshot unchanged after restart and keyed replay\n' | tee "$report_dir/restart.txt"
printf 'PASS: checks, non-root runtime, synthetic result, replay, conflict, attempt history and restart\n'
