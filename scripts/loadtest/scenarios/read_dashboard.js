// ASD-STE100 Compliant: Read Dashboard & Search Stress Scenario.
// Inspects read latency under multi-tenant load across owner dashboard, dues, and global search.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';

// Custom metric trends for dashboard components
const dashboardDuration = new Trend('owner_dashboard_ms', true);
const duesDuration = new Trend('owner_dues_ms', true);
const searchDuration = new Trend('global_search_ms', true);
const errorRate = new Rate('dashboard_error_rate');

export const options = {
  stages: [
    { duration: '30s', target: 20 },  // Ramp-up to 20 concurrent readers
    { duration: '1m', target: 50 },   // Step to 50 concurrent readers
    { duration: '2m', target: 100 },  // Peak load: 100 concurrent readers
    { duration: '30s', target: 0 },   // Graceful ramp-down
  ],
  thresholds: {
    'http_req_failed': ['rate<0.02'],                               // Error rate must stay below 2%
    'http_req_duration': ['p(95)<45000'],
    'owner_dashboard_ms': ['p(95)<45000'],
    'owner_dues_ms': ['p(95)<15000'],
    'global_search_ms': ['p(95)<15000'],
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const OWNER_TOKEN = __ENV.OWNER_TOKEN || '';
const PROPERTY_ID = __ENV.PROPERTY_ID || '';

const searchTerms = [
  'Rahul', '201', 'DUE-1001', 'Rent', 'Deposit', '9876543210', 'Sharma', 'Kumar'
];

export default function () {
  const headers = {
    'Content-Type': 'application/json',
  };
  if (OWNER_TOKEN) {
    headers['Authorization'] = `Bearer ${OWNER_TOKEN}`;
  }
  if (PROPERTY_ID) {
    headers['X-Property-ID'] = PROPERTY_ID;
  }

  const params = {
    headers: headers,
    responseCallback: http.expectedStatuses(200, 429),
  };

  // 1. Check API Liveness & Middleware Stack
  const healthRes = http.get(`${BASE_URL}/api/healthz`);
  check(healthRes, {
    'healthz status 200': (r) => r.status === 200,
  });

  // 2. Fetch Owner Dashboard Summary
  const dashStart = new Date();
  const dashRes = http.get(`${BASE_URL}/api/owner/dashboard/summary`, params);
  dashboardDuration.add(new Date() - dashStart);
  const dashOk = check(dashRes, {
    'dashboard status 200': (r) => r.status === 200,
  });
  if (!dashOk) errorRate.add(1);

  // 3. Query Open Dues
  const duesStart = new Date();
  const duesRes = http.get(`${BASE_URL}/api/owner/dues?status=pending`, params);
  duesDuration.add(new Date() - duesStart);
  const duesOk = check(duesRes, {
    'dues status 200': (r) => r.status === 200,
  });
  if (!duesOk) errorRate.add(1);

  // 4. Query Lexical & Hybrid Search (200 OK or 429 Rate Limited under multi-VU single user)
  const query = searchTerms[Math.floor(Math.random() * searchTerms.length)];
  const searchStart = new Date();
  const searchRes = http.get(`${BASE_URL}/api/search?q=${encodeURIComponent(query)}`, params);
  searchDuration.add(new Date() - searchStart);
  const searchOk = check(searchRes, {
    'search status 200 or 429': (r) => r.status === 200 || r.status === 429,
  });
  if (!searchOk) errorRate.add(1);

  // 5. Paced think time (0.5s - 1.5s)
  sleep(0.5 + Math.random() * 1.0);
}
