package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// damageScript atomically applies damage; returns {status, applied, newhp}.
// Single-threaded in Redis, so hits to one boss serialize. Credits only landed
// damage (min hp, dmg) to the leaderboard.
//
// KEYS[1]=hp  KEYS[2]=leaderboard(ZSET)  KEYS[3]=state
// ARGV[1]=player_id  ARGV[2]=damage  ARGV[3]=ttl_seconds (0 = never expire)
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
local ttl = tonumber(ARGV[3])
redis.call('ZINCRBY', KEYS[2], applied, ARGV[1])
if ttl > 0 then
  redis.call('SET', KEYS[1], newhp, 'EX', ttl)
  redis.call('EXPIRE', KEYS[2], ttl)
  if newhp <= 0 then
    redis.call('SET', KEYS[3], 'defeated', 'EX', ttl)
  else
    redis.call('EXPIRE', KEYS[3], ttl)
  end
else
  redis.call('SET', KEYS[1], newhp)
  if newhp <= 0 then
    redis.call('SET', KEYS[3], 'defeated')
  end
end
return {1, applied, newhp}
`)

// compensateScript is the inverse of damageScript, run when a durable write is
// lost (queue full or commit failed). Additive counters, so safe under concurrent
// hits. No-ops if the key is gone.
//
// KEYS[1]=hp  KEYS[2]=leaderboard(ZSET)  KEYS[3]=state
// ARGV[1]=player_id  ARGV[2]=applied  ARGV[3]=ttl_seconds (0 = never expire)
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
local ttl = tonumber(ARGV[3])
local newscore = redis.call('ZINCRBY', KEYS[2], -amt, ARGV[1])
if tonumber(newscore) <= 0 then
  redis.call('ZREM', KEYS[2], ARGV[1])
end
if ttl > 0 then
  redis.call('SET', KEYS[1], newhp, 'EX', ttl)
  redis.call('EXPIRE', KEYS[2], ttl)
  if newhp > 0 then
    redis.call('SET', KEYS[3], 'alive', 'EX', ttl)
  else
    redis.call('EXPIRE', KEYS[3], ttl)
  end
else
  redis.call('SET', KEYS[1], newhp)
  if newhp > 0 then
    redis.call('SET', KEYS[3], 'alive')
  end
end
return newhp
`)

// RedisStore is the rebuildable hot-path cache; source of truth is Postgres.
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
}

// RedisOption customizes a RedisStore at construction.
type RedisOption func(*RedisStore)

// WithCacheTTL sets a rolling expiry on every cache key. Zero (the default) keeps
// keys persistent.
func WithCacheTTL(d time.Duration) RedisOption {
	return func(r *RedisStore) { r.ttl = d }
}

// NewRedisStore dials Redis and waits (bounded by ctx) until it is ready.
func NewRedisStore(ctx context.Context, addr, password string, opts ...RedisOption) (*RedisStore, error) {
	c := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
	})
	if err := waitRedisReady(ctx, c); err != nil {
		_ = c.Close()
		return nil, err
	}
	r := &RedisStore{client: c}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// waitRedisReady blocks until PING succeeds, retrying transient startup errors
// with capped backoff. Other errors fail immediately.
func waitRedisReady(ctx context.Context, c *redis.Client) error {
	const (
		baseBackoff = 100 * time.Millisecond
		maxBackoff  = 1 * time.Second
	)
	backoff := baseBackoff
	for {
		err := c.Ping(ctx).Err()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !redisStarting(err) {
			return fmt.Errorf("redis ping: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("redis ping: %w", ctx.Err())
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// redisStarting reports whether err is Redis still coming up (replaying AOF, or
// not yet accepting connections), which a startup retry should ride out.
func redisStarting(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "LOADING") || strings.Contains(msg, "refused")
}

// ttlArg is the TTL in whole seconds, as the Lua scripts expect.
func (r *RedisStore) ttlArg() int64 {
	return int64(r.ttl / time.Second)
}

func (r *RedisStore) Close() error { return r.client.Close() }

func (r *RedisStore) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

func hpKey(id string) string    { return "boss:" + id + ":hp" }
func maxKey(id string) string   { return "boss:" + id + ":maxhp" }
func stateKey(id string) string { return "boss:" + id + ":state" }
func lbKey(id string) string    { return "boss:" + id + ":lb" }

// ApplyDamage runs the atomic Lua script and parses its reply.
func (r *RedisStore) ApplyDamage(ctx context.Context, bossID, playerID string, damage int64) (ApplyResult, error) {
	keys := []string{hpKey(bossID), lbKey(bossID), stateKey(bossID)}
	raw, err := damageScript.Run(ctx, r.client, keys, playerID, damage, r.ttlArg()).Slice()
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

// CompensateDamage undoes an applied hit when its durable write was lost. See compensateScript.
func (r *RedisStore) CompensateDamage(ctx context.Context, bossID, playerID string, applied int64) error {
	keys := []string{hpKey(bossID), lbKey(bossID), stateKey(bossID)}
	return compensateScript.Run(ctx, r.client, keys, playerID, applied, r.ttlArg()).Err()
}

// GetBossView reads HP, max HP, state and the Top-10 leaderboard in one round
// trip. The bool is false when the boss is not loaded.
func (r *RedisStore) GetBossView(ctx context.Context, bossID string) (BossView, bool, error) {
	pipe := r.client.Pipeline()
	hpCmd := pipe.Get(ctx, hpKey(bossID))
	maxCmd := pipe.Get(ctx, maxKey(bossID))
	stateCmd := pipe.Get(ctx, stateKey(bossID))
	lbCmd := pipe.ZRevRangeWithScores(ctx, lbKey(bossID), 0, 9)
	// redis.Nil on a miss is expected; inspect per command below.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return BossView{}, false, fmt.Errorf("boss view pipeline: %w", err)
	}

	hp, err := hpCmd.Int64()
	if errors.Is(err, redis.Nil) {
		return BossView{}, false, nil
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

// Rehydrate overwrites the cache for a boss from durable Postgres state. max_hp
// stays persistent (no TTL) so an actively-hit boss never reads a missing max.
func (r *RedisStore) Rehydrate(ctx context.Context, bossID string, rs RecoveryState, contribs []LeaderEntry) error {
	pipe := r.client.TxPipeline()
	pipe.Set(ctx, hpKey(bossID), rs.CurrentHP, r.ttl)
	pipe.Set(ctx, maxKey(bossID), rs.MaxHP, 0)
	pipe.Set(ctx, stateKey(bossID), rs.State, r.ttl)
	pipe.Del(ctx, lbKey(bossID))
	if len(contribs) > 0 {
		members := make([]redis.Z, 0, len(contribs))
		for _, c := range contribs {
			members = append(members, redis.Z{Score: float64(c.Damage), Member: c.PlayerID})
		}
		pipe.ZAdd(ctx, lbKey(bossID), members...)
		if r.ttl > 0 {
			pipe.Expire(ctx, lbKey(bossID), r.ttl)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("rehydrate boss %s: %w", bossID, err)
	}
	return nil
}

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
