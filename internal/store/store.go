// Package store holds the persistence layer: the Redis hot-path cache, the
// PostgreSQL source of truth, and the group-commit durable writer.
package store

import (
	"errors"
	"time"
)

// Sentinel errors surfaced to the domain/service layer.
var (
	// ErrBossNotFound means the boss id does not exist in Postgres.
	ErrBossNotFound = errors.New("boss not found")
	// ErrQueueFull means the durable writer's bounded queue is saturated;
	// the caller should fail fast (HTTP 503) rather than block.
	ErrQueueFull = errors.New("durable write queue full")
)

// Apply status codes returned by the Redis damage Lua script.
const (
	StatusApplied   = 1  // damage landed
	StatusDefeated  = 0  // boss already at 0 HP
	StatusNotLoaded = -1 // boss state absent from Redis; caller must rehydrate
	StatusInvalid   = -2 // damage <= 0 (defense-in-depth guard)
)

// DamageEvent is a single durable unit handed to the group-commit writer.
type DamageEvent struct {
	BossID   string
	PlayerID string
	Applied  int64
}

// ApplyResult is the outcome of the atomic Redis damage operation.
type ApplyResult struct {
	Status  int
	Applied int64
	NewHP   int64
}

// LeaderEntry is one row of the Top-N leaderboard.
type LeaderEntry struct {
	PlayerID string `json:"player_id"`
	Damage   int64  `json:"damage"`
}

// BossView is the read model returned by GET /boss/{id}.
type BossView struct {
	BossID      string        `json:"boss_id"`
	Name        string        `json:"name,omitempty"`
	HP          int64         `json:"hp"`
	MaxHP       int64         `json:"max_hp"`
	State       string        `json:"state"`
	Leaderboard []LeaderEntry `json:"leaderboard"`
}

// RecoveryState is the durable state used to rehydrate Redis on startup.
type RecoveryState struct {
	MaxHP     int64
	CurrentHP int64 // derived: max_hp - SUM(contributions), floored at 0
	State     string
}

// ClaimBasis is the durable data needed to authorize and price a reward claim.
type ClaimBasis struct {
	Exists          bool
	State           string
	MaxHP           int64
	HasContribution bool
	TotalDamage     int64
}

// ClaimInput is a fully-priced claim ready to be persisted exactly once.
type ClaimInput struct {
	BossID   string
	PlayerID string
	Tier     string
	Pct      float64
	Payload  []byte
}

// ClaimResult is the persisted claim (either freshly inserted or the existing
// idempotent record).
type ClaimResult struct {
	BossID         string         `json:"boss_id"`
	PlayerID       string         `json:"player_id"`
	Tier           string         `json:"tier"`
	Pct            float64        `json:"damage_pct"`
	Payload        map[string]any `json:"reward"`
	ClaimedAt      time.Time      `json:"claimed_at"`
	AlreadyClaimed bool           `json:"already_claimed"`
}
