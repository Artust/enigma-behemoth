#!/usr/bin/env bash
# BASELINE: what a naive "just UPDATE the one hot row per request" design costs,
# WITHOUT Redis + group-commit. Every pgbench transaction is a single
# `UPDATE bosses SET current_hp = current_hp - 1` on ONE row, auto-committed —
# i.e. one fsync per hit plus row-lock contention across clients.
#
# Shows the throughput COLLAPSE and p99 blow-up as concurrent writers pile onto
# the single row. Backs ARCHITECT.md §2 ("a single hot row ... risks breaking
# p99 < 100ms at 1000+ QPS"). Compare against the group-commit numbers from
# `make -C qa perf-damage` / qa/perf/load_curve.js.
#
#   ./qa/bench/pg_single_row.sh
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PG=(docker compose -f "${HERE}/../compose.qa.yaml" exec -T -e PGPASSWORD=behemoth postgres)

"${PG[@]}" sh -c 'cat > /tmp/single_row.sql <<SQL
UPDATE bosses SET current_hp = current_hp - 1 WHERE id = '"'"'boss-load'"'"';
SQL'

echo "durability: synchronous_commit=$("${PG[@]}" psql -U behemoth -d behemoth -tAc "SHOW synchronous_commit" | tr -d '[:space:]'), fsync=$("${PG[@]}" psql -U behemoth -d behemoth -tAc "SHOW fsync" | tr -d '[:space:]')"
echo "single hot-row UPDATE-per-txn (1 fsync/hit), 12s per client count:"
for c in 1 8 50 90; do
  "${PG[@]}" sh -c "rm -f /tmp/pgbench_log.* ; \
    pgbench -n -c $c -j $(( c>8?8:c )) -T 12 --log --log-prefix=/tmp/pgbench_log \
      -f /tmp/single_row.sql -h localhost -U behemoth -d behemoth 2>/dev/null | grep -E 'tps ' ; \
    cat /tmp/pgbench_log.* 2>/dev/null | awk '{print \$3}' | sort -n | awk \
      '{a[NR]=\$1} END{if(NR>0) printf \"  -> clients=$c  p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms\n\", \
       a[int(NR*0.5)]/1000, a[int(NR*0.95)]/1000, a[int(NR*0.99)]/1000, a[NR]/1000}'"
done
