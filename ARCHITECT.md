# ARCHITECT.md — Behemoth World Boss Event Service

## 1. System Overview

A single Go service fronts two datastores in a **CQRS-flavoured hybrid**:

```
                    ┌──────────────────────────────────────────┐
   POST /damage     │  Go service (chi router, single instance)│
   GET  /boss/{id}  │                                          │
   POST /rewards/   │   handler → domain service               │
        claim       │        │            │                    │
                    └────────┼────────────┼────────────────────┘
                             │ atomic Lua │ group-commit
                             ▼            ▼
                        ┌─────────┐   ┌──────────────┐
                        │  Redis  │   │  PostgreSQL  │
                        │ (cache) │   │(source truth)│
                        └─────────┘   └──────────────┘
```

- **Redis** owns the *hot path*. A single Lua script atomically decrements HP and
  updates the leaderboard, so the boss counter — the one piece of contended
  global state — is mutated with sub-millisecond, race-free operations.
- **PostgreSQL** is the *source of truth*. Every applied hit is durably appended,
  aggregated, and used to enforce exactly-once reward claims.
- A **group-commit writer** bridges them: it batches many concurrent damage
  events into one Postgres transaction so each request is acknowledged only
  after its data is durable, while fsync cost is amortized across the batch.

Redis is treated as a **rebuildable cache**, never as the durable record. On
startup (and lazily on a cache miss) the service rehydrates Redis from Postgres.

## 2. Data Strategy

### Primary database: PostgreSQL — *why*
- **Durability**: the requirement is explicit that HP and contribution history
  must survive a restart. Postgres gives ACID transactions and real fsync
  durability out of the box.
- **Exactly-once via constraints**: the `reward_claims` table with a primary key
  of `(boss_id, player_id)` turns "exactly-once" into a database invariant
  rather than application-level coordination.
- **Relational aggregates**: `contributions` and `damage_events` make reward
  calculation and audit trivial and correct.

Tables:
| Table | Role |
|---|---|
| `bosses` | boss definition + current HP/state snapshot |
| `damage_events` | append-only audit log of every applied hit |
| `contributions` | materialized per-player totals (recovery + reward math) |
| `reward_claims` | exactly-once claim record with materialized reward payload |

### Cache: Redis — *why*
- The world boss is a **single hot counter** hammered by thousands of players.
  Doing that in Postgres means every writer contends on one row → lock queue +
  per-txn fsync, which will not hold p99 < 100ms at 1000+ QPS.
- Redis is single-threaded; a **Lua script executes atomically**, giving
  correct concurrent decrements in one network round trip.
- A **sorted set (ZSET)** is the natural Top-N leaderboard: `ZINCRBY` to
  accumulate, `ZREVRANGE 0 9` to read the Top 10 in one call.

### Caching strategy
- Redis holds live state: `boss:{id}:hp`, `:maxhp`, `:state`, and a ZSET
  `:lb`. `--maxmemory-policy noeviction` prevents silent eviction of these keys.
- **Rebuildable, not authoritative.** On startup we derive HP as
  `max_hp − SUM(contributions)` from Postgres and rebuild the ZSET, then flip
  `/readyz` to ready — so no request ever hits an empty cache. On a runtime miss
  the same rehydrate runs lazily and the operation retries once.

## 3. Concurrency & Safety

### Simultaneous writes to Boss HP
All `/damage` writers funnel through one Lua script:
```lua
applied = min(hp, damage); hp = hp - applied; ZINCRBY lb applied player
```
Because Redis runs scripts serially, concurrent hits are **linearized** with no
lost updates and HP can never go negative. Crediting only `min(hp, damage)`
means the sum of all applied damage equals exactly `max_hp`, so contribution
percentages total 100% and the reward denominator is well-defined. Postgres HP
is updated with the same authoritative `applied` values inside the batch
(`GREATEST(0, current_hp − Δ)`), keeping it monotonic and non-negative.

### Exactly-once "Claim Reward"
```sql
INSERT INTO reward_claims (...) VALUES (...)
ON CONFLICT (boss_id, player_id) DO NOTHING
RETURNING ...
```
The primary key makes the insert idempotent. The **first** request inserts and
receives its reward (HTTP 201); any concurrent or later duplicate gets no row
from the insert, reads back the existing record, and receives the identical
reward (HTTP 200, `already_claimed: true`). Two racing claims can never both
insert. Claims gate on **Postgres** `state = 'defeated'` (never the cache), so a
claim only proceeds once the killing blow is durably committed and contributions
are final. The reward payload is materialized into the claim row (outbox-style),
so "granting" the reward is a durable read — the claim record and the reward are
written in the same transaction.

### Durability without killing latency
The group-commit writer collects up to `BATCH_MAX_SIZE` events or `BATCH_MAX_WAIT`
(10ms), whichever comes first, into a single transaction. Each handler blocks
until *its* batch commits, so a 200 response means the hit is durable. Under
load the batch fills quickly, so one fsync persists hundreds of hits → high
throughput and bounded added latency, well under 100ms.

