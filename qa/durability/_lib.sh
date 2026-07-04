#!/usr/bin/env bash
# Shared helpers for the QA durability (persistence-safety) scripts.
# Source this file — do not execute it directly.
#
# All commands target the ISOLATED behemoth-qa stack (compose.qa.yaml), so these
# tests never kill or restart a stack another session may be running.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE=(docker compose -f "${HERE}/../compose.qa.yaml")
BASE="${BASE_URL:-http://localhost:18080}"

# pg <sql> — run a scalar query, echo the single value.
pg() {
  "${COMPOSE[@]}" exec -T postgres psql -U behemoth -d behemoth -tAc "$1" | tr -d '[:space:]'
}

# redis_cli <args...> — run redis-cli inside the QA redis container.
redis_cli() {
  "${COMPOSE[@]}" exec -T redis redis-cli "$@"
}

# seed_boss <id> <hp> [state] — upsert a boss (no-op if it already exists).
seed_boss() {
  local id="$1" hp="$2" state="${3:-alive}"
  pg "INSERT INTO bosses (id,name,max_hp,current_hp,state)
      VALUES ('${id}','${id}',${hp},${hp},'${state}')
      ON CONFLICT (id) DO NOTHING;" >/dev/null
}

# wait_ready — block until the app reports /readyz OK.
wait_ready() {
  local i
  for i in $(seq 1 60); do
    if curl -sf "${BASE}/readyz" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "FAIL: app not ready at ${BASE}/readyz" >&2
  return 1
}

# hp_of <boss> — read HP from GET /boss/{id}.
hp_of() {
  curl -s "${BASE}/boss/$1" | grep -o '"hp":[0-9]*' | head -1 | grep -o '[0-9]*'
}

# deal_hits <boss> <player> <count> <dmg> — send N hits, echo the total
# damage_applied across only the responses that returned HTTP 200 (i.e. the
# durably-acked damage).
deal_hits() {
  local boss="$1" player="$2" count="$3" dmg="$4"
  local acked=0 i resp code body applied
  for ((i = 0; i < count; i++)); do
    resp=$(curl -s -o - -w $'\n%{http_code}' -X POST "${BASE}/damage" \
      -H 'Content-Type: application/json' \
      -d "{\"player_id\":\"${player}\",\"boss_id\":\"${boss}\",\"damage_amount\":${dmg}}") || true
    code=$(printf '%s' "$resp" | tail -n1)
    body=$(printf '%s' "$resp" | sed '$d')
    if [ "$code" = "200" ]; then
      applied=$(printf '%s' "$body" | grep -o '"damage_applied":[0-9]*' | grep -o '[0-9]*' || true)
      acked=$((acked + ${applied:-0}))
    fi
  done
  echo "$acked"
}
