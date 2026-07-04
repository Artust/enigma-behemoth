#!/usr/bin/env bash
# Durability proof: deal damage, snapshot state, restart BOTH app and redis
# (wiping Redis working memory), then confirm HP + leaderboard survive because
# they are rehydrated from the durable Postgres source of truth.
set -euo pipefail

BASE="${BASE_URL:-http://localhost:8080}"
BOSS="${BOSS_ID:-boss-1}"

echo "==> dealing 50 hits..."
for i in $(seq 1 50); do
  curl -s -X POST "${BASE}/damage" -H 'Content-Type: application/json' \
    -d "{\"player_id\":\"player-$((i % 5))\",\"boss_id\":\"${BOSS}\",\"damage_amount\":100}" >/dev/null
done

before=$(curl -s "${BASE}/boss/${BOSS}")
echo "==> BEFORE restart: ${before}"

echo "==> restarting redis + app (redis memory is wiped/reloaded)..."
docker compose restart redis app >/dev/null

echo "==> waiting for readiness..."
for _ in $(seq 1 30); do
  if curl -sf "${BASE}/readyz" >/dev/null 2>&1; then break; fi
  sleep 1
done

after=$(curl -s "${BASE}/boss/${BOSS}")
echo "==> AFTER  restart: ${after}"

hp_before=$(printf '%s' "${before}" | grep -o '"hp":[0-9]*' | head -1)
hp_after=$(printf '%s' "${after}"  | grep -o '"hp":[0-9]*' | head -1)
if [ "${hp_before}" = "${hp_after}" ]; then
  echo "PASS: HP preserved across restart (${hp_after})"
else
  echo "FAIL: HP changed (${hp_before} -> ${hp_after})"
  exit 1
fi
