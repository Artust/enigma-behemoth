// Mixed read+write load: ~1200 QPS POST /damage alongside ~300 QPS GET /boss.
// Proves the read path (single pipelined Redis round trip) is not starved by
// the write hot path, and both hold p99 < 100ms.
//
//   BASE_URL=http://localhost:18080 k6 run qa/perf/load_mixed.js
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:18080';
const BOSS = __ENV.BOSS_ID || 'boss-load';

export const options = {
  scenarios: {
    writes: {
      executor: 'constant-arrival-rate',
      rate: 1200, timeUnit: '1s', duration: '30s',
      preAllocatedVUs: 300, maxVUs: 1500,
      exec: 'writeHit',
    },
    reads: {
      executor: 'constant-arrival-rate',
      rate: 300, timeUnit: '1s', duration: '30s',
      preAllocatedVUs: 100, maxVUs: 400,
      exec: 'readBoss',
    },
  },
  thresholds: {
    'http_req_duration{endpoint:damage}': ['p(99)<100'],
    'http_req_duration{endpoint:boss}': ['p(99)<100'],
    'http_req_failed': ['rate<0.01'],
  },
};

export function writeHit() {
  const player = `player-${Math.floor(Math.random() * 20000)}`;
  const payload = JSON.stringify({ player_id: player, boss_id: BOSS, damage_amount: 10 });
  const res = http.post(`${BASE}/damage`, payload, {
    headers: { 'Content-Type': 'application/json' },
    tags: { endpoint: 'damage' },
  });
  check(res, { 'write ok': (r) => r.status === 200 || r.status === 409 });
}

export function readBoss() {
  const res = http.get(`${BASE}/boss/${BOSS}`, { tags: { endpoint: 'boss' } });
  check(res, { 'read ok': (r) => r.status === 200 });
}
