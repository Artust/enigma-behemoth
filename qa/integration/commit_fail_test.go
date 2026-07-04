//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"behemoth/internal/api"
	"behemoth/internal/boss"
	"behemoth/internal/recovery"
	"behemoth/internal/store"
)

// buildCommitFailWriter returns a Started writer wired to a closed Postgres pool, so
// every CommitBatch fails deterministically with store.ErrCommitFailed (no timing).
func buildCommitFailWriter(t *testing.T) *store.Writer {
	t.Helper()
	dead, err := store.NewPostgresStore(context.Background(), pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres (to-be-closed) store: %v", err)
	}
	dead.Close() // closed pool => CommitBatch fails, never hangs

	// MaxBatch=1: each event commits/fails on its own
	w := store.NewWriter(dead, store.WriterConfig{
		QueueSize: 100, MaxBatch: 1, MaxWait: 5 * time.Millisecond,
		TxTimeout: 2 * time.Second, Concurrency: 1,
	}, testLogger())
	w.Start()
	t.Cleanup(w.Stop)
	return w
}

// TestDamageService_CommitFailCompensates_KillingBlow: a killing blow whose commit
// fails must undo both HP and the 'defeated' flag in Redis (no zombie boss), then a
// healthy killing blow still defeats it and the finisher can claim.
func TestDamageService_CommitFailCompensates_KillingBlow(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	pg := openPG(t)
	rdb := openRedis(t)

	bossID := uniqueID("cfkill")
	const maxHP = 1000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// veteran does 900 -> boss at 100 HP durably, alive
	const veteranDmg = 900
	if err := pg.CommitBatch(ctx, []store.DamageEvent{
		{BossID: bossID, PlayerID: "veteran", Applied: veteranDmg},
	}); err != nil {
		t.Fatalf("seed durable damage: %v", err)
	}

	// load Redis from durable state
	reh := recovery.New(rdb, pg, testLogger())
	if ok, err := reh.RehydrateBoss(ctx, bossID); err != nil || !ok {
		t.Fatalf("initial rehydrate: ok=%v err=%v", ok, err)
	}

	// Stage 1: killing blow's commit fails
	failWriter := buildCommitFailWriter(t)
	failSvc := boss.New(rdb, pg, failWriter, reh, 1_000_000_000, testLogger())

	_, err := failSvc.Damage(ctx, bossID, "finisher", 100)
	// want commit failure, not queue-full/overloaded
	if !errors.Is(err, store.ErrCommitFailed) {
		t.Fatalf("Damage under commit-failure = %v, want wrapped store.ErrCommitFailed", err)
	}
	if errors.Is(err, boss.ErrOverloaded) {
		t.Fatalf("commit-failure was misclassified as ErrOverloaded (queue-full): %v", err)
	}

	// Redis reverted: HP 100, alive, finisher off the leaderboard
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	if view.HP != 100 {
		t.Fatalf("Redis HP after compensated commit-fail killing blow = %d, want 100", view.HP)
	}
	if view.State != "alive" {
		t.Fatalf("Redis state = %q after compensated commit-fail killing blow, want alive (zombie boss!)", view.State)
	}
	if _, present := lbScore(view, "finisher"); present {
		t.Fatalf("finisher still on leaderboard after commit-failed killing blow")
	}

	// Postgres untouched: alive at 100 HP
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != 100 || rs.State != "alive" {
		t.Fatalf("Postgres state=%q HP=%d, want alive/100", rs.State, rs.CurrentHP)
	}
	if view.HP != rs.CurrentHP {
		t.Fatalf("cross-store drift after commit-fail: Redis HP=%d != Postgres HP=%d", view.HP, rs.CurrentHP)
	}
	// failed hit left no durable trace
	var finisherContrib int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(total_damage),0) FROM contributions WHERE boss_id=$1 AND player_id='finisher'`,
		bossID).Scan(&finisherContrib); err != nil {
		t.Fatalf("read finisher contribution: %v", err)
	}
	if finisherContrib != 0 {
		t.Fatalf("finisher has durable contribution %d, want 0 (commit failed)", finisherContrib)
	}
	var events int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM damage_events WHERE boss_id=$1`, bossID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 { // only the veteran seed event
		t.Fatalf("durable event count = %d, want 1 (commit-failed hit not persisted)", events)
	}

	// claim reports boss not yet defeated (live, not a zombie)
	claimSvc := boss.New(nil, pg, nil, nil, 1_000_000_000, testLogger())
	if _, err := claimSvc.Claim(ctx, bossID, "finisher"); !errors.Is(err, boss.ErrBossNotDefeated) {
		t.Fatalf("Claim after compensated commit-fail killing blow = %v, want ErrBossNotDefeated", err)
	}

	// Stage 2: healthy writer lands the real killing blow, finisher can claim
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

	claimed, err := claimSvc.Claim(ctx, bossID, "finisher")
	if err != nil {
		t.Fatalf("claim after real defeat: %v", err)
	}
	if claimed.AlreadyClaimed {
		t.Fatalf("first claim flagged AlreadyClaimed")
	}
	if claimed.Tier != boss.TierEpic {
		t.Fatalf("finisher tier = %q, want epic (100/1000)", claimed.Tier)
	}
}

