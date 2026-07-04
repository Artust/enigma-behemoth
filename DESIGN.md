# Behemoth — World Boss Event Service · Thiết kế & Giải thích giải pháp

> File ghi chú thiết kế (viết trước khi hoàn tất code). Mô tả **cấu trúc cụ thể** của
> hệ thống và **giải thích chi tiết** vì sao mỗi giải pháp được chọn để đáp ứng
> từng yêu cầu của đề bài. `ARCHITECT.md` chính thức sẽ chắt lọc lại từ file này.

---

## 1. Yêu cầu → Giải pháp (bản đồ nhanh)

| Yêu cầu đề bài | Giải pháp trong hệ thống |
|---|---|
| `POST /damage`, 1000+ QPS, p99 < 100ms | Lua script **atomic** trên Redis (1 round-trip, sub-ms) |
| Ghi HP đồng thời an toàn | Redis single-threaded + Lua ⇒ serialize, không lost-update, không âm HP |
| Durability qua restart | **Group-commit writer** → batch 1 transaction / 1 fsync vào Postgres; ack sau khi durable |
| `GET /boss/{id}` HP + Top 10 | Redis pipeline: `GET hp` + `ZREVRANGE lb 0 9 WITHSCORES` |
| `POST /rewards/claim` exactly-once | Postgres `INSERT ... ON CONFLICT (boss_id,player_id) DO NOTHING` |
| Không mất data khi Redis chết | Postgres là **source of truth**; Redis là cache dựng lại được (rehydrate) |

---

## 2. Kiến trúc tổng thể

```
Client ──HTTP──> Go service (chi router, single instance)
                   │
      ┌────────────┼─────────────┐
      ▼            ▼              ▼
   Redis      group-commit    PostgreSQL
 (hot cache)    writer        (source of truth)
```

- **Redis** = cache hot-path cho live state (HP, leaderboard). Thao tác nóng chạy
  bằng Lua atomic, một round-trip. **Coi như cache dựng lại được**, KHÔNG tin làm
  durable store.
- **PostgreSQL** = primary DB, **source of truth**: event log, aggregate, và bảng
  idempotency cho claim.
- **Group-commit writer** = 1 goroutine gom event rồi batch-commit vào Postgres →
  durability thật mà vẫn giữ p99 thấp (amortize fsync trên nhiều request).

**Quyết định nền:** Go 1.24 · Redis + Postgres hybrid · **single-instance** (đơn
giản hoá, ghi rõ assumption) · payload `/damage` giữ nguyên (không idempotency key
⇒ damage **at-least-once** khi retry — trade-off được ghi rõ).

---

## 3. Cấu trúc thư mục cụ thể

```
enigma/
├── cmd/server/main.go              # entrypoint: load config → wire store/service → HTTP + graceful shutdown
│
├── internal/
│   ├── config/config.go            # đọc config từ ENV (có default) + validate
│   │
│   ├── store/                      # tầng persistence
│   │   ├── store.go                # types + errors dùng chung (DamageEvent, BossView, ClaimResult, status codes)
│   │   ├── redis.go                # Redis client + Lua script atomic (ApplyDamage), GetBossView, Rehydrate
│   │   ├── postgres.go             # pgxpool: CommitBatch, ClaimBasis, SaveClaim, RecoveryState, Contributions
│   │   └── writer.go               # group-commit writer (bounded channel + 1 goroutine batcher)
│   │
│   ├── recovery/recovery.go        # Rehydrator: derive HP từ Postgres → dựng lại Redis (startup + lazy)
│   │
│   ├── boss/service.go             # domain logic: Damage / Get / Claim + tier calc + reward payload
│   │
│   └── api/
│       ├── router.go               # khai báo route (chi)
│       ├── handlers.go             # parse request → gọi service → map error sang HTTP status
│       └── middleware.go           # logging (slog), recover, timeout, prometheus metrics
│
├── db/init.sql                     # schema + seed boss demo (chạy qua /docker-entrypoint-initdb.d)
│
├── scripts/
│   ├── seed.sh                     # tạo boss demo qua API/SQL
│   ├── load_test.js                # k6: 1000+ VUs bắn /damage, report p99
│   └── recovery_test.sh            # seed → restart app+redis → verify HP còn nguyên
│
├── Dockerfile                      # multi-stage build (builder → distroless/alpine)
├── docker-compose.yaml             # app + redis + postgres (named volume, healthcheck)
├── ARCHITECT.md                    # tài liệu kiến trúc chính thức (bản chắt lọc)
├── README.md                       # hướng dẫn chạy
├── Makefile                        # build/test/up/loadtest shortcuts
└── go.mod
```

