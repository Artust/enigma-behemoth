package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the durable source of truth.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore opens a connection pool and verifies connectivity.
func NewPostgresStore(ctx context.Context, dsn string, maxConns int32) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

// Close drains and closes the pool.
func (p *PostgresStore) Close() { p.pool.Close() }

// Ping checks liveness.
func (p *PostgresStore) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// ListBossIDs returns every boss id for startup rehydration.
func (p *PostgresStore) ListBossIDs(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT id FROM bosses`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RecoveryState returns the durable state used to rehydrate Redis. Current HP is
// derived (max_hp - SUM contributions), never read from a possibly-stale cache.
func (p *PostgresStore) RecoveryState(ctx context.Context, bossID string) (RecoveryState, bool, error) {
	var rs RecoveryState
	err := p.pool.QueryRow(ctx, `
		SELECT b.max_hp,
		       b.state,
		       GREATEST(0, b.max_hp - COALESCE(SUM(c.total_damage), 0))
		FROM bosses b
		LEFT JOIN contributions c ON c.boss_id = b.id
		WHERE b.id = $1
		GROUP BY b.id`, bossID).Scan(&rs.MaxHP, &rs.State, &rs.CurrentHP)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryState{}, false, nil
	}
	if err != nil {
		return RecoveryState{}, false, err
	}
	return rs, true, nil
}

// Contributions returns every player's total for a boss (for ZSET rebuild).
func (p *PostgresStore) Contributions(ctx context.Context, bossID string) ([]LeaderEntry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT player_id, total_damage FROM contributions WHERE boss_id = $1`, bossID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeaderEntry
	for rows.Next() {
		var e LeaderEntry
		if err := rows.Scan(&e.PlayerID, &e.Damage); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CommitBatch persists a batch of applied-damage events in one transaction:
// append the audit log, upsert per-player aggregates, and decrement boss HP.
func (p *PostgresStore) CommitBatch(ctx context.Context, events []DamageEvent) error {
	if len(events) == 0 {
		return nil
	}

	// Aggregate per key to cut row touches.
	type key struct{ boss, player string }
	perPlayer := make(map[key]int64)
	perBoss := make(map[string]int64)
	for _, e := range events {
		perPlayer[key{e.BossID, e.PlayerID}] += e.Applied
		perBoss[e.BossID] += e.Applied
	}

	// Stable lock order across committers to avoid deadlocks (map order is random).
	playerKeys := make([]key, 0, len(perPlayer))
	for k := range perPlayer {
		playerKeys = append(playerKeys, k)
	}
	sort.Slice(playerKeys, func(i, j int) bool {
		if playerKeys[i].boss != playerKeys[j].boss {
			return playerKeys[i].boss < playerKeys[j].boss
		}
		return playerKeys[i].player < playerKeys[j].player
	})
	bossKeys := make([]string, 0, len(perBoss))
	for b := range perBoss {
		bossKeys = append(bossKeys, b)
	}
	sort.Strings(bossKeys)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin batch tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Audit log via COPY.
	rows := make([][]any, 0, len(events))
	for _, e := range events {
		rows = append(rows, []any{e.BossID, e.PlayerID, e.Applied})
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"damage_events"},
		[]string{"boss_id", "player_id", "damage_applied"},
		pgx.CopyFromRows(rows),
	); err != nil {
		return fmt.Errorf("copy damage_events: %w", err)
	}

	// Aggregates + HP/state in one pipelined batch.
	b := &pgx.Batch{}
	for _, k := range playerKeys {
		b.Queue(`
			INSERT INTO contributions (boss_id, player_id, total_damage)
			VALUES ($1, $2, $3)
			ON CONFLICT (boss_id, player_id)
			DO UPDATE SET total_damage = contributions.total_damage + EXCLUDED.total_damage`,
			k.boss, k.player, perPlayer[k])
	}
	for _, boss := range bossKeys {
		b.Queue(`
			UPDATE bosses
			SET current_hp  = GREATEST(0, current_hp - $2),
			    state       = CASE WHEN current_hp - $2 <= 0 THEN 'defeated' ELSE state END,
			    defeated_at = CASE WHEN current_hp - $2 <= 0 AND defeated_at IS NULL THEN now() ELSE defeated_at END,
			    updated_at  = now()
			WHERE id = $1`,
			boss, perBoss[boss])
	}
	br := tx.SendBatch(ctx, b)
	for range b.Len() {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("batch exec: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("batch close: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	return nil
}

// ClaimBasis reads the durable data needed to authorize/price a reward claim.
// Gates on Postgres, never Redis, so a claim needs a durably committed kill.
func (p *PostgresStore) ClaimBasis(ctx context.Context, bossID, playerID string) (ClaimBasis, error) {
	var cb ClaimBasis
	err := p.pool.QueryRow(ctx,
		`SELECT max_hp, state FROM bosses WHERE id = $1`, bossID).Scan(&cb.MaxHP, &cb.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return cb, nil
	}
	if err != nil {
		return cb, err
	}
	cb.Exists = true

	err = p.pool.QueryRow(ctx,
		`SELECT total_damage FROM contributions WHERE boss_id = $1 AND player_id = $2`,
		bossID, playerID).Scan(&cb.TotalDamage)
	if errors.Is(err, pgx.ErrNoRows) {
		return cb, nil
	}
	if err != nil {
		return cb, err
	}
	cb.HasContribution = true
	return cb, nil
}

// SaveClaim persists a claim exactly once: first call inserts, duplicates return
// the existing record with AlreadyClaimed=true (enforced by the PK).
func (p *PostgresStore) SaveClaim(ctx context.Context, in ClaimInput) (ClaimResult, error) {
	res := ClaimResult{BossID: in.BossID, PlayerID: in.PlayerID}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	var (
		tier    string
		pct     float64
		payload []byte
		claimed = false
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO reward_claims (boss_id, player_id, tier, damage_pct, reward_payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (boss_id, player_id) DO NOTHING
		RETURNING tier, damage_pct, reward_payload, claimed_at`,
		in.BossID, in.PlayerID, in.Tier, in.Pct, in.Payload,
	).Scan(&tier, &pct, &payload, &res.ClaimedAt)

	switch {
	case err == nil:
		claimed = true
	case errors.Is(err, pgx.ErrNoRows):
		// Already claimed: read it back for an idempotent reply.
		if err := tx.QueryRow(ctx, `
			SELECT tier, damage_pct, reward_payload, claimed_at
			FROM reward_claims WHERE boss_id = $1 AND player_id = $2`,
			in.BossID, in.PlayerID,
		).Scan(&tier, &pct, &payload, &res.ClaimedAt); err != nil {
			return res, fmt.Errorf("read existing claim: %w", err)
		}
	default:
		return res, fmt.Errorf("insert claim: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit claim: %w", err)
	}

	res.Tier = tier
	res.Pct = pct
	res.AlreadyClaimed = !claimed
	res.Payload = decodePayload(payload)
	return res, nil
}
