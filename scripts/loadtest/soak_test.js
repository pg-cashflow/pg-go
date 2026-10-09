// ASD-STE100 Compliant: Extended Soak Test Scenario for PG Cashflow.
// Validates system stability, memory leak absence, and database connection pool health.
// Simulates sustained continuous traffic for 30–60 minutes under DATABASE_MAX_CONNS=25.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate, Counter } from 'k6/metrics';

// Metric trackers across soak duration
const soakLatency = new Trend('soak_latency_ms', true);
const soakErrors = new Rate('soak_error_rate');
const totalOps = new Counter('soak_total_operations');

// Default duration is 30m, but can be overridden with SOAK_DURATION env var (e.g., '5m' for smoke)
const duration = __ENV.SOAK_DURATION || '30m';

export const options = {
  stages: [
    { duration: '2m', target: 30 },            // Smooth ramp to 30 continuous virtual users
    { duration: duration, target: 30 },        // Hold steady soak load across target duration
    { duration: '1m', target: 0 },             // Graceful ramp-down
  ],
  thresholds: {
    'http_req_failed': ['rate<0.005'],         // Extreme stability: < 0.5% failure over soak run
    'http_req_duration': ['p(95)<500', 'p(99)<1000'], // Latency must not degrade over time
    'soak_latency_ms': ['p(95)<450'],
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const OWNER_TOKEN = __ENV.OWNER_TOKEN || '';
const PROPERTY_ID = __ENV.PROPERTY_ID || '';

export default function () {
  const headers = { 'Content-Type': 'application/json' };
  if (OWNER_TOKEN) headers['Authorization'] = `Bearer ${OWNER_TOKEN}`;
  if (PROPERTY_ID) headers['X-Property-ID'] = PROPERTY_ID;

  const params = {
    headers: headers,
    responseCallback: http.expectedStatuses(200),
  };

  const rand = Math.random();
  const start = new Date();
  let res;

  if (rand < 0.40) {
    // 40% Traffic: Root and API Health / DB Ping Check
    res = http.get(`${BASE_URL}/api/healthz`);
  } else if (rand < 0.70) {
    // 30% Traffic: Owner Dashboard Summary and Occupancy Check
    res = http.get(`${BASE_URL}/api/owner/dashboard/summary`, params);
  } else if (rand < 0.85) {
    // 15% Traffic: Global Lexical Search Queries
    const query = ['Rahul', '201', 'DUE-1001', 'Rent'][Math.floor(Math.random() * 4)];
    res = http.get(`${BASE_URL}/api/search?q=${encodeURIComponent(query)}`, params);
  } else {
    // 15% Traffic: Prometheus Runtime Metrics Check
    res = http.get(`${BASE_URL}/metrics`);
  }

  const elapsed = new Date() - start;
  soakLatency.add(elapsed);
  totalOps.add(1);

  const isOk = check(res, {
    'request success (200)': (r) => r.status === 200,
  });

  if (!isOk) {
    soakErrors.add(1);
  }

  // Realistic operator think time: 200ms - 600ms
  sleep(0.2 + Math.random() * 0.4);
}
