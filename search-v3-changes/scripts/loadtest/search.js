// k6 load test for GET /api/search. Usage:
//   BASE=https://api.example.com TOKEN=<owner jwt> k6 run scripts/loadtest/search.js
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const partial = new Rate('search_partial');
const dur = new Trend('search_ms', true);
export const options = {
  stages: [{ duration: '1m', target: 100 }, { duration: '3m', target: 300 }, { duration: '1m', target: 0 }],
  thresholds: { http_req_failed: ['rate<0.01'], search_ms: ['p(95)<300', 'p(99)<800'], search_partial: ['rate<0.02'] },
};
const queries = ['rah', 'rahul', 'rahul 201', '201', 'DUE-1001', 'UTR998877', '9876543210', 'leak', 'వీర', 'राहुल'];
export default function () {
  const q = queries[Math.floor(Math.random() * queries.length)];
  const res = http.get(`${__ENV.BASE}/api/search?q=${encodeURIComponent(q)}`, { headers: { Authorization: `Bearer ${__ENV.TOKEN}` } });
  dur.add(res.timings.duration);
  check(res, { ok: (r) => r.status === 200 });
  if (res.status === 200) partial.add(!!res.json('partial'));
  sleep(0.3 + Math.random() * 0.7);
}
