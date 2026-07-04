package recovery

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	reconcileWorkers   = 2
	reconcileQueueSize = 2048
	reconcileTimeout   = 5 * time.Second
)

// Reconciler heals a boss's cache out of band by rebuilding it from Postgres,
// when the damage path could not keep Redis consistent (client cancel, or a
// failed compensating undo). Best-effort: a dropped reconcile self-heals on TTL.
type Reconciler struct {
	rehydr *Rehydrator
	delay  time.Duration
	log    *slog.Logger

	ch        chan string
	wg        sync.WaitGroup
	closeOnce sync.Once

	mu     sync.Mutex
	queued map[string]struct{}
}

// NewReconciler builds a Reconciler. delay is a settle window before each rebuild
// so an in-flight cancelled event can commit first.
func NewReconciler(rehydr *Rehydrator, delay time.Duration, log *slog.Logger) *Reconciler {
	return &Reconciler{
		rehydr: rehydr,
		delay:  delay,
		log:    log,
		ch:     make(chan string, reconcileQueueSize),
		queued: make(map[string]struct{}),
	}
}

// Start launches the background reconcile workers.
func (r *Reconciler) Start() {
	for range reconcileWorkers {
		r.wg.Add(1)
		go r.worker()
	}
}

// Stop closes the intake and waits for in-flight reconciles to finish. Safe to call once.
func (r *Reconciler) Stop() {
	r.closeOnce.Do(func() { close(r.ch) })
	r.wg.Wait()
}

// Enqueue schedules a background rebuild for bossID. Never blocks, coalesces
// duplicates, and drops the request if the intake is saturated.
func (r *Reconciler) Enqueue(bossID string) {
	r.mu.Lock()
	if _, dup := r.queued[bossID]; dup {
		r.mu.Unlock()
		return
	}
	r.queued[bossID] = struct{}{}
	r.mu.Unlock()

	select {
	case r.ch <- bossID:
	default:
		r.mu.Lock()
		delete(r.queued, bossID)
		r.mu.Unlock()
		r.log.Warn("reconcile queue full; boss will self-heal via cache TTL", "boss", bossID)
	}
}

func (r *Reconciler) worker() {
	defer r.wg.Done()
	for bossID := range r.ch {
		if r.delay > 0 {
			time.Sleep(r.delay)
		}
		// Free the slot before rebuild so a mid-rebuild divergence re-enqueues.
		r.mu.Lock()
		delete(r.queued, bossID)
		r.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		if _, err := r.rehydr.RehydrateBoss(ctx, bossID); err != nil {
			r.log.Error("reconcile rehydrate failed; cache TTL remains the backstop",
				"boss", bossID, "err", err)
		}
		cancel()
	}
}
