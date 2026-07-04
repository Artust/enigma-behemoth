// Soak / endurance test for POST /damage — sustained load for a long duration to
// surface what the 30s burst tests can't: memory/goroutine/fd leaks, connection-
// pool exhaustion, and latency drift over time. Pair with soak.sh, which snapshots
// the app's runtime metrics before/after to assert nothing leaked.
//
//   BASE_URL=http://localhost:18080 SOAK_QPS=1000 SOAK_DUR=10m k6 run qa/perf/soak.js
//
// The p99<100ms / error<1% thresholds are gated over the WHOLE run, so any drift
// that pushes tail latency past the budget at any point fails the soak.
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:18080';
const BOSS = __ENV.BOSS_ID || 'boss-load';
const QPS = parseInt(__ENV.SOAK_QPS || '1000', 10);
const DUR = __ENV.SOAK_DUR || '10m';

// A 30s warmup absorbs VU cold-start and lets the connection pools scale to their
// steady size; the measured `soak` phase starts after it. Thresholds are scoped to
// {phase:soak} so warmup latency never pollutes the p99 gate (same pattern as
// load_damage.js scoping to phase:burst).
export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    warmup: {
      executor: 'constant-arrival-rate',
      rate: QPS, timeUnit: '1s', duration: '30s',
      preAllocatedVUs: Math.max(400, Math.ceil(QPS / 2)),
      maxVUs: Math.max(1000, QPS * 2),
      tags: { phase: 'warmup' },
    },
    soak: {
      executor: 'constant-arrival-rate',
      rate: QPS, timeUnit: '1s', duration: DUR, startTime: '30s',
      preAllocatedVUs: Math.max(400, Math.ceil(QPS / 2)),
      maxVUs: Math.max(1000, QPS * 2),
      tags: { phase: 'soak' },
    },
  },
  thresholds: {
    'http_req_duration{phase:soak}': ['p(99)<100'],
    'http_req_failed{phase:soak}': ['rate<0.01'],
  },
};

export default function () {
  const player = `player-${Math.floor(Math.random() * 20000)}`;
  const payload = JSON.stringify({ player_id: player, boss_id: BOSS, damage_amount: 10 });
  const res = http.post(`${BASE}/damage`, payload, {
    headers: { 'Content-Type': 'application/json' },
  });
  // A dead boss returns 409 (fast) — still a healthy response for a soak.
  check(res, { 'ok': (r) => r.status === 200 || r.status === 409 });
}
