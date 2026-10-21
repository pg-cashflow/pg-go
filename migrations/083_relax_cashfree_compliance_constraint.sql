-- Migration 083: Drop premises document compliance constraint blocking Cashfree activation
-- Allows property owners to activate Cashfree without being blocked by un-uploaded lease/NOC documents.
ALTER TABLE properties DROP CONSTRAINT IF EXISTS chk_property_cashfree_compliance;
