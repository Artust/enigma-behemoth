package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// damageScript applies damage to the boss atomically. Because Redis executes a
// script single-threaded, this serializes all concurrent writers to the same
// boss with no lost updates and no negative HP.
//
// KEYS[1]=hp  KEYS[2]=leaderboard(ZSET)  KEYS[3]=state
// ARGV[1]=player_id  ARGV[2]=damage
//
// Returns {status, applied, newhp}. It credits the leaderboard only with the
// damage that actually landed (min(hp, dmg)), so contributions sum exactly to
// max_hp — making reward percentages total 100%.
var damageScript = redis.NewScript(`
local hp = redis.call('GET', KEYS[1])
if hp == false then
  return {-1, 0, 0}
end
hp = tonumber(hp)
local dmg = tonumber(ARGV[2])
if dmg <= 0 then
  return {-2, 0, hp}
end
if hp <= 0 then
  return {0, 0, hp}
end
local applied = dmg
if applied > hp then applied = hp end
local newhp = hp - applied
redis.call('SET', KEYS[1], newhp)
redis.call('ZINCRBY', KEYS[2], applied, ARGV[1])
if newhp <= 0 then
  redis.call('SET', KEYS[3], 'defeated')
end
return {1, applied, newhp}
`)

// compensateScript reverses a single applied damage. It is used only when the
// durable write could not even be enqueued (writer queue full): Redis has
// already applied the hit but Postgres will never see it, so the cache would
// diverge from durable state — worst case a boss reads 'defeated' in Redis while
// Postgres never reached 0 HP, leaving the reward permanently unclaimable.
//
// It is the exact inverse of damageScript's mutations, and because all edits are
// additive counters, the undo is order-independent w.r.t. concurrent hits: the
// final totals stay consistent with Postgres even if other damage interleaves.
// If the boss key is gone (evicted / not loaded) it does nothing — a later
// rehydrate rebuilds the cache from durable state, which never saw this damage.
//
// KEYS[1]=hp  KEYS[2]=leaderboard(ZSET)  KEYS[3]=state
// ARGV[1]=player_id  ARGV[2]=applied
var compensateScript = redis.NewScript(`
local hp = redis.call('GET', KEYS[1])
if hp == false then
  return -1
end
local amt = tonumber(ARGV[2])
if amt <= 0 then
  return tonumber(hp)
end
local newhp = tonumber(hp) + amt
redis.call('SET', KEYS[1], newhp)
local newscore = redis.call('ZINCRBY', KEYS[2], -amt, ARGV[1])
if tonumber(newscore) <= 0 then
  redis.call('ZREM', KEYS[2], ARGV[1])
end
if newhp > 0 then
  redis.call('SET', KEYS[3], 'alive')
end
return newhp
`)

// RedisStore is the hot-path cache. It is treated as rebuildable: authoritative
// durability lives in Postgres, and Redis is rehydrated on startup / on miss.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore dials Redis and verifies connectivity.
func NewRedisStore(ctx context.Context, addr, password string) (*RedisStore, error) {
	c := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
	})
	if err := c.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &RedisStore{client: c}, nil
}

// Close releases the Redis connection pool.
func (r *RedisStore) Close() error { return r.client.Close() }

// Ping checks liveness.
func (r *RedisStore) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

func hpKey(id string) string    { return "boss:" + id + ":hp" }
func maxKey(id string) string   { return "boss:" + id + ":maxhp" }
func stateKey(id string) string { return "boss:" + id + ":state" }
func lbKey(id string) string    { return "boss:" + id + ":lb" }

// ApplyDamage runs the atomic Lua script and parses its reply.
func (r *RedisStore) ApplyDamage(ctx context.Context, bossID, playerID string, damage int64) (ApplyResult, error) {
	keys := []string{hpKey(bossID), lbKey(bossID), stateKey(bossID)}
	raw, err := damageScript.Run(ctx, r.client, keys, playerID, damage).Slice()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("damage script: %w", err)
	}
	if len(raw) != 3 {
		return ApplyResult{}, fmt.Errorf("unexpected script reply length %d", len(raw))
	}
	return ApplyResult{
		Status:  int(toInt64(raw[0])),
		Applied: toInt64(raw[1]),
		NewHP:   toInt64(raw[2]),
	}, nil
}

// CompensateDamage undoes a previously applied damage in Redis when the durable
// write was rejected outright (queue full), keeping the cache consistent with
// Postgres. See compensateScript for the exact semantics.
func (r *RedisStore) CompensateDamage(ctx context.Context, bossID, playerID string, applied int64) error {
	keys := []string{hpKey(bossID), lbKey(bossID), stateKey(bossID)}
	return compensateScript.Run(ctx, r.client, keys, playerID, applied).Err()
}

// GetBossView reads HP, max HP, state and the Top-10 leaderboard in a single
// round trip. The bool return is false when the boss is not loaded in Redis
// (caller should rehydrate from Postgres).
func (r *RedisStore) GetBossView(ctx context.Context, bossID string) (BossView, bool, error) {
	pipe := r.client.Pipeline()
	hpCmd := pipe.Get(ctx, hpKey(bossID))
	maxCmd := pipe.Get(ctx, maxKey(bossID))
	stateCmd := pipe.Get(ctx, stateKey(bossID))
	lbCmd := pipe.ZRevRangeWithScores(ctx, lbKey(bossID), 0, 9)
	// Exec surfaces redis.Nil if any GET missed; tolerate it and inspect per-command.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return BossView{}, false, fmt.Errorf("boss view pipeline: %w", err)
	}

	hp, err := hpCmd.Int64()
	if errors.Is(err, redis.Nil) {
		return BossView{}, false, nil // not loaded
	}
	if err != nil {
		return BossView{}, false, err
	}
	maxHP, _ := maxCmd.Int64()
	state, err := stateCmd.Result()
	if errors.Is(err, redis.Nil) || state == "" {
		state = "alive"
	}

	entries := lbCmd.Val()
	lb := make([]LeaderEntry, 0, len(entries))
	for _, z := range entries {
		player, _ := z.Member.(string)
		lb = append(lb, LeaderEntry{PlayerID: player, Damage: int64(z.Score)})
	}
	return BossView{BossID: bossID, HP: hp, MaxHP: maxHP, State: state, Leaderboard: lb}, true, nil
}

// Rehydrate overwrites the Redis cache for a boss from durable Postgres state.
// It always wins over whatever (possibly stale) data Redis held.
func (r *RedisStore) Rehydrate(ctx context.Context, bossID string, rs RecoveryState, contribs []LeaderEntry) error {
	pipe := r.client.TxPipeline()
	pipe.Set(ctx, hpKey(bossID), rs.CurrentHP, 0)
	pipe.Set(ctx, maxKey(bossID), rs.MaxHP, 0)
	pipe.Set(ctx, stateKey(bossID), rs.State, 0)
	pipe.Del(ctx, lbKey(bossID))
	if len(contribs) > 0 {
		members := make([]redis.Z, 0, len(contribs))
		for _, c := range contribs {
			members = append(members, redis.Z{Score: float64(c.Damage), Member: c.PlayerID})
		}
		pipe.ZAdd(ctx, lbKey(bossID), members...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("rehydrate boss %s: %w", bossID, err)
	}
	return nil
}

// toInt64 tolerantly coerces a Redis reply element to int64.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
