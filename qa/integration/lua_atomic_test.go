//go:build integration

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"behemoth/internal/store"
)

// TestLuaAtomic_NoLostUpdate_NoNegativeHP: under concurrent hits the Lua path keeps
// HP non-negative, sum(applied) == max_hp exactly, and ends 'defeated' at 0 HP.
func TestLuaAtomic_NoLostUpdate_NoNegativeHP(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()

	rs, err := store.NewRedisStore(ctx, redisAddr, "")
	if err != nil {
		t.Fatalf("redis store: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	bossID := uniqueID("lua")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	const maxHP = 100_000
	// Seed the cache directly; no Postgres needed here.
	if err := rs.Rehydrate(ctx, bossID,
		store.RecoveryState{MaxHP: maxHP, CurrentHP: maxHP, State: "alive"}, nil); err != nil {
		t.Fatalf("seed redis: %v", err)
	}

	const (
		goroutines = 50
		hitsEach   = 400
		dmg        = 7 // 50*400*7 = 140_000 attempted > maxHP => boss dies
	)
	var totalApplied int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			player := uniqueID("p")
			for i := 0; i < hitsEach; i++ {
				res, err := rs.ApplyDamage(ctx, bossID, player, dmg)
				if err != nil {
					t.Errorf("apply damage: %v", err)
					return
				}
				switch res.Status {
				case store.StatusApplied:
					if res.NewHP < 0 {
						t.Errorf("HP went negative: %d", res.NewHP)
					}
					atomic.AddInt64(&totalApplied, res.Applied)
				case store.StatusDefeated:
					// already at 0, valid once dead
				default:
					t.Errorf("unexpected status %d", res.Status)
				}
			}
		}(g)
	}
	wg.Wait()

	if totalApplied != maxHP {
		t.Fatalf("sum(applied)=%d, want exactly max_hp=%d (lost/over-applied update)",
			totalApplied, maxHP)
	}

	view, loaded, err := rs.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("get boss view: loaded=%v err=%v", loaded, err)
	}
	if view.HP != 0 {
		t.Fatalf("final HP = %d, want 0", view.HP)
	}
	if view.State != "defeated" {
		t.Fatalf("final state = %q, want defeated", view.State)
	}
}

// TestLuaAtomic_GuardsAndColdCache covers the defense-in-depth guards.
func TestLuaAtomic_GuardsAndColdCache(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()

	rs, err := store.NewRedisStore(ctx, redisAddr, "")
	if err != nil {
		t.Fatalf("redis store: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	// Cold cache: unloaded boss reports StatusNotLoaded, not HP 0.
	missing := uniqueID("cold")
	res, err := rs.ApplyDamage(ctx, missing, "p", 10)
	if err != nil {
		t.Fatalf("apply on cold boss: %v", err)
	}
	if res.Status != store.StatusNotLoaded {
		t.Fatalf("cold boss status = %d, want StatusNotLoaded(%d)", res.Status, store.StatusNotLoaded)
	}

	// Non-positive damage rejected inside Lua, can't revive a boss.
	bossID := uniqueID("guard")
	t.Cleanup(func() { delRedisBoss(t, bossID) })
	if err := rs.Rehydrate(ctx, bossID,
		store.RecoveryState{MaxHP: 1000, CurrentHP: 1000, State: "alive"}, nil); err != nil {
		t.Fatalf("seed redis: %v", err)
	}
	for _, bad := range []int64{0, -5, -1000} {
		res, err := rs.ApplyDamage(ctx, bossID, "p", bad)
		if err != nil {
			t.Fatalf("apply damage=%d: %v", bad, err)
		}
		if res.Status != store.StatusInvalid {
			t.Fatalf("damage=%d status=%d, want StatusInvalid(%d)", bad, res.Status, store.StatusInvalid)
		}
	}
	// HP untouched by the invalid hits.
	if view, _, _ := rs.GetBossView(ctx, bossID); view.HP != 1000 {
		t.Fatalf("HP after invalid hits = %d, want 1000", view.HP)
	}
}