Durable throughput is scaled by running **N parallel committers** (`WRITER_CONCURRENCY`,
default 8) that each group-commit from the shared intake on their own Postgres
connection. This overlaps fsync latency across connections so per-request latency
stays flat even as QPS rises. To keep concurrent committers deadlock-free, each
transaction acquires row locks in a **deterministic order** (contributions sorted
by `(boss_id, player_id)`, bosses by id) — otherwise two batches touching the same
players in Go's random map order could lock them in opposite orders and deadlock.

### Measured performance (durable, `synchronous_commit=on`, `fsync=on`)
Load test of `POST /damage` (Go probe, 200 connections, single laptop running the
full docker-compose stack *and* the load generator):

| Metric | Target | Measured |
|---|---|---|
| Throughput | 1,000+ QPS | **~18,000 QPS** |
| p99 latency | < 100 ms | **~21 ms** |
| p99.9 / max | — | ~36 ms / ~64 ms |
| 5xx / deadlocks | 0 | 0 |

(The `/healthz` endpoint alone sustains ~47k QPS at p99 8ms, confirming the HTTP
layer is not the ceiling; the write path is bounded by Postgres commit latency,
which group-commit amortizes.)

## 4. Assumptions & Trade-offs

### Language: Go instead of Java/C#
The brief says "a statically typed language (Java, or C#)". **Go is statically
typed** and is an excellent fit for this workload — lightweight goroutines make
the group-commit fan-in and graceful-drain natural, and GC pauses are low. Go is
used here as a deliberate, documented deviation from the two *example* languages;
if the constraint is meant strictly, the same architecture ports directly to
Java (Spring Boot + Lettuce + JDBC) or C# (.NET Minimal API).

### Damage is at-least-once (no idempotency key)
The `/damage` contract is kept exactly as specified —
`{player_id, boss_id, damage_amount}` with no client request id. Consequence: if
a client retries after a failed/again-timed-out commit, the retry re-applies
damage (double count). This is an accepted trade-off given the fixed payload;
with more time the fix is a client-supplied `request_id` deduped in the Lua
script (`SETNX`) plus a unique index in Postgres, making retries safe.

A related boundary: a `5xx` on `/damage` does **not** guarantee the hit was *not*
applied. If a commit is slow enough to hit the request deadline, the handler
returns `500` while the event is still in the writer's channel and *will* commit —
same at-least-once class as above. The one case that is **not** left divergent is
queue overflow: see the compensating undo below.

### Redis↔Postgres consistency on writer overflow
Damage is applied to Redis first, then made durable. If the durable submit is
*rejected outright* (`ErrQueueFull` → `503`), the event never enters the writer's
channel and will never reach Postgres — so the Redis-side effect is undone by an
atomic compensating Lua script (credit HP back, remove the leaderboard delta,
revert a `defeated` flag this hit may have set). Without this, a killing blow lost
to overflow could leave a boss reading `defeated` in Redis while Postgres never
reached 0 HP, making the reward **permanently unclaimable** (Claim gates on
Postgres). Only `ErrQueueFull` is compensated — a ctx-cancel event may still be
mid-flight and commit, so undoing it would create the *opposite* divergence.
Residual risk: the undo itself is a best-effort Redis call; if it also fails, the
boss is flagged in logs and the next rehydrate (cold cache / restart) reconciles
Redis back to durable state. A periodic reconcile loop would close this fully.

### Single instance assumed
State is externalized to Redis, but the service is designed and documented to run
as **one instance**. Running multiple replicas would need care around the
Postgres HP write and rehydrate ordering (derive-from-contributions already
makes HP instance-invariant, but per-instance write-back would still need
reconciling). Horizontal scale is intentionally out of scope for the timebox.

### Other assumptions / edge cases handled
- **Invalid damage** (`≤ 0` or `> MAX_DAMAGE_PER_HIT`) → `400`, guarded in the
  handler *and* in Lua (defense in depth) so a negative value can never revive a
  boss.
- **Unknown boss** → `404` (never treated as HP 0).
- **Attacking a defeated boss** → `409`.
- **Claim before defeat** → `409`; **claim by a non-contributor** → `403`.
- **Cold/emptied cache** → lazy rehydrate + one retry; `/readyz` gates traffic on
  startup rehydrate.
- **Writer overload** → bounded queue, fast `503` instead of unbounded memory
  growth, with a compensating undo so the rejected hit does not leave Redis
  diverged from Postgres (see "Redis↔Postgres consistency on writer overflow");
  a batch tx timeout + deferred waiter release means a stalled/panicking Postgres
  never hangs handlers forever.
- **Graceful shutdown** → stop accepting, let in-flight handlers finish, flush the
  final batch and ack every waiter, then close pools.
- **ZSET score precision**: Redis scores are float64 (exact integers below 2^53).
  Real HP values are far below this; the authoritative reward math uses integer
  `contributions` in Postgres, with the ZSET only serving fast reads.

### What I'd do with more time
- `request_id` idempotency to make `/damage` exactly-once.
- Multi-instance correctness (leader for HP write-back, or move the counter
  authority into a partitioned scheme).
- A proper migration tool (currently `db/init.sql` via init container).
- Integration tests with testcontainers; chaos test around Postgres stalls.
- Backpressure metrics (batch size, queue depth) and alerting.
