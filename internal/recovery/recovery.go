// Package recovery rebuilds the Redis hot-path cache from durable Postgres state:
// on startup (all bosses) and lazily on a cache miss (one boss).
package recovery

import (
	"context"
	"log/slog"

	"behemoth/internal/store"
)

// Rehydrator restores Redis from Postgres.
type Rehydrator struct {
	redis *store.RedisStore
	pg    *store.PostgresStore
	log   *slog.Logger
}

func New(r *store.RedisStore, pg *store.PostgresStore, log *slog.Logger) *Rehydrator {
	return &Rehydrator{redis: r, pg: pg, log: log}
}

// RehydrateBoss loads one boss's durable state into Redis. The bool is false
// when the boss does not exist in Postgres.
func (h *Rehydrator) RehydrateBoss(ctx context.Context, bossID string) (bool, error) {
	rs, ok, err := h.pg.RecoveryState(ctx, bossID)
	if err != nil || !ok {
		return false, err
	}
	contribs, err := h.pg.Contributions(ctx, bossID)
	if err != nil {
		return false, err
	}
	if err := h.redis.Rehydrate(ctx, bossID, rs, contribs); err != nil {
		return false, err
	}
	return true, nil
}

// RehydrateAll rebuilds the cache for every known boss, run before the service
// reports ready.
func (h *Rehydrator) RehydrateAll(ctx context.Context) error {
	ids, err := h.pg.ListBossIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := h.RehydrateBoss(ctx, id); err != nil {
			return err
		}
	}
	h.log.Info("rehydrated bosses from postgres", "count", len(ids))
	return nil
}
