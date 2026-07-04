// Backpressure / overload behavior.
//
// Run this against an app recreated with a STARVED durable writer (single
// committer, tiny queue) so the group-commit intake saturates under a flood:
//
//   QA_WRITER_CONCURRENCY=1 QA_WRITER_QUEUE_SIZE=50 \
//   QA_BATCH_MAX_SIZE=50 QA_BATCH_MAX_WAIT=5ms \
//     docker compose -f qa/compose.qa.yaml up -d app
//   BASE_URL=http://localhost:18080 k6 run qa/perf/load_overload.js
//
// Expectation: when the writer can't keep up, the service SHEDS load with fast
// HTTP 503 (fail-fast) instead of stalling. The key property proven here is that
// ACCEPTED (200) requests stay fast even while 503s are being shed — the queue
// is a pressure valve, not a latency sink. (Makefile `perf-overload` wires the
// starve config up and restores defaults afterwards.)
import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://localhost:18080';
const BOSS = __ENV.BOSS_ID || 'boss-load';

const shed = new Counter('overload_503_shed');
const ok = new Counter('overload_200_ok');
const lat503 = new Trend('overload_503_latency_ms', true);

export const options = {
  scenarios: {
    flood: {
      executor: 'constant-arrival-rate',
      rate: 4000, timeUnit: '1s', duration: '30s',
      preAllocatedVUs: 1000, maxVUs: 4000,
    },
  },
  thresholds: {
    // Accepted requests must stay fast even under a shedding flood.
    'http_req_duration{expected_response:true}': ['p(99)<100'],
    // Shed (503) responses must themselves be cheap — fail FAST, not slow.
    'overload_503_latency_ms': ['p(95)<50'],
  },
};

export default function () {
  const player = `player-${Math.floor(Math.random() * 99999)}`;
  const payload = JSON.stringify({ player_id: player, boss_id: BOSS, damage_amount: 10 });
  const res = http.post(`${BASE}/damage`, payload, {
    headers: { 'Content-Type': 'application/json' },
  });
  if (res.status === 503) {
    shed.add(1);
    lat503.add(res.timings.duration);
  } else if (res.status === 200) {
    ok.add(1);
  }
  // 200 applied, 503 shed (fail-fast), 409 already defeated — all acceptable.
  check(res, { 'fast-fail or ok': (r) => r.status === 200 || r.status === 503 || r.status === 409 });
}
