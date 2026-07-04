# Behemoth — World Boss Event Service

High-throughput backend for a global "World Boss" that thousands of players
attack simultaneously. Tracks damage contributions, serves a live leaderboard,
and manages exactly-once reward claims.

Built with **Go + Redis + PostgreSQL**. See [ARCHITECT.md](./ARCHITECT.md) for
the design rationale (data strategy, concurrency & safety, trade-offs).

**Measured** (durable, `synchronous_commit=on`, full stack + load gen on one laptop):
`POST /damage` sustains **~18,000 QPS at p99 ≈ 21ms** — well past the 1,000 QPS /
p99 < 100ms target. Reproduce with `make loadgo`.

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
First claim → `201`; idempotent replay → `200` with `already_claimed:true`.
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

## Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `HTTP_ADDR` | `:8080` | listen address |
| `POSTGRES_DSN` | local dsn | Postgres connection |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `MAX_DAMAGE_PER_HIT` | `1000000000` | reject a single hit above this |
| `BATCH_MAX_SIZE` | `500` | group-commit flush size |
| `BATCH_MAX_WAIT` | `5ms` | group-commit flush interval |
| `WRITER_QUEUE_SIZE` | `20000` | bounded durable-writer queue |
