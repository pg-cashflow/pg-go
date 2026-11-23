SET lock_timeout = '5s';

-- Migration 124: Platform Payout Reconciliation Maker-Checker Control
-- Standard: ASD-STE100
-- Purpose: Implement two-person maker/checker control for manual platform payout reconciliation.
-- Requirement: Maker proposes terminal status with evidence. Independent checker approves.
-- Database constraint prohibits maker self-approval (checker_id <> maker_id).

CREATE TABLE IF NOT EXISTS platform_payout_reconciliation_cases (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payout_item_id UUID NOT NULL REFERENCES payout_items(id) ON DELETE RESTRICT,
    proposed_status VARCHAR(32) NOT NULL,
    utr TEXT,
    cf_transfer_id TEXT,
    settled_at TIMESTAMPTZ,
    failure_reason TEXT,
    notes TEXT NOT NULL,
    evidence_ref TEXT NOT NULL,
    evidence_hash TEXT NOT NULL,
    maker_id UUID NOT NULL REFERENCES users(id),
    checker_id UUID REFERENCES users(id),
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    item_snapshot JSONB NOT NULL,
    rejection_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_reconciliation_case_status CHECK (status IN ('pending', 'approved', 'rejected')),
    CONSTRAINT chk_reconciliation_case_proposed_status CHECK (proposed_status IN ('succeeded', 'failed', 'rejected', 'reversed')),
    CONSTRAINT chk_maker_checker_distinct CHECK (checker_id IS NULL OR maker_id <> checker_id)
);

CREATE INDEX IF NOT EXISTS idx_payout_recon_cases_item ON platform_payout_reconciliation_cases (payout_item_id);
CREATE INDEX IF NOT EXISTS idx_payout_recon_cases_status ON platform_payout_reconciliation_cases (status);

-- RLS: Platform operations can access all cases when app.is_platform_ops = 'true'
ALTER TABLE platform_payout_reconciliation_cases ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS platform_payout_reconciliation_cases_app ON platform_payout_reconciliation_cases;
CREATE POLICY platform_payout_reconciliation_cases_app ON platform_payout_reconciliation_cases
    FOR ALL TO pgapp_app
    USING (current_setting('app.is_platform_ops', true) = 'true')
    WITH CHECK (current_setting('app.is_platform_ops', true) = 'true');

DROP POLICY IF EXISTS platform_payout_reconciliation_cases_maint ON platform_payout_reconciliation_cases;
CREATE POLICY platform_payout_reconciliation_cases_maint ON platform_payout_reconciliation_cases
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);
