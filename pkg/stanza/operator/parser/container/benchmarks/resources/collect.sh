#!/usr/bin/env bash
# collect.sh — collect a 30-second CPU profile and report log throughput.
# Usage: ./collect.sh <output.pprof>
#
# Prerequisites:
#   kubectl port-forward must NOT already be running on ports 1777/8888.
#   The otelcol DaemonSet must be running with the updated otel-daemonset.yaml.

set -euo pipefail

OUTPUT="${1:-cpu.pprof}"
DURATION=30

echo "Starting port-forward..."
kubectl port-forward daemonset/otelcol 1777:1777 8888:8888 &
PF_PID=$!
trap "kill $PF_PID 2>/dev/null" EXIT

# Wait for ports to be ready
sleep 2

# Sample metrics before
METRIC_BEFORE=$(curl -sf "http://localhost:8888/metrics" \
  | grep '^otelcol_receiver_accepted_log_records{' \
  | awk '{print $2}' \
  | head -1)

if [[ -z "$METRIC_BEFORE" ]]; then
  echo "Warning: could not read log record metric — metrics endpoint may not be ready"
  METRIC_BEFORE=0
fi

echo "Collecting ${DURATION}s CPU profile → ${OUTPUT} ..."
curl -sf -o "$OUTPUT" "http://localhost:1777/debug/pprof/profile?seconds=${DURATION}"

# Sample metrics after
METRIC_AFTER=$(curl -sf "http://localhost:8888/metrics" \
  | grep '^otelcol_receiver_accepted_log_records{' \
  | awk '{print $2}' \
  | head -1)

if [[ -z "$METRIC_AFTER" ]]; then
  METRIC_AFTER=0
fi

INGESTED=$(python3 -c "print(int(${METRIC_AFTER} - ${METRIC_BEFORE}))")

echo ""
echo "Profile saved to: ${OUTPUT}"
echo "Log records ingested during ${DURATION}s: ${INGESTED}"
echo "Throughput: $(python3 -c "print(int(${INGESTED} / ${DURATION}))") records/sec"
echo ""
echo "To analyse:"
echo "  go tool pprof -http=:8080 ${OUTPUT}"
echo "  go tool pprof -top -diff_base=resources/regex_nocache_nomap.pprof ${OUTPUT}"