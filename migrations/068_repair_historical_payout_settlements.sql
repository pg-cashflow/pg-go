-- Migration 068: Repair historical succeeded payout items lacking double-entry settlement journal entries.
--
-- Financial Invariants:
-- INV-003: Every settled payout shall have balanced double-entry journal entries:
--   - Debit  refund_payable (amount_paise) line_kind: 'refund_paid'
--   - Credit bank           (amount_paise) line_kind: 'cash_out'
-- source_type = 'payout_settlement', source_id = payout_items.id
--
-- Safety Guarantees:
-- 1. Idempotent: ON CONFLICT (source_type, source_id, line_kind) DO NOTHING
-- 2. Scoped: Derives property_id via payee_id (payout_payees) or departure_id (tenant_departures).
-- 3. Only targets payout_items where status = 'succeeded' and amount_paise > 0.

INSERT INTO financial_journal_entries (
    id,
    property_id,
    account_code,
    debit_paise,
    credit_paise,
    source_type,
    source_id,
    line_kind,
    occurred_at,
    created_at
)
SELECT
    gen_random_uuid(),
    COALESCE(pp.property_id, td.property_id) AS property_id,
    'refund_payable',
    pi.amount_paise,
    0,
    'payout_settlement',
    pi.id,
    'refund_paid',
    COALESCE(pi.settled_at, pi.updated_at, NOW()),
    NOW()
FROM payout_items pi
LEFT JOIN payout_payees pp ON pi.payee_id = pp.id
LEFT JOIN tenant_departures td ON pi.departure_id = td.id
WHERE pi.status = 'succeeded'
  AND pi.amount_paise > 0
  AND COALESCE(pp.property_id, td.property_id) IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM financial_journal_entries fje
      WHERE fje.source_type = 'payout_settlement'
        AND fje.source_id = pi.id
        AND fje.line_kind = 'refund_paid'
  )
ON CONFLICT (source_type, source_id, line_kind) DO NOTHING;

INSERT INTO financial_journal_entries (
    id,
    property_id,
    account_code,
    debit_paise,
    credit_paise,
    source_type,
    source_id,
    line_kind,
    occurred_at,
    created_at
)
SELECT
    gen_random_uuid(),
    COALESCE(pp.property_id, td.property_id) AS property_id,
    'bank',
    0,
    pi.amount_paise,
    'payout_settlement',
    pi.id,
    'cash_out',
    COALESCE(pi.settled_at, pi.updated_at, NOW()),
    NOW()
FROM payout_items pi
LEFT JOIN payout_payees pp ON pi.payee_id = pp.id
LEFT JOIN tenant_departures td ON pi.departure_id = td.id
WHERE pi.status = 'succeeded'
  AND pi.amount_paise > 0
  AND COALESCE(pp.property_id, td.property_id) IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM financial_journal_entries fje
      WHERE fje.source_type = 'payout_settlement'
        AND fje.source_id = pi.id
        AND fje.line_kind = 'cash_out'
  )
ON CONFLICT (source_type, source_id, line_kind) DO NOTHING;
