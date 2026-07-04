# Behemoth — World Boss Event Service

High-throughput backend for a global "World Boss" that thousands of players
attack simultaneously. Tracks damage contributions, serves a live leaderboard,
and manages exactly-once reward claims.

Built with **Go + Redis + PostgreSQL**. See [ARCHITECT.md](./ARCHITECT.md) for
the design rationale (data strategy, concurrency & safety, trade-offs).

**Measured** (durable, `synchronous_commit=on`, full stack + load gen on one laptop):
`POST /damage` holds **p99 ≈ 12–26 ms up to 8,000 QPS** (8× the requirement) with
zero errors — well under the 1,000 QPS / p99 < 100 ms target; throughput knee is
~12k QPS. Full method and numbers in [ARCHITECT.md §3](./ARCHITECT.md#3-concurrency--safety);
reproduce the p99 proof with `make -C qa perf-damage` (or `make loadgo` for a
dependency-free probe).

## Quick start

```bash
docker compose up --build -d      # start app + redis + postgres (one command)
# wait for readiness
curl -s localhost:8080/readyz
```

A demo boss `boss-1` (10,000,000 HP) is seeded automatically.

## API

### `POST /damage`
```bash
curl -X POST localhost:8080/damage -H 'Content-Type: application/json' \
  -d '{"player_id":"alice","boss_id":"boss-1","damage_amount":500}'
```
```json
{"boss_id":"boss-1","player_id":"alice","damage_applied":500,"boss_hp":9999500,"defeated":false}
```
Errors: `400` invalid amount, `404` unknown boss, `409` boss already defeated,
`503` overloaded.

### `GET /boss/{id}`
```bash
curl localhost:8080/boss/boss-1
```
```json
{"boss_id":"boss-1","hp":9999500,"max_hp":10000000,"state":"alive",
 "leaderboard":[{"player_id":"alice","damage":500}]}
```

### `POST /rewards/claim` (only after the boss is defeated)
```bash
curl -X POST localhost:8080/rewards/claim -H 'Content-Type: application/json' \
  -d '{"player_id":"alice","boss_id":"boss-1"}'
```
```json
{"boss_id":"boss-1","player_id":"alice","tier":"legendary","damage_pct":37.5,
 "reward":{"gold":10000,"items":["Behemoth Crown","Legendary Chest"]},
 "claimed_at":"...","already_claimed":false}
```
Both a fresh claim and an idempotent replay return `200`; tell them apart via the
`already_claimed` flag in the body (replay → `already_claimed:true`).
Errors: `404` unknown boss, `409` boss not yet defeated, `403` non-contributor.

### Ops endpoints
- `GET /healthz` — liveness
- `GET /readyz` — ready only after cache rehydration
- `GET /metrics` — Prometheus metrics

## Development

```bash
make build          # go build ./...
make test           # go test ./...
make smoke          # quick curl end-to-end (stack must be up)
make load           # k6 load test (needs k6 installed) → asserts p99 < 100ms
make loadgo         # built-in Go load probe (no deps) → asserts p99 < 100ms
make recovery       # durability test: damage → restart redis+app → verify HP
make down           # stop + wipe volumes
```

### Full QA suite (`qa/`)

The authoritative correctness / durability / performance proofs run against an
isolated `behemoth-qa` stack (ports `18080/16379/15432`, own volumes) so they
never collide with a dev stack on the defaults. See [qa/README.md](./qa/README.md).

```bash
make -C qa all            # stack → correctness (race) → durability → perf
make -C qa integration    # Go race-enabled correctness/integration tests
make -C qa durability     # black-box persistence-safety (restart, SIGKILL crash)
make -C qa perf-damage    # steady-state p99 < 100ms proof (needs k6)
make -C qa soak           # endurance/leak soak (SOAK_DUR, SOAK_QPS overridable)
make -C qa bench          # backing measurements cited in ARCHITECT.md §3
```

## Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `HTTP_ADDR` | `:8080` | listen address |
| `POSTGRES_DSN` | local dsn | Postgres connection |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `REDIS_PASSWORD` | _(empty)_ | Redis auth, if set |
| `REDIS_CACHE_TTL` | `60s` | stale-key TTL → cold key lazily rehydrates |
| `RECONCILE_DELAY` | `3s` | delay before a targeted boss reconcile from Postgres |
| `MAX_DAMAGE_PER_HIT` | `1000000000` | reject a single hit above this |
| `WRITER_QUEUE_SIZE` | `20000` | bounded durable-writer queue (overflow → `503`) |
| `WRITER_CONCURRENCY` | `8` | parallel group-commit committers |
| `BATCH_MAX_SIZE` | `500` | group-commit flush size |
| `BATCH_MAX_WAIT` | `10ms` | group-commit flush interval |
| `BATCH_TX_TIMEOUT` | `5s` | per-batch transaction timeout |
| `PG_MAX_CONNS` | `20` | Postgres connection-pool size |
| `SHUTDOWN_TIMEOUT` | `15s` | graceful-drain deadline on shutdown |
