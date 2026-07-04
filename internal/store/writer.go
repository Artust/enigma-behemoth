package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// WriterConfig tunes the group-commit durable writer.
type WriterConfig struct {
	QueueSize   int           // bounded intake capacity; full => ErrQueueFull
	MaxBatch    int           // flush after this many events
	MaxWait     time.Duration // ...or after this long, whichever first
	TxTimeout   time.Duration // per-batch DB transaction timeout
	Concurrency int           // number of parallel committer goroutines (sharded group-commit)
}

type writeReq struct {
	ev   DamageEvent
	done chan error
}

// Writer batches DamageEvents from many concurrent handlers into single
// Postgres transactions. Each handler blocks on Submit until the batch
// containing its event is durably committed — giving true durability while
// amortizing fsync cost across the batch to keep p99 low.
type Writer struct {
	pg  *PostgresStore
	cfg WriterConfig
	log *slog.Logger

	ch       chan writeReq
	wg       sync.WaitGroup
	closeOne sync.Once
}

// NewWriter constructs a writer. Call Start to launch its background loops.
func NewWriter(pg *PostgresStore, cfg WriterConfig, log *slog.Logger) *Writer {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return &Writer{
		pg:  pg,
		cfg: cfg,
		log: log,
		ch:  make(chan writeReq, cfg.QueueSize),
	}
}

// Start launches N batching goroutines that each independently group-commit
// from the shared intake. Parallel committers overlap fsync latency across
// separate connections, raising durable write throughput and cutting queue wait.
func (w *Writer) Start() {
	for i := 0; i < w.cfg.Concurrency; i++ {
		w.wg.Add(1)
		go w.loop()
	}
}

// Submit enqueues an event and blocks until it is durably committed. It returns
// ErrQueueFull immediately if the bounded intake is saturated (fail fast), or
// the request context's error if the caller gives up waiting.
func (w *Writer) Submit(ctx context.Context, ev DamageEvent) error {
	req := writeReq{ev: ev, done: make(chan error, 1)}
	select {
	case w.ch <- req:
	default:
		return ErrQueueFull
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop closes the intake and waits for all buffered events to be flushed and
// their waiters acked. Safe to call once.
func (w *Writer) Stop() {
	w.closeOne.Do(func() { close(w.ch) })
	w.wg.Wait()
}

func (w *Writer) loop() {
	defer w.wg.Done()

	batch := make([]writeReq, 0, w.cfg.MaxBatch)
	timer := time.NewTimer(w.cfg.MaxWait)
	timer.Stop()
	timerActive := false

	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.commit(batch)
		batch = batch[:0]
		if timerActive {
			timer.Stop()
			timerActive = false
		}
	}

	for {
		select {
		case req, ok := <-w.ch:
			if !ok {
				flush() // drain on shutdown
				return
			}
			batch = append(batch, req)
			if len(batch) >= w.cfg.MaxBatch {
				flush()
			} else if !timerActive {
				timer.Reset(w.cfg.MaxWait)
				timerActive = true
			}
		case <-timer.C:
			timerActive = false
			flush()
		}
	}
}

// commit persists one batch and releases every waiter with the outcome. A
// panic or DB error never leaves a handler hung: the deferred release always
// runs and every done channel receives the (possibly error) result.
func (w *Writer) commit(batch []writeReq) {
	var err error
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("writer panic: %v", r)
			w.log.Error("group-commit panic recovered", "panic", r)
		}
		for _, req := range batch {
			req.done <- err // done is buffered (cap 1) => never blocks
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), w.cfg.TxTimeout)
	defer cancel()

	events := make([]DamageEvent, len(batch))
	for i, req := range batch {
		events[i] = req.ev
	}
	err = w.pg.CommitBatch(ctx, events)
	if err != nil {
		w.log.Error("group-commit batch failed", "size", len(batch), "err", err)
	}
}

// decodePayload turns stored JSONB bytes into a map for the API response.
func decodePayload(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
