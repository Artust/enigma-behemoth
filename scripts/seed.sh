#!/usr/bin/env bash
# Seed (or upsert) a boss directly into Postgres. A newly seeded boss is picked
# up lazily by the service on first access (cache miss -> rehydrate).
set -euo pipefail

BOSS_ID="${1:-boss-1}"
NAME="${2:-Behemoth}"
HP="${3:-10000000}"

docker compose exec -T postgres psql -U behemoth -d behemoth -c \
  "INSERT INTO bosses (id, name, max_hp, current_hp, state)
   VALUES ('${BOSS_ID}', '${NAME}', ${HP}, ${HP}, 'alive')
   ON CONFLICT (id) DO NOTHING;"

echo "seeded boss id=${BOSS_ID} name=${NAME} hp=${HP}"
