// Package boss holds the domain logic: applying damage, reading boss state, and
// claiming rewards. It orchestrates the Redis cache, the durable writer, and
// Postgres, keeping persistence details out of the HTTP layer.
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

// Service is the domain entry point.
type Service struct {
	redis  *store.RedisStore
	pg     *store.PostgresStore
	writer *store.Writer
	rehydr *recovery.Rehydrator
	maxHit int64
	log    *slog.Logger
}

// New builds the domain service.
func New(r *store.RedisStore, pg *store.PostgresStore, w *store.Writer, rh *recovery.Rehydrator, maxHit int64, log *slog.Logger) *Service {
	return &Service{redis: r, pg: pg, writer: w, rehydr: rh, maxHit: maxHit, log: log}
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
		// Redis has recorded the applied damage; now make it durable.
		if err := s.writer.Submit(ctx, store.DamageEvent{
			BossID: bossID, PlayerID: playerID, Applied: res.Applied,
		}); err != nil {
			if errors.Is(err, store.ErrQueueFull) {
				// The event never entered the writer's intake, so it will never be
				// committed to Postgres — yet Redis already applied it. Undo the
				// Redis-side effect so the cache does not diverge from durable state
				// (otherwise a "defeated" boss in Redis could become permanently
				// unclaimable, since Claim gates on Postgres). Only ErrQueueFull is
				// safe to compensate: on a ctx-cancel the event may still be in the
				// channel and will commit, so we must NOT undo there. Use a detached
				// context so the undo completes even if the client has disconnected.
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				defer cancel()
				if cerr := s.redis.CompensateDamage(cctx, bossID, playerID, res.Applied); cerr != nil {
					s.log.Error("compensating undo failed; redis may diverge from postgres until rehydrate",
						"boss", bossID, "player", playerID, "applied", res.Applied, "err", cerr)
				}
				return DamageResult{}, ErrOverloaded
			}
			return DamageResult{}, err
		}
		return DamageResult{
			BossID: bossID, PlayerID: playerID,
			Applied: res.Applied, BossHP: res.NewHP, Defeated: res.NewHP <= 0,
		}, nil
	default:
		return DamageResult{}, ErrBossNotFound
	}
}

// applyWithRehydrate runs the Redis script; on a cold cache (StatusNotLoaded)
// it lazily rehydrates the boss from Postgres and retries once.
func (s *Service) applyWithRehydrate(ctx context.Context, bossID, playerID string, amount int64) (store.ApplyResult, error) {
	res, err := s.redis.ApplyDamage(ctx, bossID, playerID, amount)
	if err != nil {
		return res, err
	}
	if res.Status != store.StatusNotLoaded {
		return res, nil
	}
	// Cold cache: rehydrate from durable state, then retry once.
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
