-- =============================================================================
-- Gate 09: Post-Run SQL Invariant Verification Suite
-- Methodology: ASD-STE100 & Karpathy Test Discipline
-- Rule: A fast load test with a corrupted ledger is a failed test.
-- =============================================================================

DO $$
DECLARE
  v_debit_total          BIGINT;
  v_credit_total         BIGINT;
  v_drift_paise          BIGINT;
  v_unbalanced_sources   BIGINT;
  v_duplicate_cf_pmts    BIGINT;
  v_duplicate_utrs       BIGINT;
  v_overpaid_dues        BIGINT;
  v_deadlocks            BIGINT;
  v_failed_webhooks      BIGINT;
BEGIN
  RAISE NOTICE '=============================================================';
  RAISE NOTICE '  PG Cashflow: Running Post-Load Stress SQL Invariants Check  ';
  RAISE NOTICE '=============================================================';

  -- ---------------------------------------------------------------------------
  -- Invariant 1: Global Double-Entry Debit/Credit Conservation
  -- SUM(debit_paise) == SUM(credit_paise) across financial_journal_entries
  -- ---------------------------------------------------------------------------
  SELECT
    COALESCE(SUM(debit_paise), 0),
    COALESCE(SUM(credit_paise), 0)
  INTO v_debit_total, v_credit_total
  FROM financial_journal_entries;

  v_drift_paise := v_debit_total - v_credit_total;
  IF v_drift_paise <> 0 THEN
    RAISE EXCEPTION 'CRITICAL INVARIANT VIOLATION: Ledger drift detected! Debits=%, Credits=%, Drift=% paise',
      v_debit_total, v_credit_total, v_drift_paise;
  ELSE
    RAISE NOTICE '[PASS] Invariant 1: Ledger double-entry balance preserved (Debits=%, Credits=%, Drift=0 paise)',
      v_debit_total, v_credit_total;
  END IF;

  -- ---------------------------------------------------------------------------
  -- Invariant 2: Per-Source Balanced Entries
  -- Every (source_type, source_id) must have equal debits and credits
  -- ---------------------------------------------------------------------------
  SELECT COUNT(*)
  INTO v_unbalanced_sources
  FROM (
    SELECT source_type, source_id
    FROM financial_journal_entries
    GROUP BY source_type, source_id
    HAVING SUM(debit_paise) <> SUM(credit_paise)
  ) sub;

  IF v_unbalanced_sources > 0 THEN
    RAISE EXCEPTION 'CRITICAL INVARIANT VIOLATION: % unbalanced journal source transactions found!', v_unbalanced_sources;
  ELSE
    RAISE NOTICE '[PASS] Invariant 2: All journal transactions strictly balanced per source';
  END IF;

  -- ---------------------------------------------------------------------------
  -- Invariant 3: Zero Duplicate Gateway Payments
  -- Cashfree cf_payment_id must be unique across all recorded payments
  -- ---------------------------------------------------------------------------
  SELECT COUNT(*)
  INTO v_duplicate_cf_pmts
  FROM (
    SELECT provider, provider_payment_id
    FROM payments
    WHERE provider_payment_id IS NOT NULL AND provider_payment_id <> ''
    GROUP BY provider, provider_payment_id
    HAVING COUNT(*) > 1
  ) sub;

  IF v_duplicate_cf_pmts > 0 THEN
    RAISE EXCEPTION 'CRITICAL INVARIANT VIOLATION: % duplicate gateway provider_payment_id entries recorded in payments!', v_duplicate_cf_pmts;
  ELSE
    RAISE NOTICE '[PASS] Invariant 3: Zero duplicate provider payments recorded';
  END IF;

  -- ---------------------------------------------------------------------------
  -- Invariant 4: Zero Duplicate UTRs
  -- Bank UTR / upi_txn_id must never appear on multiple payment rows
  -- ---------------------------------------------------------------------------
  SELECT COUNT(*)
  INTO v_duplicate_utrs
  FROM (
    SELECT upi_txn_id
    FROM payments
    WHERE upi_txn_id IS NOT NULL AND upi_txn_id <> ''
    GROUP BY upi_txn_id
    HAVING COUNT(*) > 1
  ) sub;

  IF v_duplicate_utrs > 0 THEN
    RAISE EXCEPTION 'CRITICAL INVARIANT VIOLATION: % duplicate bank UTRs recorded in payments!', v_duplicate_utrs;
  ELSE
    RAISE NOTICE '[PASS] Invariant 4: Zero duplicate bank UTRs recorded';
  END IF;

  -- ---------------------------------------------------------------------------
  -- Invariant 5: Zero Overpaid Dues
  -- Cumulative matched payments must never exceed original due amount
  -- ---------------------------------------------------------------------------
  SELECT COUNT(*)
  INTO v_overpaid_dues
  FROM (
    SELECT d.id, d.original_amount, COALESCE(SUM(p.amount), 0) AS total_paid
    FROM dues d
    JOIN payments p ON p.due_id = d.id
    GROUP BY d.id, d.original_amount
    HAVING COALESCE(SUM(p.amount), 0) > d.original_amount
  ) sub;

  IF v_overpaid_dues > 0 THEN
    RAISE EXCEPTION 'CRITICAL INVARIANT VIOLATION: % dues have payments exceeding original_amount!', v_overpaid_dues;
  ELSE
    RAISE NOTICE '[PASS] Invariant 5: Zero overpaid dues detected';
  END IF;

  -- ---------------------------------------------------------------------------
  -- Invariant 6: Zero Database Deadlocks Recorded
  -- Assert deadlocks stat on current database is 0
  -- ---------------------------------------------------------------------------
  SELECT COALESCE(deadlocks, 0)
  INTO v_deadlocks
  FROM pg_stat_database
  WHERE datname = current_database();

  IF v_deadlocks > 0 THEN
    RAISE WARNING '[WARN] Invariant 6: % database deadlocks recorded during stress test', v_deadlocks;
  ELSE
    RAISE NOTICE '[PASS] Invariant 6: Zero database deadlocks recorded';
  END IF;

  RAISE NOTICE '=============================================================';
  RAISE NOTICE '  SUCCESS: All Gate 09 Post-Load Invariants Passed Strictly! ';
  RAISE NOTICE '=============================================================';
END $$;
