//go:build integration

// Package integration holds integration tests against a live Redis + Postgres.
//
// Gated behind the `integration` build tag and the REDIS_ADDR / POSTGRES_DSN env
// vars. Each test uses a unique boss/player id and cleans up, so it is safe
// against a shared database.
//
//	REDIS_ADDR=localhost:16379 \
//	POSTGRES_DSN=postgres://behemoth:behemoth@localhost:15432/behemoth?sslmode=disable \
//	go test -tags=integration -race -v ./qa/integration/...
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"behemoth/internal/store"
)

var (
	redisAddr string
	pgDSN     string
	enabled   bool
)

// pgMaxConns is the pool size for store.PostgresStore in these tests.
const pgMaxConns int32 = 10

func TestMain(m *testing.M) {
	redisAddr = os.Getenv("REDIS_ADDR")
	pgDSN = os.Getenv("POSTGRES_DSN")
	enabled = redisAddr != "" && pgDSN != ""
	if !enabled {
		fmt.Fprintln(os.Stderr,
			"[qa/integration] REDIS_ADDR and POSTGRES_DSN not set - skipping integration tests")
	}
	os.Exit(m.Run())
}

// requireEnv skips a test unless the live-infra env is configured.
func requireEnv(t *testing.T) {
	t.Helper()
	if !enabled {
		t.Skip("integration env (REDIS_ADDR, POSTGRES_DSN) not set")
	}
}

// testLogger discards writer/recovery logs during tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// uniqueID returns an id unique to this run to avoid collisions.
func uniqueID(prefix string) string {
	return fmt.Sprintf("qa-%s-%d", prefix, time.Now().UnixNano())
}

// openPG opens a live store.PostgresStore with test cleanup registered.
func openPG(t *testing.T) *store.PostgresStore {
	t.Helper()
	pg, err := store.NewPostgresStore(context.Background(), pgDSN, pgMaxConns)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// openRedis opens a live store.RedisStore with test cleanup registered.
func openRedis(t *testing.T) *store.RedisStore {
	t.Helper()
	rdb, err := store.NewRedisStore(context.Background(), redisAddr, "")
	if err != nil {
		t.Fatalf("redis store: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// newPool opens a raw pgx pool for setup/teardown/assertions via direct SQL.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("postgres ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newRedisClient opens a raw go-redis client for setup/cleanup of keys.
func newRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// seedBoss inserts a fresh boss row and registers cleanup of all its rows.
func seedBoss(t *testing.T, pool *pgxpool.Pool, id string, maxHP int64, state string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO bosses (id, name, max_hp, current_hp, state)
		VALUES ($1, $1, $2, $2, $3)
		ON CONFLICT (id) DO UPDATE
		  SET max_hp = $2, current_hp = $2, state = $3, defeated_at = NULL`,
		id, maxHP, state)
	if err != nil {
		t.Fatalf("seed boss %s: %v", id, err)
	}
	t.Cleanup(func() { cleanupBoss(pool, id) })
}

// cleanupBoss removes a boss and all of its child rows (best effort).
func cleanupBoss(pool *pgxpool.Pool, id string) {
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM reward_claims WHERE boss_id = $1`,
		`DELETE FROM damage_events WHERE boss_id = $1`,
		`DELETE FROM contributions WHERE boss_id = $1`,
		`DELETE FROM bosses WHERE id = $1`,
	} {
		_, _ = pool.Exec(ctx, q, id)
	}
}

// delRedisBoss clears the cache keys for a boss id.
func delRedisBoss(t *testing.T, id string) {
	t.Helper()
	c := newRedisClient(t)
	ctx := context.Background()
	c.Del(ctx, "boss:"+id+":hp", "boss:"+id+":maxhp", "boss:"+id+":state", "boss:"+id+":lb")
}
