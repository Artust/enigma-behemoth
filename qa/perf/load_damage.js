// Steady-state performance proof for POST /damage — the headline requirement:
// 1000+ QPS with p99 < 100ms.
//
// A short warmup precedes a 30s burst of 1500 QPS (comfortably above the 1000
// bar). Thresholds are scoped to the burst phase so warmup cold-start does not
// pollute the p99. Targets boss-load (huge HP) so it never dies mid-test.
//
//   BASE_URL=http://localhost:18080 k6 run qa/perf/load_damage.js
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:18080';
const BOSS = __ENV.BOSS_ID || 'boss-load';

export const options = {
  scenarios: {
    warmup: {
      executor: 'constant-arrival-rate',
      rate: 200, timeUnit: '1s', duration: '5s',
      preAllocatedVUs: 50, maxVUs: 200,
      tags: { phase: 'warmup' },
    },
    burst: {
      executor: 'constant-arrival-rate',
      rate: 1500, timeUnit: '1s', duration: '30s', startTime: '6s',
      preAllocatedVUs: 300, maxVUs: 1500,
      tags: { phase: 'burst' },
    },
  },
  thresholds: {
    'http_req_duration{phase:burst}': ['p(99)<100'], // the requirement
    'http_req_failed{phase:burst}': ['rate<0.01'],
  },
};

export default function () {
  const player = `player-${Math.floor(Math.random() * 20000)}`;
  const payload = JSON.stringify({ player_id: player, boss_id: BOSS, damage_amount: 10 });
  const res = http.post(`${BASE}/damage`, payload, {
    headers: { 'Content-Type': 'application/json' },
  });
  // 200 = applied, 409 = already defeated — both are healthy responses.
  check(res, { 'ok': (r) => r.status === 200 || r.status === 409 });
}