### Quan hệ import (không có cycle)
```
config  ← (không phụ thuộc ai)
store   ← config
recovery← store
boss    ← store, recovery
api     ← boss
main    ← config, store, recovery, boss, api
```

---

## 4. Data model cụ thể

### Redis (`--maxmemory-policy noeviction` để boss key không bị evict)
| Key | Kiểu | Ý nghĩa |
|---|---|---|
| `boss:{id}:hp` | int | HP hiện tại (arbiter live) |
| `boss:{id}:maxhp` | int | máu tối đa |
| `boss:{id}:state` | string | `alive` \| `defeated` |
| `boss:{id}:lb` | ZSET | member=`player_id`, score=cumulative applied damage |

### PostgreSQL (`db/init.sql`)
- `bosses(id, name, max_hp, current_hp, state, defeated_at, updated_at)`
- `damage_events(id BIGSERIAL, boss_id, player_id, damage_applied, created_at)` — audit log append-only
- `contributions(boss_id, player_id, total_damage, PK(boss_id,player_id))` — aggregate cho recovery + tính thưởng
- `reward_claims(boss_id, player_id, tier, damage_pct, reward_payload, claimed_at, PK(boss_id,player_id))` — idempotency exactly-once

---

## 5. Giải thích chi tiết từng luồng

### 5.1 `POST /damage` — chịu 1000+ QPS, p99 < 100ms trên MỘT hot counter
**Vấn đề gốc:** hàng nghìn player cùng ghi vào một biến HP → dễ lost-update, âm HP,
và nghẽn nếu dùng lock/row-lock trên DB.

**Giải pháp:** một **Lua script atomic** trên Redis. Vì Redis single-threaded, toàn
bộ chuỗi `đọc HP → applied = min(hp, damage) → SET hp-applied → ZINCRBY leaderboard`
chạy như **một thao tác không thể chen ngang**:
- Không cần lock, không lost-update.
- HP **không bao giờ âm** (nhờ `min(hp, damage)`).
- Mỗi cú đánh chỉ được credit đúng phần damage **thực sự trúng** (last-hit công bằng).
- Một round-trip Redis ≈ sub-millisecond ⇒ thừa sức p99 < 100ms.

**Status codes script trả về:**
| Code | Ý nghĩa | Handler xử lý |
|---|---|---|
| `1` | applied | submit writer → 200 |
| `0` | boss đã chết | 409 Conflict |
| `-1` | state chưa có trong Redis | rehydrate boss từ Postgres → retry |
| `-2` | damage ≤ 0 (guard) | 400 |

**Sau khi apply:** handler đẩy `DamageEvent` cho **group-commit writer** và **chờ
commit durable rồi mới trả 200** (đã ack = đã bền).

### 5.2 Durability — restart không mất HP & lịch sử đóng góp
**Vấn đề:** Redis nhanh nhưng không đáng tin làm durable store; ghi thẳng Postgres
mỗi request thì fsync giết p99.

**Giải pháp — group-commit writer:**
- Handler đẩy event vào channel **bounded**.
- 1 goroutine gom tối đa `BatchMaxSize` (mặc định 500) hoặc `BatchMaxWait` (5ms) →
  commit **cả batch trong 1 transaction = 1 fsync**.
- Chi phí fsync chia đều cho hàng trăm request ⇒ throughput cao mà vẫn durable.
- 1 transaction ghi đồng thời: `INSERT damage_events` + upsert `contributions` +
  update `bosses.current_hp`/state → nhất quán.

**Chống treo / backpressure:**
- Channel đầy → trả **503** ngay (fail-fast), không block.
- Mỗi batch txn có **context timeout**.
- Nếu commit lỗi/panic → `defer` **release TẤT CẢ waiter kèm error**, tuyệt đối
  không để request treo vĩnh viễn.

### 5.3 Recovery — dựng lại Redis sau restart
**Giải pháp:** khi khởi động, **derive** HP thay vì tin giá trị cache:
```
current_hp = max_hp − SUM(contributions.total_damage)   (floor 0)
```
Bất biến này không phụ thuộc số instance ⇒ đúng kể cả khi Redis mất hẳn.
- Nạp lại Redis: SET hp/maxhp/state, rebuild ZSET leaderboard từ `contributions`.
- `/readyz` **chỉ báo OK sau khi rehydrate xong** → không nhận traffic vào Redis rỗng.
- **Lazy re-hydrate:** nếu lúc chạy Lua thấy HP key mất (status `-1`), service tự
  rehydrate boss đó rồi retry → chống Redis bị evict/mất giữa chừng.

