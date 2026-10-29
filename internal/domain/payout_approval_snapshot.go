package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	CurrentSnapshotSchemaVersion = 1
	MinApprovalSigningKeyLength  = 32
)

var (
	ErrInvalidSigningKey       = errors.New("payout approval: signing key must be at least 32 bytes")
	ErrEmptySnapshotItems      = errors.New("payout approval: snapshot must contain at least one item")
	ErrSignatureMismatch       = errors.New("payout approval: signature mismatch — possible tampering detected")
	ErrMissingApprovalSnapshot = errors.New("payout approval: batch missing approval snapshot")
)

// ApprovedItemSnapshot captures an individual transfer instruction bound to its destination instrument.
type ApprovedItemSnapshot struct {
	ItemID            uuid.UUID `json:"item_id"`
	PayeeID           uuid.UUID `json:"payee_id"`
	ReferenceNumber   string    `json:"reference_number"`
	AmountPaise       int64     `json:"amount_paise"`
	Purpose           string    `json:"purpose"`
	PeriodLabel       string    `json:"period_label"`
	AccountNumberHash string    `json:"account_number_hash"` // Salted hash of bank account or VPA
	IFSC              string    `json:"ifsc,omitempty"`      // Normalized bank IFSC
	UPIVPA            string    `json:"upi_vpa,omitempty"`   // Normalized UPI VPA
}

// PayoutApprovalSnapshot is the immutable authorization record created atomically when an owner approves a batch.
type PayoutApprovalSnapshot struct {
	SchemaVersion int                    `json:"schema_version"`
	BatchID       uuid.UUID              `json:"batch_id"`
	PropertyID    uuid.UUID              `json:"property_id"`
	ApprovedBy    uuid.UUID              `json:"approved_by"`
	ApprovedAt    time.Time              `json:"approved_at"`
	TotalAmount   int64                  `json:"total_amount_paise"`
	ItemCount     int                    `json:"item_count"`
	Items         []ApprovedItemSnapshot `json:"items"`
}

// CanonicalPayload returns a deterministic, canonical string representation for HMAC-SHA256 signing.
// Items are sorted by ItemID to guarantee canonical reproducibility regardless of retrieval order.
func (s *PayoutApprovalSnapshot) CanonicalPayload() string {
	sortedItems := make([]ApprovedItemSnapshot, len(s.Items))
	copy(sortedItems, s.Items)
	sort.Slice(sortedItems, func(i, j int) bool {
		return sortedItems[i].ItemID.String() < sortedItems[j].ItemID.String()
	})

	var b strings.Builder
	b.WriteString(fmt.Sprintf("v=%d|batch=%s|prop=%s|by=%s|at=%d|total=%d|count=%d\n",
		s.SchemaVersion,
		s.BatchID.String(),
		s.PropertyID.String(),
		s.ApprovedBy.String(),
		s.ApprovedAt.Unix(),
		s.TotalAmount,
		s.ItemCount,
	))

	for _, it := range sortedItems {
		b.WriteString(fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s|%s|%s\n",
			it.ItemID.String(),
			it.PayeeID.String(),
			it.ReferenceNumber,
			it.AmountPaise,
			it.Purpose,
			it.PeriodLabel,
			it.AccountNumberHash,
			strings.ToUpper(strings.TrimSpace(it.IFSC)),
			strings.ToLower(strings.TrimSpace(it.UPIVPA)),
		))
	}
	return b.String()
}

// SignApprovalSnapshot computes HMAC-SHA256 over the canonical snapshot representation using a dedicated secret.
func SignApprovalSnapshot(secret []byte, snapshot *PayoutApprovalSnapshot) (string, error) {
	if len(secret) < MinApprovalSigningKeyLength {
		return "", ErrInvalidSigningKey
	}
	if snapshot == nil || len(snapshot.Items) == 0 {
		return "", ErrEmptySnapshotItems
	}
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(snapshot.CanonicalPayload()))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyApprovalSnapshot checks in constant time that the signature matches the snapshot's canonical payload.
func VerifyApprovalSnapshot(secret []byte, snapshot *PayoutApprovalSnapshot, signature string) error {
	if len(secret) < MinApprovalSigningKeyLength {
		return ErrInvalidSigningKey
	}
	if snapshot == nil || len(snapshot.Items) == 0 {
		return ErrEmptySnapshotItems
	}
	if strings.TrimSpace(signature) == "" {
		return ErrSignatureMismatch
	}
	expectedSig, err := SignApprovalSnapshot(secret, snapshot)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return ErrSignatureMismatch
	}
	return nil
}
