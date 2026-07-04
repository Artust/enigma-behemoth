//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"behemoth/internal/store"
)

// TestWriter_Durability: once every Submit returns, all events are durably
// committed (audit log + aggregate) - the "200 means durable" contract.
func TestWriter_Durability(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pool := newPool(t)

	bossID := uniqueID("writer")
	seedBoss(t, pool, bossID, 10_000_000, "alive")

	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)

	w := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 1000, MaxBatch: 100, MaxWait: 5 * time.Millisecond, TxTimeout: 5 * time.Second, Concurrency: 4,
	}, testLogger())
	w.Start()

	const n = 250
	const per = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = w.Submit(ctx, store.DamageEvent{BossID: bossID, PlayerID: "p1", Applied: per})
		}(i)
	}
	wg.Wait()
	w.Stop() // flush + ack tail

	for i, e := range errs {
		if e != nil {
			t.Fatalf("submit[%d] returned error: %v", i, e)
		}
	}

	var events int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM damage_events WHERE boss_id = $1 AND player_id = 'p1'`,
		bossID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != n {
		t.Fatalf("durable events = %d, want %d", events, n)
	}
	var total int64
	if err := pool.QueryRow(ctx,
		`SELECT total_damage FROM contributions WHERE boss_id = $1 AND player_id = 'p1'`,
		bossID).Scan(&total); err != nil {
		t.Fatalf("read contribution: %v", err)
	}
	if want := int64(n * per); total != want {
		t.Fatalf("aggregate total_damage = %d, want %d", total, want)
	}
}

// TestWriter_FailFastWhenQueueFull: a saturated buffer makes Submit return
// ErrQueueFull immediately (mapped to HTTP 503) instead of blocking.
func TestWriter_FailFastWhenQueueFull(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)

	const queueSize = 2
	// Not started, so nothing drains the channel.
	w := store.NewWriter(pg, store.WriterConfig{
		QueueSize: queueSize, MaxBatch: 100, MaxWait: time.Second, TxTimeout: time.Second, Concurrency: 1,
	}, testLogger())

	blockCtx, cancel := context.WithCancel(context.Background())
	defer cancel() // release blocked submitters at test end

	// Fill the buffer with blocked submitters.
	for i := 0; i < queueSize; i++ {
		go func() {
			_ = w.Submit(blockCtx, store.DamageEvent{BossID: "x", PlayerID: "p", Applied: 1})
		}()
	}
	time.Sleep(150 * time.Millisecond) // let them occupy the buffer

	err = w.Submit(context.Background(), store.DamageEvent{BossID: "x", PlayerID: "p", Applied: 1})
	if err != store.ErrQueueFull {
		t.Fatalf("Submit on a full queue = %v, want ErrQueueFull", err)
	}
}

// TestWriter_NoHangOnCommitError: a failing commit (FK violation on a ghost boss)
// surfaces the error to Submit within a bound instead of hanging the handler.
func TestWriter_NoHangOnCommitError(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	pg, err := store.NewPostgresStore(ctx, pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)

	w := store.NewWriter(pg, store.WriterConfig{
		QueueSize: 10, MaxBatch: 5, MaxWait: 5 * time.Millisecond, TxTimeout: 3 * time.Second, Concurrency: 2,
	}, testLogger())
	w.Start()
	t.Cleanup(w.Stop)

	// boss id never inserted => CommitBatch fails the FK.
	ev := store.DamageEvent{BossID: uniqueID("ghost"), PlayerID: "p", Applied: 1}

	done := make(chan error, 1)
	go func() { done <- w.Submit(ctx, ev) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Submit returned nil, want the commit error propagated")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Submit hung: a failed commit must release its waiter")
	}
}
