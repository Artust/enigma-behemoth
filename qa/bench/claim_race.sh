#!/usr/bin/env bash
# Exactly-once "Claim Reward" under a concurrent race. Creates a throwaway boss,
# lets ONE player land the killing blow, then fires N concurrent claims for the
# same (boss, player). Asserts: exactly ONE response has already_claimed=false,
# all N reward payloads are byte-identical, and Postgres holds exactly ONE
# reward_claims row. Backs ARCHITECT.md §3 (exactly-once claim).
#
#   N=50 ./qa/bench/claim_race.sh
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PG=(docker compose -f "${HERE}/../compose.qa.yaml" exec -T postgres)
BASE="${BASE_URL:-http://localhost:18080}"
N="${N:-50}"
TS=$(date +%s%N); BOSS="qa-claimrace-$TS"; P="p-$TS"

"${PG[@]}" psql -U behemoth -d behemoth -tAc \
  "INSERT INTO bosses (id,name,max_hp,current_hp,state) VALUES ('$BOSS','$BOSS',1000,1000,'alive');" >/dev/null
for i in $(seq 1 10); do
  curl -s -o /dev/null -X POST "$BASE/damage" -H 'Content-Type: application/json' \
    -d "{\"player_id\":\"$P\",\"boss_id\":\"$BOSS\",\"damage_amount\":100}"
done
echo "boss state after killing blow: $("${PG[@]}" psql -U behemoth -d behemoth -tAc "SELECT state FROM bosses WHERE id='$BOSS'" | tr -d '[:space:]')"

echo "firing $N concurrent claims for the same (boss,player)..."
tmp=$(mktemp -d)
for i in $(seq 1 "$N"); do
  curl -s -X POST "$BASE/rewards/claim" -H 'Content-Type: application/json' \
    -d "{\"boss_id\":\"$BOSS\",\"player_id\":\"$P\"}" > "$tmp/$i.json" &
done
wait
fresh=$(grep -l '"already_claimed":false' "$tmp"/*.json 2>/dev/null | wc -l | tr -d ' ')
dup=$(grep -l '"already_claimed":true' "$tmp"/*.json 2>/dev/null | wc -l | tr -d ' ')
distinct=$(grep -o '"reward":[^}]*}' "$tmp"/*.json | sed 's/^[^:]*://' | sort -u | wc -l | tr -d ' ')
rows=$("${PG[@]}" psql -U behemoth -d behemoth -tAc "SELECT count(*) FROM reward_claims WHERE boss_id='$BOSS' AND player_id='$P'" | tr -d '[:space:]')
echo "fresh=$fresh dup=$dup distinct_reward_payloads=$distinct reward_claims_rows=$rows"
rm -rf "$tmp"
[ "$fresh" = "1" ] && [ "$distinct" = "1" ] && [ "$rows" = "1" ] \
  && echo "PASS: exactly-once (1 grant, $dup idempotent replays, identical reward)" \
  || { echo "FAIL: exactly-once violated"; exit 1; }
