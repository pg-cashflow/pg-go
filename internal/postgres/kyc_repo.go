package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type KYCRepo struct {
	pool *pgxpool.Pool
}

func NewKYCRepo(pool *pgxpool.Pool) *KYCRepo {
	return &KYCRepo{pool: pool}
}

const (
	kycVerificationCols = `id, tenant_id, consent_id, vendor_name, vendor_reference_id,
		masked_uid, identity_hash, hash_key_version, is_dedupable, duplicate_detected, method,
		status, failure_reason, verified_at, expires_at, created_at, updated_at`

	kycConsentCols = `id, tenant_id, purpose, consent_version, consent_text,
		consent_text_hash, consent_given_at, ip_address, user_agent, revoked_at`

	kycAuditCols = `id, tenant_id, actor, action, detail_hash, prev_hash, created_at`
)

func scanKYCVerification(row pgx.Row) (*domain.KYCVerification, error) {
	var v domain.KYCVerification
	err := row.Scan(
		&v.ID, &v.TenantID, &v.ConsentID, &v.VendorName, &v.VendorReferenceID,
		&v.MaskedUID, &v.IdentityHash, &v.HashKeyVersion, &v.IsDedupable, &v.DuplicateDetected, &v.Method,
		&v.Status, &v.FailureReason, &v.VerifiedAt, &v.ExpiresAt, &v.CreatedAt, &v.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrVerificationNotFound
		}
		return nil, err
	}
	return &v, nil
}

func scanKYCConsent(row pgx.Row) (*domain.KYCConsent, error) {
	var c domain.KYCConsent
	err := row.Scan(
		&c.ID, &c.TenantID, &c.Purpose, &c.ConsentVersion, &c.ConsentText,
		&c.ConsentTextHash, &c.ConsentGivenAt, &c.IPAddress, &c.UserAgent, &c.RevokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrVerificationNotFound
		}
		return nil, err
	}
	return &c, nil
}

