package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// WriterConfig tunes the group-commit durable writer. A batch flushes at MaxBatch
// events or MaxWait.
type WriterConfig struct {
	QueueSize   int
	MaxBatch    int
	MaxWait     time.Duration
	TxTimeout   time.Duration
	Concurrency int
}

type writeReq struct {
	ev   DamageEvent
	done chan error
}

// Writer batches DamageEvents from many concurrent handlers into single Postgres
// transactions, amortizing fsync cost across the batch. Submit blocks until the
// batch is durably committed.
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

// Start launches N goroutines that each group-commit from the shared intake,
// overlapping fsync latency across separate connections.
func (w *Writer) Start() {
	for range w.cfg.Concurrency {
		w.wg.Add(1)
		go w.loop()
	}
}

// Submit enqueues an event and blocks until it is durably committed. Returns
// ErrQueueFull if the intake is saturated, or the ctx error if the caller gives up.
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

// Stop closes the intake and waits for buffered events to flush. Safe to call once.
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
				flush()
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

// commit persists one batch and releases every waiter with the outcome. The
// deferred release always runs, so a panic or DB error never leaves a waiter hung.
func (w *Writer) commit(batch []writeReq) {
	var err error
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("writer panic: %v", r)
			w.log.Error("group-commit panic recovered", "panic", r)
		}
		// Tag with ErrCommitFailed: a real commit failure (safe to undo Redis),
		// distinct from a ctx cancel that may still commit.
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrCommitFailed, err)
		}
		for _, req := range batch {
			req.done <- err
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
