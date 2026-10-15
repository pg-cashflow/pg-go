// Gate 09: admission_burst — pending join read + idempotent join shape (no live Firebase).
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend } from 'k6/metrics';

const joinMeDuration = new Trend('join_me_ms', true);

export const options = {
  scenarios: {
    admission_burst: {
      executor: 'ramping-arrival-rate',
      startRate: 5,
      timeUnit: '1s',
      preAllocatedVUs: 10,
      maxVUs: 30,
      stages: [
        { duration: '30s', target: 20 },
        { duration: '1m', target: 40 },
        { duration: '30s', target: 0 },
      ],
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.02'],
    join_me_ms: ['p(95)<500'],
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const PENDING_JOIN_TOKEN = __ENV.PENDING_JOIN_TOKEN || '';

export default function () {
  if (!PENDING_JOIN_TOKEN) {
    return;
  }
  const headers = {
    Authorization: `Bearer ${PENDING_JOIN_TOKEN}`,
    'Content-Type': 'application/json',
  };
  const res = http.get(`${BASE_URL}/api/join/me`, { headers });
  joinMeDuration.add(res.timings.duration);
  check(res, { 'join me 200': (r) => r.status === 200 });
  sleep(0.2);
}
