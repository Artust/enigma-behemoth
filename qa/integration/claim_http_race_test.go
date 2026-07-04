//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"behemoth/internal/store"
)

// TestHTTP_Claim_ExactlyOnceUnderConcurrentRequests fires n concurrent claims for
// one (player, boss) through the real router: all 200, exactly one fresh grant
// (already_claimed=false), the rest replays, all agreeing on tier/pct.
func TestHTTP_Claim_ExactlyOnceUnderConcurrentRequests(t *testing.T) {
	requireEnv(t)
	pool := newPool(t)
	h := buildHTTPHandler(t)

	bossID := uniqueID("http-claim-race")
	seedBoss(t, pool, bossID, 1000, "defeated")
	if _, err := pool.Exec(pgCtx(),
		`INSERT INTO contributions (boss_id, player_id, total_damage) VALUES ($1, 'winner', 250)`,
		bossID); err != nil {
		t.Fatalf("seed contribution: %v", err)
	}
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	const n = 32
	body := fmt.Sprintf(`{"player_id":"winner","boss_id":%q}`, bossID)

	codes := make([]int, n)
	bodies := make([]string, n)
	results := make([]store.ClaimResult, n)
	decodeErrs := make([]error, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start // release together for max contention
			rec := serve(h, http.MethodPost, "/rewards/claim", body)
			codes[i] = rec.Code
			bodies[i] = rec.Body.String()
			decodeErrs[i] = json.Unmarshal(rec.Body.Bytes(), &results[i])
		}(i)
	}
	close(start)
	wg.Wait()

	fresh := 0
	for i := range n {
		if codes[i] != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200; body=%s", i, codes[i], bodies[i])
		}
		if decodeErrs[i] != nil {
			t.Fatalf("request %d: decode %q: %v", i, bodies[i], decodeErrs[i])
		}
		if !results[i].AlreadyClaimed {
			fresh++
		}
		if results[i].Tier != results[0].Tier || results[i].Pct != results[0].Pct {
			t.Fatalf("request %d priced differently: tier=%q pct=%v vs tier=%q pct=%v",
				i, results[i].Tier, results[i].Pct, results[0].Tier, results[0].Pct)
		}
	}
	if fresh != 1 {
		t.Fatalf("exactly-once violated: %d responses reported a fresh grant (already_claimed=false), want exactly 1", fresh)
	}
}
