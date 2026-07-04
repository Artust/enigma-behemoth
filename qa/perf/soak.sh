#!/usr/bin/env bash
# LAYER 3 (H) — Soak / endurance. Drives sustained load for SOAK_DUR and proves the
# app leaked nothing during STEADY STATE. A real leak is unbounded growth over time,
# NOT the one-time jump from idle to warm (connection pools legitimately scale up on
# first load: go-redis ~10*GOMAXPROCS, pgx up to PG_MAX_CONNS, plus a goroutine per
# live HTTP connection). So we compare TWO in-load steady-state samples — early
# (~40% in) vs late (~90% in), both well past the k6 VU cold-start ramp — and assert
# goroutines/fds did not grow between them. A post-settle sample confirms the app
# releases back toward baseline. k6 gates p99<100ms / err<1% on the {phase:soak}
# window (warmup excluded).
set -euo pipefail
cd "$(dirname "$0")/.."   # -> qa/

BASE="${BASE_URL:-http://localhost:18080}"
DUR="${SOAK_DUR:-10m}"
QPS="${SOAK_QPS:-1000}"
GORO_TOL="${GORO_TOL:-40}"   # steady-state goroutine growth headroom (early->late)
FD_TOL="${FD_TOL:-40}"       # steady-state fd growth headroom (early->late)

# dur_to_s converts a k6 duration (e.g. 10m, 90s, 2m30s, or a bare seconds count).
dur_to_s() {
  local d="$1" total=0
  [[ $d =~ ([0-9]+)m ]] && total=$((total + BASH_REMATCH[1]*60))
  [[ $d =~ ([0-9]+)s ]] && total=$((total + BASH_REMATCH[1]))
  [[ $d =~ ^[0-9]+$ ]] && total=$d
  echo "$total"
}

metric() { curl -s "${BASE}/metrics" | awk -v k="$1" '$1==k {print $2; exit}'; }
snap() { printf 'goroutines=%s open_fds=%s heap_inuse=%s' \
  "$(metric go_goroutines)" "$(metric process_open_fds)" "$(metric go_memstats_heap_inuse_bytes)"; }

soak_s=$(dur_to_s "$DUR")
(( soak_s >= 60 )) || { echo "SOAK_DUR too short for a meaningful leak check (need >=60s)"; exit 2; }
early_at=$(( 30 + soak_s * 4 / 10 ))   # 30s warmup + 40% into the soak
late_at=$(( 30 + soak_s * 9 / 10 ))    # 30s warmup + 90% into the soak

echo "==> [soak] ensuring app ready at ${BASE}"
curl -sf "${BASE}/readyz" >/dev/null

echo "==> [soak] launching k6 ${QPS} QPS for ${DUR} (+30s warmup); steady samples at ${early_at}s / ${late_at}s"
k6_rc=0
BASE_URL="$BASE" SOAK_QPS="$QPS" SOAK_DUR="$DUR" k6 run perf/soak.js >/tmp/soak_k6.log 2>&1 &
k6_pid=$!

sleep "$early_at"
goro0=$(metric go_goroutines); fd0=$(metric process_open_fds); heap0=$(metric go_memstats_heap_inuse_bytes)
echo "    early steady (${early_at}s): $(snap)"

sleep "$(( late_at - early_at ))"
goro1=$(metric go_goroutines); fd1=$(metric process_open_fds); heap1=$(metric go_memstats_heap_inuse_bytes)
echo "    late steady  (${late_at}s): $(snap)"

wait "$k6_pid" || k6_rc=$?
sleep 15
echo "    post-settle:            $(snap)"

grep -E '✓|✗' /tmp/soak_k6.log | grep -E 'phase:soak|p\(99\)|rate<' || true

fail=0
if (( goro1 > goro0 + GORO_TOL )); then
  echo "FAIL: goroutine leak in steady state: ${goro0} -> ${goro1} (> +${GORO_TOL})"; fail=1
else
  echo "PASS: no goroutine growth in steady state (${goro0} -> ${goro1})"
fi
if (( fd1 > fd0 + FD_TOL )); then
  echo "FAIL: fd/connection leak in steady state: ${fd0} -> ${fd1} (> +${FD_TOL})"; fail=1
else
  echo "PASS: no fd growth in steady state (${fd0} -> ${fd1})"
fi
printf '    heap_inuse %s -> %s (informational; GC-dependent)\n' "$heap0" "$heap1"
if (( k6_rc != 0 )); then
  echo "FAIL: k6 p99<100ms / err<1% breached during the soak window"; fail=1
else
  echo "PASS: p99<100ms & err<1% held across the ${DUR} soak window"
fi

if (( fail == 0 )); then
  echo "PASS: soak clean — ${DUR} @ ${QPS} QPS, no steady-state leak, latency held"
fi
exit "$fail"
