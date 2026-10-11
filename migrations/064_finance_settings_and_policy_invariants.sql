-- 064: Database check constraints for finance settings and approval policies.
-- In accordance with ADR-022 / ADR-024, constraints are added NOT VALID
-- during the expand phase to ensure zero-downtime application without blocking
-- or failing on existing legacy rows.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_property_finance_settings_fiscal_day'
    ) THEN
        ALTER TABLE property_finance_settings
            ADD CONSTRAINT chk_property_finance_settings_fiscal_day
            CHECK (fiscal_month_start_day >= 1 AND fiscal_month_start_day <= 28)
            NOT VALID;
    END IF;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_property_finance_settings_tdr_bps'
    ) THEN
        ALTER TABLE property_finance_settings
            ADD CONSTRAINT chk_property_finance_settings_tdr_bps
            CHECK (tdr_effective_bps >= 0 AND tdr_effective_bps <= 10000)
            NOT VALID;
    END IF;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_approval_policies_ordering'
    ) THEN
        ALTER TABLE approval_policies
            ADD CONSTRAINT chk_approval_policies_ordering
            CHECK (single_expense_limit_paise <= manager_daily_limit_paise AND manager_daily_limit_paise <= manager_monthly_limit_paise)
            NOT VALID;
    END IF;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_approval_policies_non_negative'
    ) THEN
        ALTER TABLE approval_policies
            ADD CONSTRAINT chk_approval_policies_non_negative
            CHECK (
                single_expense_limit_paise >= 0 AND
                manager_daily_limit_paise >= 0 AND
                manager_monthly_limit_paise >= 0 AND
                owner_approval_threshold_paise >= 0 AND
                reimbursement_threshold_paise >= 0
            )
            NOT VALID;
    END IF;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_approval_policies_max_ceiling'
    ) THEN
        ALTER TABLE approval_policies
            ADD CONSTRAINT chk_approval_policies_max_ceiling
            CHECK (
                single_expense_limit_paise <= 100000000000 AND
                manager_daily_limit_paise <= 100000000000 AND
                manager_monthly_limit_paise <= 100000000000 AND
                owner_approval_threshold_paise <= 100000000000 AND
                reimbursement_threshold_paise <= 100000000000
            )
            NOT VALID;
    END IF;
END $$;
