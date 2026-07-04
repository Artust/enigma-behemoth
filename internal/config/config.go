// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all tunables, each overridable via env.
type Config struct {
	HTTPAddr        string
	PostgresDSN     string
	RedisAddr       string
	RedisPassword   string
	RedisCacheTTL   time.Duration
	ReconcileDelay  time.Duration
	MaxDamagePerHit int64

	// Group-commit durable writer tuning.
	WriterQueueSize   int
	WriterConcurrency int
	BatchMaxSize      int
	BatchMaxWait      time.Duration
	BatchTxTimeout    time.Duration
	PGMaxConns        int

	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment, applying sensible defaults.
func Load() (Config, error) {
	c := Config{
		HTTPAddr:          env("HTTP_ADDR", ":8080"),
		PostgresDSN:       env("POSTGRES_DSN", "postgres://behemoth:behemoth@localhost:5432/behemoth?sslmode=disable"),
		RedisAddr:         env("REDIS_ADDR", "localhost:6379"),
		RedisPassword:     env("REDIS_PASSWORD", ""),
		RedisCacheTTL:     envDuration("REDIS_CACHE_TTL", 60*time.Second),
		ReconcileDelay:    envDuration("RECONCILE_DELAY", 3*time.Second),
		MaxDamagePerHit:   envInt64("MAX_DAMAGE_PER_HIT", 1_000_000_000),
		WriterQueueSize:   envInt("WRITER_QUEUE_SIZE", 20_000),
		WriterConcurrency: envInt("WRITER_CONCURRENCY", 8),
		BatchMaxSize:      envInt("BATCH_MAX_SIZE", 500),
		BatchMaxWait:      envDuration("BATCH_MAX_WAIT", 10*time.Millisecond),
		BatchTxTimeout:    envDuration("BATCH_TX_TIMEOUT", 5*time.Second),
		PGMaxConns:        envInt("PG_MAX_CONNS", 20),
		ShutdownTimeout:   envDuration("SHUTDOWN_TIMEOUT", 15*time.Second),
	}
	if c.MaxDamagePerHit <= 0 {
		return c, fmt.Errorf("MAX_DAMAGE_PER_HIT must be > 0")
	}
	if c.BatchMaxSize <= 0 || c.WriterQueueSize <= 0 {
		return c, fmt.Errorf("batch/queue sizes must be > 0")
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
