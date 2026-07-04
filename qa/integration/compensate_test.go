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

// newStores opens a fresh Postgres + Redis store pair for a test.
func newStores(t *testing.T) (*store.PostgresStore, *store.RedisStore) {
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
	return pg, rdb
}

// buildRejectingService wires a boss.Service whose writer is permanently saturated
// (zero-capacity intake, never Started), so every Submit returns store.ErrQueueFull.
// Deterministic stand-in for a writer overloaded under burst.
func buildRejectingService(t *testing.T, pg *store.PostgresStore, rdb *store.RedisStore) *boss.Service {
	t.Helper()
	w := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 0, MaxBatch: 1, MaxWait: time.Second, TxTimeout: time.Second, Concurrency: 1,
	}, testLogger())
	// Not Started, so the intake is never drained.
	reh := recovery.New(rdb, pg, testLogger())
	return boss.New(rdb, pg, w, reh, 1_000_000_000, testLogger())
}

// lbScore returns a player's leaderboard score and whether they appear at all.
func lbScore(view store.BossView, player string) (int64, bool) {
	for _, e := range view.Leaderboard {
		if e.PlayerID == player {
			return e.Damage, true
		}
	}
	return 0, false
}

// TestDamageService_QueueFullCompensates_NoGhost: a hit rejected by a saturated
// writer (ErrQueueFull so ErrOverloaded) must be undone in Redis so the cache
// reconverges with the unchanged durable truth, leaving no ghost contributor.
func TestDamageService_QueueFullCompensates_NoGhost(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	pg, rdb := newStores(t)

	bossID := uniqueID("qfull")
	const maxHP = 10_000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// Durable truth: veteran did 300, HP maxHP-300, alive.
	const veteranDmg = 300
	if err := pg.CommitBatch(ctx, []store.DamageEvent{
		{BossID: bossID, PlayerID: "veteran", Applied: veteranDmg},
	}); err != nil {
		t.Fatalf("seed durable damage: %v", err)
	}

	// Load Redis from durable state. Cache == truth.
	reh := recovery.New(rdb, pg, testLogger())
	if ok, err := reh.RehydrateBoss(ctx, bossID); err != nil || !ok {
		t.Fatalf("initial rehydrate: ok=%v err=%v", ok, err)
	}

	// New player hits while the writer is saturated.
	svc := buildRejectingService(t, pg, rdb)
	const ghostDmg = 250
	if _, err := svc.Damage(ctx, bossID, "ghost", ghostDmg); !errors.Is(err, boss.ErrOverloaded) {
		t.Fatalf("Damage under queue-full = %v, want ErrOverloaded", err)
	}

	// Redis cache must have reconverged to durable state.
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	wantHP := int64(maxHP - veteranDmg)
	if view.HP != wantHP {
		t.Fatalf("Redis HP after compensated overload = %d, want %d (rejected hit undone)", view.HP, wantHP)
	}
	if _, ghostPresent := lbScore(view, "ghost"); ghostPresent {
		t.Fatalf("leaderboard still lists rejected player 'ghost' - phantom contribution not undone")
	}
	if s, ok := lbScore(view, "veteran"); !ok || s != veteranDmg {
		t.Fatalf("veteran score = %d present=%v, want %d (untouched by compensate)", s, ok, veteranDmg)
	}

	// Postgres (source of truth) must be unchanged from before the hit.
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != wantHP {
		t.Fatalf("Postgres-derived HP = %d, want %d", rs.CurrentHP, wantHP)
	}
	if view.HP != rs.CurrentHP {
		t.Fatalf("cross-store drift after overload: Redis HP=%d != Postgres HP=%d", view.HP, rs.CurrentHP)
	}
	// Rejected hit must leave no durable trace.
	var ghostContrib int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(total_damage),0) FROM contributions WHERE boss_id=$1 AND player_id='ghost'`,
		bossID).Scan(&ghostContrib); err != nil {
		t.Fatalf("read ghost contribution: %v", err)
	}
	if ghostContrib != 0 {
		t.Fatalf("ghost has durable contribution %d, want 0 (hit was rejected)", ghostContrib)
	}
	var events int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM damage_events WHERE boss_id=$1`, bossID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 { // only the veteran's seed event
		t.Fatalf("durable event count = %d, want 1 (rejected hit not persisted)", events)
	}
}

