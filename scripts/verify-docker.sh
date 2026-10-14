#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
if [[ "$ROOT" == /mnt/d/* || "$ROOT" == /mnt/d ]]; then
  if ! command -v findmnt >/dev/null 2>&1; then
    echo "findmnt is required to verify the D: mount" >&2
    exit 2
  fi
  mounted="$(findmnt -n -o TARGET --target "$ROOT")"
  if [[ "$mounted" != /mnt/d ]]; then
    echo "D: is not mounted at /mnt/d (resolved mount: $mounted)" >&2
    exit 2
  fi
fi
cd "$ROOT"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
PROJECT="rcnotif-verify-$(printf '%s' "$RUN_ID" | tr '[:upper:]' '[:lower:]')"
REPORT="$ROOT/reports/$RUN_ID"
EVIDENCE="$ROOT/docs/evidence/$RUN_ID"
mkdir -p "$REPORT" "$EVIDENCE"
export APP_IMAGE=rc-notification:0.1.0
export TEST_IMAGE=rc-notification-test:0.1.0
export ADMIN_ACTOR=integration-test
export DELIVERY_BUDGET=90s
export MQ_CONSUME_TIMEOUT=2m
export VERIFY_RUN_ID="$RUN_ID"
export APP_BIND=127.0.0.1
export APP_PORT=$((24000 + RANDOM % 20000))
COMPOSE=(docker compose -p "$PROJECT" -f compose.yaml -f compose.verify.yaml)
TOUCHED=0
OUTAGE_RUNNER_ID=""
cleanup() {
  status=$?
  trap - EXIT
  if (( TOUCHED )); then
    if [[ -n "$OUTAGE_RUNNER_ID" ]]; then
      if docker rm -f "$OUTAGE_RUNNER_ID" >> "$REPORT/verify.log" 2>&1; then :; else code=$?; if (( status == 0 )); then status=$code; fi; fi
    fi
    for service in app mysql namesrv broker mock-target; do
      if "${COMPOSE[@]}" logs --no-color --tail=200 "$service" > "$REPORT/$service.log" 2>&1; then
        :
      else
        code=$?
        echo "log_collection_$service_exit_code=$code" >> "$REPORT/summary.txt"
        if (( status == 0 )); then status=$code; fi
      fi
    done
    if [[ -f "$REPORT/app.log" ]]; then
      if grep -E 'notification-body-secret-canary-9ba8|demo-client-token|demo-admin-token|Authorization:|Cookie:' "$REPORT/app.log" > "$REPORT/log-secret-scan.txt"; then
        echo "log_secret_scan_exit_code=1" >> "$REPORT/summary.txt"
        echo "log_secret_scan=FAIL" >> "$REPORT/summary.txt"
        if (( status == 0 )); then status=1; fi
      else
        grep_code=$?
        if (( grep_code != 1 )); then
          echo "log_secret_scan_exit_code=$grep_code" >> "$REPORT/summary.txt"
          if (( status == 0 )); then status=$grep_code; fi
        else
          echo "log_secret_scan_exit_code=0" >> "$REPORT/summary.txt"
          echo "log_secret_scan=PASS" >> "$REPORT/summary.txt"
        fi
      fi
    fi
    context_failure=0
    if [[ ! -f "$REPORT/log-context.tsv" ]]; then
      context_failure=1
    else
      while IFS=$'\t' read -r task_id trace_id; do
        [[ -n "$task_id" && -n "$trace_id" ]] || continue
        if ! grep -F "task_id=$task_id" "$REPORT/app.log" | grep -F 'msg="notification accepted"' | grep -F "trace_id=$trace_id" >/dev/null; then
          context_failure=1
          break
        fi
        if ! grep -F "task_id=$task_id" "$REPORT/app.log" | grep -F 'msg="notification processing finished"' | grep -F "trace_id=$trace_id" >/dev/null; then
          context_failure=1
          break
        fi
      done < "$REPORT/log-context.tsv"
    fi
    if (( context_failure )); then
      echo 'log_context_scan_exit_code=1' >> "$REPORT/summary.txt"
      echo 'log_context_scan=FAIL' >> "$REPORT/summary.txt"
      if (( status == 0 )); then status=1; fi
    else
      echo 'log_context_scan_exit_code=0' >> "$REPORT/summary.txt"
      echo 'log_context_scan=PASS' >> "$REPORT/summary.txt"
    fi
    if "${COMPOSE[@]}" ps -a > "$REPORT/compose-ps.txt" 2>&1; then
      :
    else
      code=$?
      echo "compose_ps_exit_code=$code" >> "$REPORT/summary.txt"
      if (( status == 0 )); then status=$code; fi
    fi
    if "${COMPOSE[@]}" down --volumes --remove-orphans >> "$REPORT/verify.log" 2>&1; then
      echo "cleanup_exit_code=0" >> "$REPORT/summary.txt"
    else
      code=$?
      echo "cleanup_exit_code=$code" >> "$REPORT/summary.txt"
      if (( status == 0 )); then status=$code; fi
    fi
  fi
  if docker image inspect "$APP_IMAGE" --format 'app_image_id={{.Id}}' >> "$REPORT/summary.txt" 2>&1; then :; else code=$?; if (( status == 0 )); then status=$code; fi; fi
  if docker image inspect "$TEST_IMAGE" --format 'test_image_id={{.Id}}' >> "$REPORT/summary.txt" 2>&1; then :; else code=$?; if (( status == 0 )); then status=$code; fi; fi
  if (( status == 0 )); then echo "result=PASS" >> "$REPORT/summary.txt"; else echo "result=FAIL" >> "$REPORT/summary.txt"; fi
  echo "finished_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$REPORT/summary.txt"
  cp "$REPORT/summary.txt" "$EVIDENCE/summary.txt"
  for file in checks.log checks-summary.txt integration.json outage.json sigterm.json log-context.tsv outage-runner.log sigterm-runner.log sigterm-app.log log-secret-scan.txt compose-ps.txt app.log mysql.log namesrv.log broker.log mock-target.log; do
    if [[ -f "$REPORT/$file" ]]; then cp "$REPORT/$file" "$EVIDENCE/$file"; fi
  done
  echo "run_id=$RUN_ID"
  echo "report=$REPORT"
  echo "evidence=$EVIDENCE"
  echo "exit_code=$status"
  exit "$status"
}
trap cleanup EXIT
{
  echo "run_id=$RUN_ID"
  echo "project=$PROJECT"
  echo "started_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "project_root=$ROOT"
} > "$REPORT/summary.txt"
run_phase() {
  name="$1"
  shift
  echo "## $name" | tee -a "$REPORT/verify.log"
  printf 'command=' | tee -a "$REPORT/verify.log"
  printf '%q ' "$@" | tee -a "$REPORT/verify.log"
  echo | tee -a "$REPORT/verify.log"
  set +e
  "$@" 2>&1 | tee -a "$REPORT/verify.log"
  code="${PIPESTATUS[0]}"
  set -e
  echo "exit_code=$code" | tee -a "$REPORT/verify.log"
  printf '%s_exit_code=%s\n' "$name" "$code" >> "$REPORT/summary.txt"
  return "$code"
}
run_phase 'docker info' docker info
run_phase 'docker version' docker version
run_phase 'docker compose version' docker compose version
run_phase 'compose config --quiet' "${COMPOSE[@]}" config --quiet
run_phase 'build application image' docker build --pull --target runtime -t "$APP_IMAGE" .
run_phase 'build test-runner image' docker build --pull --target test-runner -t "$TEST_IMAGE" .
TOUCHED=1
run_phase 'start isolated stack' "${COMPOSE[@]}" up -d app
deadline=$((SECONDS + 300))
healthy=0
while (( SECONDS < deadline )); do
  app_id=$("${COMPOSE[@]}" ps -q app)
  if [[ -n "$app_id" ]]; then
    health="$(docker inspect "$app_id" --format '{{.State.Health.Status}}')"
    [[ "$health" == healthy ]] && { healthy=1; break; }
    [[ "$health" == unhealthy ]] && break
  fi
  sleep 2
done
if (( ! healthy )); then echo "service readiness timed out" | tee -a "$REPORT/verify.log"; exit 1; fi
echo "stack_health=PASS" >> "$REPORT/summary.txt"
set +e
"${COMPOSE[@]}" run -d --no-deps -e TEST_REPORT=/reports/outage.json -e TEST_CONTROL_DIR=/reports test-runner go test -tags=integration -run '^TestBrokerOutageOutboxRecoveryAndExpiry$' -count=1 -v ./tests/integration > "$REPORT/outage-runner-id.txt" 2>> "$REPORT/verify.log"
code=$?
set -e
printf 'broker_outage_runner_start_exit_code=%s\n' "$code" | tee -a "$REPORT/verify.log" "$REPORT/summary.txt"
(( code == 0 )) || exit "$code"
OUTAGE_RUNNER_ID="$(tr -d '\r\n' < "$REPORT/outage-runner-id.txt")"
[[ -n "$OUTAGE_RUNNER_ID" ]] || { echo "Docker returned no outage test container id" | tee -a "$REPORT/verify.log"; exit 1; }
wait_for_file() {
  local path="$1" timeout_seconds="$2" deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    [[ -f "$path" ]] && return 0
    sleep 0.2
  done
  return 1
}
wait_for_file "$REPORT/broker-outage-armed" 120 || { echo "outage test did not arm" | tee -a "$REPORT/verify.log"; exit 1; }
run_phase 'stop broker for outage test' "${COMPOSE[@]}" stop broker
touch "$REPORT/broker-outage-stopped"
wait_for_file "$REPORT/broker-outage-queued" 120 || { echo "outage test did not confirm pending outbox" | tee -a "$REPORT/verify.log"; exit 1; }
run_phase 'restart broker after outage' "${COMPOSE[@]}" start broker
deadline=$((SECONDS + 180))
broker_healthy=0
while (( SECONDS < deadline )); do
  broker_id=$("${COMPOSE[@]}" ps -q broker)
  if [[ -n "$broker_id" ]]; then
    health=$(docker inspect "$broker_id" --format '{{.State.Health.Status}}')
    [[ "$health" == healthy ]] && { broker_healthy=1; break; }
    [[ "$health" == unhealthy ]] && break
  fi
  sleep 2
done
(( broker_healthy )) || { echo "broker failed health check after restart" | tee -a "$REPORT/verify.log"; exit 1; }
touch "$REPORT/broker-outage-resume"
deadline=$((SECONDS + 240))
runner_finished=0
while (( SECONDS < deadline )); do
  runner_running=$(docker inspect "$OUTAGE_RUNNER_ID" --format '{{.State.Running}}')
  [[ "$runner_running" == false ]] && { runner_finished=1; break; }
  sleep 1
done
(( runner_finished )) || { echo "outage test runner did not finish" | tee -a "$REPORT/verify.log"; exit 1; }
docker logs "$OUTAGE_RUNNER_ID" > "$REPORT/outage-runner.log" 2>&1
cat "$REPORT/outage-runner.log" | tee -a "$REPORT/verify.log"
runner_code=$(docker inspect "$OUTAGE_RUNNER_ID" --format '{{.State.ExitCode}}')
printf 'broker_outage_integration_exit_code=%s\n' "$runner_code" | tee -a "$REPORT/summary.txt"
docker rm "$OUTAGE_RUNNER_ID" >> "$REPORT/verify.log" 2>&1
OUTAGE_RUNNER_ID=""
[[ "$runner_code" == 0 ]] || exit "$runner_code"

set +e
"${COMPOSE[@]}" run -d --no-deps -e TEST_REPORT=/reports/sigterm.json -e TEST_CONTROL_DIR=/reports test-runner go test -tags=integration -run '^TestSIGTERMInterruptsInFlightDelivery$' -count=1 -v ./tests/integration > "$REPORT/sigterm-runner-id.txt" 2>> "$REPORT/verify.log"
code=$?
set -e
printf 'sigterm_runner_start_exit_code=%s\n' "$code" | tee -a "$REPORT/verify.log" "$REPORT/summary.txt"
(( code == 0 )) || exit "$code"
OUTAGE_RUNNER_ID="$(tr -d '\r\n' < "$REPORT/sigterm-runner-id.txt")"
[[ -n "$OUTAGE_RUNNER_ID" ]] || { echo "Docker returned no SIGTERM test container id" | tee -a "$REPORT/verify.log"; exit 1; }
wait_for_file "$REPORT/sigterm-armed" 120 || { echo "SIGTERM test did not observe an in-flight request" | tee -a "$REPORT/verify.log"; exit 1; }
sigterm_app_id=$("${COMPOSE[@]}" ps -q app)
[[ -n "$sigterm_app_id" ]] || { echo "could not identify app container before SIGTERM" | tee -a "$REPORT/verify.log"; exit 1; }
run_phase 'stop app with SIGTERM during HTTP request' "${COMPOSE[@]}" stop -t 20 app
app_stop_state=$(docker inspect "$sigterm_app_id" --format '{{.State.Status}}|{{.State.ExitCode}}')
[[ "$app_stop_state" == 'exited|0' ]] || { echo "app did not exit cleanly after SIGTERM: $app_stop_state" | tee -a "$REPORT/verify.log"; exit 1; }
if "${COMPOSE[@]}" logs --no-color app > "$REPORT/sigterm-app.log" 2>&1; then :; else code=$?; echo "SIGTERM app log collection exit_code=$code" | tee -a "$REPORT/verify.log"; exit "$code"; fi
grep -q 'shutdown signal received' "$REPORT/sigterm-app.log" || { echo "app logs lack shutdown signal marker" | tee -a "$REPORT/verify.log"; exit 1; }
grep -q 'server stopped' "$REPORT/sigterm-app.log" || { echo "app logs lack graceful stop marker" | tee -a "$REPORT/verify.log"; exit 1; }
touch "$REPORT/sigterm-stopped"
run_phase 'restart app after SIGTERM test' "${COMPOSE[@]}" start app
deadline=$((SECONDS + 180))
app_healthy=0
while (( SECONDS < deadline )); do
  health=$(docker inspect "$sigterm_app_id" --format '{{.State.Health.Status}}')
  [[ "$health" == healthy ]] && { app_healthy=1; break; }
  [[ "$health" == unhealthy ]] && break
  sleep 2
done
(( app_healthy )) || { echo "app failed health check after SIGTERM restart" | tee -a "$REPORT/verify.log"; exit 1; }
touch "$REPORT/sigterm-resume"
deadline=$((SECONDS + 240))
runner_finished=0
while (( SECONDS < deadline )); do
  runner_running=$(docker inspect "$OUTAGE_RUNNER_ID" --format '{{.State.Running}}')
  [[ "$runner_running" == false ]] && { runner_finished=1; break; }
  sleep 1
done
(( runner_finished )) || { echo "SIGTERM test runner did not finish" | tee -a "$REPORT/verify.log"; exit 1; }
docker logs "$OUTAGE_RUNNER_ID" > "$REPORT/sigterm-runner.log" 2>&1
cat "$REPORT/sigterm-runner.log" | tee -a "$REPORT/verify.log"
runner_code=$(docker inspect "$OUTAGE_RUNNER_ID" --format '{{.State.ExitCode}}')
printf 'sigterm_integration_exit_code=%s\n' "$runner_code" | tee -a "$REPORT/summary.txt"
docker rm "$OUTAGE_RUNNER_ID" >> "$REPORT/verify.log" 2>&1
OUTAGE_RUNNER_ID=""
[[ "$runner_code" == 0 ]] || exit "$runner_code"

run_phase 'Docker test-runner full verification' "${COMPOSE[@]}" run --rm test-runner
