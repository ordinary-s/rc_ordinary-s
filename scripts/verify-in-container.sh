#!/usr/bin/env bash
set -Eeuo pipefail
REPORT_DIR=${REPORT_DIR:-/reports}
mkdir -p "$REPORT_DIR"
REPORT="$REPORT_DIR/checks.log"
SUMMARY="$REPORT_DIR/checks-summary.txt"
: > "$REPORT"
: > "$SUMMARY"
printf 'started_at_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$SUMMARY"

run_phase() {
  local name="$1"
  shift
  echo "== $name ==" | tee -a "$REPORT"
  set +e
  "$@" 2>&1 | tee -a "$REPORT"
  local code=${PIPESTATUS[0]}
  set -e
  printf '%s_exit_code=%s\n' "$name" "$code" | tee -a "$SUMMARY"
  if [ "$code" -ne 0 ]; then
    return "$code"
  fi
}

go_files=$(find . -type f -name '*.go' -not -path './.git/*' -not -path './.tmp_pdf_review/*' | sort)
format_diff=$(gofmt -d $go_files)
if [ -n "$format_diff" ]; then
  echo "$format_diff" | tee -a "$REPORT"
  printf 'gofmt_exit_code=1\n' | tee -a "$SUMMARY"
  exit 1
fi
echo "gofmt: no differences" | tee -a "$REPORT"
printf 'gofmt_exit_code=0\n' | tee -a "$SUMMARY"

run_phase go_vet go vet ./...
run_phase go_test go test -count=1 ./...
run_phase go_race go test -race -count=1 ./...
run_phase integration_compile go test -run '^$' -tags=integration ./tests/integration
run_phase real_integration go test -tags=integration -run '^TestEndToEndWithMySQLAndRocketMQ$' -count=1 -v ./tests/integration
printf 'finished_at_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$SUMMARY"