// RecordConsent persists tenant KYC consent and logs an audit record.
func (r *KYCRepo) RecordConsent(ctx context.Context, c *domain.KYCConsent, actor string) error {
	now := time.Now().UTC()
	if c.ConsentGivenAt.IsZero() {
		c.ConsentGivenAt = now
	}

	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO kyc_consent (
				tenant_id, purpose, consent_version, consent_text, consent_text_hash,
				consent_given_at, ip_address, user_agent
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id
		`, c.TenantID, c.Purpose, c.ConsentVersion, c.ConsentText, c.ConsentTextHash,
			c.ConsentGivenAt, c.IPAddress, c.UserAgent).Scan(&c.ID)
		if err != nil {
			return fmt.Errorf("kyc: insert consent: %w", err)
		}

		detail := fmt.Sprintf("purpose=%s version=%s", c.Purpose, c.ConsentVersion)
		if err := r.appendAuditLogTx(ctx, tx, c.TenantID, actor, "consent_given", detail, now); err != nil {
			return fmt.Errorf("kyc: append consent audit log: %w", err)
		}

		return nil
	})
}

// GetActiveConsentByTenant fetches the most recent unrevoked consent for a tenant.
func (r *KYCRepo) GetActiveConsentByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCConsent, error) {
	return scanKYCConsent(r.pool.QueryRow(ctx, `
		SELECT `+kycConsentCols+`
		FROM kyc_consent
		WHERE tenant_id = $1 AND revoked_at IS NULL
		ORDER BY consent_given_at DESC
		LIMIT 1
	`, tenantID))
}

// ============================================================================
// LOCK HIERARCHY INVARIANT:
// To prevent deadlocks (SQLSTATE 40P01) across concurrent transactions touching
// both `tenants` and `kyc_verification`, ALL transactions must acquire row locks
// in strict top-down order:
//   1. tenants (SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE)
//   2. kyc_verification (SELECT ... FROM kyc_verification WHERE ... FOR UPDATE)
// NEVER acquire a row lock on `kyc_verification` and subsequently lock or update
// `tenants`!
// ============================================================================

// RevokeConsent implements DPDP Rule 8 data erasure:
// Marks consent revoked, nulls out masked_uid and identity_hash on verification records,
// clears tenant profile Aadhaar reference, and records an unbroken audit trail.
func (r *KYCRepo) RevokeConsent(ctx context.Context, tenantID uuid.UUID, actor string) error {
	now := time.Now().UTC()

	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		// Enforce lock hierarchy: Step 1 = acquire exclusive row lock on tenant
		var exists int
		err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&exists)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("kyc: tenant %s not found", tenantID)
			}
			return fmt.Errorf("kyc: lock tenant: %w", err)
		}

		// 1. Mark consent revoked
		_, err = tx.Exec(ctx, `
			UPDATE kyc_consent
			SET revoked_at = $2
			WHERE tenant_id = $1 AND revoked_at IS NULL
		`, tenantID, now)
		if err != nil {
			return fmt.Errorf("kyc: revoke consent records: %w", err)
		}

		// 2. Erase PII from verification table while retaining skeleton for compliance
		_, err = tx.Exec(ctx, `
			UPDATE kyc_verification
			SET status = 'revoked',
				masked_uid = NULL,
				identity_hash = NULL,
				updated_at = $2
			WHERE tenant_id = $1 AND status IN ('pending', 'verified')
		`, tenantID, now)
		if err != nil {
			return fmt.Errorf("kyc: revoke verification records: %w", err)
		}

		// 3. Clear tenant profile Aadhaar last 4
		_, err = tx.Exec(ctx, `
			UPDATE tenants
			SET aadhaar_last4 = NULL,
				updated_at = $2
			WHERE id = $1
		`, tenantID, now)
		if err != nil {
			return fmt.Errorf("kyc: clear tenant aadhaar_last4: %w", err)
		}

		// 4. Audit trail entry
		if err := r.appendAuditLogTx(ctx, tx, tenantID, actor, "consent_revoked", "consent revoked and PII scrubbed", now); err != nil {
			return fmt.Errorf("kyc: append revoke audit log: %w", err)
		}

		return nil
	})
}

// InitiateVerificationTx atomizes superseding any stale pending/expired verification and
// inserting a new pending verification record while maintaining unbroken audit hash chains.
func (r *KYCRepo) InitiateVerificationTx(
	ctx context.Context,
	tenantID, consentID uuid.UUID,
	method domain.KYCMethod,
	vendorName, vendorRefID, actor string,
) (*domain.KYCVerification, error) {
	now := time.Now().UTC()
	var newVerification *domain.KYCVerification

	err := WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		// 1. Acquire row lock on tenant to serialize attempts and prevent race conditions
		var exists int
		err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&exists)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("kyc: tenant %s not found", tenantID)
			}
			return fmt.Errorf("kyc: lock tenant: %w", err)
		}

		// 2. Check for active (pending or verified) verification
		existing, err := scanKYCVerification(tx.QueryRow(ctx, `
			SELECT `+kycVerificationCols+`
			FROM kyc_verification
			WHERE tenant_id = $1 AND status IN ('pending', 'verified')
			FOR UPDATE
		`, tenantID))
		if err != nil && !errors.Is(err, domain.ErrVerificationNotFound) {
			return fmt.Errorf("kyc: query existing verification: %w", err)
		}

		if existing != nil {
			if existing.Status == domain.KYCStatusVerified {
				// Check if verified record has expired
				if existing.ExpiresAt != nil && !now.Before(*existing.ExpiresAt) {
					// Record has expired; transition to 'expired', clear active tenant profile last4, and audit
					_, err = tx.Exec(ctx, `
						UPDATE kyc_verification
						SET status = 'expired', updated_at = $2
						WHERE id = $1
					`, existing.ID, now)
					if err != nil {
						return fmt.Errorf("kyc: mark expired verification: %w", err)
					}

					_, err = tx.Exec(ctx, `
						UPDATE tenants
						SET aadhaar_last4 = NULL, updated_at = $2
						WHERE id = $1
					`, tenantID, now)
					if err != nil {
						return fmt.Errorf("kyc: clear lapsed tenant aadhaar_last4: %w", err)
					}

					if err := r.appendAuditLogTx(ctx, tx, tenantID, actor, "verification_expired", "lapsed verification marked expired and profile cleared", now); err != nil {
						return fmt.Errorf("kyc: append expired audit log: %w", err)
					}
				} else {
					return domain.ErrAlreadyVerified
				}
			} else if existing.Status == domain.KYCStatusPending {
				// Pending verification superseded by fresh attempt
				reason := "superseded by new attempt"
				_, err = tx.Exec(ctx, `
					UPDATE kyc_verification
					SET status = 'expired', failure_reason = $2, updated_at = $3
					WHERE id = $1
				`, existing.ID, reason, now)
				if err != nil {
					return fmt.Errorf("kyc: supersede pending verification: %w", err)
				}

				if err := r.appendAuditLogTx(ctx, tx, tenantID, actor, "verification_superseded", "pending verification superseded by new attempt", now); err != nil {
					return fmt.Errorf("kyc: append superseded audit log: %w", err)
				}
			}
		}

		// 3. Insert new pending verification record
		newVerification, err = scanKYCVerification(tx.QueryRow(ctx, `
			INSERT INTO kyc_verification (
				tenant_id, consent_id, vendor_name, vendor_reference_id,
				method, status, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, 'pending', $6, $6)
			RETURNING `+kycVerificationCols+`
		`, tenantID, consentID, vendorName, vendorRefID, method, now))
		if err != nil {
			return fmt.Errorf("kyc: insert pending verification: %w", err)
		}

		// 4. Append verification_initiated audit record
		detail := fmt.Sprintf("method=%s vendor_ref=%s", method, vendorRefID)
		if err := r.appendAuditLogTx(ctx, tx, tenantID, actor, "verification_initiated", detail, now); err != nil {
			return fmt.Errorf("kyc: append initiated audit log: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}
	return newVerification, nil
}

// CompleteVerificationTx marks a pending verification as verified, populates PII hashes,
// updates tenant profile with masked UID, and logs completion in the audit ledger.
func (r *KYCRepo) CompleteVerificationTx(
	ctx context.Context,
	verificationID uuid.UUID,
	maskedUID, identityHash string,
	isDedupable bool,
	hashKeyVersion int16,
	verifiedAt, expiresAt time.Time,
	actor string,
) (*domain.KYCVerification, error) {
	now := time.Now().UTC()
	var updated *domain.KYCVerification

	err := WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		// 1. Lookup tenant_id first to acquire locks in consistent hierarchical order
		var tenantID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT tenant_id FROM kyc_verification WHERE id = $1`, verificationID).Scan(&tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrVerificationNotFound
			}
			return fmt.Errorf("kyc: lookup verification tenant: %w", err)
		}

		// 2. Acquire row lock on tenant FIRST (enforces global lock hierarchy: tenants -> kyc_verification)
		var exists int
		err = tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("kyc: lock tenant: %w", err)
		}

		// 3. Now acquire row lock on kyc_verification
		existing, err := scanKYCVerification(tx.QueryRow(ctx, `
			SELECT `+kycVerificationCols+`
			FROM kyc_verification
			WHERE id = $1
			FOR UPDATE
		`, verificationID))
		if err != nil {
			return fmt.Errorf("kyc: query verification to complete: %w", err)
		}

		if existing.Status != domain.KYCStatusPending {
			return fmt.Errorf("%w: verification %s has status %s", domain.ErrVerificationNotPending, verificationID, existing.Status)
		}

		// Check for identity hash collision (flag-don't-block dedup check)
		var collidingTenantIDs []uuid.UUID
		if isDedupable && identityHash != "" {
			rows, err := tx.Query(ctx, `
				SELECT tenant_id FROM kyc_verification
				WHERE identity_hash = $1
				  AND hash_key_version = $2
				  AND status = 'verified'
				  AND is_dedupable = TRUE
				  AND tenant_id != $3
			`, identityHash, hashKeyVersion, existing.TenantID)
			if err != nil {
				return fmt.Errorf("kyc: check duplicate collision: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var tid uuid.UUID
				if err := rows.Scan(&tid); err != nil {
					return fmt.Errorf("kyc: scan colliding tenant: %w", err)
				}
				collidingTenantIDs = append(collidingTenantIDs, tid)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("kyc: iterate colliding tenants: %w", err)
			}
		}
		duplicateDetected := len(collidingTenantIDs) > 0

		updated, err = scanKYCVerification(tx.QueryRow(ctx, `
			UPDATE kyc_verification
			SET status = 'verified',
				masked_uid = $2,
				identity_hash = $3,
				is_dedupable = $4,
				duplicate_detected = $5,
				hash_key_version = $6,
				verified_at = $7,
				expires_at = $8,
				updated_at = $9
			WHERE id = $1 AND status = 'pending'
			RETURNING `+kycVerificationCols+`
		`, verificationID, maskedUID, identityHash, isDedupable, duplicateDetected, hashKeyVersion, verifiedAt, expiresAt, now))
		if err != nil {
			return fmt.Errorf("kyc: update verification to verified: %w", err)
		}

		// Update tenant profile with masked UID
		_, err = tx.Exec(ctx, `
			UPDATE tenants
			SET aadhaar_last4 = $2, updated_at = $3
			WHERE id = $1
		`, existing.TenantID, maskedUID, now)
		if err != nil {
			return fmt.Errorf("kyc: update tenant aadhaar_last4: %w", err)
		}

		// Symmetric collision flagging: flag both incoming and existing colliding records
		if duplicateDetected {
			// 1. Audit log for the incoming tenant
			if err := r.appendAuditLogTx(ctx, tx, existing.TenantID, actor, "duplicate_detected", fmt.Sprintf("collided_identity_hash=%s", identityHash), now); err != nil {
				return fmt.Errorf("kyc: append duplicate_detected audit log: %w", err)
			}

			// 2. Symmetric update and audit log on all colliding existing tenants
			for _, tid := range collidingTenantIDs {
				_, err = tx.Exec(ctx, `
					UPDATE kyc_verification
					SET duplicate_detected = TRUE, updated_at = $2
					WHERE tenant_id = $1 AND status = 'verified'
				`, tid, now)
				if err != nil {
					return fmt.Errorf("kyc: backfill duplicate_detected on colliding tenant %s: %w", tid, err)
				}

				if err := r.appendAuditLogTx(ctx, tx, tid, actor, "duplicate_detected", fmt.Sprintf("collided_with_incoming_tenant=%s identity_hash=%s", existing.TenantID, identityHash), now); err != nil {
					return fmt.Errorf("kyc: append duplicate_detected audit log for colliding tenant %s: %w", tid, err)
				}
			}
		}

		// Append completion audit log
		detail := fmt.Sprintf("masked_uid=%s dedupable=%t duplicate_detected=%t key_ver=%d", maskedUID, isDedupable, duplicateDetected, hashKeyVersion)
		if err := r.appendAuditLogTx(ctx, tx, existing.TenantID, actor, "verification_completed", detail, now); err != nil {
			return fmt.Errorf("kyc: append completed audit log: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}
	return updated, nil
}

// FailVerificationTx records a verification failure and logs it in the audit ledger.
func (r *KYCRepo) FailVerificationTx(ctx context.Context, verificationID uuid.UUID, reason, actor string) error {
	now := time.Now().UTC()

	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		var tenantID uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE kyc_verification
			SET status = 'failed', failure_reason = $2, updated_at = $3
			WHERE id = $1 AND status = 'pending'
			RETURNING tenant_id
		`, verificationID, reason, now).Scan(&tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("kyc: pending verification %s not found", verificationID)
			}
			return fmt.Errorf("kyc: update verification failure: %w", err)
		}

		if err := r.appendAuditLogTx(ctx, tx, tenantID, actor, "verification_failed", reason, now); err != nil {
			return fmt.Errorf("kyc: append failed audit log: %w", err)
		}

		return nil
	})
}

// GetActiveVerificationByTenant retrieves any currently pending or verified verification for a tenant.
func (r *KYCRepo) GetActiveVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error) {
	return scanKYCVerification(r.pool.QueryRow(ctx, `
		SELECT `+kycVerificationCols+`
		FROM kyc_verification
		WHERE tenant_id = $1 AND status IN ('pending', 'verified')
	`, tenantID))
}

// GetVerificationByVendorRefID retrieves a verification record by its unique vendor reference ID.
func (r *KYCRepo) GetVerificationByVendorRefID(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error) {
	return scanKYCVerification(r.pool.QueryRow(ctx, `
		SELECT `+kycVerificationCols+`
		FROM kyc_verification
		WHERE vendor_reference_id = $1
	`, vendorRefID))
}

// CheckDuplicateIdentity checks if an active, verified identity hash matches another tenant.
func (r *KYCRepo) CheckDuplicateIdentity(ctx context.Context, identityHash string, hashKeyVersion int16, excludeTenantID uuid.UUID) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM kyc_verification
			WHERE identity_hash = $1
			  AND hash_key_version = $2
			  AND status = 'verified'
			  AND is_dedupable = TRUE
			  AND tenant_id != $3
		)
	`, identityHash, hashKeyVersion, excludeTenantID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("kyc: check duplicate identity: %w", err)
	}
	return exists, nil
}

// ListAuditLogsByTenant returns the cryptographic audit trail for a tenant in sequential order.
func (r *KYCRepo) ListAuditLogsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.KYCAuditLog, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+kycAuditCols+`
		FROM kyc_audit_log
		WHERE tenant_id = $1
		ORDER BY id ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("kyc: query audit logs: %w", err)
	}
	defer rows.Close()

	var logs []domain.KYCAuditLog
	for rows.Next() {
		var l domain.KYCAuditLog
		if err := rows.Scan(&l.ID, &l.TenantID, &l.Actor, &l.Action, &l.DetailHash, &l.PrevHash, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("kyc: scan audit log: %w", err)
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// ExpireStalePendingVerifications batch expires pending verifications past their TTL cutoff.
func (r *KYCRepo) ExpireStalePendingVerifications(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE kyc_verification
		SET status = 'expired',
			failure_reason = 'expired by TTL reaper',
			updated_at = NOW()
		WHERE status = 'pending' AND created_at <= $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("kyc: expire stale verifications: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ExpireLapsedVerifications transitions verified records that have passed their expires_at date
// to 'expired' and atomically nulls out tenants.aadhaar_last4 to prevent stale PII display.
// Strictly enforces the global lock hierarchy (tenants -> kyc_verification) per tenant.
func (r *KYCRepo) ExpireLapsedVerifications(ctx context.Context, now time.Time) (int64, error) {
	// 1. Snapshot query without holding row locks
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id
		FROM kyc_verification
		WHERE status = 'verified' AND expires_at <= $1
		ORDER BY tenant_id
	`, now)
	if err != nil {
		return 0, fmt.Errorf("query lapsed verifications: %w", err)
	}
	defer rows.Close()

	type lapsedItem struct {
		id       uuid.UUID
		tenantID uuid.UUID
	}
	var items []lapsedItem
	for rows.Next() {
		var item lapsedItem
		if err := rows.Scan(&item.id, &item.tenantID); err != nil {
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var count int64
	for _, item := range items {
		err := WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
			// Step 1: Enforce lock hierarchy - lock tenant first
			var exists int
			err := tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, item.tenantID).Scan(&exists)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil // tenant deleted concurrently, skip
				}
				return fmt.Errorf("lock tenant %s: %w", item.tenantID, err)
			}

			// Step 2: Lock and update kyc_verification
			tag, err := tx.Exec(ctx, `
				UPDATE kyc_verification
				SET status = 'expired',
					failure_reason = 'validity period lapsed',
					updated_at = $2
				WHERE id = $1 AND status = 'verified'
			`, item.id, now)
			if err != nil {
				return fmt.Errorf("expire verification %s: %w", item.id, err)
			}
			if tag.RowsAffected() == 0 {
				return nil // already updated or superseded concurrently
			}

			// Step 3: Clear tenant aadhaar_last4 (tenant lock held)
			_, err = tx.Exec(ctx, `
				UPDATE tenants
				SET aadhaar_last4 = NULL, updated_at = $2
				WHERE id = $1
			`, item.tenantID, now)
			if err != nil {
				return fmt.Errorf("clear tenant %s aadhaar_last4: %w", item.tenantID, err)
			}

			// Step 4: Append audit log
			if err := r.appendAuditLogTx(ctx, tx, item.tenantID, "system_reaper", "verification_expired", "lapsed validity period", now); err != nil {
				return fmt.Errorf("append expired audit log: %w", err)
			}

			count++
			return nil
		})
		if err != nil {
			return count, fmt.Errorf("kyc: expire lapsed verification for tenant %s: %w", item.tenantID, err)
		}
	}
	return count, nil
}

