# qa/bench — measurement harness behind ARCHITECT.md's "Measured" numbers

These scripts produce the numbers quoted in `ARCHITECT.md` so every "measured"
claim is reproducible. All run against the isolated `behemoth-qa` stack
(`compose.qa.yaml`, ports 18080/16379/15432) — bring it up first:

```bash
make -C qa up && make -C qa seed
```

| Script / command | Backs | What it measures |
|---|---|---|
| `qa/perf/load_curve.js` (via `RATE=… k6 run`) | §3 latency-vs-QPS | p50/p90/p99 of `POST /damage` at a swept offered rate (1k→16k) |
| `bench/pg_single_row.sh` | §2 hot-row baseline | throughput collapse + p99 blow-up of naive single-row UPDATE-per-txn |
| `bench/redis_lua.sh` | §1 sub-ms Lua | per-op latency of the atomic damage Lua script |
| `bench/claim_race.sh` | §3 exactly-once | N concurrent claims → 1 grant, identical payload, 1 DB row |
| `make -C qa perf-mixed` | §1 read path | `GET /boss/{id}` p99 under concurrent write load |
| `make -C qa perf-overload` | §4 backpressure | accepted p99 while excess load sheds as fast 503 |
| `make -C qa integration` | §3/§4 correctness | compensating-undo + exactly-once + durable-invariant (race) |
| `bench/crash + rehydrate` (`qa/durability/crash_hard.sh`) | Req durability | acked==durable after SIGKILL; cache rehydrates to source of truth |

Latency curve sweep (backs §3):
```bash
for r in 1000 2000 4000 8000 12000 16000; do
  BASE_URL=http://localhost:18080 RATE=$r DUR=20s k6 run --quiet qa/perf/load_curve.js 2>/dev/null | grep '>>>'
done
```

Batch efficiency (hits per fsync) — snapshot Postgres `xact_commit` and
`damage_events` inserts around a load run; hits/commit rises with offered load.
