import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate, Counter } from 'k6/metrics';

// Custom Metrics
const healthzDuration = new Trend('healthz_req_duration', true);
const apiHealthzDuration = new Trend('api_healthz_req_duration', true);
const vapidKeyDuration = new Trend('vapid_key_req_duration', true);
const metricsDuration = new Trend('metrics_req_duration', true);
const failureRate = new Rate('failed_requests_rate');
const requestCount = new Counter('total_http_requests');

export const options = {
  stages: [
    { duration: '15s', target: 100 },   // Warm-up to 100 users
    { duration: '20s', target: 100 },   // Sustain 100 users
    { duration: '20s', target: 500 },   // Scale to 500 users
    { duration: '25s', target: 500 },   // Sustain 500 users
    { duration: '20s', target: 1000 },  // Scale to 1,000 users peak
    { duration: '30s', target: 1000 },  // Sustain 1,000 users peak
    { duration: '15s', target: 0 },     // Graceful ramp-down
  ],
  thresholds: {
    'http_req_failed': ['rate<0.01'],                                // Error rate < 1%
    'http_req_duration': ['p(95)<400', 'p(99)<800'],                 // p95 < 400ms, p99 < 800ms
    'healthz_req_duration': ['p(95)<200'],
    'api_healthz_req_duration': ['p(95)<250'],
    'metrics_req_duration': ['p(95)<400'],
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';

export default function () {
  const rand = Math.random();

  let res;
  if (rand < 0.35) {
    // 35% traffic: Root healthz check
    const start = new Date();
    res = http.get(`${BASE_URL}/healthz`);
    healthzDuration.add(new Date() - start);
    const ok = check(res, {
      'healthz status 200': (r) => r.status === 200,
      'healthz body ok': (r) => r.body.includes('ok'),
    });
    failureRate.add(!ok);
  } else if (rand < 0.65) {
    // 30% traffic: API healthz check with full middleware stack
    const start = new Date();
    res = http.get(`${BASE_URL}/api/healthz`);
    apiHealthzDuration.add(new Date() - start);
    const ok = check(res, {
      'api/healthz status 200': (r) => r.status === 200,
    });
    failureRate.add(!ok);
  } else if (rand < 0.85) {
    // 20% traffic: VAPID public key query
    const start = new Date();
    res = http.get(`${BASE_URL}/api/push/vapid-public-key`);
    vapidKeyDuration.add(new Date() - start);
    const ok = check(res, {
      'vapid key status 200': (r) => r.status === 200,
      'vapid key present': (r) => r.body.includes('public_key'),
    });
    failureRate.add(!ok);
  } else {
    // 15% traffic: Live metrics and connection pool state
    const start = new Date();
    res = http.get(`${BASE_URL}/metrics`);
    metricsDuration.add(new Date() - start);
    const ok = check(res, {
      'metrics status 200': (r) => r.status === 200,
      'metrics has runtime': (r) => r.body.includes('runtime'),
    });
    failureRate.add(!ok);
  }

  requestCount.add(1);

  // Paced user think time: 50ms - 150ms
  sleep(0.05 + Math.random() * 0.1);
}
