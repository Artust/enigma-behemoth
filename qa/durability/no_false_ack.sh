#!/usr/bin/env bash
# NEGATIVE persistence-safety proof: an HTTP 200 must NEVER be issued before the
# hit is durable.
#
# Recreate the app with a long commit window (BATCH_MAX_WAIT=2s), send a single
# /damage that blocks waiting for its group-commit, then SIGKILL the app inside
# that window — before the batch commits. The client must NOT receive 200, and
# the damage must be ABSENT from Postgres after restart.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOSS="boss-durab"
PLAYER="noack-$(date +%s)-$$"
DMG=777

echo "==> [no_false_ack] recreating app with delayed commit (BATCH_MAX_WAIT=2s)"
QA_BATCH_MAX_WAIT=2s QA_BATCH_MAX_SIZE=1000000 "${COMPOSE[@]}" up -d app >/dev/null
wait_ready
seed_boss "$BOSS" 5000000

echo "==> sending ONE damage in the background (it will block until durable)"
CODE_FILE="$(mktemp)"
(
  curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE}/damage" \
    -H 'Content-Type: application/json' \
    -d "{\"player_id\":\"${PLAYER}\",\"boss_id\":\"${BOSS}\",\"damage_amount\":${DMG}}" \
    >"$CODE_FILE" 2>/dev/null || echo "000" >"$CODE_FILE"
) &
CURL_PID=$!

sleep 0.3   # comfortably inside the 2s commit window
echo "==> HARD CRASH before the commit window elapses"
"${COMPOSE[@]}" kill -s SIGKILL app >/dev/null
wait "$CURL_PID" 2>/dev/null || true
CODE=$(cat "$CODE_FILE"); rm -f "$CODE_FILE"
echo "    client saw HTTP code: ${CODE}"

echo "==> restarting app with default settings"
"${COMPOSE[@]}" up -d app >/dev/null
wait_ready

DURABLE=$(pg "SELECT COALESCE(SUM(damage_applied),0)
              FROM damage_events WHERE boss_id='${BOSS}' AND player_id='${PLAYER}';")
echo "    durable damage for ${PLAYER}: ${DURABLE}"

rc=0
if [ "${CODE}" = "200" ]; then
  echo "FAIL: client got 200 for a hit that never committed"
  rc=1
fi
if [ "${DURABLE}" != "0" ]; then
  echo "FAIL: un-acked damage leaked into Postgres (${DURABLE})"
  rc=1
fi
if [ "$rc" = "0" ]; then
  echo "PASS: no false ack — the in-flight hit was neither acked (code=${CODE}) nor persisted"
fi
exit "$rc"
