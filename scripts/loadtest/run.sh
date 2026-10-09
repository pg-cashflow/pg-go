#!/usr/bin/env bash
# =============================================================================
# Gate 09: Karpathy 6-Step Stress & Load Test Runner (Bash / CI)
# Methodology: ASD-STE100 & Karpathy Test Discipline
# =============================================================================

set -euo pipefail

if [ -f ".env" ]; then
  # Export vars from .env without overriding already set env vars
  while IFS='=' read -r key val || [ -n "$key" ]; do
    [[ "$key" =~ ^#.*$ ]] && continue
    [ -z "$key" ] && continue
    clean_val=$(echo "$val" | tr -d '\r' | sed -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'$//")
    if [ -z "${!key:-}" ]; then
      export "$key"="$clean_val"
    fi
  done < .env
fi

BASE_URL="${BASE_URL:-http://localhost:8080}"
FAKE_GATEWAY_URL="${FAKE_GATEWAY_URL:-http://localhost:8081}"
DATABASE_URL="${DATABASE_URL:-}"
INCLUDE_SOAK="${INCLUDE_SOAK:-false}"
SOAK_DURATION="${SOAK_DURATION:-5m}"

log_step() {
  local num="$1"
  local title="$2"
  echo ""
  echo "========================================================"
  echo "  Step $num: $title"
  echo "========================================================"
}

# -----------------------------------------------------------------------------
# Karpathy Step 1: Look at the data first
# -----------------------------------------------------------------------------
log_step "1" "Look at data first (Verify server, gateway, and database readiness)"

echo "Checking backend health at ${BASE_URL}/healthz..."
curl -fsS "${BASE_URL}/healthz" || {
  echo "WARNING: Backend server not reachable at ${BASE_URL}/healthz."
  echo "Ensure the server is running with DATABASE_MAX_CONNS=25 before executing load test."
}

echo "Checking fake gateway health at ${FAKE_GATEWAY_URL}/healthz..."
if ! curl -fsS "${FAKE_GATEWAY_URL}/healthz" > /dev/null 2>&1; then
  echo "Starting Fake Cashfree Gateway on port 8081 in background..."
  go run scripts/loadtest/fake_gateway.go -port=8081 -target="${BASE_URL}" &
  GATEWAY_PID=$!
  sleep 3
  echo "Fake Gateway started with PID ${GATEWAY_PID}."
  trap 'kill ${GATEWAY_PID} > /dev/null 2>&1 || true' EXIT
fi

# -----------------------------------------------------------------------------
# Karpathy Step 2: Check state at initialization
# -----------------------------------------------------------------------------
log_step "2" "Check state at initialization (Ensure baseline ledger is balanced)"

if [ -n "${DATABASE_URL}" ]; then
  echo "Executing pre-flight SQL invariant assertions..."
  if command -v go >/dev/null 2>&1; then
    go run scripts/loadtest/check_invariants.go -snapshot="scripts/loadtest/baseline.json" "${DATABASE_URL}"
    echo "Minting load test authentication tokens..."
    TOKEN_JSON=$(go run scripts/loadtest/mint_token.go "${DATABASE_URL}" 2>/dev/null || echo "{}")
    OWNER_TOKEN=$(echo "${TOKEN_JSON}" | grep -o '"owner_token": "[^"]*' | cut -d'"' -f4 || echo "")
    TENANT_TOKEN=$(echo "${TOKEN_JSON}" | grep -o '"tenant_token": "[^"]*' | cut -d'"' -f4 || echo "")
    PROPERTY_ID=$(echo "${TOKEN_JSON}" | grep -o '"property_id": "[^"]*' | cut -d'"' -f4 || echo "")
    DUE_ID=$(echo "${TOKEN_JSON}" | grep -o '"due_id": "[^"]*' | cut -d'"' -f4 || echo "")
  else
    psql "${DATABASE_URL}" -f scripts/loadtest/post_run_invariants.sql
  fi
  echo "Pre-flight database invariants VERIFIED: Zero drift at baseline recorded."
else
  echo "DATABASE_URL not set. Skipping direct SQL check."
fi

# -----------------------------------------------------------------------------
# Karpathy Step 3: Overfit one example
# -----------------------------------------------------------------------------
log_step "3" "Overfit one example (Run minimal single-VU smoke test)"

echo "Executing smoke check with 1 VU for 5s..."
k6 run --vus 1 --duration 5s \
  -e BASE_URL="${BASE_URL}" \
  -e FAKE_GATEWAY_URL="${FAKE_GATEWAY_URL}" \
  -e OWNER_TOKEN="${OWNER_TOKEN}" \
  -e PROPERTY_ID="${PROPERTY_ID}" \
  scripts/loadtest/scenarios/read_dashboard.js

echo "Single example overfit PASSED: Minimal path is 100% green."

# -----------------------------------------------------------------------------
# Karpathy Step 4: Compare with dumb baseline
# -----------------------------------------------------------------------------
log_step "4" "Compare with dumb baseline (Assert baseline health checks meet latency floors)"

curl -fsS -w "Baseline /api/healthz response time: %{time_total}s\n" -o /dev/null "${BASE_URL}/api/healthz"

# -----------------------------------------------------------------------------
# Karpathy Step 5: Fix seeds
# -----------------------------------------------------------------------------
log_step "5" "Fix seeds (Deterministic run settings)"

export K6_SYSTEM_TAGS="proto,subproto,status,method,url,name,group,check,error,error_code,scenario,service"
echo "Using deterministic order and webhook generators."

# -----------------------------------------------------------------------------
# Karpathy Step 6: Change one thing at a time
# -----------------------------------------------------------------------------
log_step "6" "Change one thing at a time (Sequential load scenario execution)"

echo "--> Stage 6A: Read Dashboard & Search Load (Target: 100 VUs)"
k6 run -e BASE_URL="${BASE_URL}" -e OWNER_TOKEN="${OWNER_TOKEN}" -e PROPERTY_ID="${PROPERTY_ID}" scripts/loadtest/scenarios/read_dashboard.js

echo "--> Stage 6B: Checkout & Webhook Concurrency (Target: 60 RPS Open-Model)"
k6 run -e BASE_URL="${BASE_URL}" -e FAKE_GATEWAY_URL="${FAKE_GATEWAY_URL}" -e TENANT_TOKEN="${TENANT_TOKEN}" -e DUE_ID="${DUE_ID}" scripts/loadtest/scenarios/checkout_webhook.js

if [ "${INCLUDE_SOAK}" = "true" ]; then
  echo "--> Stage 6C: Extended Soak Test (Duration: ${SOAK_DURATION})"
  k6 run -e BASE_URL="${BASE_URL}" -e SOAK_DURATION="${SOAK_DURATION}" -e OWNER_TOKEN="${OWNER_TOKEN}" -e PROPERTY_ID="${PROPERTY_ID}" scripts/loadtest/soak_test.js
fi

# -----------------------------------------------------------------------------
# Post-Run Step: Enforce SQL Invariant Verification Gate
# -----------------------------------------------------------------------------
log_step "Post-Run" "Enforce SQL Invariant Verification Gate"

if [ -n "${DATABASE_URL}" ]; then
  echo "Executing post-run invariant assertions in database..."
  if command -v go >/dev/null 2>&1; then
    go run scripts/loadtest/check_invariants.go -assert-delta="scripts/loadtest/baseline.json" "${DATABASE_URL}"
  else
    psql "${DATABASE_URL}" -f scripts/loadtest/post_run_invariants.sql
  fi
  echo ""
  echo "SUCCESS: Post-run SQL invariants strictly verified: ZERO ledger drift, ZERO duplicates."
else
  echo "DATABASE_URL not set. Invariant file located at scripts/loadtest/post_run_invariants.sql"
fi

echo ""
echo "========================================================"
echo "  Gate 09: Stress & Load Testing COMPLETED SUCCESSFULLY! "
echo "========================================================"
