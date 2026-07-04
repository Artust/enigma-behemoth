// k6 load test for POST /damage.
// Verifies the p99 < 100ms target under a sustained 1000+ QPS burst.
//
//   k6 run scripts/load_test.js
//   BASE_URL=http://localhost:8080 BOSS_ID=boss-1 k6 run scripts/load_test.js
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  scenarios: {
    burst: {
      executor: 'constant-arrival-rate',
      rate: 1500,          // 1500 requests/sec — comfortably above the 1000 QPS bar
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 300,
      maxVUs: 1500,
    },
  },
  thresholds: {
    'http_req_duration{endpoint:damage}': ['p(99)<100'], // ms
    http_req_failed: ['rate<0.01'],
  },
};

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const BOSS = __ENV.BOSS_ID || 'boss-1';

export default function () {
  const player = `player-${Math.floor(Math.random() * 20000)}`;
  const payload = JSON.stringify({ player_id: player, boss_id: BOSS, damage_amount: 10 });
  const res = http.post(`${BASE}/damage`, payload, {
    headers: { 'Content-Type': 'application/json' },
    tags: { endpoint: 'damage' },
  });
  // 200 = applied, 409 = boss already defeated (both are healthy responses).
  check(res, { 'ok': (r) => r.status === 200 || r.status === 409 });
}
