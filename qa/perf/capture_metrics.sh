#!/usr/bin/env bash
# Snapshot the app's Prometheus metrics around a load run and print deltas.
#
#   ./capture_metrics.sh snapshot /tmp/before.txt
#   k6 run load_damage.js
#   ./capture_metrics.sh snapshot /tmp/after.txt
#   ./capture_metrics.sh delta /tmp/before.txt /tmp/after.txt
#
# Best-effort: label formats depend on the app's middleware; the request-counter
# keys below match behemoth_http_requests_total{route,method,status}.
set -euo pipefail

BASE="${BASE_URL:-http://localhost:18080}"
cmd="${1:-snapshot}"

case "$cmd" in
  snapshot)
    f="${2:?usage: capture_metrics.sh snapshot <file>}"
    curl -s "${BASE}/metrics" | grep -E '^behemoth_' >"$f"
    echo "saved $(wc -l <"$f" | tr -d ' ') metric lines -> $f"
    ;;
  delta)
    before="${2:?}"; after="${3:?}"
    printf '%-64s %12s %12s %12s\n' "metric" "before" "after" "delta"
    while IFS= read -r key; do
      b=$(grep -F -- "$key" "$before" | awk '{print $2}' | head -1); b=${b:-0}
      a=$(grep -F -- "$key" "$after"  | awk '{print $2}' | head -1); a=${a:-0}
      printf '%-64s %12s %12s %12s\n' "$key" "$b" "$a" "$(awk "BEGIN{printf \"%.0f\", $a-$b}")"
    done <<'KEYS'
behemoth_damage_applied_total
behemoth_http_requests_total{route="/damage",method="POST",status="200"}
behemoth_http_requests_total{route="/damage",method="POST",status="503"}
behemoth_http_requests_total{route="/damage",method="POST",status="409"}
KEYS
    echo
    echo "(duration histogram: inspect behemoth_http_request_duration_seconds_bucket in the snapshot files)"
    ;;
  *)
    echo "usage: capture_metrics.sh {snapshot <file>|delta <before> <after>}" >&2
    exit 2
    ;;
esac