// TestDamageService_CommitFailCompensates_NoGhost: a non-killing hit whose commit
// fails leaves no ghost contributor; Redis reconverges with durable state.
func TestDamageService_CommitFailCompensates_NoGhost(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	pg := openPG(t)
	rdb := openRedis(t)

	bossID := uniqueID("cfghost")
	const maxHP = 10_000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// veteran landed 300 -> derived HP maxHP-300, alive
	const veteranDmg = 300
	if err := pg.CommitBatch(ctx, []store.DamageEvent{
		{BossID: bossID, PlayerID: "veteran", Applied: veteranDmg},
	}); err != nil {
		t.Fatalf("seed durable damage: %v", err)
	}

	reh := recovery.New(rdb, pg, testLogger())
	if ok, err := reh.RehydrateBoss(ctx, bossID); err != nil || !ok {
		t.Fatalf("initial rehydrate: ok=%v err=%v", ok, err)
	}

	// new player hits while every commit fails
	failWriter := buildCommitFailWriter(t)
	failSvc := boss.New(rdb, pg, failWriter, reh, 1_000_000_000, testLogger())

	const ghostDmg = 250
	if _, err := failSvc.Damage(ctx, bossID, "ghost", ghostDmg); !errors.Is(err, store.ErrCommitFailed) {
		t.Fatalf("Damage under commit-failure = %v, want wrapped store.ErrCommitFailed", err)
	}

	// Redis reconverged to durable state
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	wantHP := int64(maxHP - veteranDmg)
	if view.HP != wantHP {
		t.Fatalf("Redis HP after compensated commit-fail = %d, want %d (failed hit undone)", view.HP, wantHP)
	}
	if _, ghostPresent := lbScore(view, "ghost"); ghostPresent {
		t.Fatalf("leaderboard still lists commit-failed player 'ghost' - phantom contribution not undone")
	}
	if s, ok := lbScore(view, "veteran"); !ok || s != veteranDmg {
		t.Fatalf("veteran score = %d present=%v, want %d (untouched by compensate)", s, ok, veteranDmg)
	}

	// Postgres unchanged, no durable trace of the ghost
	rs, ok, err := pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		t.Fatalf("recovery state: ok=%v err=%v", ok, err)
	}
	if rs.CurrentHP != wantHP {
		t.Fatalf("Postgres-derived HP = %d, want %d", rs.CurrentHP, wantHP)
	}
	if view.HP != rs.CurrentHP {
		t.Fatalf("cross-store drift after commit-fail: Redis HP=%d != Postgres HP=%d", view.HP, rs.CurrentHP)
	}
	var ghostContrib int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(total_damage),0) FROM contributions WHERE boss_id=$1 AND player_id='ghost'`,
		bossID).Scan(&ghostContrib); err != nil {
		t.Fatalf("read ghost contribution: %v", err)
	}
	if ghostContrib != 0 {
		t.Fatalf("ghost has durable contribution %d, want 0 (commit failed)", ghostContrib)
	}
}

// TestHTTP_Damage_CommitFail500: a commit failure maps to HTTP 500 through the real
// router (not 503/2xx), and compensation still runs (Redis reconverges, no phantom).
func TestHTTP_Damage_CommitFail500(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)
	pg := openPG(t)
	rdb := openRedis(t)

	bossID := uniqueID("cfhttp")
	const maxHP = 1000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// load Redis from durable state (warm cache)
	reh := recovery.New(rdb, pg, testLogger())
	if ok, err := reh.RehydrateBoss(ctx, bossID); err != nil || !ok {
		t.Fatalf("initial rehydrate: ok=%v err=%v", ok, err)
	}

	failWriter := buildCommitFailWriter(t)
	svc := boss.New(rdb, pg, failWriter, reh, httpMaxHit, testLogger())
	ready := &atomic.Bool{}
	ready.Store(true)
	h := api.NewServer(svc, ready, testLogger(), newTestMetrics()).Router()

	const hit = 30
	body := fmt.Sprintf(`{"player_id":"p1","boss_id":%q,"damage_amount":%d}`, bossID, hit)
	rec := serve(h, http.MethodPost, "/damage", body)

	// commit failure => 500, not 503, not 2xx
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (commit-failure); body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("commit-failure misclassified as 503 (queue-full/overloaded)")
	}

	// compensation ran: Redis back to full HP, no phantom contributor
	view, loaded, err := rdb.GetBossView(ctx, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	if view.HP != maxHP {
		t.Fatalf("Redis HP after commit-fail = %d, want %d (hit must be compensated)", view.HP, maxHP)
	}
	if _, present := lbScore(view, "p1"); present {
		t.Fatalf("p1 still on leaderboard after commit-failed hit - not compensated")
	}
}

// TestDamageService_CtxCancelNotCompensated: a ctx-cancel while awaiting durability
// must NOT compensate (the event may still commit); only queue-full/commit-failed do.
// Writer is never Started with QueueSize=1, so Submit blocks until cancel deterministically.
func TestDamageService_CtxCancelNotCompensated(t *testing.T) {
	requireEnv(t)
	bg := context.Background()
	pool := newPool(t)
	pg := openPG(t)
	rdb := openRedis(t)

	bossID := uniqueID("cfcancel")
	const maxHP = 1000
	seedBoss(t, pool, bossID, maxHP, "alive")
	t.Cleanup(func() { delRedisBoss(t, bossID) })

	// veteran 300 -> derived HP 700, alive
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

	// never Started, so `done` never fires; Submit only returns via ctx cancel
	stalled := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 1, MaxBatch: 1, MaxWait: time.Hour, TxTimeout: time.Second, Concurrency: 1,
	}, testLogger())
	svc := boss.New(rdb, pg, stalled, reh, 1_000_000_000, testLogger())

	ctx, cancel := context.WithCancel(bg)
	type result struct {
		err error
	}
	resCh := make(chan result, 1)
	const hit = 250
	go func() {
		_, err := svc.Damage(ctx, bossID, "canceller", hit)
		resCh <- result{err: err}
	}()

	// let Damage apply to Redis and enqueue, then cancel to release Submit
	time.Sleep(200 * time.Millisecond)
	cancel()

	var got result
	select {
	case got = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Damage did not return after ctx cancel - Submit must honor ctx.Done()")
	}

	// error must be context.Canceled, not a commit/queue failure
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Damage on ctx-cancel = %v, want context.Canceled", got.err)
	}
	if errors.Is(got.err, store.ErrCommitFailed) || errors.Is(got.err, store.ErrQueueFull) || errors.Is(got.err, boss.ErrOverloaded) {
		t.Fatalf("ctx-cancel misclassified as a compensatable failure: %v", got.err)
	}

	// Redis NOT compensated: the cancelled hit remains applied (may yet commit)
	view, loaded, err := rdb.GetBossView(bg, bossID)
	if err != nil || !loaded {
		t.Fatalf("redis view: loaded=%v err=%v", loaded, err)
	}
	wantHP := int64(maxHP - veteranDmg - hit) // 450: cancelled hit stays applied
	if view.HP != wantHP {
		t.Fatalf("Redis HP after ctx-cancel = %d, want %d (hit must NOT be compensated on cancel)", view.HP, wantHP)
	}
	if s, present := lbScore(view, "canceller"); !present || s != hit {
		t.Fatalf("canceller score = %d present=%v, want %d (leaderboard delta must remain on cancel)", s, present, hit)
	}
}
