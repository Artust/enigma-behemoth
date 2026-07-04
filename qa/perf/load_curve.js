// Latency-vs-QPS curve for POST /damage. One constant-arrival-rate scenario at
// a configurable RATE, so an outer driver can sweep RATE and plot how p99 moves
// as offered load rises (backs "latency stays low as QPS rises").
//
//   BASE_URL=http://localhost:18080 RATE=5000 DUR=20s k6 run qa/perf/load_curve.js
//
// No hard threshold here — this script MEASURES the curve (including where it
// breaks); the pass/fail gate lives in load_damage.js.
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://localhost:18080';
const BOSS = __ENV.BOSS_ID || 'boss-load';
const RATE = parseInt(__ENV.RATE || '1000', 10);
const DUR = __ENV.DUR || '20s';

export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    curve: {
      executor: 'constant-arrival-rate',
      rate: RATE, timeUnit: '1s', duration: DUR,
      preAllocatedVUs: Math.max(200, Math.ceil(RATE / 4)),
      maxVUs: Math.max(500, RATE * 2),
    },
  },
};

export default function () {
  const player = `player-${Math.floor(Math.random() * 20000)}`;
  const payload = JSON.stringify({ player_id: player, boss_id: BOSS, damage_amount: 10 });
  const res = http.post(`${BASE}/damage`, payload, {
    headers: { 'Content-Type': 'application/json' },
  });
  check(res, { 'ok': (r) => r.status === 200 || r.status === 409 });
}

export function handleSummary(data) {
  const d = data.metrics.http_req_duration.values;
  const reqs = data.metrics.http_reqs.values;
  const failed = data.metrics.http_req_failed ? data.metrics.http_req_failed.values.rate : 0;
  const line = `RATE=${RATE} actual_rps=${reqs.rate.toFixed(0)} p50=${d.med.toFixed(1)} p90=${d['p(90)'].toFixed(1)} p95=${d['p(95)'].toFixed(1)} p99=${d['p(99)'].toFixed(1)} max=${d.max.toFixed(1)} fail=${(failed * 100).toFixed(2)}%`;
  return {
    stdout: '\n>>> ' + line + '\n',
    [`/tmp/curve_${RATE}.txt`]: line + '\n',
  };
}
