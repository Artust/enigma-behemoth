#!/usr/bin/env bash
# Per-op latency of the atomic damage Lua script (GET + SET + ZINCRBY, the hot
# path). Runs redis-benchmark EVAL of the exact damageScript against throwaway
# bench:* keys inside the QA redis container, so it never mutates real bosses.
#
# Backs the "low-latency / sub-millisecond" claim in ARCHITECT.md §1.
#   ./qa/bench/redis_lua.sh
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RC=(docker compose -f "${HERE}/../compose.qa.yaml" exec -T redis)

# Exact damageScript from internal/store/redis.go (single-line form for eval).
LUA='local hp = redis.call("GET", KEYS[1]); if hp == false then return {-1,0,0} end; hp = tonumber(hp); local dmg = tonumber(ARGV[2]); if dmg <= 0 then return {-2,0,hp} end; if hp <= 0 then return {0,0,hp} end; local applied = dmg; if applied > hp then applied = hp end; local newhp = hp - applied; redis.call("SET", KEYS[1], newhp); redis.call("ZINCRBY", KEYS[2], applied, ARGV[1]); if newhp <= 0 then redis.call("SET", KEYS[3], "defeated") end; return {1, applied, newhp}'

for c in 1 50; do
  "${RC[@]}" redis-cli SET bench:hp 1000000000000 >/dev/null
  echo "=========== concurrency=${c} clients ==========="
  "${RC[@]}" redis-benchmark -n 200000 -c "$c" eval "$LUA" 3 \
    bench:hp bench:lb bench:state benchplayer 10 2>/dev/null \
    | grep -aA3 "throughput summary" | head -4
done
"${RC[@]}" redis-cli DEL bench:hp bench:lb bench:state >/dev/null
