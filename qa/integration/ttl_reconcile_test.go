//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"behemoth/internal/boss"
	"behemoth/internal/recovery"
	"behemoth/internal/store"
)

// TestCache_TTLExpiryHealsZombie: a zombie boss (defeated in Redis, alive in
// Postgres) has no TTL refresh from 409'd hits, so its stale key expires and the
// next cold read lazy-rehydrates from Postgres, healing it.
func TestCache_TTLExpiryHealsZombie(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	pg := openPG(t)

	const ttl = 1 * time.Second
	rdb, err := store.NewRedisStore(ctx, redisAddr, "", store.WithCacheTTL(ttl))
	if err != nil {
		t.Fatalf("redis store (ttl): %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	bossID := uniqueID("ttlzombie")
	const maxHP = 1000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// Durable truth: veteran did 900, HP 100, alive.
	const veteranDmg = 900
	if err := pg.CommitBatch(ctx, []store.DamageEvent{
		{BossID: bossID, PlayerID: "veteran", Applied: veteranDmg},
	}); err != nil {
		t.Fatalf("seed durable damage: %v", err)
	}
	reh := recovery.New(rdb, pg, testLogger())
	if ok, err := reh.RehydrateBoss(ctx, bossID); err != nil || !ok {
		t.Fatalf("initial rehydrate: ok=%v err=%v", ok, err)
	}

	// Manufacture the zombie: killing blow straight to Redis (as a lost durable
	// write would). Redis defeated at HP 0; Postgres untouched, alive at 100.
	res, err := rdb.ApplyDamage(ctx, bossID, "finisher", 100)
	if err != nil {
		t.Fatalf("apply killing blow to redis: %v", err)
	}
	if res.Status != store.StatusApplied || res.NewHP != 0 {
		t.Fatalf("setup apply = %+v, want status=applied hp=0", res)
	}
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	if view.HP != 0 || view.State != "defeated" {
		t.Fatalf("expected zombie (defeated/0), got HP=%d state=%q", view.HP, view.State)
	}

	// The zombie hp key must carry a TTL so it can expire.
	c := newRedisClient(t)
	pttl := c.PTTL(ctx, "boss:"+bossID+":hp").Val()
	if pttl <= 0 || pttl > ttl {
		t.Fatalf("zombie hp key PTTL = %v, want in (0, %v] (key must be able to expire)", pttl, ttl)
	}

	// Let the stale key expire; a cold read then lazy-rehydrates and heals.
	svc := boss.New(rdb, pg, nil, reh, 1_000_000_000, testLogger())
	time.Sleep(ttl + 400*time.Millisecond)

	healed, err := svc.Get(ctx, bossID)
	if err != nil {
		t.Fatalf("Get after TTL expiry: %v", err)
	}
	if healed.HP != 100 || healed.State != "alive" {
		t.Fatalf("after TTL expiry HP=%d state=%q, want 100/alive (zombie not healed)", healed.HP, healed.State)
	}
	if _, present := lbScore(healed, "finisher"); present {
		t.Fatalf("phantom finisher still on leaderboard after TTL heal")
	}
	if s, ok := lbScore(healed, "veteran"); !ok || s != veteranDmg {
		t.Fatalf("veteran score = %d present=%v, want %d (durable truth)", s, ok, veteranDmg)
	}
}

// TestReconciler_HealsCtxCancelDivergence: a hit from a ctx-cancelled client is
// not compensated inline (may still commit) but a reconcile is enqueued; the
// worker rebuilds from Postgres. TTL is 0 so only the reconciler heals (Cách B).
func TestReconciler_HealsCtxCancelDivergence(t *testing.T) {
	requireEnv(t)
	bg := context.Background()
	pool := newPool(t)
	pg := openPG(t)
	rdb := openRedis(t) // ttl=0: only the reconciler can heal here.

	bossID := uniqueID("reconcile")
	const maxHP = 1000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// Durable truth: veteran 300, HP 700, alive. Load into Redis.
	const veteranDmg = 300
	if err := pg.CommitBatch(bg, []store.DamageEvent{
		{BossID: bossID, PlayerID: "veteran", Applied: veteranDmg},
	}); err != nil {
		t.Fatalf("seed durable damage: %v", err)
	}
	reh := recovery.New(rdb, pg, testLogger())
	if ok, err := reh.RehydrateBoss(bg, bossID); err != nil || !ok {
		t.Fatalf("initial rehydrate: ok=%v err=%v", ok, err)
	}

	// Reconciler with no settle delay for a deterministic test.
	rc := recovery.NewReconciler(reh, 0, testLogger())
	rc.Start()
	t.Cleanup(rc.Stop)

	// Writer with room but never Started: the hit enqueues, done never fires, so
	// Submit returns only via ctx cancel and the event never commits.
	stalled := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 1, MaxBatch: 1, MaxWait: time.Hour, TxTimeout: time.Second, Concurrency: 1,
	}, testLogger())
	svc := boss.New(rdb, pg, stalled, reh, 1_000_000_000, testLogger(), boss.WithReconciler(rc))

	ctx, cancel := context.WithCancel(bg)
	errCh := make(chan error, 1)
	const hit = 250
	go func() {
		_, err := svc.Damage(ctx, bossID, "canceller", hit)
		errCh <- err
	}()

	// Let Damage apply to Redis and enqueue, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Damage on ctx-cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Damage did not return after ctx cancel")
	}

	// Hit applied to Redis, not compensated, but the reconciler rebuilds from
	// Postgres. Redis must reconverge: HP back to durable value, canceller erased.
	wantHP := int64(maxHP - veteranDmg)
	deadline := time.Now().Add(4 * time.Second)
	for {
		view, loaded, err := rdb.GetBossView(bg, bossID)
		if err != nil {
			t.Fatalf("redis view: %v", err)
		}
		_, ghost := lbScore(view, "canceller")
		if loaded && view.HP == wantHP && !ghost {
			break // reconverged via reconcile
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache did not reconverge via reconcile: HP=%d ghost=%v, want HP=%d ghost=false",
				view.HP, ghost, wantHP)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
