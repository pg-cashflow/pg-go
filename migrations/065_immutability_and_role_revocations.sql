-- Migration 065: Privilege hardening for append-only financial records
-- Revoke UPDATE and DELETE on immutable financial ledger tables from runtime roles.
--
-- Follows 058.
-- Ensures capital_transactions, expense_payments, and payout_records are strictly append-only.
-- Note: budgets is deliberately excluded because UpsertBudget requires UPDATE privilege.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app') THEN
        REVOKE UPDATE, DELETE ON capital_transactions, expense_payments, payout_records, payout_record_items FROM pgapp_app;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
        REVOKE UPDATE, DELETE ON capital_transactions, expense_payments, payout_records, payout_record_items FROM pgapp_maint;
    END IF;
END $$;
