//go:build integration

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"behemoth/internal/boss"
	"behemoth/internal/recovery"
	"behemoth/internal/store"
)

// buildDamageService wires the full damage path (Redis + durable writer +
// rehydrator) exactly as main.go does, so these tests exercise the real
// cross-store flow, not isolated layers.
func buildDamageService(t *testing.T) (*boss.Service, *store.RedisStore, *store.PostgresStore) {
	t.Helper()
	ctx := context.Background()

	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)

	rdb, err := store.NewRedisStore(ctx, redisAddr, "")
	if err != nil {
		t.Fatalf("redis store: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	w := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 5000, MaxBatch: 200, MaxWait: 5 * time.Millisecond,
		TxTimeout: 5 * time.Second, Concurrency: 4,
	}, testLogger())
	w.Start()
	t.Cleanup(w.Stop)

	reh := recovery.New(rdb, pg, testLogger())
	svc := boss.New(rdb, pg, w, reh, 1_000_000_000, testLogger())
	return svc, rdb, pg
}

// TestDamageService_RedisPostgresConsistency is the headline cross-store
// atomicity check: after many concurrent /damage calls (each of which only
// returns once its hit is DURABLY committed), the live Redis HP must equal the
// HP derived from Postgres contributions, and both must equal maxHP minus the
// total applied. A drift between the cache and the source of truth here would
// mean a restart silently changes the boss's HP — violating the durability
// requirement.
func TestDamageService_RedisPostgresConsistency(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	svc, rdb, pg := buildDamageService(t)

	bossID := uniqueID("dmgsvc")
	const maxHP = 5_000_000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// Load the boss into Redis the way startup rehydrate would.
	reh := recovery.New(rdb, pg, testLogger())
	if ok, err := reh.RehydrateBoss(ctx, bossID); err != nil || !ok {
		t.Fatalf("initial rehydrate: ok=%v err=%v", ok, err)
	}

	const (
		goroutines = 40
		hitsEach   = 100
		dmg        = 3 // 40*100*3 = 12_000 total, well below maxHP (no defeat)
	)
	var applied int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			player := uniqueID("pl")
			for i := 0; i < hitsEach; i++ {
				res, err := svc.Damage(ctx, bossID, player, dmg)
				if err != nil {
					t.Errorf("damage: %v", err)
					return
				}
				atomic.AddInt64(&applied, res.Applied)
			}
		}(g)
	}
	wg.Wait()

	wantApplied := int64(goroutines * hitsEach * dmg)
	if applied != wantApplied {
		t.Fatalf("total applied = %d, want %d", applied, wantApplied)
	}

	// Redis (live cache) HP.
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	wantHP := int64(maxHP) - wantApplied
	if view.HP != wantHP {
		t.Fatalf("Redis HP = %d, want %d (maxHP-applied)", view.HP, wantHP)
	}

	// Postgres-derived HP (source of truth, what a restart would rehydrate to).
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != wantHP {
		t.Fatalf("Postgres-derived HP = %d, want %d", rs.CurrentHP, wantHP)
	}
	if view.HP != rs.CurrentHP {
		t.Fatalf("cross-store drift: Redis HP=%d != Postgres-derived HP=%d", view.HP, rs.CurrentHP)
	}

	// The durable audit log must account for every applied hit.
	var events, total int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(damage_applied),0) FROM damage_events WHERE boss_id = $1`,
		bossID).Scan(&events, &total); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != int64(goroutines*hitsEach) {
		t.Fatalf("durable events = %d, want %d", events, goroutines*hitsEach)
	}
	if total != wantApplied {
		t.Fatalf("durable applied sum = %d, want %d", total, wantApplied)
	}
}

// TestDamageService_LazyRehydrateOnColdCache proves the "Redis lost mid-flight"
// recovery branch: if the boss's cache keys vanish, a /damage still succeeds by
// rehydrating from Postgres and retrying (never a spurious 404), and the applied
// hit lands on the correctly-derived HP rather than a fresh full-HP boss.
func TestDamageService_LazyRehydrateOnColdCache(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	svc, rdb, pg := buildDamageService(t)

	bossID := uniqueID("cold")
	const maxHP = 1000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// Give the boss prior durable damage so its derived HP != maxHP.
	if err := pg.CommitBatch(ctx, []store.DamageEvent{
		{BossID: bossID, PlayerID: "earlier", Applied: 400},
	}); err != nil {
		t.Fatalf("seed prior damage: %v", err)
	}

	// Cache is COLD (never rehydrated / evicted). A damage must still work.
	res, err := svc.Damage(ctx, bossID, "latecomer", 100)
	if err != nil {
		t.Fatalf("damage on cold cache: %v", err)
	}
	// Derived HP was 1000-400=600; after a 100 hit it must be 500 — proving the
	// hit applied to the rehydrated HP, not to a mistaken full 1000.
	if res.BossHP != 500 {
		t.Fatalf("HP after lazy-rehydrate hit = %d, want 500 (600 derived - 100)", res.BossHP)
	}
	if res.Applied != 100 {
		t.Fatalf("applied = %d, want 100", res.Applied)
	}

	// And it is durable: derived HP now 1000-400-100 = 500.
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != 500 {
		t.Fatalf("durable derived HP = %d, want 500", rs.CurrentHP)
	}
	_ = rdb
}
