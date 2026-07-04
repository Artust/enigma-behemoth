-- Behemoth — World Boss Event Service schema.
-- Postgres is the source of truth. Redis is a rebuildable hot-path cache.
-- This file runs once on a fresh volume via /docker-entrypoint-initdb.d.

CREATE TABLE IF NOT EXISTS bosses (
    id          TEXT PRIMARY KEY,
    name        TEXT        NOT NULL,
    max_hp      BIGINT      NOT NULL CHECK (max_hp > 0),
    current_hp  BIGINT      NOT NULL CHECK (current_hp >= 0),
    state       TEXT        NOT NULL DEFAULT 'alive' CHECK (state IN ('alive', 'defeated')),
    defeated_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Append-only audit log of every applied damage. Durable history of contributions.
CREATE TABLE IF NOT EXISTS damage_events (
    id             BIGSERIAL PRIMARY KEY,
    boss_id        TEXT   NOT NULL REFERENCES bosses(id),
    player_id      TEXT   NOT NULL,
    damage_applied BIGINT NOT NULL CHECK (damage_applied >= 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_damage_events_boss ON damage_events (boss_id);

-- Materialized per-player aggregate. Used for recovery (rehydrate Redis) and
-- reward-tier calculation. Kept consistent with damage_events in the same txn.
CREATE TABLE IF NOT EXISTS contributions (
    boss_id      TEXT   NOT NULL REFERENCES bosses(id),
    player_id    TEXT   NOT NULL,
    total_damage BIGINT NOT NULL DEFAULT 0 CHECK (total_damage >= 0),
    PRIMARY KEY (boss_id, player_id)
);

-- Exactly-once reward claims. PK (boss_id, player_id) makes a claim idempotent.
-- reward_payload materializes what the player is owed (outbox-style) so the
-- "grant" is a read, keeping the claim atomic.
CREATE TABLE IF NOT EXISTS reward_claims (
    boss_id        TEXT        NOT NULL REFERENCES bosses(id),
    player_id      TEXT        NOT NULL,
    tier           TEXT        NOT NULL,
    damage_pct     DOUBLE PRECISION NOT NULL,
    reward_payload JSONB       NOT NULL,
    claimed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (boss_id, player_id)
);

-- Seed a demo boss so `docker compose up` is immediately usable.
INSERT INTO bosses (id, name, max_hp, current_hp, state)
VALUES ('boss-1', 'Behemoth', 10000000, 10000000, 'alive')
ON CONFLICT (id) DO NOTHING;