// appendAuditLogTx queries the most recent audit entry for the tenant strictly ordered by id DESC
// (guaranteeing monotonic ordering within the same transaction regardless of clock timestamp resolution),
// calculates the SHA-256 chained hash, and inserts the audit record.
// Any error returned MUST cause the caller to abort the transaction.
func (r *KYCRepo) appendAuditLogTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor, action, detail string, now time.Time) error {
	var prevHash *string
	// STRICT ORDER BY id DESC: Multiple audit records in the same transaction share the exact same timestamp.
	// BIGSERIAL id is the only monotonically increasing sequence guaranteed within a transaction.
	err := tx.QueryRow(ctx, `
		SELECT detail_hash
		FROM kyc_audit_log
		WHERE tenant_id = $1
		ORDER BY id DESC
		LIMIT 1
	`, tenantID).Scan(&prevHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("fetch prev audit hash: %w", err)
	}

	prevHashVal := ""
	if prevHash != nil {
		prevHashVal = *prevHash
	}

	entryHash := domain.ComputeAuditHash(prevHashVal, tenantID, actor, action, detail, now)

	var pHash *string
	if prevHashVal != "" {
		pHash = &prevHashVal
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO kyc_audit_log (tenant_id, actor, action, detail_hash, prev_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, tenantID, actor, action, entryHash, pHash, now)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}

	return nil
}