### 5.4 `GET /boss/{id}` — HP + Top 10
**Giải pháp:** 1 round-trip Redis pipeline: `GET hp` + `GET maxhp` + `GET state` +
`ZREVRANGE lb 0 9 WITHSCORES`. ZSET cho leaderboard top-N gần như free. Redis miss →
read-repair từ Postgres rồi trả.

### 5.5 `POST /rewards/claim` — exactly-once
- **Gate trên Postgres** `state='defeated'` (KHÔNG đọc Redis, tránh race lúc boss
  vừa chết). Chưa chết → 409; player không đóng góp → 403.
- **Exactly-once:**
  ```sql
  INSERT INTO reward_claims (boss_id, player_id, tier, damage_pct, reward_payload)
  VALUES (...) ON CONFLICT (boss_id, player_id) DO NOTHING RETURNING ...;
  ```
  Unique PK `(boss_id, player_id)` đảm bảo dù nhiều request trùng chạy song song
  **chỉ tạo đúng 1 row**; request sau nhận idempotent response (đã claim, cùng tier).
- **Reward payload ghi ngay trong cùng txn** (outbox) → "giao thưởng" = đọc bảng đó,
  giữ nguyên tử; tránh crash-giữa-chừng ghi claim mà chưa phát thưởng.
- **Tier** = `total_damage / max_hp` → map ngưỡng. Vì `SUM(applied) = max_hp` nên
  tổng % = 100%.

| Ngưỡng % đóng góp | Tier |
|---|---|
| ≥ 20% | legendary |
| ≥ 10% | epic |
| ≥ 5% | rare |
| ≥ 1% | uncommon |
| còn lại | common |

---

## 6. Concurrency & Safety — tổng hợp

| Rủi ro | Giải pháp |
|---|---|
| Ghi HP đồng thời | Redis single-thread + Lua atomic (serialize hoàn toàn) |
| HP âm / over-damage | `applied = min(hp, damage)` trong Lua |
| Exactly-once claim | Postgres unique constraint + `ON CONFLICT DO NOTHING` |
| Durability | group-commit 1 txn / 1 fsync, ack sau commit |
| Writer treo | bounded channel + 503, tx timeout, `defer` release waiter |
| Redis mất giữa chừng | `noeviction` + lazy rehydrate + readyz gate |
| Input xấu (âm/0/overflow) | validate ở handler + guard lại trong Lua |
| Boss lạ | Lua trả `-1` → Postgres không có → 404 |

---

## 7. Trade-off đã chấp nhận (ghi rõ trong ARCHITECT.md)

1. **Go thay Java/C#** — Go vẫn là ngôn ngữ static-typed, rất hợp workload
   low-latency high-concurrency. Đề ghi "(Java, or C#)" như ví dụ; sẽ biện luận rõ.
   *(Rủi ro: nếu người chấm coi đây là ràng buộc cứng.)*
2. **Damage at-least-once** — payload không có idempotency key; nếu client retry sau
   lỗi commit, damage có thể apply 2 lần. Chấp nhận vì single-instance + queue lớn
   nên 503/lỗi hiếm. Cải tiến tương lai: thêm `request_id` + `SETNX` dedup.
3. **Single-instance** — không xử lý clobber `current_hp` khi chạy nhiều replica.
4. **ZSET score là float64** — chính xác số nguyên tới ~2^53; leaderboard
   authoritative cho chia thưởng lấy từ Postgres (integer), ZSET chỉ để đọc nhanh.
5. **Boss creation** — coi là admin/seed path, không expose endpoint tạo boss công khai.

---

## 8. Verification (cách kiểm thử end-to-end)

1. `docker compose up` → cả 3 service healthy, app `/readyz` OK sau rehydrate.
2. Seed boss → `POST /damage` nhiều lần → `GET /boss/{id}` thấy HP giảm + leaderboard đúng.
3. Edge: `damage_amount` âm/0 → `400`; boss lạ → `404`.
4. `k6 run scripts/load_test.js` → xác nhận 1000+ QPS, p99 < 100ms cho `/damage`.
5. Recovery: seed damage → `docker compose restart app` và `restart redis` →
   verify HP + contributions còn nguyên (rehydrate từ Postgres).
6. Claim: boss chết → 2 request claim đồng thời → chỉ 1 lần cấp thưởng, lần sau
   idempotent; non-contributor → `403`.
7. `go test ./...` pass.
