//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"behemoth/internal/boss"
	"behemoth/internal/store"
)

// newClaimService builds a boss.Service wired with only Postgres. Claim gates on
// and prices from the durable store exclusively (never Redis/writer), so the
// cache, writer and rehydrator are intentionally nil here — if Claim ever
// touched them this test would panic, which is itself a useful guard.
func newClaimService(t *testing.T, pg *store.PostgresStore) *boss.Service {
	t.Helper()
	return boss.New(nil, pg, nil, nil, 1_000_000_000, testLogger())
}

// TestClaimService_TierFromContribution is the requirement-level check that
// "reward tier depends on the % of damage contributed": it drives the full
// Service.Claim path (durable gating + pricing) and asserts each contribution
// percentage yields the correct tier and matching reward payload. The existing
// SaveClaim race test hardcodes a tier, so this closes that gap.
func TestClaimService_TierFromContribution(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)

	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)
	svc := newClaimService(t, pg)

	const maxHP = 1000
	cases := []struct {
		name       string
		contrib    int64
		wantTier   string
		wantPctApx float64
	}{
		{"20% is legendary (inclusive lower bound)", 200, boss.TierLegendary, 20},
		{"19.9% falls to epic", 199, boss.TierEpic, 19.9},
		{"10% is epic", 100, boss.TierEpic, 10},
		{"5% is rare", 50, boss.TierRare, 5},
		{"1% is uncommon", 10, boss.TierUncommon, 1},
		{"0.5% is common", 5, boss.TierCommon, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bossID := uniqueID("claimsvc")
			player := "contributor"
			seedBoss(t, pool, bossID, maxHP, "defeated")
			if _, err := pool.Exec(ctx,
				`INSERT INTO contributions (boss_id, player_id, total_damage) VALUES ($1, $2, $3)`,
				bossID, player, c.contrib); err != nil {
				t.Fatalf("seed contribution: %v", err)
			}

			res, err := svc.Claim(ctx, bossID, player)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if res.Tier != c.wantTier {
				t.Fatalf("contribution %d/%d: tier = %q, want %q (pct≈%.1f%%)",
					c.contrib, maxHP, res.Tier, c.wantTier, c.wantPctApx)
			}
			if res.AlreadyClaimed {
				t.Fatalf("first claim reported AlreadyClaimed=true")
			}
			// Reward payload must be the one owed for the computed tier.
			wantGold := boss.RewardFor(c.wantTier)["gold"]
			gotGold := res.Payload["gold"]
			// JSONB round-trips ints as float64; compare numerically.
			if toF(gotGold) != toF(wantGold) {
				t.Fatalf("reward gold = %v, want %v for tier %q", gotGold, wantGold, c.wantTier)
			}

			// Second claim by the same player is idempotent: same tier, flagged.
			res2, err := svc.Claim(ctx, bossID, player)
			if err != nil {
				t.Fatalf("second Claim: %v", err)
			}
			if !res2.AlreadyClaimed {
				t.Fatalf("duplicate claim not flagged AlreadyClaimed")
			}
			if res2.Tier != c.wantTier {
				t.Fatalf("idempotent claim tier = %q, want %q", res2.Tier, c.wantTier)
			}
		})
	}
}

// TestClaimService_Gating asserts the domain error mapping the API relies on:
// unknown boss, alive boss, and non-contributor each produce the specific
// sentinel error (which the HTTP layer turns into 404 / 409 / 403).
func TestClaimService_Gating(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)

	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)
	svc := newClaimService(t, pg)

	t.Run("unknown boss -> ErrBossNotFound", func(t *testing.T) {
		_, err := svc.Claim(ctx, uniqueID("ghost"), "p")
		if !errors.Is(err, boss.ErrBossNotFound) {
			t.Fatalf("err = %v, want ErrBossNotFound", err)
		}
	})

	t.Run("alive boss -> ErrBossNotDefeated", func(t *testing.T) {
		id := uniqueID("alive")
		seedBoss(t, pool, id, 1000, "alive")
		if _, err := pool.Exec(ctx,
			`INSERT INTO contributions (boss_id, player_id, total_damage) VALUES ($1, 'p', 500)`,
			id); err != nil {
			t.Fatalf("seed contribution: %v", err)
		}
		_, err := svc.Claim(ctx, id, "p")
		if !errors.Is(err, boss.ErrBossNotDefeated) {
			t.Fatalf("err = %v, want ErrBossNotDefeated", err)
		}
	})

	t.Run("non-contributor -> ErrNoContribution", func(t *testing.T) {
		id := uniqueID("noc")
		seedBoss(t, pool, id, 1000, "defeated")
		_, err := svc.Claim(ctx, id, "stranger")
		if !errors.Is(err, boss.ErrNoContribution) {
			t.Fatalf("err = %v, want ErrNoContribution", err)
		}
	})
}

// toF numerically normalizes an any that may be int or float64 (JSONB decode).
func toF(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return -1
	}
}
