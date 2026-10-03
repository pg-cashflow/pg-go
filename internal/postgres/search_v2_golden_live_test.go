package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

func TestLivePostgresSearchV2_GoldenRelevanceSuite(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatalf("REQUIRE_DB=1 but DATABASE_URL not set")
		}
		t.Skip("DATABASE_URL not set, skipping live Postgres search test")
	}

	cfg, err := config.Load()
	if err != nil {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatalf("REQUIRE_DB=1 but config load failed: %v", err)
		}
		t.Skip("config load failed, skipping live Postgres search test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// Ensure pg_trgm and migration 038 indexes are applied
	var extSchema string
	err = pool.QueryRow(ctx, `SELECT extnamespace::regnamespace::text FROM pg_extension WHERE extname = 'pg_trgm'`).Scan(&extSchema)
	t.Logf("pg_trgm extSchema=%q, err=%v", extSchema, err)

	_, err = pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm;`)
	t.Logf("CREATE EXTENSION pg_trgm err=%v", err)

	m038Path := filepath.Join("..", "..", "migrations", "038_search_v2_indexes.sql")
	m038SQL, err := os.ReadFile(m038Path)
	if err != nil {
		t.Fatalf("read 038 sql failed: %v", err)
	}
	_, err = pool.Exec(ctx, string(m038SQL))
	if err != nil {
		t.Fatalf("apply 038 sql failed: %v", err)
	}

	repo := NewSearchRepo(pool)
	searchSvc := &search.Service{Repo: repo}

	// 1. Setup primary test property and cross property
	propID := uuid.New()
	crossPropID := uuid.New()

	cleanup := func() {
		exec := func(sql string, args ...any) {
			_, _ = pool.Exec(ctx, sql, args...)
		}
		exec(`DELETE FROM gateway_refunds WHERE payment_id IN (SELECT id FROM payments WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id IN ($1, $2)))`, propID, crossPropID)
		exec(`DELETE FROM payment_allocations WHERE payment_id IN (SELECT id FROM payments WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id IN ($1, $2)))`, propID, crossPropID)
		exec(`DELETE FROM payment_tokens WHERE due_id IN (SELECT id FROM dues WHERE property_id IN ($1, $2))`, propID, crossPropID)
		exec(`DELETE FROM payment_intents WHERE due_id IN (SELECT id FROM dues WHERE property_id IN ($1, $2))`, propID, crossPropID)
		exec(`DELETE FROM push_subscriptions WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id IN ($1, $2))`, propID, crossPropID)
		exec(`DELETE FROM payment_allocations WHERE payment_id IN (SELECT id FROM payments WHERE upi_txn_id LIKE 'UTR%') OR due_id IN (SELECT id FROM dues WHERE due_code LIKE 'DUE-%')`)
		exec(`DELETE FROM payment_tokens WHERE due_id IN (SELECT id FROM dues WHERE due_code LIKE 'DUE-%')`)
		exec(`DELETE FROM payment_reports WHERE property_id IN ($1, $2) OR due_id IN (SELECT id FROM dues WHERE due_code LIKE 'DUE-%') OR upi_txn_id LIKE 'UTR%'`, propID, crossPropID)
		exec(`DELETE FROM payments WHERE upi_txn_id LIKE 'UTR%' OR tenant_id IN (SELECT id FROM tenants WHERE property_id IN ($1, $2))`, propID, crossPropID)
		exec(`DELETE FROM dues WHERE due_code LIKE 'DUE-%' OR property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM join_requests WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM inspections WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM hazards WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM violations WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM events WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM users WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM tenants WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM properties WHERE id IN ($1, $2)`, propID, crossPropID)
	}
	cleanup()
	defer cleanup()

	for _, pid := range []uuid.UUID{propID, crossPropID} {
		_, err = pool.Exec(ctx, `
			INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
			VALUES ($1, 'Golden Test Property', '+919876543210', 'Test Owner', 'owner@golden.test', $2, 'prop@upi')`,
			pid, uuid.New().String()[:8],
		)
		if err != nil {
			t.Fatalf("insert property failed: %v", err)
		}
	}

	ownerUserID := uuid.New()
	ownerPhone := fmt.Sprintf("+9188%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)`,
		ownerUserID, ownerPhone, propID,
	)
	if err != nil {
		t.Fatalf("insert owner user failed: %v", err)
	}

	// 2. Seed primary property tenants
	// Three same-name tenants to verify distinct rooms in subtitles:
	rahul201ID := uuid.New()
	rahul202ID := uuid.New()
	rahul303ID := uuid.New()
	rahulSharmaID := uuid.New()
	sureshID := uuid.New()
	crossRahulID := uuid.New()

	nano := time.Now().UnixNano()
	phonePrefix := fmt.Sprintf("+917%05d", (nano/1000)%100000)
	rahul201Phone := phonePrefix + "2010"
	rahul202Phone := phonePrefix + "2020"
	rahul303Phone := phonePrefix + "3030"
	rahulSharmaPhone := phonePrefix + "2040"
	sureshPhone := phonePrefix + "1050"
	crossRahulPhone := phonePrefix + "9990"

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
		VALUES
			($1, $4, 'Rahul', '201', $7, 'active', 500000, 5),
			($2, $4, 'Rahul', '202', $8, 'active', 500000, 5),
			($3, $4, 'Rahul', '303', $9, 'vacated', 500000, 5),
			($13, $4, 'Rahul Sharma', '204', $14, 'active', 500000, 5),
			($5, $4, 'Suresh Kumar', '105', $10, 'active', 500000, 5),
			($6, $11, 'Cross Rahul',  '999', $12, 'active', 500000, 5)`,
		rahul201ID, rahul202ID, rahul303ID, propID,
		sureshID, crossRahulID, rahul201Phone, rahul202Phone, rahul303Phone, sureshPhone, crossPropID, crossRahulPhone,
		rahulSharmaID, rahulSharmaPhone,
	)
	if err != nil {
		t.Fatalf("seed tenants failed: %v", err)
	}

	// 12 extra tenants named "Rahul" to test starvation prevention
	for i := 1; i <= 12; i++ {
		extraPhone := fmt.Sprintf("+916%04d%04d", (nano/1000)%10000, i)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
			VALUES ($1, $2, $3, $4, $5, 'active', 500000, 5)`,
			uuid.New(), propID, fmt.Sprintf("Rahul Starvation %d", i), fmt.Sprintf("50%d", i), extraPhone,
		)
		if err != nil {
			t.Fatalf("seed extra tenants failed: %v", err)
		}
	}

	// Dues:
	dueRahulID := uuid.New()
	dueSureshID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, amount, original_amount, period_start, period_end, status, due_date)
		VALUES
			($1, $3, $4, 'DUE-JAN1', 'rent', 500000, 500000, CURRENT_DATE, CURRENT_DATE + INTERVAL '30 days', 'pending', CURRENT_DATE + INTERVAL '5 days'),
			($2, $3, $5, 'DUE-SUR1', 'rent', 500000, 500000, CURRENT_DATE, CURRENT_DATE + INTERVAL '30 days', 'pending', CURRENT_DATE + INTERVAL '5 days')`,
		dueRahulID, dueSureshID, propID, rahul201ID, sureshID,
	)
	if err != nil {
		t.Fatalf("seed dues failed: %v", err)
	}

	// 12 extra dues with "Rahul" to test starvation
	for i := 1; i <= 12; i++ {
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, property_id, tenant_id, due_code, kind, amount, original_amount, period_start, period_end, status, due_date)
			VALUES ($1, $2, $3, $4, 'rent', 500000, 500000, CURRENT_DATE + ($5 * INTERVAL '30 days'), CURRENT_DATE + (($5 + 1) * INTERVAL '30 days'), 'pending', CURRENT_DATE + ($5 * INTERVAL '30 days') + INTERVAL '5 days')`,
			uuid.New(), propID, rahul201ID, fmt.Sprintf("DUE-%04d", i), i,
		)
		if err != nil {
			t.Fatalf("seed extra dues failed: %v", err)
		}
	}

	// Payments:
	payRahulID := uuid.New()
	paySureshID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, due_id, tenant_id, provider, amount, matched_by, upi_txn_id, raw_note, created_at)
		VALUES
			($1, $5, $3, 'bank', 500000, 'manual', 'UTR99887766', 'Rahul rent Jan payment', NOW()),
			($2, $6, $4, 'bank', 500000, 'manual', 'UTR11223344', 'Suresh rent payment', NOW())`,
		payRahulID, paySureshID, rahul201ID, sureshID, dueRahulID, dueSureshID,
	)
	if err != nil {
		t.Fatalf("seed payments failed: %v", err)
	}

	// Payment report:
	_, err = pool.Exec(ctx, `
		INSERT INTO payment_reports (id, due_id, property_id, tenant_id, upi_txn_id, amount, note, status, reported_by, created_at)
		VALUES ($1, $4, $2, $3, 'UTR77665544', 500000, 'Paid by brother for Rahul', 'pending_review', $5, NOW())`,
		uuid.New(), propID, rahul201ID, dueRahulID, ownerUserID,
	)
	if err != nil {
		t.Fatalf("seed payment reports failed: %v", err)
	}

	// Join Request:
	_, err = pool.Exec(ctx, `
		INSERT INTO join_requests (id, property_id, user_id, phone, name, status, created_at)
		VALUES ($1, $2, $3, '+919876512345', 'Amit Kumar', 'pending', NOW())`,
		uuid.New(), propID, ownerUserID,
	)
	if err != nil {
		t.Fatalf("seed join requests failed: %v", err)
	}

	// Inspection:
	_, err = pool.Exec(ctx, `
		INSERT INTO inspections (id, property_id, inspector_user_id, inspection_type, score_percent, passed, notes, created_at)
		VALUES ($1, $2, $3, 'Move-in', 90, true, 'Clean room electricity verified', NOW())`,
		uuid.New(), propID, ownerUserID,
	)
	if err != nil {
		t.Fatalf("seed inspection failed: %v", err)
	}

	// Hazard (reported by Rahul):
	_, err = pool.Exec(ctx, `
		INSERT INTO hazards (id, property_id, category, description, status, reported_by_tenant_id, created_at)
		VALUES ($1, $2, 'electrical_wire', 'Sparking wire in corridor near room', 'open', $3, NOW())`,
		uuid.New(), propID, rahul201ID,
	)
	if err != nil {
		t.Fatalf("seed hazard failed: %v", err)
	}

	// Violation:
	_, err = pool.Exec(ctx, `
		INSERT INTO violations (id, property_id, tenant_id, rule_code, severity, step, description, created_by, created_at)
		VALUES ($1, $2, $3, 'NOISE-01', 'lifestyle', 1, 'Loud music after 11 PM reported', $4, NOW())`,
		uuid.New(), propID, rahul201ID, ownerUserID,
	)
	if err != nil {
		t.Fatalf("seed violation failed: %v", err)
	}

	// ---------------------------------------------------------
	// SECTION 1: OWNER CASES (~12 cases)
	// ---------------------------------------------------------
	t.Run("Owner: exact name match", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahul Sharma", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected results for exact name, err=%v, count=%d", err, len(results))
		}
		if results[0].Title != "Rahul Sharma" {
			t.Errorf("expected top result Rahul Sharma, got %s", results[0].Title)
		}
	})

	t.Run("Owner: prefix name match", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahu", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected prefix results, got %d", len(results))
		}
		if !strings.HasPrefix(results[0].Title, "Rahul") {
			t.Errorf("expected prefix match Rahul, got %s", results[0].Title)
		}
	})

	t.Run("Owner: multi-word AND match 'rahul 201'", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "rahul 201", 20, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		found := false
		for _, r := range results {
			if r.Type == search.TypeTenant {
				if r.ID != rahul201ID.String() {
					t.Fatalf("expected ONLY Rahul in room 201, but got tenant %s (id=%s)", r.Title, r.ID)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("expected Rahul in 201 to match")
		}
	})

	t.Run("Owner: room only '201'", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "201", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("search room 201 failed, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("expected tenant in room 201, got %s", results[0].Title)
		}
	})

	t.Run("Owner: partial phone allowed", func(t *testing.T) {
		partPhone := rahul201Phone[len(rahul201Phone)-5:]
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, partPhone, 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected partial phone match for owner, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("expected rahul201 on phone match, got %s", results[0].Title)
		}
	})

	t.Run("Owner: full phone match", func(t *testing.T) {
		fullPhone := rahul201Phone[3:]
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, fullPhone, 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected full phone match for owner, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("expected rahul201 on full phone, got %s", results[0].Title)
		}
	})

	t.Run("Owner: due code exact", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "DUE-JAN1", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected due code match, got %d hits", len(results))
		}
		if results[0].Type != search.TypeDue || results[0].ID != dueRahulID.String() {
			t.Errorf("expected due match, got %+v", results[0])
		}
	})

	t.Run("Owner: UTR exact", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "UTR99887766", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected UTR match, got %d hits", len(results))
		}
		if results[0].Type != search.TypePayment || results[0].ID != payRahulID.String() {
			t.Errorf("expected payment match, got %+v", results[0])
		}
	})

	t.Run("Owner: near-miss UTR must NOT match (no fuzzy on money identifiers)", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "UTR99887765", 20, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		for _, r := range results {
			if r.Type == search.TypePayment && r.ID == payRahulID.String() {
				t.Fatalf("SECURITY/FINANCE BUG: near miss UTR matched payment: %+v", r)
			}
		}
	})

	t.Run("Owner: typo name does match via trigram", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahuk", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected typo match for Rahuk -> Rahul, got %d hits", len(results))
		}
		if !strings.HasPrefix(results[0].Title, "Rahul") {
			t.Errorf("expected typo match on Rahul, got %s", results[0].Title)
		}
	})

	t.Run("Owner: uppercase insensitive match", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "RAHUL", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected uppercase match, got %d hits", len(results))
		}
	})

	t.Run("Owner: starvation check (12 tenants + 12 dues still returns payment hit, <= 5 per type)", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahul", 20, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		counts := make(map[search.EntityType]int)
		for _, r := range results {
			counts[r.Type]++
		}
		for k, v := range counts {
			if v > 5 {
				t.Errorf("type %s exceeded per-type cap of 5: got %d", k, v)
			}
		}
		if counts[search.TypePayment] == 0 {
			t.Errorf("STARVATION BUG: payment was starved by tenants and dues: counts=%+v", counts)
		}
	})

	// ---------------------------------------------------------
	// SECTION 2: MANAGER CASES (~8 cases)
	// ---------------------------------------------------------
	t.Run("Manager: name match", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("manager name search failed, got %d hits", len(results))
		}
	})

	t.Run("Manager: room match '201'", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "201", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("manager room search failed, got %d hits", len(results))
		}
	})

	t.Run("Manager: exact full 10-digit phone matches", func(t *testing.T) {
		fullPhone := rahul201Phone[3:]
		_, _, results, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, fullPhone, 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("manager exact phone match failed, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("manager expected rahul201 on exact phone, got %s", results[0].Title)
		}
	})

	t.Run("Manager: partial phone returns NOTHING (scraping protection)", func(t *testing.T) {
		partPhone := rahul201Phone[len(rahul201Phone)-5:]
		_, _, results, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, partPhone, 20, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		for _, r := range results {
			if r.Type == search.TypeTenant {
				t.Fatalf("PRIVACY/SCRAPING LEAK: manager saw tenant on partial phone: %+v", r)
			}
		}
	})

	t.Run("Manager: financial types never appear even when types= asks for them", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical,
			[]search.EntityType{search.TypeDue, search.TypePayment, search.TypePaymentReport},
		)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		for _, r := range results {
			if r.Type == search.TypeDue || r.Type == search.TypePayment || r.Type == search.TypePaymentReport {
				t.Fatalf("SECURITY LEAK: Manager saw financial hit: %+v", r)
			}
		}
	})

	t.Run("Manager: hazard reporter tenant name finds no hazard (anonymity)", func(t *testing.T) {
		// Rahul reported the electrical hazard; searching "Rahul" should return tenants, but NOT the hazard!
		_, _, results, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		for _, r := range results {
			if r.Type == search.TypeHazard {
				t.Fatalf("ANONYMITY LEAK: Manager found hazard by searching reporter's name: %+v", r)
			}
		}
	})

	t.Run("Manager: three same-name tenants appear distinct with room in subtitle", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical, []search.EntityType{search.TypeTenant})
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		roomsFound := make(map[string]bool)
		for _, r := range results {
			if strings.Contains(r.Subtitle, "Room 201") {
				roomsFound["201"] = true
			}
			if strings.Contains(r.Subtitle, "Room 202") {
				roomsFound["202"] = true
			}
			if strings.Contains(r.Subtitle, "Room 303") {
				roomsFound["303"] = true
			}
		}
		if !roomsFound["201"] || !roomsFound["202"] || !roomsFound["303"] {
			t.Errorf("expected distinct room subtitles for same-name tenants, got %+v", roomsFound)
		}
	})

	t.Run("Manager: inspection, violation and hazard lookup", func(t *testing.T) {
		_, _, rInsp, _ := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "electricity", 10, search.ModeLexical, nil)
		if len(rInsp) == 0 || rInsp[0].Type != search.TypeInspection {
			t.Errorf("expected inspection hit for electricity, got %+v", rInsp)
		}

		_, _, rHaz, _ := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "corridor", 10, search.ModeLexical, nil)
		if len(rHaz) == 0 || rHaz[0].Type != search.TypeHazard {
			t.Errorf("expected hazard hit for corridor, got %+v", rHaz)
		}

		_, _, rViol, _ := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "NOISE", 10, search.ModeLexical, nil)
		if len(rViol) == 0 || rViol[0].Type != search.TypeViolation {
			t.Errorf("expected violation hit for NOISE, got %+v", rViol)
		}
	})

	// ---------------------------------------------------------
	// SECTION 3: TENANT CASES (~8 cases)
	// ---------------------------------------------------------
	t.Run("Tenant: own due matches", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "DUE-JAN", 10, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected tenant to find own due, got %d hits", len(results))
		}
		if results[0].ID != dueRahulID.String() {
			t.Errorf("expected dueRahulID, got %+v", results[0])
		}
	})

	t.Run("Tenant: own payment matches", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "UTR99887766", 10, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected tenant to find own payment, got %d hits", len(results))
		}
		if results[0].ID != payRahulID.String() {
			t.Errorf("expected payRahulID, got %+v", results[0])
		}
	})

	t.Run("Tenant: another tenant due code returns zero", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "DUE-SUR1", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant saw another tenant due: %+v", results)
		}
	})

	t.Run("Tenant: another tenant UTR returns zero", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "UTR11223344", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant saw another tenant payment UTR: %+v", results)
		}
	})

	t.Run("Tenant: another tenant name returns zero", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "Suresh", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant saw another tenant by name: %+v", results)
		}
	})

	t.Run("Tenant: missing tenant ID fails closed (0 hits)", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, nil, "DUE-JAN", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search unexpected error: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: nil tenantID returned results: %+v", results)
		}
	})

	t.Run("Tenant: types=tenant is ignored", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "Rahul", 10, search.ModeLexical, []search.EntityType{search.TypeTenant})
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant was able to query tenant entities: %+v", results)
		}
	})

	t.Run("Tenant: cross property returns zero", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "Cross Rahul", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("CROSS-PROPERTY LEAK: tenant saw cross property tenant: %+v", results)
		}
	})

	// ---------------------------------------------------------
	// SECTION 4: INPUT VALIDATION & SECURITY (~4 cases)
	// ---------------------------------------------------------
	t.Run("Input: 1 character query is rejected", func(t *testing.T) {
		_, _, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "a", 10, search.ModeLexical, nil)
		if err == nil || !strings.Contains(err.Error(), "too short") {
			t.Fatalf("expected 'query too short' error for 1 char, got %v", err)
		}
	})

	t.Run("Input: 101 character query is rejected", func(t *testing.T) {
		longQ := strings.Repeat("x", 101)
		_, _, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, longQ, 10, search.ModeLexical, nil)
		if err == nil || !strings.Contains(err.Error(), "too long") {
			t.Fatalf("expected 'query too long' error for 101 chars, got %v", err)
		}
	})

	t.Run("Input: literal % or _ is treated as text", func(t *testing.T) {
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "%_", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		// Should not match random records due to unescaped % or _
		if len(results) != 0 {
			t.Fatalf("expected 0 results for literal %%_, got %d", len(results))
		}
	})

	t.Run("Input: SQL-looking strings are inert", func(t *testing.T) {
		sqlInjection := "'; DROP TABLE tenants; --"
		_, _, results, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, sqlInjection, 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("expected 0 hits for SQL injection string, got %d", len(results))
		}
		// Verify tenants table is completely intact:
		var count int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE property_id = $1`, propID).Scan(&count)
		if count == 0 {
			t.Fatalf("CRITICAL SECURITY FLAW: SQL injection succeeded!")
		}
	})
}
