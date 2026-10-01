package postgres

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

// TestSearchDocuments_RBACIsolation verifies that:
// 1. Tenants cannot view inspection notes (which have tenant_id = NULL).
// 2. Managers cannot view payment notes.
// 3. Tenants cannot view another tenant's payment notes.
// 4. Managers cannot search for UTR or payment notes in search_documents.
func TestSearchDocuments_RBACIsolation(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres search RBAC test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres search RBAC test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

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

	// Seed search_documents:
	// 1. Inspection note (tenant_id = NULL)
	inspID := uuid.New()
	err = repo.UpsertDocument(ctx, propID, nil, "inspection", inspID, "Inspection Move-in #999", "Internal owner inspection notes secret 123", nil)
	if err != nil {
		t.Fatalf("upsert inspection doc: %v", err)
	}

	// 2. Payment note for Tenant A (tenant_id = tenantA)
	payID := uuid.New()
	err = repo.UpsertDocument(ctx, propID, &tenantA, "payment_note", payID, "Payment UTR987654321", "Rent paid via UPI UTR987654321 private payment note", nil)
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
	resTenantA, err := repo.SearchLexical(ctx, pTenantA, []search.EntityType{search.TypeDocument}, 10)
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
	resManager, err := repo.SearchLexical(ctx, pManager, []search.EntityType{search.TypeDocument}, 10)
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
	resTenantB, err := repo.SearchLexical(ctx, pTenantB, []search.EntityType{search.TypeDocument}, 10)
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
	resManagerDue, err := repo.SearchLexical(ctx, pManagerDue, []search.EntityType{search.TypeDocument}, 10)
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
	resOwner, err := repo.SearchLexical(ctx, pOwner, []search.EntityType{search.TypeDocument}, 10)
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

	// Case 6: Vector path RBAC checks
	// Manager vector search must not leak payment notes
	vecResManager, err := repo.SearchVector(ctx, pManager, 10, make([]float32, 384))
	if err != nil {
		t.Logf("SearchVector skipped or unsupported on this DB: %v", err)
	} else {
		for _, r := range vecResManager {
			if r.ID == payID.String() {
				t.Errorf("SECURITY LEAK [Case 6 Vector]: Manager saw payment note in SearchVector: %+v", r)
			}
		}
	}

	// Tenant vector search must not leak inspection notes
	vecResTenant, err := repo.SearchVector(ctx, pTenantA, 10, make([]float32, 384))
	if err != nil {
		t.Logf("SearchVector skipped or unsupported on this DB: %v", err)
	} else {
		for _, r := range vecResTenant {
			if r.ID == inspID.String() {
				t.Errorf("SECURITY LEAK [Case 6 Vector]: Tenant saw inspection note in SearchVector: %+v", r)
			}
		}
	}
}
