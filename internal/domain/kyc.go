package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

type KYCStatus string

const (
	KYCStatusPending  KYCStatus = "pending"
	KYCStatusVerified KYCStatus = "verified"
	KYCStatusFailed   KYCStatus = "failed"
	KYCStatusExpired  KYCStatus = "expired"
	KYCStatusRevoked  KYCStatus = "revoked"
)

type KYCMethod string

const (
	KYCMethodDigiLocker KYCMethod = "digilocker"
	KYCMethodOCR        KYCMethod = "ocr"
	KYCMethodQR         KYCMethod = "qr"
)

var (
	ErrAlreadyVerified        = errors.New("kyc: tenant is already verified")
	ErrVerificationNotFound   = errors.New("kyc: verification record not found")
	ErrConsentRevoked         = errors.New("kyc: consent revoked")
	ErrVerificationNotPending = errors.New("kyc: verification is not in pending status")
)

type KYCConsent struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenant_id"`
	Purpose         string     `json:"purpose"`
	ConsentVersion  string     `json:"consent_version"`
	ConsentText     string     `json:"consent_text"`
	ConsentTextHash string     `json:"consent_text_hash"`
	ConsentGivenAt  time.Time  `json:"consent_given_at"`
	IPAddress       *string    `json:"ip_address,omitempty"`
	UserAgent       *string    `json:"user_agent,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}

func (c *KYCConsent) IsActive() bool {
	return c.RevokedAt == nil
}

type KYCVerification struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	ConsentID         uuid.UUID  `json:"consent_id"`
	VendorName        string     `json:"vendor_name"`
	VendorReferenceID string     `json:"vendor_reference_id"`
	MaskedUID         *string    `json:"masked_uid,omitempty"`
	IdentityHash      *string    `json:"identity_hash,omitempty"`
	HashKeyVersion    int16      `json:"hash_key_version"`
	IsDedupable       bool       `json:"is_dedupable"`
	DuplicateDetected bool       `json:"duplicate_detected"`
	Method            KYCMethod  `json:"method"`
	Status            KYCStatus  `json:"status"`
	FailureReason     *string    `json:"failure_reason,omitempty"`
	VerifiedAt        *time.Time `json:"verified_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func (v *KYCVerification) IsValid(now time.Time) bool {
	if v.Status != KYCStatusVerified {
		return false
	}
	if v.ExpiresAt != nil && !now.Before(*v.ExpiresAt) {
		return false
	}
	return true
}

type KYCAuditLog struct {
	ID          int64     `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	Actor       string    `json:"actor"`
	Action      string    `json:"action"`
	DetailHash  *string   `json:"detail_hash,omitempty"`
	PrevHash    *string   `json:"prev_hash,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// NormalizeName applies Unicode NFC normalization, converts to lowercase,
// and collapses multiple whitespace characters into single spaces.
func NormalizeName(name string) string {
	normalized := norm.NFC.String(name)
	fields := strings.Fields(strings.ToLower(normalized))
	return strings.Join(fields, " ")
}

// NormalizeDOB parses common UIDAI date formats into canonical ISO YYYY-MM-DD.
// If only a year is available (YYYY), it normalizes to YYYY-01-01 and returns isDedupable=false.
func NormalizeDOB(dobStr string) (canonicalDOB string, isDedupable bool, err error) {
	trimmed := strings.TrimSpace(dobStr)
	if len(trimmed) == 4 {
		// Year-only format
		var y int
		if _, err := fmt.Sscanf(trimmed, "%d", &y); err == nil && y >= 1900 && y <= 2100 {
			return fmt.Sprintf("%04d-01-01", y), false, nil
		}
	}

	layouts := []string{"2006-01-02", "02-01-2006", "02/01/2006", "2006/01/02"}
	for _, l := range layouts {
		if t, err := time.Parse(l, trimmed); err == nil {
			return t.Format("2006-01-02"), true, nil
		}
	}

	return "", false, fmt.Errorf("kyc: unparseable DOB format %q", dobStr)
}

// ComputeIdentityHash generates an irreversible HMAC-SHA256 signature for deduplication.
func ComputeIdentityHash(secret string, name, dob, gender, maskedUID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	payload := fmt.Sprintf("%s|%s|%s|%s", NormalizeName(name), dob, strings.ToUpper(strings.TrimSpace(gender)), strings.TrimSpace(maskedUID))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// ComputeAuditHash computes the SHA-256 hash for an audit log entry.
func ComputeAuditHash(prevHash string, tenantID uuid.UUID, actor, action, detail string, createdAt time.Time) string {
	h := sha256.New()
	entry := fmt.Sprintf("%s|%s|%s|%s|%s|%s", prevHash, tenantID.String(), actor, action, detail, createdAt.UTC().Format(time.RFC3339Nano))
	h.Write([]byte(entry))
	return hex.EncodeToString(h.Sum(nil))
}
