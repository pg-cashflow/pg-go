SET lock_timeout = '5s';

-- Migration 133: Purge KYC source PII retained on rows superseded by Migration 126.
-- Standard: ASD-STE100
-- Purpose:
--   1. Migration 126 marked duplicate active KYC rows as 'expired' with failure_reason
--      'superseded during active state constraint consolidation', without clearing source PII.
--   2. ExpireStaleManualReviews filters only 'pending_manual_review', so those rows are skipped.
--   3. Purge document_bytes, document_content_type, document_url, ocr_data,
--      attested_photo_bytes, and photo_stored on those rows and append one audit entry
--      per purged verification to kyc_audit_log.

DO $$
DECLARE
    v_rec RECORD;
    v_prev_hash TEXT;
    v_now TIMESTAMPTZ := date_trunc('second', clock_timestamp() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    v_ts_str TEXT;
    v_detail TEXT;
    v_entry_hash TEXT;
BEGIN
    v_ts_str := to_char(v_now AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"');

    FOR v_rec IN
        SELECT kv.id, kv.tenant_id
        FROM kyc_verification kv
        WHERE kv.status = 'expired'
          AND kv.failure_reason = 'superseded during active state constraint consolidation'
          AND (
              kv.document_bytes IS NOT NULL
              OR kv.document_content_type IS NOT NULL
              OR kv.document_url IS NOT NULL
              OR kv.ocr_data IS NOT NULL
              OR kv.attested_photo_bytes IS NOT NULL
              OR kv.photo_stored = TRUE
          )
        ORDER BY kv.tenant_id, kv.id
    LOOP
        -- Lock tenant row first to respect global lock hierarchy (tenants -> kyc_verification).
        PERFORM 1 FROM tenants WHERE id = v_rec.tenant_id FOR UPDATE;

        UPDATE kyc_verification
        SET document_bytes = NULL,
            document_content_type = NULL,
            document_url = NULL,
            ocr_data = NULL,
            attested_photo_bytes = NULL,
            photo_stored = FALSE,
            updated_at = v_now
        WHERE id = v_rec.id
          AND status = 'expired'
          AND (
              document_bytes IS NOT NULL
              OR document_content_type IS NOT NULL
              OR document_url IS NOT NULL
              OR ocr_data IS NOT NULL
              OR attested_photo_bytes IS NOT NULL
              OR photo_stored = TRUE
          );

        IF FOUND THEN
            SELECT detail_hash INTO v_prev_hash
            FROM kyc_audit_log
            WHERE tenant_id = v_rec.tenant_id
            ORDER BY id DESC
            LIMIT 1;

            v_detail := 'verification_id=' || v_rec.id::text || ' reason=migration_126_superseded_pii_purge';
            v_entry_hash := encode(
                sha256(
                    convert_to(
                        COALESCE(v_prev_hash, '') || '|' || v_rec.tenant_id::text || '|migration_133|superseded_pii_purged|' || v_detail || '|' || v_ts_str,
                        'UTF8'
                    )
                ),
                'hex'
            );

            INSERT INTO kyc_audit_log (tenant_id, actor, action, detail_hash, prev_hash, created_at)
            VALUES (v_rec.tenant_id, 'migration_133', 'superseded_pii_purged', v_entry_hash, v_prev_hash, v_now);
        END IF;
    END LOOP;
END $$;
