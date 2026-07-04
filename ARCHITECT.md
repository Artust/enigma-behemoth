# ARCHITECT.md — Behemoth World Boss Event Service

## 1. Overview

A single Go service (chi router) fronts two datastores in a CQRS-flavoured hybrid:

```
   POST /damage        handler → domain service
   GET  /boss/{id}          │            │
   POST /rewards/claim      ▼            ▼
                        ┌─────────┐  ┌──────────────┐
                        │  Redis  │  │  PostgreSQL  │
                        │ (cache) │  │(source truth)│
                        └─────────┘  └──────────────┘
```

- **Redis = hot path.** One Lua script atomically decrements HP and updates the
  leaderboard (ZSET) — the contended global counter is mutated race-free in one
  round trip (sub-millisecond, §3).
- **PostgreSQL = source of truth.** Every applied hit is durably appended and
  aggregated; a PK constraint enforces exactly-once reward claims.
- **Group-commit writer** bridges them: many concurrent damage events are batched
  into one Postgres transaction so each request acks only after its data is
  durable, while fsync cost is amortized across the batch.

Redis is a **rebuildable cache**, never the durable record. On startup (and lazily
on cache miss) HP is derived as `max_hp − SUM(contributions)` from Postgres and the
ZSET is rebuilt; `/readyz` gates traffic until rehydrate completes.

## 2. Data strategy

**PostgreSQL (primary)** — durability is required (HP + history survive restart);
ACID + real fsync give it. Exactly-once is a DB invariant via `reward_claims` PK
`(boss_id, player_id)`. Tables: `bosses` (HP/state snapshot), `damage_events`
(append-only audit), `contributions` (per-player totals for reward math),
`reward_claims` (claim + materialized reward payload).

**Redis (cache)** — a world boss is one hot counter hammered by thousands. Doing
that in Postgres means every writer contends on one row (lock queue + per-txn
fsync); measured, that baseline collapses 7,942→3,382 TPS and crosses p99 100 ms
at ~90 writers (§3). Redis is single-threaded so a Lua script runs atomically; a
ZSET is the natural Top-N (`ZINCRBY` to accumulate, `ZREVRANGE 0 9` to read).
Keys `boss:{id}:{hp,maxhp,state,lb}` with `noeviction` to prevent silent loss.

## 3. Concurrency & safety

**Simultaneous HP writes.** All `/damage` writers funnel through one Lua script:
`applied = min(hp, damage); hp -= applied; ZINCRBY lb applied player`. Redis runs
scripts serially → hits are linearized, no lost updates, HP never negative.
Crediting `min(hp, damage)` means total applied damage equals exactly `max_hp` once
defeated, so contribution percentages total 100%. Postgres HP is updated with the
same `applied` values inside the batch (`GREATEST(0, hp − Δ)`), kept monotonic.

**Exactly-once claim.** `INSERT ... ON CONFLICT (boss_id, player_id) DO NOTHING
RETURNING`. The first request inserts and gets its reward; duplicates read back the
same record and get the identical reward. Both return **HTTP 200**, distinguished
only by an `already_claimed` flag. Claims gate on **Postgres** `state='defeated'`
(never the cache), so a claim only proceeds once the killing blow is durably
committed. The reward payload is materialized into the claim row (outbox-style), so
granting is a durable read written in the same transaction.

**Durability without killing latency.** The writer collects up to `BATCH_MAX_SIZE`
events or `BATCH_MAX_WAIT` (10 ms), whichever first, into one transaction; each
handler blocks until *its* batch commits (a 200 means durable). Throughput scales
via **N parallel committers** (`WRITER_CONCURRENCY`, default 8) on separate
connections, overlapping fsync latency. Each txn locks rows in a **deterministic
order** (`contributions` by `(boss_id, player_id)`, `bosses` by id) to stay
deadlock-free under concurrent committers.

### Measured performance (durable, `synchronous_commit=on`)

Single laptop running both the stack (`behemoth-qa`, `WRITER_CONCURRENCY=8`) and the
load generator — pessimistic co-located setup. Reproducible via `qa/bench/README.md`.

`POST /damage`, p99 latency (ms) vs offered QPS, `boss-load` @ 138k contributors:

| QPS | 1k | 2k | 4k | 8k | 12k | 16k |
|---|---|---|---|---|---|---|
| p99 | 13.2 | 12.1 | 14.4 | 26.1 | 248.6 | 667.7 |
| errors | 0% | 0% | 0% | 0% | 0.03% | 0% |

p99 stays **≈12–26 ms (4–8× under the 100 ms budget) up to 8,000 QPS** (8× the
requirement) with zero errors. Knee ~12k QPS; box saturates ~13k actual. The write
path is bounded by Postgres commit latency, which group-commit amortizes.