// TestDamageService_KillingBlowQueueFull_NotDefeatedUnclaimable: a rejected killing
// blow must undo both HP and the 'defeated' flag in Redis, else the boss is a
// defeated-but-unclaimable zombie. Then a healthy killing blow really defeats it.
func TestDamageService_KillingBlowQueueFull_NotDefeatedUnclaimable(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	pg, rdb := newStores(t)

	bossID := uniqueID("killblow")
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

	// Stage 1: killing blow rejected by a saturated writer.
	rejSvc := buildRejectingService(t, pg, rdb)
	if _, err := rejSvc.Damage(ctx, bossID, "finisher", 100); !errors.Is(err, boss.ErrOverloaded) {
		t.Fatalf("killing blow under queue-full = %v, want ErrOverloaded", err)
	}

	// Redis must not be left defeated: HP 100, state alive, finisher erased.
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	if view.HP != 100 {
		t.Fatalf("Redis HP after compensated killing blow = %d, want 100", view.HP)
	}
	if view.State != "alive" {
		t.Fatalf("Redis state = %q after compensated killing blow, want alive (zombie boss!)", view.State)
	}
	if _, present := lbScore(view, "finisher"); present {
		t.Fatalf("finisher still on leaderboard after rejected killing blow")
	}

	// Postgres untouched: alive at 100 HP, no finisher contribution.
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != 100 || rs.State != "alive" {
		t.Fatalf("Postgres state=%q HP=%d, want alive/100", rs.State, rs.CurrentHP)
	}

	// Claim reports the boss not yet defeated (a live boss, not a zombie).
	claimSvc := boss.New(nil, pg, nil, nil, 1_000_000_000, testLogger())
	if _, err := claimSvc.Claim(ctx, bossID, "finisher"); !errors.Is(err, boss.ErrBossNotDefeated) {
		t.Fatalf("Claim after compensated killing blow = %v, want ErrBossNotDefeated", err)
	}

	// Stage 2: healthy writer lands the real killing blow; Postgres flips to defeated.
	healthy := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 100, MaxBatch: 10, MaxWait: 5 * time.Millisecond, TxTimeout: 5 * time.Second, Concurrency: 2,
	}, testLogger())
	healthy.Start()
	t.Cleanup(healthy.Stop)
	goodSvc := boss.New(rdb, pg, healthy, reh, 1_000_000_000, testLogger())

	res, err := goodSvc.Damage(ctx, bossID, "finisher", 100)
	if err != nil {
		t.Fatalf("healthy killing blow: %v", err)
	}
	if !res.Defeated || res.BossHP != 0 {
		t.Fatalf("healthy killing blow result = %+v, want Defeated=true HP=0", res)
	}

	// Defeated in Postgres, so the finisher's claim succeeds (100/1000 = 10%, epic).
	claimed, err := claimSvc.Claim(ctx, bossID, "finisher")
	if err != nil {
		t.Fatalf("claim after real defeat: %v", err)
	}
	if claimed.AlreadyClaimed {
		t.Fatalf("first claim flagged AlreadyClaimed")
	}
	if claimed.Tier != boss.TierEpic {
		t.Fatalf("finisher tier = %q, want epic (100/1000 = 10%%)", claimed.Tier)
	}
}

