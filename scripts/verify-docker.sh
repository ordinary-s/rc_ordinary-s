#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
if [[ "$ROOT" == /mnt/d || "$ROOT" == /mnt/d/* ]]; then
  command -v findmnt >/dev/null 2>&1 || { echo 'findmnt is required to verify the D: mount' >&2; exit 2; }
  mounted="$(findmnt -n -o TARGET --target "$ROOT")"
  [[ "$mounted" == /mnt/d ]] || { echo "D: is not mounted at /mnt/d (resolved mount: $mounted)" >&2; exit 2; }
fi
cd "$ROOT"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
PROJECT="rcnotif-verify-$(printf '%s' "$RUN_ID" | tr '[:upper:]' '[:lower:]')"
REPORT="$ROOT/reports/$RUN_ID"
EVIDENCE="$ROOT/docs/evidence/$RUN_ID"
mkdir -p "$REPORT" "$EVIDENCE"
export APP_IMAGE=rc-notification:0.1.0
export TEST_IMAGE=rc-notification-test:0.1.0
export CERT_IMAGE=rc-notification-cert-init:0.1.0
export ADMIN_ACTOR=integration-test
export DELIVERY_BUDGET=90s
export MQ_CONSUME_TIMEOUT=2m
export VERIFY_RUN_ID="$RUN_ID"
export APP_BIND=127.0.0.1
export APP_PORT=$((24000 + RANDOM % 20000))
COMPOSE=(docker compose -p "$PROJECT" --project-directory "$ROOT" -f "$ROOT/compose.yaml" -f "$ROOT/compose.verify.yaml")
TOUCHED=0
RUNNER_ID=''
STATUS=0

record() { printf '%s\n' "$*" | tee -a "$REPORT/summary.txt"; }
run_phase() {
  local name="$1" code
  shift
  {
    printf '## %s\ncommand=' "$name"
    printf '%q ' "$@"
    printf '\n'
  } | tee -a "$REPORT/verify.log"
  set +e
  "$@" 2>&1 | tee -a "$REPORT/verify.log"
  code=${PIPESTATUS[0]}
  set -e
  printf 'exit_code=%s\n' "$code" | tee -a "$REPORT/verify.log"
  record "stage_$(printf '%s' "$name" | tr '[:upper:] ' '[:lower:]_')_exit_code=$code"
  return "$code"
}
wait_healthy() {
  local service="$1" timeout="$2" deadline=$((SECONDS + timeout)) id health
  while (( SECONDS < deadline )); do
    id="$("${COMPOSE[@]}" ps -q "$service")"
    if [[ -n "$id" ]]; then
      health="$(docker inspect "$id" --format '{{.State.Health.Status}}')"
      [[ "$health" == healthy ]] && return 0
      [[ "$health" == unhealthy ]] && return 1
    fi
    sleep 2
  done
  return 1
}
wait_for_file() {
  local path="$1" timeout="$2" deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    [[ -f "$path" ]] && return 0
    sleep 0.2
  done
  return 1
}
cleanup() {
  local trap_code=$? service id code scan_code context_failure=0 task_id trace_id
  trap - EXIT
  if (( TOUCHED )); then
    if [[ -n "$RUNNER_ID" ]]; then
      if docker rm -f "$RUNNER_ID" >> "$REPORT/verify.log" 2>&1; then :; else code=$?; record "runner_cleanup_exit_code=$code"; (( STATUS == 0 )) && STATUS=$code; fi
    fi
    for service in app mysql namesrv broker mock-target; do
      if id="$("${COMPOSE[@]}" ps -q "$service" 2>> "$REPORT/verify.log")"; then :; else code=$?; record "service_lookup_${service}_exit_code=$code"; (( STATUS == 0 )) && STATUS=$code; continue; fi
      if [[ -n "$id" ]]; then
        if "${COMPOSE[@]}" logs --no-color --tail=200 "$service" > "$REPORT/$service.log" 2>&1; then :; else code=$?; record "log_collection_${service}_exit_code=$code"; (( STATUS == 0 )) && STATUS=$code; fi
      fi
    done
    if [[ -f "$REPORT/app.log" ]]; then
      if grep -E 'notification-body-secret-canary-9ba8|demo-client-token|demo-admin-token|Authorization:|Cookie:' "$REPORT/app.log" > "$REPORT/log-secret-scan.txt"; then
        record 'log_secret_scan=FAIL'; (( STATUS == 0 )) && STATUS=1
      else
        scan_code=$?
        if (( scan_code == 1 )); then record 'log_secret_scan=PASS'; else record "log_secret_scan_exit_code=$scan_code"; (( STATUS == 0 )) && STATUS=$scan_code; fi
      fi
      if [[ -f "$REPORT/log-context.tsv" ]]; then
        while IFS=$'\t' read -r task_id trace_id; do
          [[ -n "$task_id" && -n "$trace_id" ]] || continue
          if ! grep -F "task_id=$task_id" "$REPORT/app.log" | grep -F 'msg="notification processing finished"' | grep -F "trace_id=$trace_id" >/dev/null; then
            context_failure=1; break
          fi
        done < "$REPORT/log-context.tsv"
        if (( context_failure )); then record 'log_context_scan=FAIL'; (( STATUS == 0 )) && STATUS=1; else record 'log_context_scan=PASS'; fi
        cp "$REPORT/log-context.tsv" "$EVIDENCE/log-context.tsv"
      elif (( STATUS == 0 )); then
        record 'log_context_scan=FAIL (integration context file missing)'; STATUS=1
      fi
      if grep -F 'msg="notification processing finished"' "$REPORT/app.log" | tail -n 100 > "$EVIDENCE/app-results.log"; then :; else code=$?; if (( code != 1 )); then record "app_result_export_exit_code=$code"; (( STATUS == 0 )) && STATUS=$code; fi; fi
    elif (( STATUS == 0 )); then
      record 'log_secret_scan=NOT_RUN'; STATUS=1
    fi
    if "${COMPOSE[@]}" ps -a > "$REPORT/compose-ps.txt" 2>&1; then :; else code=$?; record "compose_ps_exit_code=$code"; (( STATUS == 0 )) && STATUS=$code; fi
    if "${COMPOSE[@]}" down --volumes --remove-orphans >> "$REPORT/verify.log" 2>&1; then record 'cleanup_exit_code=0'; else code=$?; record "cleanup_exit_code=$code"; (( STATUS == 0 )) && STATUS=$code; fi
  fi
  for service in "$APP_IMAGE" "$TEST_IMAGE" "$CERT_IMAGE"; do
    if docker image inspect "$service" --format "image=$service id={{.Id}}" >> "$REPORT/summary.txt" 2>&1; then :; else code=$?; record "image_inspect_exit_code=$code image=$service"; (( STATUS == 0 )) && STATUS=$code; fi
  done
  if (( STATUS == 0 && trap_code != 0 )); then STATUS=$trap_code; fi
  if (( STATUS == 0 )); then record 'result=PASS'; else record 'result=FAIL'; fi
  record "finished_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  cp "$REPORT/summary.txt" "$EVIDENCE/summary.txt"
  for file in checks.log checks-summary.txt integration.json sigterm.json compose-ps.txt; do
    [[ -f "$REPORT/$file" ]] && cp "$REPORT/$file" "$EVIDENCE/$file"
  done
  printf 'run_id=%s\nreport=%s\nevidence=%s\nexit_code=%s\n' "$RUN_ID" "$REPORT" "$EVIDENCE" "$STATUS"
  exit "$STATUS"
}
trap cleanup EXIT
record "run_id=$RUN_ID"
record "project=$PROJECT"
record "started_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
record "project_root=$ROOT"

run_phase 'docker info' docker info
run_phase 'docker version' docker version
run_phase 'docker compose version' docker compose version
run_phase 'compose config --quiet' "${COMPOSE[@]}" config --quiet
run_phase 'build application image' docker build --pull --target runtime -t "$APP_IMAGE" .
run_phase 'build test-runner image' docker build --pull --target test-runner -t "$TEST_IMAGE" .
run_phase 'build certificate initializer image' docker build --pull --target cert-init -t "$CERT_IMAGE" .
TOUCHED=1
run_phase 'start isolated stack' "${COMPOSE[@]}" up -d app
wait_healthy app 300 || { echo 'app failed health check within five minutes' | tee -a "$REPORT/verify.log"; exit 1; }
record 'stack_health=PASS'

set +e
runner_output="$("${COMPOSE[@]}" run -d --no-deps -e TEST_REPORT=/reports/sigterm.json -e TEST_CONTROL_DIR=/reports test-runner go test -tags=integration -run '^TestSIGTERMInterruptsInFlightDelivery$' -count=1 -v ./tests/integration 2>> "$REPORT/verify.log")"
code=$?
set -e
record "sigterm_runner_start_exit_code=$code"
[[ "$code" == 0 ]] || exit "$code"
RUNNER_ID="$(printf '%s\n' "$runner_output" | tail -n 1 | tr -d '\r\n')"
[[ -n "$RUNNER_ID" ]] || { echo 'Docker returned no SIGTERM runner container id' | tee -a "$REPORT/verify.log"; exit 1; }
wait_for_file "$REPORT/sigterm-armed" 120 || { echo 'SIGTERM test did not observe an in-flight request' | tee -a "$REPORT/verify.log"; exit 1; }
run_phase 'stop app during outbound HTTP with SIGTERM' "${COMPOSE[@]}" stop -t 20 app
app_stop_state="$(docker inspect "$("${COMPOSE[@]}" ps -q app)" --format '{{.State.Status}}|{{.State.ExitCode}}')"
[[ "$app_stop_state" == 'exited|0' ]] || { echo "app did not exit cleanly: $app_stop_state" | tee -a "$REPORT/verify.log"; exit 1; }
if "${COMPOSE[@]}" logs --no-color app > "$REPORT/sigterm-app.log" 2>&1; then :; else code=$?; echo "SIGTERM log collection failed: $code"; exit "$code"; fi
grep -q 'shutdown signal received' "$REPORT/sigterm-app.log" || { echo 'missing shutdown signal log' | tee -a "$REPORT/verify.log"; exit 1; }
grep -q 'server stopped' "$REPORT/sigterm-app.log" || { echo 'missing graceful stop log' | tee -a "$REPORT/verify.log"; exit 1; }
touch "$REPORT/sigterm-stopped"
run_phase 'restart app after SIGTERM test' "${COMPOSE[@]}" start app
wait_healthy app 180 || { echo 'app failed health check after restart' | tee -a "$REPORT/verify.log"; exit 1; }
touch "$REPORT/sigterm-resume"
deadline=$((SECONDS + 240))
runner_finished=0
while (( SECONDS < deadline )); do
  [[ "$(docker inspect "$RUNNER_ID" --format '{{.State.Running}}')" == false ]] && { runner_finished=1; break; }
  sleep 1
done
(( runner_finished )) || { echo 'SIGTERM integration runner did not finish' | tee -a "$REPORT/verify.log"; exit 1; }
docker logs "$RUNNER_ID" > "$REPORT/sigterm-runner.log" 2>&1
cat "$REPORT/sigterm-runner.log" | tee -a "$REPORT/verify.log"
runner_code="$(docker inspect "$RUNNER_ID" --format '{{.State.ExitCode}}')"
record "sigterm_integration_exit_code=$runner_code"
docker rm "$RUNNER_ID" >> "$REPORT/verify.log" 2>&1
RUNNER_ID=''
[[ "$runner_code" == 0 ]] || exit "$runner_code"

run_phase 'full Docker verification' "${COMPOSE[@]}" run --rm test-runner
record 'full_verification_exit_code=0'