- **Baseline (naive single-row Postgres UPDATE, fsync/hit):** 7,942 TPS @ 1 writer →
  3,382 TPS @ 90 writers, p99 crossing 122 ms — anti-scaling that motivates moving
  the counter into Redis. (`qa/bench/pg_single_row.sh`)
- **Redis Lua hot path:** sub-ms (p99 0.071 ms @ 1 client, 0.50 ms @ 50), ~160k
  ops/s — ~12× the app ceiling, confirming Postgres commit is the bottleneck, not
  Redis. (`qa/bench/redis_lua.sh`)
- **Read under write load:** `GET /boss/{id}` p99 1.26 ms while 1,200 QPS writes
  run (p99 12.9 ms), 0 errors. (`make -C qa perf-mixed`)
- **Backpressure:** 4k QPS flood vs starved writer (`WRITER_CONCURRENCY=1, queue=50`)
  → accepted p99 10.3 ms, excess shed as `503`. (`make -C qa perf-overload`)
- **Fsync amortization:** ~6/12/20 hits per commit @ 4k/8k/12k QPS (bounded by
  `BATCH_MAX_WAIT=10 ms`) — packs more per fsync the busier the boss.
- **Soak / endurance:** sustained 1,000 QPS for 10 min — p99 held **13–19 ms**, 0%
  errors, no latency drift. No leak: goroutines/fds flat between the 40%- and 90%-in
  steady-state samples, and both collapse back to baseline (~20 goroutines, pooled
  fds) once load stops. (`make -C qa soak`) *(The ~2,500 goroutines/fds seen under
  load are the load generator's held keep-alive connections — one goroutine + fd
  each — not app state; they vanish when k6 disconnects.)*

### Measured correctness & durability

| Property | How | Result |
|---|---|---|
| Crash recovery (`SIGKILL` mid-load) | `qa/durability/crash_hard.sh` | acked = durable = 30,000; Redis rehydrated to Postgres HP |
| Exactly-once claim, 50-way race | `qa/bench/claim_race.sh` | 1 grant + 49 idempotent replays, identical payload, exactly 1 row |
| Compensating undo (queue-full / commit-fail) | `make -C qa integration` | all race tests pass |
| Rehydrate at scale | restart @ 138,562 contributors | `/readyz` ready in ~113 ms |

**Startup resilience.** A simultaneous restart of Redis **and** the app used to
crash it — the startup ping hit `LOADING Redis is loading the dataset` and exited
fatally. Now handled: `waitRedisReady` retries transient startup errors (`LOADING`
and connection-refused) with capped backoff (100 ms → 1 s) until Redis answers or
ctx is cancelled; other errors fail fast.

## 4. Assumptions & trade-offs

- **Go, not Java/C#.** Go is statically typed (fits the brief) and ideal here —
  goroutines make the group-commit fan-in and graceful drain natural. Same
  architecture ports to Spring Boot or .NET if the constraint is strict.
- **Damage is at-least-once.** The `/damage` payload has no request id, so a client
  retry double-counts. A `5xx` doesn't guarantee the hit wasn't applied (a slow
  commit can hit the deadline and still commit). Fix would be a client `request_id`
  deduped in Lua (`SETNX`) + a unique index.
- **Redis↔Postgres consistency on write failure.** Damage hits Redis first, then is
  made durable. Two modes leave Redis applied but un-persisted — **queue overflow**
  (`503`) and **commit failure** (`500`) — both undone by an atomic compensating Lua
  script (credit HP back, remove leaderboard delta, revert a wrong `defeated`).
  Without it, a lost killing blow could read `defeated` in Redis while Postgres
  never hit 0, making the reward permanently unclaimable. A ctx-cancel is
  *deliberately not* undone (may still commit). Two best-effort backstops bound
  residual divergence: **cache TTL** (`REDIS_CACHE_TTL`, 60s — stale keys go cold
  and lazy-rehydrate) and a **targeted reconcile** (rebuilds just that boss from
  Postgres after `RECONCILE_DELAY`, default 3s, for cases the hot path knows it
  couldn't fix).
- **Single instance.** State is externalized to Redis but the service runs as one
  instance; multi-replica would need care around Postgres HP write-back ordering.
  Out of scope for the timebox.
- **Edge cases:** invalid damage (`≤0` or `> MAX_DAMAGE_PER_HIT`) → 400 (guarded in
  handler *and* Lua); unknown boss → 404; attacking a defeated boss → 409; claim
  before defeat → 409; non-contributor claim → 403; cold cache → lazy rehydrate + one
  retry; graceful shutdown drains in-flight handlers and flushes the final batch.
