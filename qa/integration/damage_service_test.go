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

// buildDamageService wires the full damage path (Redis + writer + rehydrator) as main.go does.
func buildDamageService(t *testing.T) (*boss.Service, *store.RedisStore, *store.PostgresStore) {
	t.Helper()

	pg := openPG(t)
	rdb := openRedis(t)

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

// TestDamageService_RedisPostgresConsistency: after many concurrent durable
// /damage calls, live Redis HP == Postgres-derived HP == maxHP-applied (no drift).
func TestDamageService_RedisPostgresConsistency(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	svc, rdb, pg := buildDamageService(t)

	bossID := uniqueID("dmgsvc")
	const maxHP = 5_000_000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// Load the boss into Redis as startup rehydrate would.
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

	// Postgres-derived HP (source of truth).
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

	// Audit log must account for every applied hit.
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

// TestDamageService_LazyRehydrateOnColdCache: with cache keys gone, /damage still
// succeeds by rehydrating from Postgres, hitting derived HP not a full-HP boss.
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

	// Cold cache: damage must still work.
	res, err := svc.Damage(ctx, bossID, "latecomer", 100)
	if err != nil {
		t.Fatalf("damage on cold cache: %v", err)
	}
	// Derived HP 1000-400=600; after a 100 hit must be 500.
	if res.BossHP != 500 {
		t.Fatalf("HP after lazy-rehydrate hit = %d, want 500 (600 derived - 100)", res.BossHP)
	}
	if res.Applied != 100 {
		t.Fatalf("applied = %d, want 100", res.Applied)
	}

	// Durable: derived HP now 1000-400-100 = 500.
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != 500 {
		t.Fatalf("durable derived HP = %d, want 500", rs.CurrentHP)
	}
	_ = rdb
}
