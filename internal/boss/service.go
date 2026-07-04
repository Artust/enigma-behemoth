// Package boss holds the domain logic: apply damage, read boss state, claim
// rewards, over the Redis cache, durable writer, and Postgres.
package boss

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"behemoth/internal/recovery"
	"behemoth/internal/store"
)

// Domain errors. The API layer maps these to HTTP status codes.
var (
	ErrInvalidDamage       = errors.New("damage_amount must be a positive integer within limits")
	ErrBossNotFound        = errors.New("boss not found")
	ErrBossAlreadyDefeated = errors.New("boss already defeated")
	ErrBossNotDefeated     = errors.New("boss not yet defeated")
	ErrNoContribution      = errors.New("player has no contribution to this boss")
	ErrOverloaded          = errors.New("service overloaded, retry shortly")
)

// compensateTimeout bounds the detached compensating undo.
const compensateTimeout = 2 * time.Second

// Reconciler schedules an out-of-band cache rebuild for a boss that may have
// diverged from Postgres. A nil reconciler leaves the cache TTL as the backstop.
type Reconciler interface {
	Enqueue(bossID string)
}

// Service is the domain entry point.
type Service struct {
	redis      *store.RedisStore
	pg         *store.PostgresStore
	writer     *store.Writer
	rehydr     *recovery.Rehydrator
	maxHit     int64
	log        *slog.Logger
	reconciler Reconciler
}

// Option customizes a Service.
type Option func(*Service)

// WithReconciler wires a background cache reconciler.
func WithReconciler(rc Reconciler) Option {
	return func(s *Service) { s.reconciler = rc }
}

// New builds the domain service.
func New(r *store.RedisStore, pg *store.PostgresStore, w *store.Writer, rh *recovery.Rehydrator, maxHit int64, log *slog.Logger, opts ...Option) *Service {
	s := &Service{redis: r, pg: pg, writer: w, rehydr: rh, maxHit: maxHit, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// DamageResult is returned by Damage.
type DamageResult struct {
	BossID   string `json:"boss_id"`
	PlayerID string `json:"player_id"`
	Applied  int64  `json:"damage_applied"`
	BossHP   int64  `json:"boss_hp"`
	Defeated bool   `json:"defeated"`
}

// Damage validates, applies damage atomically in Redis, then blocks until the
// event is durably committed before returning success.
func (s *Service) Damage(ctx context.Context, bossID, playerID string, amount int64) (DamageResult, error) {
	if amount <= 0 || amount > s.maxHit {
		return DamageResult{}, ErrInvalidDamage
	}

	res, err := s.applyWithRehydrate(ctx, bossID, playerID, amount)
	if err != nil {
		return DamageResult{}, err
	}

	switch res.Status {
	case store.StatusInvalid:
		return DamageResult{}, ErrInvalidDamage
	case store.StatusDefeated:
		return DamageResult{}, ErrBossAlreadyDefeated
	case store.StatusApplied:
		if err := s.writer.Submit(ctx, store.DamageEvent{
			BossID: bossID, PlayerID: playerID, Applied: res.Applied,
		}); err != nil {
			return DamageResult{}, s.reconcileFailedWrite(ctx, bossID, playerID, res.Applied, err)
		}
		return DamageResult{
			BossID: bossID, PlayerID: playerID,
			Applied: res.Applied, BossHP: res.NewHP, Defeated: res.NewHP <= 0,
		}, nil
	default:
		return DamageResult{}, ErrBossNotFound
	}
}

// reconcileFailedWrite keeps the cache consistent after a durable write fails and
// maps the failure to a domain error. Redis already applied the hit; on a definite
// non-persist (queue full / commit failed) undo it, on a ctx cancel it may still
// commit so reconcile instead.
func (s *Service) reconcileFailedWrite(ctx context.Context, bossID, playerID string, applied int64, err error) error {
	switch {
	case errors.Is(err, store.ErrQueueFull), errors.Is(err, store.ErrCommitFailed):
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensateTimeout)
		defer cancel()
		if cerr := s.redis.CompensateDamage(cctx, bossID, playerID, applied); cerr != nil {
			s.log.Error("compensating undo failed; scheduling background reconcile",
				"boss", bossID, "player", playerID, "applied", applied, "err", cerr)
			s.scheduleReconcile(bossID)
		}
		if errors.Is(err, store.ErrQueueFull) {
			return ErrOverloaded
		}
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.scheduleReconcile(bossID)
		return err
	default:
		return err
	}
}

// scheduleReconcile fires a best-effort background rebuild, if configured.
func (s *Service) scheduleReconcile(bossID string) {
	if s.reconciler != nil {
		s.reconciler.Enqueue(bossID)
	}
}

// applyWithRehydrate runs the Redis script; on a cold cache it rehydrates the
// boss from Postgres and retries once.
func (s *Service) applyWithRehydrate(ctx context.Context, bossID, playerID string, amount int64) (store.ApplyResult, error) {
	res, err := s.redis.ApplyDamage(ctx, bossID, playerID, amount)
	if err != nil {
		return res, err
	}
	if res.Status != store.StatusNotLoaded {
		return res, nil
	}
	exists, err := s.rehydr.RehydrateBoss(ctx, bossID)
	if err != nil {
		return res, err
	}
	if !exists {
		return store.ApplyResult{}, ErrBossNotFound
	}
	return s.redis.ApplyDamage(ctx, bossID, playerID, amount)
}

// Get returns current HP and the Top-10 leaderboard, rehydrating on a miss.
func (s *Service) Get(ctx context.Context, bossID string) (store.BossView, error) {
	view, loaded, err := s.redis.GetBossView(ctx, bossID)
	if err != nil {
		return store.BossView{}, err
	}
	if loaded {
		return view, nil
	}
	exists, err := s.rehydr.RehydrateBoss(ctx, bossID)
	if err != nil {
		return store.BossView{}, err
	}
	if !exists {
		return store.BossView{}, ErrBossNotFound
	}
	view, _, err = s.redis.GetBossView(ctx, bossID)
	return view, err
}

// Claim authorizes and persists a reward exactly once. It gates on the durable
// Postgres state, never the cache.
func (s *Service) Claim(ctx context.Context, bossID, playerID string) (store.ClaimResult, error) {
	basis, err := s.pg.ClaimBasis(ctx, bossID, playerID)
	if err != nil {
		return store.ClaimResult{}, err
	}
	if !basis.Exists {
		return store.ClaimResult{}, ErrBossNotFound
	}
	if basis.State != "defeated" {
		return store.ClaimResult{}, ErrBossNotDefeated
	}
	if !basis.HasContribution {
		return store.ClaimResult{}, ErrNoContribution
	}

	pct := float64(basis.TotalDamage) / float64(basis.MaxHP) * 100
	tier := TierFor(pct)
	payload, _ := json.Marshal(RewardFor(tier))

	return s.pg.SaveClaim(ctx, store.ClaimInput{
		BossID: bossID, PlayerID: playerID, Tier: tier, Pct: pct, Payload: payload,
	})
}
