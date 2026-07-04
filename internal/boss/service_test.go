package boss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"behemoth/internal/store"
)

// discardLogger drops all log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeReconciler records enqueued boss ids for assertions.
type fakeReconciler struct {
	enqueued []string
}

func (f *fakeReconciler) Enqueue(bossID string) { f.enqueued = append(f.enqueued, bossID) }

// TestDamageRejectsInvalidAmount: damage_amount must be positive and within the
// per-hit cap, rejected before any Redis/DB work (so nil stores are fine). The
// valid boundary (amount == maxHit) needs Redis; covered by integration.
func TestDamageRejectsInvalidAmount(t *testing.T) {
	const maxHit = 1000
	s := New(nil, nil, nil, nil, maxHit, discardLogger())

	cases := []struct {
		name   string
		amount int64
	}{
		{"zero", 0},
		{"negative", -1},
		{"large negative", -1_000_000},
		{"one over the cap", maxHit + 1},
		{"far over the cap", maxHit * 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Damage(context.Background(), "boss1", "player1", c.amount)
			if !errors.Is(err, ErrInvalidDamage) {
				t.Fatalf("Damage(amount=%d) err = %v, want ErrInvalidDamage", c.amount, err)
			}
		})
	}
}

// TestReconcileFailedWrite_CtxCancelDoesNotUndo: on a ctx cancel the event may
// still commit, so don't undo Redis; schedule a reconcile and return the error.
// nil RedisStore is deliberate: a wrong undo would panic, proving none happened.
func TestReconcileFailedWrite_CtxCancelDoesNotUndo(t *testing.T) {
	rc := &fakeReconciler{}
	s := New(nil, nil, nil, nil, 1000, discardLogger(), WithReconciler(rc))

	got := s.reconcileFailedWrite(context.Background(), "bossX", "playerY", 42, context.Canceled)

	if !errors.Is(got, context.Canceled) {
		t.Fatalf("returned err = %v, want context.Canceled", got)
	}
	if len(rc.enqueued) != 1 || rc.enqueued[0] != "bossX" {
		t.Fatalf("reconcile enqueued = %v, want exactly [bossX]", rc.enqueued)
	}
}

// TestReconcileFailedWrite_DeadlineExceededDoesNotUndo: same rule for a deadline-
// exceeded ctx: reconcile, surface the error, never undo.
func TestReconcileFailedWrite_DeadlineExceededDoesNotUndo(t *testing.T) {
	rc := &fakeReconciler{}
	s := New(nil, nil, nil, nil, 1000, discardLogger(), WithReconciler(rc))

	got := s.reconcileFailedWrite(context.Background(), "bossX", "playerY", 7, context.DeadlineExceeded)

	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("returned err = %v, want context.DeadlineExceeded", got)
	}
	if len(rc.enqueued) != 1 || rc.enqueued[0] != "bossX" {
		t.Fatalf("reconcile enqueued = %v, want exactly [bossX]", rc.enqueued)
	}
}

// TestReconcileFailedWrite_WrappedCtxCancel: classification uses errors.Is, so a
// wrapped ctx cancel still hits the reconcile-don't-undo branch.
func TestReconcileFailedWrite_WrappedCtxCancel(t *testing.T) {
	rc := &fakeReconciler{}
	s := New(nil, nil, nil, nil, 1000, discardLogger(), WithReconciler(rc))

	wrapped := fmt.Errorf("submit: %w", context.Canceled)
	got := s.reconcileFailedWrite(context.Background(), "bossZ", "p", 1, wrapped)

	if !errors.Is(got, context.Canceled) {
		t.Fatalf("returned err = %v, want it to wrap context.Canceled", got)
	}
	if len(rc.enqueued) != 1 || rc.enqueued[0] != "bossZ" {
		t.Fatalf("reconcile enqueued = %v, want exactly [bossZ]", rc.enqueued)
	}
}

// TestReconcileFailedWrite_UnknownErrorIsPropagated: an unclassified error is
// returned as-is, with no undo (nil RedisStore would panic) and no reconcile.
func TestReconcileFailedWrite_UnknownErrorIsPropagated(t *testing.T) {
	rc := &fakeReconciler{}
	s := New(nil, nil, nil, nil, 1000, discardLogger(), WithReconciler(rc))

	sentinel := errors.New("some unexpected failure")
	got := s.reconcileFailedWrite(context.Background(), "bossX", "playerY", 5, sentinel)

	if !errors.Is(got, sentinel) {
		t.Fatalf("returned err = %v, want the original error propagated", got)
	}
	if len(rc.enqueued) != 0 {
		t.Fatalf("reconcile enqueued = %v, want none for an unclassified error", rc.enqueued)
	}
}

// TestReconcileFailedWrite_QueueFullMapsToOverloaded marks a branch not unit-tested
// here: ErrQueueFull/ErrCommitFailed undo Redis and map to ErrOverloaded, which
// needs a live Redis. Covered by qa/integration.
func TestReconcileFailedWrite_QueueFullMapsToOverloaded(t *testing.T) {
	t.Skip("requires live Redis to run the compensating undo; covered by qa/integration commit_fail/compensate tests")
	_ = store.ErrQueueFull
}