// TestCompensateDamage_InvertsApplyDamage: CompensateDamage must be the exact
// inverse of ApplyDamage on HP, the leaderboard ZSET, and the defeated flag.
func TestCompensateDamage_InvertsApplyDamage(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()

	rdb, err := store.NewRedisStore(ctx, redisAddr, "")
	if err != nil {
		t.Fatalf("redis store: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	t.Run("non-killing hit fully reversed, member removed", func(t *testing.T) {
		id := uniqueID("cmp-basic")
		t.Cleanup(func() { delRedisBoss(t, id) })
		if err := rdb.Rehydrate(ctx, id,
			store.RecoveryState{MaxHP: 1000, CurrentHP: 1000, State: "alive"}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		mustApply(t, ctx, rdb, id, "p1", 50)
		if err := rdb.CompensateDamage(ctx, id, "p1", 50); err != nil {
			t.Fatalf("compensate: %v", err)
		}
		view, _, _ := rdb.GetBossView(ctx, id)
		if view.HP != 1000 {
			t.Fatalf("HP = %d, want 1000 (fully restored)", view.HP)
		}
		if view.State != "alive" {
			t.Fatalf("state = %q, want alive", view.State)
		}
		if _, present := lbScore(view, "p1"); present {
			t.Fatalf("p1 still on leaderboard; a zeroed member must be ZREM'd, not left at 0")
		}
	})

	t.Run("only the last applied amount is reversed; remaining score kept", func(t *testing.T) {
		id := uniqueID("cmp-partial")
		t.Cleanup(func() { delRedisBoss(t, id) })
		if err := rdb.Rehydrate(ctx, id,
			store.RecoveryState{MaxHP: 1000, CurrentHP: 1000, State: "alive"}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		mustApply(t, ctx, rdb, id, "p1", 50)
		mustApply(t, ctx, rdb, id, "p1", 30) // score 80, HP 920
		if err := rdb.CompensateDamage(ctx, id, "p1", 30); err != nil {
			t.Fatalf("compensate: %v", err)
		}
		view, _, _ := rdb.GetBossView(ctx, id)
		if view.HP != 950 {
			t.Fatalf("HP = %d, want 950 (1000-50)", view.HP)
		}
		if s, present := lbScore(view, "p1"); !present || s != 50 {
			t.Fatalf("p1 score = %d present=%v, want 50 (only last hit undone)", s, present)
		}
	})

	t.Run("killing blow reversal restores alive state", func(t *testing.T) {
		id := uniqueID("cmp-kill")
		t.Cleanup(func() { delRedisBoss(t, id) })
		if err := rdb.Rehydrate(ctx, id,
			store.RecoveryState{MaxHP: 100, CurrentHP: 100, State: "alive"}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		res := mustApply(t, ctx, rdb, id, "p", 100)
		if res.NewHP != 0 { // sanity: killing blow
			t.Fatalf("setup: killing blow left HP=%d, want 0", res.NewHP)
		}
		if v, _, _ := rdb.GetBossView(ctx, id); v.State != "defeated" {
			t.Fatalf("setup: state=%q, want defeated before compensate", v.State)
		}
		if err := rdb.CompensateDamage(ctx, id, "p", 100); err != nil {
			t.Fatalf("compensate: %v", err)
		}
		view, _, _ := rdb.GetBossView(ctx, id)
		if view.HP != 100 {
			t.Fatalf("HP = %d, want 100 (restored)", view.HP)
		}
		if view.State != "alive" {
			t.Fatalf("state = %q, want alive (defeated flag must be reverted)", view.State)
		}
		if _, present := lbScore(view, "p"); present {
			t.Fatalf("p still on leaderboard after full reversal")
		}
	})

	t.Run("missing boss key is a safe no-op", func(t *testing.T) {
		id := uniqueID("cmp-missing")
		t.Cleanup(func() { delRedisBoss(t, id) })
		// Keys absent: Compensate must not error (no-op).
		if err := rdb.CompensateDamage(ctx, id, "p", 50); err != nil {
			t.Fatalf("compensate on missing key = %v, want nil (no-op)", err)
		}
		if _, loaded, _ := rdb.GetBossView(ctx, id); loaded {
			t.Fatalf("boss unexpectedly present after no-op compensate")
		}
	})
}

func mustApply(t *testing.T, ctx context.Context, rdb *store.RedisStore, bossID, player string, dmg int64) store.ApplyResult {
	t.Helper()
	res, err := rdb.ApplyDamage(ctx, bossID, player, dmg)
	if err != nil {
		t.Fatalf("apply damage: %v", err)
	}
	if res.Status != store.StatusApplied {
		t.Fatalf("apply status = %d, want StatusApplied", res.Status)
	}
	return res
}
