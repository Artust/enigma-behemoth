//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"behemoth/internal/api"
	"behemoth/internal/boss"
	"behemoth/internal/recovery"
	"behemoth/internal/store"
)

func pgCtx() context.Context { return context.Background() }

// httpMaxHit is the MaxDamagePerHit wired into the service (for the 400 boundary).
const httpMaxHit int64 = 1_000_000_000

// newTestMetrics builds non-registered collectors, avoiding promauto's global
// registry (which panics on a second server in the same process).
func newTestMetrics() *api.Metrics {
	return &api.Metrics{
		Requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_http_requests_total"},
			[]string{"route", "method", "status"}),
		Duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{Name: "test_http_request_duration_seconds"},
			[]string{"route"}),
		DamageApplied: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_damage_applied_total"}),
	}
}

// buildHTTPHandler wires the real Service behind the real chi router (writer
// started, so /damage completes durably).
func buildHTTPHandler(t *testing.T) http.Handler {
	t.Helper()
	svc, _, _ := buildDamageService(t)
	ready := &atomic.Bool{}
	ready.Store(true)
	srv := api.NewServer(svc, ready, testLogger(), newTestMetrics())
	return srv.Router()
}

// serve runs one request through the handler; empty body sends none.
func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// TestHTTP_Damage_StatusMapping: POST /damage returns the right status per domain
// outcome, end-to-end through the real router.
func TestHTTP_Damage_StatusMapping(t *testing.T) {
	requireEnv(t)
	pool := newPool(t)
	h := buildHTTPHandler(t)

	aliveID := uniqueID("http-dmg-alive")
	seedBoss(t, pool, aliveID, 1_000_000, "alive")
	t.Cleanup(func() { delRedisBoss(t, aliveID) })

	// defeated boss: derived HP 0 so a further hit is 409
	defeatedID := uniqueID("http-dmg-dead")
	seedBoss(t, pool, defeatedID, 100, "defeated")
	if _, err := pool.Exec(pgCtx(), // contributor damage zeroes HP
		`INSERT INTO contributions (boss_id, player_id, total_damage) VALUES ($1, 'slayer', 100)`,
		defeatedID); err != nil {
		t.Fatalf("seed defeated contribution: %v", err)
	}
	t.Cleanup(func() { delRedisBoss(t, defeatedID) })

	t.Run("200 applied", func(t *testing.T) {
		body := fmt.Sprintf(`{"player_id":"p1","boss_id":%q,"damage_amount":10}`, aliveID)
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var res boss.DamageResult
		decodeInto(t, rec, &res)
		if res.Applied != 10 {
			t.Fatalf("damage_applied = %d, want 10", res.Applied)
		}
	})

	t.Run("400 damage <= 0", func(t *testing.T) {
		for _, amt := range []int64{0, -5} {
			body := fmt.Sprintf(`{"player_id":"p1","boss_id":%q,"damage_amount":%d}`, aliveID, amt)
			rec := serve(h, http.MethodPost, "/damage", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("amount=%d: status = %d, want 400; body=%s", amt, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("400 damage > MAX_DAMAGE_PER_HIT", func(t *testing.T) {
		body := fmt.Sprintf(`{"player_id":"p1","boss_id":%q,"damage_amount":%d}`, aliveID, httpMaxHit+1)
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 malformed JSON", func(t *testing.T) {
		rec := serve(h, http.MethodPost, "/damage", `{not json`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 missing required field", func(t *testing.T) {
		// boss_id omitted -> rejected before the service
		body := `{"player_id":"p1","damage_amount":5}`
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 unknown field (DisallowUnknownFields)", func(t *testing.T) {
		body := fmt.Sprintf(`{"player_id":"p1","boss_id":%q,"damage_amount":5,"bogus":1}`, aliveID)
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("404 unknown boss", func(t *testing.T) {
		body := fmt.Sprintf(`{"player_id":"p1","boss_id":%q,"damage_amount":5}`, uniqueID("ghost"))
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("409 boss already defeated", func(t *testing.T) {
		body := fmt.Sprintf(`{"player_id":"latecomer","boss_id":%q,"damage_amount":5}`, defeatedID)
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestHTTP_Damage_Overloaded503: a zero-capacity, undrained writer fails every Submit
// with ErrQueueFull (ErrOverloaded), which the API must surface as 503.
func TestHTTP_Damage_Overloaded503(t *testing.T) {
	requireEnv(t)
	ctx := pgCtx()
	pool := newPool(t)

	pg := openPG(t)
	rdb := openRedis(t)

	// QueueSize 0, not started => Submit always hits ErrQueueFull
	w := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 0, MaxBatch: 1, MaxWait: time.Millisecond,
		TxTimeout: time.Second, Concurrency: 1,
	}, testLogger())

	reh := recovery.New(rdb, pg, testLogger())
	svc := boss.New(rdb, pg, w, reh, httpMaxHit, testLogger())
	ready := &atomic.Bool{}
	ready.Store(true)
	h := api.NewServer(svc, ready, testLogger(), newTestMetrics()).Router()

	bossID := uniqueID("http-overload")
	seedBoss(t, pool, bossID, 1_000_000, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	body := fmt.Sprintf(`{"player_id":"p1","boss_id":%q,"damage_amount":10}`, bossID)
	rec := serve(h, http.MethodPost, "/damage", body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}

	// overload fully compensated: durable HP and Redis HP both back at max
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != 1_000_000 {
		t.Fatalf("durable HP = %d, want 1000000 (overloaded hit must not persist)", rs.CurrentHP)
	}
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	if view.HP != 1_000_000 {
		t.Fatalf("redis HP = %d, want 1000000 (compensation must roll back)", view.HP)
	}
}

// TestHTTP_GetBoss_StatusMapping: GET /boss/{id} returns 200 with HP+leaderboard for
// a known boss, 404 for an unknown one.
func TestHTTP_GetBoss_StatusMapping(t *testing.T) {
	requireEnv(t)
	pool := newPool(t)
	h := buildHTTPHandler(t)

	bossID := uniqueID("http-get")
	seedBoss(t, pool, bossID, 500, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// land one hit to populate HP and leaderboard
	body := fmt.Sprintf(`{"player_id":"hero","boss_id":%q,"damage_amount":120}`, bossID)
	if rec := serve(h, http.MethodPost, "/damage", body); rec.Code != http.StatusOK {
		t.Fatalf("setup damage status = %d; body=%s", rec.Code, rec.Body.String())
	}

	t.Run("200 with hp and leaderboard", func(t *testing.T) {
		rec := serve(h, http.MethodGet, "/boss/"+bossID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var view store.BossView
		decodeInto(t, rec, &view)
		if view.HP != 380 { // 500 - 120
			t.Fatalf("hp = %d, want 380", view.HP)
		}
		if len(view.Leaderboard) != 1 || view.Leaderboard[0].PlayerID != "hero" ||
			view.Leaderboard[0].Damage != 120 {
			t.Fatalf("leaderboard = %+v, want [hero:120]", view.Leaderboard)
		}
	})

	t.Run("404 unknown boss", func(t *testing.T) {
		rec := serve(h, http.MethodGet, "/boss/"+uniqueID("ghost"), "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestHTTP_GetBoss_Top10Leaderboard: with >10 distinct contributors, GET /boss returns
// exactly the top 10 by damage descending, excluding the rest.
func TestHTTP_GetBoss_Top10Leaderboard(t *testing.T) {
	requireEnv(t)
	pool := newPool(t)
	h := buildHTTPHandler(t)

	bossID := uniqueID("http-top10")
	const players = 13 // > 10
	seedBoss(t, pool, bossID, 1_000_000, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// player k (1..13) deals k*100, all distinct, none defeats the boss
	type pd struct {
		player string
		dmg    int64
	}
	want := make([]pd, 0, players)
	for k := 1; k <= players; k++ {
		player := fmt.Sprintf("p%02d", k)
		dmg := int64(k * 100)
		body := fmt.Sprintf(`{"player_id":%q,"boss_id":%q,"damage_amount":%d}`, player, bossID, dmg)
		if rec := serve(h, http.MethodPost, "/damage", body); rec.Code != http.StatusOK {
			t.Fatalf("damage for %s status = %d; body=%s", player, rec.Code, rec.Body.String())
		}
		want = append(want, pd{player, dmg})
	}

	rec := serve(h, http.MethodGet, "/boss/"+bossID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var view store.BossView
	decodeInto(t, rec, &view)

	// exactly Top 10
	if len(view.Leaderboard) != 10 {
		t.Fatalf("leaderboard has %d entries, want exactly 10 (Top 10 requirement)",
			len(view.Leaderboard))
	}

	// top 10 = 10 highest damages, descending
	sort.Slice(want, func(i, j int) bool { return want[i].dmg > want[j].dmg })
	wantTop := want[:10]

	// strictly descending and matching the expected players
	for i, entry := range view.Leaderboard {
		if entry.PlayerID != wantTop[i].player || entry.Damage != wantTop[i].dmg {
			t.Fatalf("position %d = %s:%d, want %s:%d (board=%+v)",
				i, entry.PlayerID, entry.Damage, wantTop[i].player, wantTop[i].dmg, view.Leaderboard)
		}
		if i > 0 && view.Leaderboard[i-1].Damage < entry.Damage {
			t.Fatalf("not sorted descending at %d: %d then %d",
				i, view.Leaderboard[i-1].Damage, entry.Damage)
		}
	}

	// 3 lowest contributors must not appear
	present := map[string]bool{}
	for _, e := range view.Leaderboard {
		present[e.PlayerID] = true
	}
	for _, excluded := range want[10:] {
		if present[excluded.player] {
			t.Fatalf("excluded contributor %s (dmg %d) appeared in Top 10",
				excluded.player, excluded.dmg)
		}
	}
}

// TestHTTP_Claim_StatusMapping: POST /rewards/claim returns 200 for both a fresh claim
// and a replay (distinguished by already_claimed), plus the guarded error codes.
func TestHTTP_Claim_StatusMapping(t *testing.T) {
	requireEnv(t)
	pool := newPool(t)
	h := buildHTTPHandler(t)

	t.Run("200 first claim and 200 replay, distinguished by already_claimed", func(t *testing.T) {
		bossID := uniqueID("http-claim-ok")
		seedBoss(t, pool, bossID, 1000, "defeated")
		if _, err := pool.Exec(pgCtx(),
			`INSERT INTO contributions (boss_id, player_id, total_damage) VALUES ($1, 'winner', 250)`,
			bossID); err != nil {
			t.Fatalf("seed contribution: %v", err)
		}
		body := fmt.Sprintf(`{"player_id":"winner","boss_id":%q}`, bossID)

		rec := serve(h, http.MethodPost, "/rewards/claim", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("first claim status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var first store.ClaimResult
		decodeInto(t, rec, &first)
		if first.AlreadyClaimed {
			t.Fatalf("first claim reported already_claimed=true")
		}

		rec2 := serve(h, http.MethodPost, "/rewards/claim", body)
		if rec2.Code != http.StatusOK {
			t.Fatalf("replay status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
		}
		var replay store.ClaimResult
		decodeInto(t, rec2, &replay)
		if !replay.AlreadyClaimed {
			t.Fatalf("replay claim not flagged already_claimed")
		}
	})

	t.Run("409 boss not yet defeated", func(t *testing.T) {
		bossID := uniqueID("http-claim-alive")
		seedBoss(t, pool, bossID, 1000, "alive")
		if _, err := pool.Exec(pgCtx(),
			`INSERT INTO contributions (boss_id, player_id, total_damage) VALUES ($1, 'p', 100)`,
			bossID); err != nil {
			t.Fatalf("seed contribution: %v", err)
		}
		body := fmt.Sprintf(`{"player_id":"p","boss_id":%q}`, bossID)
		rec := serve(h, http.MethodPost, "/rewards/claim", body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("403 non-contributor", func(t *testing.T) {
		bossID := uniqueID("http-claim-noc")
		seedBoss(t, pool, bossID, 1000, "defeated")
		body := fmt.Sprintf(`{"player_id":"stranger","boss_id":%q}`, bossID)
		rec := serve(h, http.MethodPost, "/rewards/claim", body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("404 unknown boss", func(t *testing.T) {
		body := fmt.Sprintf(`{"player_id":"p","boss_id":%q}`, uniqueID("ghost"))
		rec := serve(h, http.MethodPost, "/rewards/claim", body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 malformed JSON", func(t *testing.T) {
		rec := serve(h, http.MethodPost, "/rewards/claim", `{bad`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestHTTP_DefeatLifecycle_OverkillAndClaim: the full happy path through the live HTTP
// stack - damage until death (overkill capped so contributions sum to max_hp, HP never
// negative), durable defeat in Postgres, each contributor claims with the right tier/%,
// and a post-defeat hit is 409.
func TestHTTP_DefeatLifecycle_OverkillAndClaim(t *testing.T) {
	requireEnv(t)
	ctx := pgCtx()
	pool := newPool(t)
	h := buildHTTPHandler(t)

	bossID := uniqueID("http-lifecycle")
	const maxHP = 1000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// alpha lands the bulk, beta a mid share, gamma delivers an overkill kill
	hit := func(player string, amount int64) boss.DamageResult {
		t.Helper()
		body := fmt.Sprintf(`{"player_id":%q,"boss_id":%q,"damage_amount":%d}`, player, bossID, amount)
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("damage %s(%d) status = %d, want 200; body=%s", player, amount, rec.Code, rec.Body.String())
		}
		var res boss.DamageResult
		decodeInto(t, rec, &res)
		return res
	}

	if r := hit("alpha", 800); r.Defeated || r.BossHP != 200 || r.Applied != 800 {
		t.Fatalf("alpha hit = %+v, want applied 800, hp 200, not defeated", r)
	}
	if r := hit("beta", 180); r.Defeated || r.BossHP != 20 || r.Applied != 180 {
		t.Fatalf("beta hit = %+v, want applied 180, hp 20, not defeated", r)
	}

	// overkill: gamma requests 1000, only 20 HP remains
	kill := hit("gamma", 1000)
	if !kill.Defeated {
		t.Fatalf("killing blow not flagged defeated: %+v", kill)
	}
	if kill.BossHP != 0 {
		t.Fatalf("HP after killing blow = %d, want 0 (never negative)", kill.BossHP)
	}
	if kill.Applied != 20 {
		t.Fatalf("overkill applied = %d, want 20 (capped to remaining HP, not 1000)", kill.Applied)
	}

	// GET /boss: defeated, 0 HP, full ordered leaderboard
	var view store.BossView
	{
		rec := serve(h, http.MethodGet, "/boss/"+bossID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d; body=%s", rec.Code, rec.Body.String())
		}
		decodeInto(t, rec, &view)
	}
	if view.HP != 0 {
		t.Fatalf("view HP = %d, want 0", view.HP)
	}
	if view.State != "defeated" {
		t.Fatalf("view state = %q, want defeated", view.State)
	}
	wantBoard := []store.LeaderEntry{{PlayerID: "alpha", Damage: 800}, {PlayerID: "beta", Damage: 180}, {PlayerID: "gamma", Damage: 20}}
	if len(view.Leaderboard) != 3 {
		t.Fatalf("leaderboard = %+v, want 3 entries", view.Leaderboard)
	}
	for i, w := range wantBoard {
		if view.Leaderboard[i] != w {
			t.Fatalf("leaderboard[%d] = %+v, want %+v", i, view.Leaderboard[i], w)
		}
	}

	// durable gate for Claim: Postgres defeated, HP 0, contributions sum to max_hp
	var (
		pgState string
		pgHP    int64
		pgSum   int64
	)
	if err := pool.QueryRow(ctx,
		`SELECT state, current_hp FROM bosses WHERE id = $1`, bossID).Scan(&pgState, &pgHP); err != nil {
		t.Fatalf("read boss row: %v", err)
	}
	if pgState != "defeated" {
		t.Fatalf("durable state = %q, want defeated (killing blow must persist defeat, else boss is unclaimable)", pgState)
	}
	if pgHP != 0 {
		t.Fatalf("durable current_hp = %d, want 0", pgHP)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(total_damage),0) FROM contributions WHERE boss_id = $1`, bossID).Scan(&pgSum); err != nil {
		t.Fatalf("sum contributions: %v", err)
	}
	if pgSum != maxHP {
		t.Fatalf("durable contribution sum = %d, want %d (overkill must not inflate the reward denominator)", pgSum, maxHP)
	}

	// each contributor claims; tier + % reflects their share
	claim := func(player string, wantTier string, wantPct float64) {
		t.Helper()
		body := fmt.Sprintf(`{"player_id":%q,"boss_id":%q}`, player, bossID)
		rec := serve(h, http.MethodPost, "/rewards/claim", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("claim %s status = %d, want 200; body=%s", player, rec.Code, rec.Body.String())
		}
		var res store.ClaimResult
		decodeInto(t, rec, &res)
		if res.AlreadyClaimed {
			t.Fatalf("claim %s: first claim flagged already_claimed", player)
		}
		if res.Tier != wantTier {
			t.Fatalf("claim %s tier = %q, want %q (pct %.1f)", player, res.Tier, wantTier, res.Pct)
		}
		if res.Pct < wantPct-0.001 || res.Pct > wantPct+0.001 {
			t.Fatalf("claim %s pct = %.4f, want %.1f", player, res.Pct, wantPct)
		}
	}
	claim("alpha", boss.TierLegendary, 80) // 800/1000
	claim("beta", boss.TierEpic, 18)       // 180/1000
	claim("gamma", boss.TierUncommon, 2)   // 20/1000

	// hit on the already-dead boss must be 409
	{
		body := fmt.Sprintf(`{"player_id":"latecomer","boss_id":%q,"damage_amount":5}`, bossID)
		rec := serve(h, http.MethodPost, "/damage", body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("post-defeat damage status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
	}
}
