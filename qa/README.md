# QA suite — Performance & Persistence safety

Bộ test kiểm chứng hai ràng buộc phi chức năng cứng của Behemoth trong
`REQUIREMENTS.md`, cộng lớp **correctness** làm nền tin cậy:

| Lớp | Mục tiêu | Vị trí |
|-----|----------|--------|
| **1. Correctness** | Lua atomic (HP≥0, Σapplied=max_hp), writer không treo + fail-fast, claim exactly-once, bất biến durability | `integration/` (Go, `-tags=integration`) |
| **2. Persistence safety** | Không mất data khi restart **và crash cứng (SIGKILL)**; 200 không bao giờ phát trước khi bền; lazy rehydrate | `durability/` (shell black-box) |
| **3. Performance** | p99 < 100ms @ 1000+ QPS cho `/damage`; mixed read/write; overload → 503 fail-fast | `perf/` (k6) |

> **Cô lập hoàn toàn.** Mọi thứ chạy trên stack riêng `behemoth-qa`
> (`compose.qa.yaml`) ở cổng **18080 / 16379 / 15432** với volume riêng — không
> đụng stack mặc định (8080/6379/5432) mà session khác có thể đang chạy. Không
> file nào ngoài `qa/` bị sửa; `go.mod` không đổi.

## Yêu cầu công cụ

- **Docker + docker-compose** — đã có.
- **Go 1.25** — đã có (chạy test tích hợp).
- **k6** — CHƯA cài. Cài để chạy Lớp 3:
  ```bash
  brew install k6            # macOS (Homebrew)
  ```
  Lớp 1 và Lớp 2 **không** cần k6.

## Chạy nhanh

```bash
# từ thư mục gốc repo
make -C qa up            # dựng stack QA cô lập + chờ /readyz
make -C qa seed          # seed boss-load / boss-durab
make -C qa integration   # Lớp 1 — go test -tags=integration -race
make -C qa durability    # Lớp 2 — crash/restart/no-false-ack/redis-wipe
make -C qa perf          # Lớp 3 — cần k6 (load_damage + load_mixed)
make -C qa perf-overload # Lớp 3 — kịch bản backpressure 503
make -C qa down          # dọn stack QA + volume

make -C qa all           # chạy tuần tự tất cả
```

## Cách đọc kết quả (PASS)

- **Lớp 1:** `go test` báo `ok` / `PASS`. Đặc biệt `-race` phải sạch (Lua/writer).
- **Lớp 2:** mỗi script in `PASS: ...` và exit 0. Điểm mấu chốt:
  - `crash_hard.sh` → `durable >= acked` (mọi hit đã nhận 200 sống sót sau `SIGKILL`).
  - `no_false_ack.sh` → hit in-flight bị kill **không** nhận 200 và **vắng mặt** trong Postgres.
- **Lớp 3:** k6 in `✓ threshold` cho `p(99)<100` (burst). `perf-overload` cho thấy có
  `overload_503_shed` (đúng — van xả tải) nhưng p99 request thành công vẫn bị chặn.

Đo metric quanh một lần load:
```bash
BASE_URL=http://localhost:18080 perf/capture_metrics.sh snapshot /tmp/before.txt
k6 run perf/load_damage.js
perf/capture_metrics.sh snapshot /tmp/after.txt
perf/capture_metrics.sh delta /tmp/before.txt /tmp/after.txt
```

## Đối chiếu với REQUIREMENTS.md

- *Performance: 1000+ QPS, p99 < 100ms cho /damage* → `perf/load_damage.js` (threshold cứng).
- *Persistence safety: restart không mất HP + lịch sử đóng góp* → `durability/*` (mở rộng sang crash cứng, không chỉ graceful).

## Giả định & lưu ý

- **Hợp đồng durability đang test:** `POST /damage` trả 200 ⇔ hit đã commit vào
  Postgres (writer ack sau khi group-commit xong). `no_false_ack.sh` bám giả định
  này; nếu writer được thiết kế lại (ví dụ ack trước khi bền, hoặc commit ngay từng
  event), cần chỉnh lại kịch bản đó.
- **Test tích hợp an toàn trên DB dùng chung:** mỗi test dùng boss/player id
  duy nhất (`qa-<name>-<nano>`) và tự dọn, nên không đụng dữ liệu session khác.
- **Durability/overload restart container `app` của stack QA** — không phải stack
  mặc định của bạn. Vẫn nên chạy khi không có ai đang thao tác trên chính stack QA.
- **Metric queue-depth/batch-size của writer chưa được expose** (ARCHITECT.md ghi là
  future work). Suite này quan sát backpressure qua tỉ lệ 503 + latency. Nếu muốn
  quan sát sâu hơn, thêm gauge `behemoth_writer_queue_depth` vào source — nhưng đó là
  sửa code dùng chung nên để dành, tránh đụng session đang code.