// GetLatestVerificationByTenant returns the most recent kyc_verification for a
// tenant regardless of status — used by the owner view which needs to surface
// even failed/revoked/expired records.
func (r *KYCRepo) GetLatestVerificationByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, error) {
	return scanKYCVerification(r.pool.QueryRow(ctx, `
		SELECT `+kycVerificationCols+`
		FROM kyc_verification
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, tenantID))
}

// ClearDuplicateFlag clears the duplicate_detected flag on a tenant's currently
// verified verification record and appends an audited reason to the hash chain.
// This is the only legitimate way for an owner to dismiss a false positive.
func (r *KYCRepo) ClearDuplicateFlag(ctx context.Context, verificationID uuid.UUID, actor, reason string) error {
	now := time.Now().UTC()

	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		var tenantID uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE kyc_verification
			SET duplicate_detected = FALSE, updated_at = $2
			WHERE id = $1 AND duplicate_detected = TRUE
			RETURNING tenant_id
		`, verificationID, now).Scan(&tenantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("kyc: no flagged verification found for id %s", verificationID)
			}
			return fmt.Errorf("kyc: clear duplicate flag: %w", err)
		}

		detail := fmt.Sprintf("duplicate_cleared reason=%s", reason)
		if err := r.appendAuditLogTx(ctx, tx, tenantID, actor, "duplicate_cleared", detail, now); err != nil {
			return fmt.Errorf("kyc: append duplicate_cleared audit log: %w", err)
		}

		return nil
	})
}
