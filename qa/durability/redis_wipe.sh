#!/usr/bin/env bash
# Redis-only failure: wipe the hot cache while the app keeps running, then prove
# the next read lazily rehydrates from Postgres (service.go applyWithRehydrate /
# Get). The cache is rebuildable; the source of truth is untouched.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOSS="boss-durab"

echo "==> [redis_wipe] ensuring QA app is ready"
wait_ready
seed_boss "$BOSS" 5000000

# Ensure the boss is loaded, then apply some damage.
curl -s -o /dev/null "${BASE}/boss/${BOSS}"
for i in $(seq 1 10); do
  curl -s -o /dev/null -X POST "${BASE}/damage" -H 'Content-Type: application/json' \
    -d "{\"player_id\":\"wipe-p\",\"boss_id\":\"${BOSS}\",\"damage_amount\":100}"
done

PG_HP=$(pg "SELECT current_hp FROM bosses WHERE id='${BOSS}';")
echo "==> FLUSHALL Redis (wipe hot cache; app stays up)"
redis_cli FLUSHALL >/dev/null

# The next read must find an empty cache and lazily rehydrate from Postgres.
API_HP=$(hp_of "$BOSS")
echo "    postgres current_hp=${PG_HP}  api hp after wipe=${API_HP}"

if [ "${PG_HP}" = "${API_HP}" ]; then
  echo "PASS: lazy rehydrate restored the cache from the source of truth after a full Redis wipe"
else
  echo "FAIL: after wipe api_hp=${API_HP} != pg_hp=${PG_HP}"
  exit 1
fi
