-- Migration 015: Owner Payment Proofs Enhancement
-- 1. Add payment collection mode to properties with forward-compatibility placeholders.
-- 2. Add duplicate image detection (image_hash, is_duplicate) and OCR metadata to payment_reports.

ALTER TABLE properties
  ADD COLUMN IF NOT EXISTS payment_collection_mode VARCHAR(20) NOT NULL DEFAULT 'manual_proof',
  ADD COLUMN IF NOT EXISTS gateway_enabled_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS gateway_sub_merchant_id TEXT;

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'chk_properties_payment_collection_mode'
  ) THEN
    ALTER TABLE properties ADD CONSTRAINT chk_properties_payment_collection_mode
      CHECK (payment_collection_mode IN ('manual_proof', 'gateway'));
  END IF;
END $$;

ALTER TABLE payment_reports
  ADD COLUMN IF NOT EXISTS image_hash CHAR(64),
  ADD COLUMN IF NOT EXISTS is_duplicate BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS ocr_amount INTEGER,
  ADD COLUMN IF NOT EXISTS ocr_utr VARCHAR(50),
  ADD COLUMN IF NOT EXISTS ocr_txn_date DATE,
  ADD COLUMN IF NOT EXISTS ocr_confidence REAL;

DROP INDEX IF EXISTS idx_payment_reports_image_hash;

CREATE INDEX IF NOT EXISTS idx_payment_reports_prop_image_hash 
  ON payment_reports(property_id, image_hash) WHERE image_hash IS NOT NULL;
