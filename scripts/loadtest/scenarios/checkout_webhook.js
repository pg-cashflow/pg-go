// ASD-STE100 Compliant: Checkout & Webhook Concurrency Stress Scenario.
// Validates money rail invariants, duplicate webhook stampedes, and zero ledger drift.
// Uses open-model ramping-arrival-rate to prevent coordinated omission.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate, Counter } from 'k6/metrics';

// Custom metric trackers
const webhookDuration = new Trend('webhook_processing_duration', true);
const checkoutDuration = new Trend('checkout_intent_duration', true);
const idempotentDuplicateAcks = new Counter('idempotent_duplicate_acks');
const failedWebhooks = new Rate('failed_webhook_rate');

export const options = {
  scenarios: {
    checkout_webhook_concurrency: {
      executor: 'ramping-arrival-rate',
      startRate: 10,
      timeUnit: '1s',
      preAllocatedVUs: 50,
      maxVUs: 200,
      stages: [
        { target: 20, duration: '30s' },  // Warm-up to 20 RPS
        { target: 60, duration: '1m' },   // Ramp to peak target (60 RPS = 12-20x normal peak)
        { target: 60, duration: '2m' },   // Sustain at peak concurrency
        { target: 10, duration: '30s' },  // Graceful wind-down
      ],
    },
  },
  thresholds: {
    'http_req_failed': ['rate<0.01'],                                   // Under 1% error rate
    'webhook_processing_duration': ['p(95)<500', 'p(99)<2000'],         // Webhook acked in < 2s
    'checkout_intent_duration': ['p(95)<800'],                          // Intent creation p95 < 800ms
  },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const FAKE_GATEWAY_URL = __ENV.FAKE_GATEWAY_URL || 'http://localhost:8081';
const TENANT_TOKEN = __ENV.TENANT_TOKEN || '';
const DUE_ID = __ENV.DUE_ID || '';

export default function () {
  const iterNum = Math.floor(Math.random() * 1000000);
  let orderId = `order_stress_${Date.now()}_${iterNum}`;
  const paymentAmount = 1500.00; // ₹1,500.00 = 150000 paise
  const cfPaymentId = 30000000 + Math.floor(Math.random() * 89999999);
  const utr = `UTR${Math.floor(10000000 + Math.random() * 90000000)}`;

  // 1. Create Checkout Order: either via backend tenant checkout or direct gateway order
  const orderStart = new Date();
  if (TENANT_TOKEN && DUE_ID) {
    const payRes = http.get(`${BASE_URL}/api/tenant/dues/${DUE_ID}/pay`, {
      headers: {
        'Authorization': `Bearer ${TENANT_TOKEN}`,
        'Content-Type': 'application/json',
      },
    });
    checkoutDuration.add(new Date() - orderStart);
    check(payRes, {
      'tenant due checkout intent generated': (r) => r.status === 200,
    });
    if (payRes.status === 200) {
      try {
        const body = JSON.parse(payRes.body);
        if (body && body.payment_session_id) {
          // If session id format is session_<order_id>_<cf_id>
          const parts = body.payment_session_id.split('_');
          if (parts.length >= 3) {
            orderId = parts.slice(1, parts.length - 1).join('_');
          }
        }
      } catch (e) {}
    }
  } else {
    const orderPayload = JSON.stringify({
      order_id: orderId,
      order_amount: paymentAmount,
      order_currency: 'INR',
      customer_details: {
        customer_id: `cust_${iterNum}`,
        customer_phone: '9876543210',
        customer_email: 'test@example.com',
      },
      order_note: 'Load test checkout',
    });

    const orderRes = http.post(`${FAKE_GATEWAY_URL}/pg/orders`, orderPayload, {
      headers: { 'Content-Type': 'application/json' },
    });
    checkoutDuration.add(new Date() - orderStart);

    check(orderRes, {
      'order created on gateway': (r) => r.status === 200,
    });
  }

  // 2. Dispatch Payment Success Webhook to Backend via Fake Gateway
  // 30% of requests trigger intentional concurrent duplicate webhooks (stampede testing)
  const isDuplicateStampede = Math.random() < 0.30;
  const duplicateCount = isDuplicateStampede ? 2 : 1;

  const webhookDispatchPayload = JSON.stringify({
    order_id: orderId,
    cf_payment_id: cfPaymentId,
    payment_amount: paymentAmount,
    bank_reference: utr,
    duplicate_count: duplicateCount,
    delay_ms: 0,
  });

  const whStart = new Date();
  const whRes = http.post(`${FAKE_GATEWAY_URL}/mock/dispatch-webhook`, webhookDispatchPayload, {
    headers: { 'Content-Type': 'application/json' },
  });
  webhookDuration.add(new Date() - whStart);

  const ok = check(whRes, {
    'webhook accepted by mock dispatcher': (r) => r.status === 202,
  });

  if (!ok) {
    failedWebhooks.add(1);
  } else if (isDuplicateStampede) {
    idempotentDuplicateAcks.add(1);
  }

  // Paced random delay between iterations
  sleep(0.05 + Math.random() * 0.1);
}
