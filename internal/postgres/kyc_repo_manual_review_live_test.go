package postgres

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLiveKYCRepo_ManualReviewAndAadhaarOTP(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}

	propID := uuid.New()
	tenantID := uuid.New()
	reviewerID := uuid.New()

	inviteCode := fmt.Sprintf("M%s", uuid.New().String()[:7])
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
		VALUES ($1, 'Live Review Test Property', '123 Test St', '+919999900001', 'test@upi', 'Test Owner', 'owner@test.com', $2, 'manual_proof')
		ON CONFLICT (id) DO NOTHING
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("seed property: %v", err)
	}

	reviewerPhone := fmt.Sprintf("+9198%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role)
		VALUES ($1, $2, 'owner')
		ON CONFLICT (id) DO NOTHING
	`, reviewerID, reviewerPhone)
	if err != nil {
		t.Fatalf("seed reviewer user: %v", err)
	}

	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_verification WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_consent WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, reviewerID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	testPhone := fmt.Sprintf("+9199%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, status, rent_amount, due_day)
		VALUES ($1, $2, 'Manual Review Tenant', $3, 'active', 10000, 5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, propID, testPhone)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	repo := NewKYCRepo(pool)

	ip := "127.0.0.1"
	ua := "Go-Test"

	// 1. Consent
	consent := &domain.KYCConsent{
		ID:             uuid.New(),
		TenantID:       tenantID,
		Purpose:        "manual_review_test",
		ConsentVersion: "v1.0",
		ConsentText:    "Test DPDP consent",
		IPAddress:      &ip,
		UserAgent:      &ua,
		ConsentGivenAt: time.Now().UTC(),
	}
	if err := repo.RecordConsent(ctx, consent, "test"); err != nil {
		t.Fatalf("record consent: %v", err)
	}

	// 2. Initiate OCR/manual verification
	initVerif, err := repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodOCR, "cashfree_doc", "cf_doc_123", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("initiate verification: %v", err)
	}

	// 3. Submit for manual review
	verif, err := repo.SubmitManualReviewTx(ctx, initVerif.ID, "1234", "hash123", `{"name":"Manual Tenant"}`, "http://docs.example/doc1.pdf", []byte("%PDF-1.4 mock"), "application/pdf", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("submit manual review: %v", err)
	}
	if verif.Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("expected status 'pending_manual_review', got '%s'", verif.Status)
	}

	// 4. List pending reviews for property
	pendingList, err := repo.ListPendingReviews(ctx, propID)
	if err != nil {
		t.Fatalf("list pending reviews: %v", err)
	}
	found := false
	for _, p := range pendingList {
		if p.ID == verif.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected verif %s in pending review list for property %s", verif.ID, propID)
	}

	// 5. Reject manual review
	rejected, err := repo.RejectManualReviewTx(ctx, verif.ID, propID, reviewerID, "Blurry document", "owner:"+reviewerID.String())
	if err != nil {
		t.Fatalf("reject manual review: %v", err)
	}
	if rejected.Status != domain.KYCStatusRejected {
		t.Fatalf("expected status 'rejected', got '%s'", rejected.Status)
	}
	if rejected.RejectionReason == nil || *rejected.RejectionReason != "Blurry document" {
		t.Fatalf("expected rejection reason 'Blurry document', got %v", rejected.RejectionReason)
	}
	if rejected.OCRData != nil && *rejected.OCRData != "" {
		t.Fatalf("expected ocr_data to be wiped on rejection, got %v", *rejected.OCRData)
	}
	if rejected.DocumentURL != nil && *rejected.DocumentURL != "" {
		t.Fatalf("expected document_url to be wiped on rejection, got %v", *rejected.DocumentURL)
	}

	// KYC-03: verify document bytes are wiped on rejection
	var docBytes []byte
	err = repo.pool.QueryRow(ctx, "SELECT document_bytes FROM kyc_verification WHERE id = $1", verif.ID).Scan(&docBytes)
	if err != nil {
		t.Fatalf("could not query document_bytes: %v", err)
	}
	if docBytes != nil {
		t.Errorf("KYC-03: document_bytes should be NULL after rejection, got %d bytes", len(docBytes))
	}

	// 6. Submit second verification and approve it
	initVerif2, err := repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodOCR, "cashfree_doc", "cf_doc_456", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("initiate second verification: %v", err)
	}
	verif2, err := repo.SubmitManualReviewTx(ctx, initVerif2.ID, "1234", "hash456", `{"name":"Manual Tenant"}`, "http://docs.example/doc2.pdf", []byte("%PDF-1.4 mock 2"), "application/pdf", "tenant:"+tenantID.String())
	if err != nil {
		t.Fatalf("submit second manual review: %v", err)
	}
	approved, err := repo.ApproveManualReviewTx(ctx, verif2.ID, propID, reviewerID, "owner:"+reviewerID.String())
	if err != nil {
		t.Fatalf("approve manual review: %v", err)
	}
	if approved.Status != domain.KYCStatusVerified {
		t.Fatalf("expected status 'verified', got '%s'", approved.Status)
	}
	if approved.ReviewedBy == nil || *approved.ReviewedBy != reviewerID {
		t.Fatalf("expected reviewed_by %s, got %v", reviewerID, approved.ReviewedBy)
	}
	if approved.OCRData != nil && *approved.OCRData != "" {
		t.Fatalf("expected ocr_data to be wiped on approval, got %v", *approved.OCRData)
	}
	if approved.DocumentURL != nil && *approved.DocumentURL != "" {
		t.Fatalf("expected document_url to be wiped on approval, got %v", *approved.DocumentURL)
	}

	// KYC-04: verify document bytes and content type are wiped on approval
	var docBytes2 []byte
	var docContentType2 *string
	err = repo.pool.QueryRow(ctx, "SELECT document_bytes, document_content_type FROM kyc_verification WHERE id = $1", verif2.ID).Scan(&docBytes2, &docContentType2)
	if err != nil {
		t.Fatalf("could not query document_bytes after approval: %v", err)
	}
	if docBytes2 != nil {
		t.Errorf("KYC-04: document_bytes should be NULL after approval, got %d bytes", len(docBytes2))
	}
	if docContentType2 != nil {
		t.Errorf("KYC-04: document_content_type should be NULL after approval, got %v", *docContentType2)
	}
}
