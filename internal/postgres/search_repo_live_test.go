package postgres

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

// TestSearchDocuments_RBACIsolation verifies that:
// 1. Tenants cannot view inspection notes (which have tenant_id = NULL).
// 2. Managers cannot view payment notes.
// 3. Tenants cannot view another tenant's payment notes.
// 4. Managers cannot search for UTR or payment notes in search_documents.
//
// NOTE: search_documents is legacy infrastructure (ADR-012). This test is retained
// to ensure RBAC scoping cannot regress if the table is ever queried in future.
// The production search path (SearchLexical) does NOT use search_documents.
func TestSearchDocuments_RBACIsolation(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Ensure 011_search_semantic.sql migration is applied
	mBytes, err := os.ReadFile(filepath.Join("..", "..", "migrations", "011_search_semantic.sql"))
	if err != nil {
		t.Fatalf("read 011 migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mBytes)); err != nil {
		t.Fatalf("apply 011 migration: %v", err)
	}

	repo := NewSearchRepo(pool)

	// Create test property
	propID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
		VALUES ($1, 'Search RBAC Prop', '+919999988888', 'Prop Owner', 'owner@rbac.test', $2, 'owner@upi')`,
		propID, uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM search_documents WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	tenantA := uuid.New()
	tenantB := uuid.New()

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, rent_amount, due_day)
		VALUES ($1, $3, 'Tenant A', 500000, 5), ($2, $3, 'Tenant B', 500000, 5)`,
		tenantA, tenantB, propID,
	)
	if err != nil {
		t.Fatalf("insert tenants: %v", err)
	}

	// Seed search_documents directly with raw SQL (UpsertDocument removed per ADR-012):
	// 1. Inspection note (tenant_id = NULL)
	inspID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO search_documents (property_id, tenant_id, entity_type, entity_id, title, body, updated_at)
		VALUES ($1, NULL, 'inspection', $2, 'Inspection Move-in #999', 'Internal owner inspection notes secret 123', NOW())
		ON CONFLICT (property_id, entity_type, entity_id) DO UPDATE SET
			title = EXCLUDED.title, body = EXCLUDED.body, updated_at = NOW()`,
		propID, inspID,
	)
	if err != nil {
		t.Fatalf("upsert inspection doc: %v", err)
	}

	// 2. Payment note for Tenant A (tenant_id = tenantA)
	payID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO search_documents (property_id, tenant_id, entity_type, entity_id, title, body, updated_at)
		VALUES ($1, $2, 'payment_note', $3, 'Payment UTR987654321', 'Rent paid via UPI UTR987654321 private payment note', NOW())
		ON CONFLICT (property_id, entity_type, entity_id) DO UPDATE SET
			title = EXCLUDED.title, body = EXCLUDED.body, updated_at = NOW()`,
		propID, tenantA, payID,
	)
	if err != nil {
		t.Fatalf("upsert payment note doc: %v", err)
	}

	// Case 1: Tenant A searches for inspection notes.
	// Must NOT see the inspection document even though tenant_id is NULL.
	pTenantA := search.Params{
		Query:      "secret",
		Limit:      10,
		Mode:       search.ModeLexical,
		PropertyID: propID,
		TenantID:   &tenantA,
		Role:       string(domain.RoleTenant),
	}
	resTenantA, _, err := repo.SearchLexical(ctx, pTenantA, []search.EntityType{search.TypeDocument}, 10)
	if err != nil {
		t.Fatalf("tenant search failed: %v", err)
	}
	for _, r := range resTenantA {
		if r.ID == inspID.String() {
			t.Errorf("SECURITY LEAK [Case 1]: Tenant was able to read inspection note: %+v", r)
		}
	}

	// Case 2: Manager searches for payment notes / UTR.
	// Must NOT see payment_note documents.
	pManager := search.Params{
		Query:      "UTR987654321",
		Limit:      10,
		Mode:       search.ModeLexical,
		PropertyID: propID,
		TenantID:   nil,
		Role:       string(domain.RoleManager),
	}
	resManager, _, err := repo.SearchLexical(ctx, pManager, []search.EntityType{search.TypeDocument}, 10)
	if err != nil {
		t.Fatalf("manager search failed: %v", err)
	}
	for _, r := range resManager {
		if r.ID == payID.String() {
			t.Errorf("SECURITY LEAK [Case 2]: Manager was able to read payment note: %+v", r)
		}
	}

	// Case 3: Tenant B searches for Tenant A's payment note / UTR.
	// Must NOT see Tenant A's payment note.
	pTenantB := search.Params{
		Query:      "UTR987654321",
		Limit:      10,
		Mode:       search.ModeLexical,
		PropertyID: propID,
		TenantID:   &tenantB,
		Role:       string(domain.RoleTenant),
	}
	resTenantB, _, err := repo.SearchLexical(ctx, pTenantB, []search.EntityType{search.TypeDocument}, 10)
	if err != nil {
		t.Fatalf("tenant B search failed: %v", err)
	}
	for _, r := range resTenantB {
		if r.ID == payID.String() {
			t.Errorf("SECURITY LEAK [Case 3]: Tenant B was able to read Tenant A's payment note: %+v", r)
		}
	}

	// Case 4: Manager searching for due code in document search must get 0 results
	pManagerDue := search.Params{
		Query:      "DUE99001",
		Limit:      10,
		Mode:       search.ModeLexical,
		PropertyID: propID,
		TenantID:   nil,
		Role:       string(domain.RoleManager),
	}
	resManagerDue, _, err := repo.SearchLexical(ctx, pManagerDue, []search.EntityType{search.TypeDocument}, 10)
	if err != nil {
		t.Fatalf("manager due search failed: %v", err)
	}
	for _, r := range resManagerDue {
		if r.ID == payID.String() {
			t.Errorf("SECURITY LEAK [Case 4]: Manager was able to find payment note by due code: %+v", r)
		}
	}

	// Case 5: Owner CAN see both inspection notes and payment notes.
	pOwner := search.Params{
		Query:      "UTR987654321",
		Limit:      10,
		Mode:       search.ModeLexical,
		PropertyID: propID,
		TenantID:   nil,
		Role:       string(domain.RoleOwner),
	}
	resOwner, _, err := repo.SearchLexical(ctx, pOwner, []search.EntityType{search.TypeDocument}, 10)
	if err != nil {
		t.Fatalf("owner search failed: %v", err)
	}
	foundPay := false
	for _, r := range resOwner {
		if r.ID == payID.String() {
			foundPay = true
		}
	}
	if !foundPay {
		t.Errorf("Owner expected to see payment note but did not find it")
	}

	// Case 6: Fail-closed verification: RoleTenant with nil TenantID must return 0 results
	pTenantNil := search.Params{
		Query:      "secret",
		Limit:      10,
		Mode:       search.ModeLexical,
		PropertyID: propID,
		TenantID:   nil,
		Role:       string(domain.RoleTenant),
	}
	resTenantNil, _, err := repo.SearchLexical(ctx, pTenantNil, []search.EntityType{search.TypeDocument}, 10)
	if err != nil {
		t.Fatalf("tenant nil search failed: %v", err)
	}
	if len(resTenantNil) != 0 {
		t.Errorf("SECURITY LEAK [Case 6]: RoleTenant with nil TenantID returned documents: %+v", resTenantNil)
	}
}
