//go:build integration

package integration

import (
	"context"
	"testing"

	"behemoth/internal/store"
)

// TestClaim_ExactlyOnceUnderRace: concurrent identical claims yield one insert, rest idempotent.
func TestClaim_ExactlyOnceUnderRace(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)

	bossID := uniqueID("claim")
	seedBoss(t, pool, bossID, 1000, "defeated")
	if _, err := pool.Exec(ctx,
		`INSERT INTO contributions (boss_id, player_id, total_damage) VALUES ($1, 'winner', 600)`,
		bossID); err != nil {
		t.Fatalf("seed contribution: %v", err)
	}

	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)

	in := store.ClaimInput{
		BossID: bossID, PlayerID: "winner",
		Tier: "legendary", Pct: 60, Payload: []byte(`{"gold":1000}`),
	}

	const racers = 32
	results := make([]store.ClaimResult, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	done := make(chan int, racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			<-start // release all at once
			results[i], errs[i] = pg.SaveClaim(ctx, in)
			done <- i
		}(i)
	}
	close(start)
	for i := 0; i < racers; i++ {
		<-done
	}

	fresh := 0
	for i := 0; i < racers; i++ {
		if errs[i] != nil {
			t.Fatalf("claim[%d] error: %v", i, errs[i])
		}
		if !results[i].AlreadyClaimed {
			fresh++
		}
		if results[i].Tier != "legendary" {
			t.Fatalf("claim[%d] tier = %q, want legendary (idempotent reply)", i, results[i].Tier)
		}
	}
	if fresh != 1 {
		t.Fatalf("exactly-once violated: %d fresh inserts (want 1)", fresh)
	}

	var rows int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM reward_claims WHERE boss_id = $1 AND player_id = 'winner'`,
		bossID).Scan(&rows); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if rows != 1 {
		t.Fatalf("reward_claims rows = %d, want 1", rows)
	}
}

// TestClaimBasis_Gating: durable authorization gates checked before pricing a claim.
func TestClaimBasis_Gating(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)

	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)

	t.Run("unknown boss", func(t *testing.T) {
		cb, err := pg.ClaimBasis(ctx, uniqueID("nope"), "p")
		if err != nil {
			t.Fatalf("claim basis: %v", err)
		}
		if cb.Exists {
			t.Fatalf("Exists=true for unknown boss")
		}
	})

	t.Run("alive boss blocks claim", func(t *testing.T) {
		id := uniqueID("alive")
		seedBoss(t, pool, id, 1000, "alive")
		cb, err := pg.ClaimBasis(ctx, id, "p")
		if err != nil {
			t.Fatalf("claim basis: %v", err)
		}
		if !cb.Exists || cb.State != "alive" {
			t.Fatalf("got Exists=%v State=%q, want exists+alive", cb.Exists, cb.State)
		}
	})

	t.Run("non-contributor", func(t *testing.T) {
		id := uniqueID("noc")
		seedBoss(t, pool, id, 1000, "defeated")
		cb, err := pg.ClaimBasis(ctx, id, "stranger")
		if err != nil {
			t.Fatalf("claim basis: %v", err)
		}
		if !cb.Exists || cb.State != "defeated" {
			t.Fatalf("got Exists=%v State=%q, want exists+defeated", cb.Exists, cb.State)
		}
		if cb.HasContribution {
			t.Fatalf("HasContribution=true for a non-contributor")
		}
	})
}
