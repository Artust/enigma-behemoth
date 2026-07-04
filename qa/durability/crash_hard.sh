#!/usr/bin/env bash
# PRIMARY persistence-safety proof.
#
# Deal damage, record the total the SERVER acknowledged with HTTP 200, then
# HARD-CRASH the app (SIGKILL — no graceful flush). After restart, every acked
# hit must still be durable in Postgres, and the cache must rehydrate to match
# the source of truth. Uses a unique player id so re-runs never interfere.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOSS="boss-durab"
PLAYER="crash-$(date +%s)-$$"
HITS="${HITS:-200}"
DMG="${DMG:-100}"

echo "==> [crash_hard] ensuring QA app is ready"
wait_ready
seed_boss "$BOSS" 5000000

echo "==> dealing ${HITS} hits as ${PLAYER} (dmg=${DMG} each)"
ACKED=$(deal_hits "$BOSS" "$PLAYER" "$HITS" "$DMG")
echo "    acked_applied (server returned 200) = ${ACKED}"

echo "==> HARD CRASH: docker compose kill -s SIGKILL app"
"${COMPOSE[@]}" kill -s SIGKILL app >/dev/null

echo "==> restarting app"
"${COMPOSE[@]}" start app >/dev/null
wait_ready

DURABLE=$(pg "SELECT COALESCE(SUM(damage_applied),0)
              FROM damage_events WHERE boss_id='${BOSS}' AND player_id='${PLAYER}';")
echo "    durable_applied (postgres)           = ${DURABLE}"

rc=0
if [ "${DURABLE}" -ge "${ACKED}" ]; then
  echo "PASS: every acked hit survived SIGKILL (durable=${DURABLE} >= acked=${ACKED})"
else
  echo "FAIL: DATA LOSS — durable=${DURABLE} < acked=${ACKED}"
  rc=1
fi

# The cache is rebuildable: after restart it must derive back to the truth.
PG_HP=$(pg "SELECT current_hp FROM bosses WHERE id='${BOSS}';")
API_HP=$(hp_of "$BOSS")
echo "    postgres current_hp=${PG_HP}  api hp=${API_HP}"
if [ "${PG_HP}" = "${API_HP}" ]; then
  echo "PASS: Redis rehydrated to the source of truth after restart"
else
  echo "FAIL: cache/source mismatch (pg=${PG_HP} api=${API_HP})"
  rc=1
fi

exit "$rc"
