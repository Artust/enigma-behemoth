#!/usr/bin/env bash
# Graceful-restart durability: restart BOTH redis and app (wiping Redis working
# memory) and confirm HP, the leaderboard aggregate, and the durability
# invariant all survive — because they are rehydrated from Postgres.
#
# Broader than the original scripts/recovery_test.sh: it also checks the
# contribution total and the max_hp-current_hp == SUM(contrib) invariant.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOSS="boss-durab"

echo "==> [restart_graceful] ensuring QA app is ready"
wait_ready
seed_boss "$BOSS" 5000000

echo "==> dealing 50 hits across 5 players"
for i in $(seq 1 50); do
  curl -s -o /dev/null -X POST "${BASE}/damage" -H 'Content-Type: application/json' \
    -d "{\"player_id\":\"grp-$((i % 5))\",\"boss_id\":\"${BOSS}\",\"damage_amount\":100}"
done

HP_B=$(hp_of "$BOSS")
SUM_B=$(pg "SELECT COALESCE(SUM(total_damage),0) FROM contributions WHERE boss_id='${BOSS}';")
echo "    before: hp=${HP_B} contrib_sum=${SUM_B}"

echo "==> graceful restart: redis + app (Redis memory reloaded)"
"${COMPOSE[@]}" restart redis app >/dev/null
wait_ready

HP_A=$(hp_of "$BOSS")
SUM_A=$(pg "SELECT COALESCE(SUM(total_damage),0) FROM contributions WHERE boss_id='${BOSS}';")
echo "    after:  hp=${HP_A} contrib_sum=${SUM_A}"

rc=0
[ "${HP_B}" = "${HP_A}" ]   || { echo "FAIL: HP changed ${HP_B} -> ${HP_A}"; rc=1; }
[ "${SUM_B}" = "${SUM_A}" ] || { echo "FAIL: contributions changed ${SUM_B} -> ${SUM_A}"; rc=1; }
"$(dirname "$0")/verify_invariant.sh" "$BOSS" || rc=1

if [ "$rc" = "0" ]; then
  echo "PASS: HP + contributions + invariant preserved across graceful restart"
fi
exit "$rc"
