#!/usr/bin/env bash
# Assert the durability invariant for a boss:
#   max_hp - current_hp  ==  SUM(contributions.total_damage)
# i.e. HP loss is fully accounted for by durable per-player contributions.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

BOSS="${1:?usage: verify_invariant.sh <boss_id>}"

DERIVED=$(pg "SELECT max_hp - current_hp FROM bosses WHERE id='${BOSS}';")
AGG=$(pg "SELECT COALESCE(SUM(total_damage),0) FROM contributions WHERE boss_id='${BOSS}';")

echo "    invariant[${BOSS}]: max_hp-current_hp=${DERIVED}  sum(contrib)=${AGG}"
if [ "${DERIVED}" = "${AGG}" ]; then
  echo "PASS: durability invariant holds for ${BOSS}"
else
  echo "FAIL: invariant broken for ${BOSS} (${DERIVED} != ${AGG})"
  exit 1
fi
