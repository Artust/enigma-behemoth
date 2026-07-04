//go:build integration

package integration

import (
	"context"
	"testing"

	"behemoth/internal/recovery"
	"behemoth/internal/store"
)

// TestDurableInvariant_AndRehydrate asserts the persistence-safety invariants
// that survive a restart:
//   - derived HP == max_hp - SUM(contributions) (RecoveryState is authoritative),
//   - after rehydrating the cache from Postgres, Redis exactly mirrors the
//     durable state (HP + leaderboard).
func TestDurableInvariant_AndRehydrate(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)

	bossID := uniqueID("inv")
	const maxHP = 1_000_000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)

	// Persist a set of applied hits the same way the writer would.
	events := []store.DamageEvent{
		{BossID: bossID, PlayerID: "a", Applied: 100},
		{BossID: bossID, PlayerID: "b", Applied: 250},
		{BossID: bossID, PlayerID: "a", Applied: 50},
	}
	if err := pg.CommitBatch(ctx, events); err != nil {
		t.Fatalf("commit batch: %v", err)
	}
	const sumApplied = 400 // a=150, b=250

	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if want := int64(maxHP - sumApplied); rs.CurrentHP != want {
		t.Fatalf("derived CurrentHP = %d, want max_hp-SUM = %d", rs.CurrentHP, want)
	}

	// bosses.current_hp (updated inside CommitBatch) must match the derived value.
	var storedHP int64
	if err := pool.QueryRow(ctx,
		`SELECT current_hp FROM bosses WHERE id = $1`, bossID).Scan(&storedHP); err != nil {
		t.Fatalf("read current_hp: %v", err)
	}
	if storedHP != rs.CurrentHP {
		t.Fatalf("bosses.current_hp=%d != derived=%d", storedHP, rs.CurrentHP)
	}

	// Rehydrate the cache from durable state and confirm Redis mirrors Postgres.
	redisStore, err := store.NewRedisStore(ctx, redisAddr, "")
	if err != nil {
		t.Fatalf("redis store: %v", err)
	}
	t.Cleanup(func() { _ = redisStore.Close() })

	reh := recovery.New(redisStore, pg, testLogger())
	if ok, err := reh.RehydrateBoss(ctx, bossID); err != nil || !ok {
		t.Fatalf("rehydrate: ok=%v err=%v", ok, err)
	}

	view, loaded, err := redisStore.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("get boss view: loaded=%v err=%v", loaded, err)
	}
	if view.HP != rs.CurrentHP {
		t.Fatalf("cache HP=%d != durable HP=%d", view.HP, rs.CurrentHP)
	}
	// Leaderboard rebuilt from contributions: b=250 (rank 1), a=150 (rank 2).
	if len(view.Leaderboard) < 2 {
		t.Fatalf("leaderboard has %d entries, want >= 2", len(view.Leaderboard))
	}
	if view.Leaderboard[0].PlayerID != "b" || view.Leaderboard[0].Damage != 250 {
		t.Fatalf("rank 1 = %+v, want {b 250}", view.Leaderboard[0])
	}
	if view.Leaderboard[1].PlayerID != "a" || view.Leaderboard[1].Damage != 150 {
		t.Fatalf("rank 2 = %+v, want {a 150}", view.Leaderboard[1])
	}
}
