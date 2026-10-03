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

// TestLivePostgresSearchV2_GoldenRelevanceSuite executes the complete ~30-case golden relevance suite
// against a live PostgreSQL database instance with pg_trgm and bare-column indexes.
func TestLivePostgresSearchV2_GoldenRelevanceSuite(t *testing.T) {
	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")

	cfg, err := config.Load()
	if err != nil || cfg.DatabaseURL == "" {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatalf("REQUIRE_DB=1 but DATABASE_URL is unset")
		}
		t.Skip("skipping search v2 live test: DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	// Ensure extensions
	_, _ = pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"; CREATE EXTENSION IF NOT EXISTS pg_trgm;`)

	// Apply migration 038 and 039
	migrationsDir := "../../migrations"
	if _, err := os.Stat(migrationsDir); err != nil {
		migrationsDir = "migrations"
		if _, err := os.Stat(migrationsDir); err != nil {
			migrationsDir = "../migrations"
		}
	}
	m038Path := filepath.Join(migrationsDir, "038_search_v2_indexes.sql")
	if m038SQL, err := os.ReadFile(m038Path); err == nil {
		_, _ = pool.Exec(ctx, string(m038SQL))
	}
	m039Path := filepath.Join(migrationsDir, "039_search_v2_tuning.sql")
	if m039SQL, err := os.ReadFile(m039Path); err == nil {
		_, _ = pool.Exec(ctx, string(m039SQL))
	}
	m040Path := filepath.Join(migrationsDir, "040_search_property_scoped_trgm.sql")
	if m040SQL, err := os.ReadFile(m040Path); err == nil {
		_, _ = pool.Exec(ctx, string(m040SQL))
	}
	m041Path := filepath.Join(migrationsDir, "041_search_prefix_pattern_ops.sql")
	if m041SQL, err := os.ReadFile(m041Path); err == nil {
		_, _ = pool.Exec(ctx, string(m041SQL))
	}
	m042Path := filepath.Join(migrationsDir, "042_search_drop_redundant_global_trgm.sql")
	if m042SQL, err := os.ReadFile(m042Path); err == nil {
		_, _ = pool.Exec(ctx, string(m042SQL))
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
		exec(`DELETE FROM payout_payees WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM gateway_refunds WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM gateway_settlements WHERE property_id IN ($1, $2)`, propID, crossPropID)
		exec(`DELETE FROM bank_transactions WHERE property_id IN ($1, $2)`, propID, crossPropID)
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
	hindiTenantID := uuid.New()
	teluguTenantID := uuid.New()

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
			($2, $4, 'Rahul', '202', $8, 'active', 550000, 5),
			($3, $4, 'Rahul', '303', $9, 'active', 600000, 5),
			($5, $4, 'Rahul Sharma', '204', $10, 'active', 500000, 5),
			($6, $4, 'Suresh Kumar', '105', $11, 'active', 450000, 5),
			($12, $13, 'Cross Rahul', '999', $14, 'active', 500000, 5),
			($15, $4, 'राहुल वर्मा', '305', '+919876543217', 'active', 500000, 5),
			($16, $4, 'రాహుల్ రెడ్డి', '306', '+919876543216', 'active', 500000, 5)`,
		rahul201ID, rahul202ID, rahul303ID, propID, rahulSharmaID, sureshID,
		rahul201Phone, rahul202Phone, rahul303Phone, rahulSharmaPhone, sureshPhone,
		crossRahulID, crossPropID, crossRahulPhone, hindiTenantID, teluguTenantID,
	)
	if err != nil {
		t.Fatalf("seed tenants failed: %v", err)
	}

	// 3. Seed 12 dummy tenants and 12 dummy dues named "Rahul Extra" to test starvation protection (<= 5 cap per type)
	for i := 1; i <= 12; i++ {
		extraTenantID := uuid.New()
		extraDueID := uuid.New()
		extraPhone := fmt.Sprintf("+916888888%03d", 500+i)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
			VALUES ($1, $2, $3, $4, $5, 'active', 500000, 5)`,
			extraTenantID, propID, fmt.Sprintf("Rahul Extra %d", i), fmt.Sprintf("9%02d", i), extraPhone,
		)
		if err != nil {
			t.Fatalf("seed extra tenant %d failed: %v", i, err)
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
			VALUES ($1, $2, $3, $4, 'rent', 'pending', 500000, 500000, CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE)`,
			extraDueID, propID, extraTenantID, fmt.Sprintf("DUE-E%02d", i),
		)
		if err != nil {
			t.Fatalf("seed extra due %d failed: %v", i, err)
		}
	}

	// 4. Primary dues and payments
	dueRahulID := uuid.New()
	dueSureshID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES
			($1, $3, $4, 'DUE-JAN1', 'rent', 'pending', 500000, 500000, CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE),
			($2, $3, $5, 'DUE-SUR1', 'electricity', 'pending', 450000, 450000, CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE)`,
		dueRahulID, dueSureshID, propID, rahul201ID, sureshID,
	)
	if err != nil {
		t.Fatalf("seed dues failed: %v", err)
	}

	payRahulID := uuid.New()
	paySureshID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, tenant_id, provider, amount, matched_by, upi_txn_id, raw_note, created_at)
		VALUES
			($1, $3, 'bank', 500000, 'manual', 'UTR99887766', 'Rent paid for Rahul', NOW()),
			($2, $4, 'bank', 500000, 'manual', 'UTR11223344', 'Suresh rent payment', NOW())`,
		payRahulID, paySureshID, rahul201ID, sureshID,
	)
	if err != nil {
		t.Fatalf("seed payments failed: %v", err)
	}

	payDigitRefID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, tenant_id, provider, amount, matched_by, upi_txn_id, raw_note, created_at)
		VALUES ($1, $2, 'bank', 500000, 'manual', '412345678901', 'Digit only UPI RRN payment', NOW())`,
		payDigitRefID, rahul201ID,
	)
	if err != nil {
		t.Fatalf("seed digit only payment failed: %v", err)
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

	// Bank transaction (unmatched credit):
	bankTxnID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO bank_transactions (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		VALUES ($1, $2, 'TXNBANK9988', 500000, 'credit', CURRENT_DATE, 'Unmatched Bank Deposit from Ramesh', 'dedup_hash_9988', 'unmatched')`,
		bankTxnID, propID,
	)
	if err != nil {
		t.Fatalf("seed bank transaction failed: %v", err)
	}

	bankRecallTxnID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO bank_transactions (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		VALUES ($1, $2, 'UTR202609301234', 500000, 'credit', CURRENT_DATE, 'NEFT CR RENT SEP', 'dedup_hash_1234', 'unmatched')`,
		bankRecallTxnID, propID,
	)
	if err != nil {
		t.Fatalf("seed bank recall transaction failed: %v", err)
	}

	// Gateway settlement:
	settleID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO gateway_settlements (id, property_id, cf_settlement_id, utr, currency, gross_amount_paise, net_amount_paise, settlement_status, reconciliation_status)
		VALUES ($1, $2, 'CF_SETTLE_99', 'UTR_SETTLE_99', 'INR', 500000, 490000, 'SUCCESS', 'matched')`,
		settleID, propID,
	)
	if err != nil {
		t.Fatalf("seed gateway settlement failed: %v", err)
	}

	// Gateway refund:
	refundID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO gateway_refunds (id, payment_id, property_id, cf_refund_id, provider_refund_id, amount_paise, status, reason, source)
		VALUES ($1, $2, $3, 'CF_REF_88', 'CF_REF_88', 100000, 'succeeded', 'Deposit refund', 'system')`,
		refundID, payRahulID, propID,
	)
	if err != nil {
		t.Fatalf("seed gateway refund failed: %v", err)
	}

	// Payout payee:
	payeeID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_payees (id, property_id, payee_type, name, phone, upi_vpa, account_number_hash)
		VALUES ($1, $2, 'vendor', 'Mahesh Electrician', '+919876543219', 'mahesh@upi', '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef')`,
		payeeID, propID,
	)
	if err != nil {
		t.Fatalf("seed payout payee failed: %v", err)
	}

	// ---------------------------------------------------------
	// SECTION 1: OWNER CASES (~12 cases)
	// ---------------------------------------------------------
	t.Run("Owner: exact name match", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahul Sharma", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected results for exact name, err=%v, count=%d", err, len(results))
		}
		if results[0].Title != "Rahul Sharma" {
			t.Errorf("expected top result Rahul Sharma, got %s", results[0].Title)
		}
	})

	t.Run("Owner: prefix name match", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahu", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected prefix results, got %d", len(results))
		}
		if !strings.HasPrefix(results[0].Title, "Rahul") {
			t.Errorf("expected prefix match Rahul, got %s", results[0].Title)
		}
	})

	t.Run("Owner: multi-word AND match 'rahul 201'", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "rahul 201", 20, search.ModeLexical, nil)
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
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "201", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("search room 201 failed, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("expected tenant in room 201, got %s", results[0].Title)
		}
	})

	t.Run("Owner: partial phone allowed", func(t *testing.T) {
		partPhone := rahul201Phone[len(rahul201Phone)-5:]
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, partPhone, 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected partial phone match for owner, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("expected rahul201 on phone match, got %s", results[0].Title)
		}
	})

	t.Run("Owner: full phone match", func(t *testing.T) {
		fullPhone := rahul201Phone[3:]
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, fullPhone, 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected full phone match for owner, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("expected rahul201 on full phone, got %s", results[0].Title)
		}
	})

	t.Run("Owner: due code exact", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "DUE-JAN1", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected due code match, got %d hits", len(results))
		}
		if results[0].Type != search.TypeDue || results[0].ID != dueRahulID.String() {
			t.Errorf("expected due match, got %+v", results[0])
		}
	})

	t.Run("Owner: UTR exact", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "UTR99887766", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected UTR match, got %d hits", len(results))
		}
		if results[0].Type != search.TypePayment || results[0].ID != payRahulID.String() {
			t.Errorf("expected payment match, got %+v", results[0])
		}
	})

	t.Run("Owner: near-miss UTR must NOT match (no fuzzy on money identifiers)", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "UTR99887765", 20, search.ModeLexical, nil)
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
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahuk", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected typo match for Rahuk -> Rahul, got %d hits", len(results))
		}
		if !strings.HasPrefix(results[0].Title, "Rahul") {
			t.Errorf("expected typo match on Rahul, got %s", results[0].Title)
		}
	})

	t.Run("Owner: uppercase insensitive match", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "RAHUL", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected uppercase match, got %d hits", len(results))
		}
	})

	t.Run("Owner: starvation check (12 tenants + 12 dues still returns payment hit, <= 5 per type)", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Rahul", 20, search.ModeLexical, nil)
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

	// Extended Coverage tests for Owner: Bank Txns, Settlements, Refunds, Payees
	t.Run("Owner: unmatched bank credit lookup via txn_id and narration", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "TXNBANK9988", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected bank transaction hit for TXNBANK9988, err=%v, count=%d", err, len(results))
		}
		if results[0].Type != search.TypeBankTransaction || results[0].ID != bankTxnID.String() {
			t.Errorf("expected bank transaction match, got %+v", results[0])
		}
	})

	t.Run("Owner: unmatched bank credit lookup via token found only in narration", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Unmatched Bank Deposit", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected bank transaction hit for narration 'Unmatched Bank Deposit', err=%v, count=%d", err, len(results))
		}
		found := false
		for _, r := range results {
			if r.Type == search.TypeBankTransaction && r.ID == bankTxnID.String() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected bank transaction %s in results for narration search, got %+v", bankTxnID, results)
		}
	})

	t.Run("Owner: digit-only 12-digit UPI reference exact and prefix match", func(t *testing.T) {
		// 1. Full 12 digits
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "412345678901", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected payment hit for 12-digit UPI ref 412345678901, err=%v, count=%d", err, len(results))
		}
		if results[0].Type != search.TypePayment || results[0].ID != payDigitRefID.String() {
			t.Errorf("expected payDigitRefID match, got %+v", results[0])
		}

		// 2. 6-digit prefix
		_, _, results, _, err = searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "412345", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected payment hit for 6-digit prefix 412345, err=%v, count=%d", err, len(results))
		}
		found := false
		for _, r := range results {
			if r.Type == search.TypePayment && r.ID == payDigitRefID.String() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected payDigitRefID in prefix results, got %+v", results)
		}
	})

	t.Run("Owner: digit-only UPI reference suffix match via stage 2 fallback", func(t *testing.T) {
		// Search last 6 digits of 412345678901
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "678901", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected stage 2 fallback hit for 678901, err=%v, count=%d", err, len(results))
		}
		found := false
		for _, r := range results {
			if r.Type == search.TypePayment && r.ID == payDigitRefID.String() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected payDigitRefID in suffix results, got %+v", results)
		}
	})

	t.Run("Owner: bank txn_id fragment match via stage 2 fallback ('1234' in 'UTR202609301234')", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "1234", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected bank txn hit for fragment 1234, err=%v, count=%d", err, len(results))
		}
		found := false
		for _, r := range results {
			if r.Type == search.TypeBankTransaction && r.ID == bankRecallTxnID.String() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected bankRecallTxnID in fragment results, got %+v", results)
		}
	})

	t.Run("Owner: gateway settlement lookup via UTR and settlement ID", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "UTR_SETTLE_99", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected settlement hit for UTR_SETTLE_99, err=%v, count=%d", err, len(results))
		}
		if results[0].Type != search.TypeSettlement || results[0].ID != settleID.String() {
			t.Errorf("expected settlement match, got %+v", results[0])
		}
	})

	t.Run("Owner: gateway refund lookup via refund ID", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "CF_REF_88", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected refund hit for CF_REF_88, err=%v, count=%d", err, len(results))
		}
		if results[0].Type != search.TypeRefund || results[0].ID != refundID.String() {
			t.Errorf("expected refund match, got %+v", results[0])
		}
	})

	t.Run("Owner: payout payee lookup via name", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "Electrician", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected payout payee hit for Electrician, err=%v, count=%d", err, len(results))
		}
		if results[0].Type != search.TypePayout || results[0].ID != payeeID.String() {
			t.Errorf("expected payee match, got %+v", results[0])
		}
	})

	// Indic Unicode Name Matching (Defect 11)
	t.Run("Owner: Hindi Unicode search", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "राहुल", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected Hindi Unicode match for राहुल, got count=%d err=%v", len(results), err)
		}
		found := false
		for _, r := range results {
			if r.ID == hindiTenantID.String() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected hindi tenant %s in results: %+v", hindiTenantID, results)
		}
	})

	t.Run("Owner: Telugu Unicode search", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "రాహుల్", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected Telugu Unicode match for రాహుల్, got count=%d err=%v", len(results), err)
		}
		found := false
		for _, r := range results {
			if r.ID == teluguTenantID.String() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected telugu tenant %s in results: %+v", teluguTenantID, results)
		}
	})

	// ---------------------------------------------------------
	// SECTION 2: MANAGER CASES (~8 cases)
	// ---------------------------------------------------------
	t.Run("Manager: name match", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("manager name search failed, got %d hits", len(results))
		}
	})

	t.Run("Manager: room match '201'", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "201", 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("manager room search failed, got %d hits", len(results))
		}
	})

	t.Run("Manager: exact full 10-digit phone matches", func(t *testing.T) {
		fullPhone := rahul201Phone[3:]
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, fullPhone, 20, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("manager exact phone match failed, got %d hits", len(results))
		}
		if results[0].ID != rahul201ID.String() {
			t.Errorf("manager expected rahul201 on exact phone, got %s", results[0].Title)
		}
	})

	t.Run("Manager: partial phone returns NOTHING (scraping protection)", func(t *testing.T) {
		partPhone := rahul201Phone[len(rahul201Phone)-5:]
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, partPhone, 20, search.ModeLexical, nil)
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
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical,
			[]search.EntityType{search.TypeDue, search.TypePayment, search.TypePaymentReport, search.TypeBankTransaction, search.TypeSettlement},
		)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		for _, r := range results {
			if r.Type == search.TypeDue || r.Type == search.TypePayment || r.Type == search.TypePaymentReport ||
				r.Type == search.TypeBankTransaction || r.Type == search.TypeSettlement {
				t.Fatalf("SECURITY LEAK: Manager saw financial hit: %+v", r)
			}
		}
	})

	t.Run("Manager: hazard reporter tenant name finds no hazard (anonymity)", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical, nil)
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
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "Rahul", 20, search.ModeLexical, []search.EntityType{search.TypeTenant})
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
		_, _, rInsp, _, _ := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "electricity", 10, search.ModeLexical, nil)
		if len(rInsp) == 0 || rInsp[0].Type != search.TypeInspection {
			t.Errorf("expected inspection hit for electricity, got %+v", rInsp)
		}

		_, _, rHaz, _, _ := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "corridor", 10, search.ModeLexical, nil)
		if len(rHaz) == 0 || rHaz[0].Type != search.TypeHazard {
			t.Errorf("expected hazard hit for corridor, got %+v", rHaz)
		}

		_, _, rViol, _, _ := searchSvc.Search(ctx, domain.RoleManager, propID, nil, "NOISE", 10, search.ModeLexical, nil)
		if len(rViol) == 0 || rViol[0].Type != search.TypeViolation {
			t.Errorf("expected violation hit for NOISE, got %+v", rViol)
		}
	})

	// ---------------------------------------------------------
	// SECTION 3: TENANT CASES (~8 cases)
	// ---------------------------------------------------------
	t.Run("Tenant: own due matches", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "DUE-JAN", 10, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected tenant to find own due, got %d hits", len(results))
		}
		if results[0].ID != dueRahulID.String() {
			t.Errorf("expected dueRahulID, got %+v", results[0])
		}
	})

	t.Run("Tenant: own payment matches", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "UTR99887766", 10, search.ModeLexical, nil)
		if err != nil || len(results) == 0 {
			t.Fatalf("expected tenant to find own payment, got %d hits", len(results))
		}
		if results[0].ID != payRahulID.String() {
			t.Errorf("expected payRahulID, got %+v", results[0])
		}
	})

	t.Run("Tenant: another tenant due code returns zero", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "DUE-SUR1", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant saw another tenant due: %+v", results)
		}
	})

	t.Run("Tenant: another tenant UTR returns zero", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "UTR11223344", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant saw another tenant payment UTR: %+v", results)
		}
	})

	t.Run("Tenant: another tenant name returns zero", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "Suresh", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant saw another tenant by name: %+v", results)
		}
	})

	t.Run("Tenant: missing tenant ID fails closed (0 hits)", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, nil, "DUE-JAN", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search unexpected error: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: nil tenantID returned results: %+v", results)
		}
	})

	t.Run("Tenant: types=tenant is ignored", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "Rahul", 10, search.ModeLexical, []search.EntityType{search.TypeTenant})
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("SECURITY LEAK: tenant was able to query tenant entities: %+v", results)
		}
	})

	t.Run("Tenant: cross property returns zero", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleTenant, propID, &rahul201ID, "Cross Rahul", 10, search.ModeLexical, nil)
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
		_, _, _, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "a", 10, search.ModeLexical, nil)
		if err == nil || !strings.Contains(err.Error(), "too short") {
			t.Fatalf("expected 'query too short' error for 1 char, got %v", err)
		}
	})

	t.Run("Input: 101 character query is rejected", func(t *testing.T) {
		longQ := strings.Repeat("x", 101)
		_, _, _, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, longQ, 10, search.ModeLexical, nil)
		if err == nil || !strings.Contains(err.Error(), "too long") {
			t.Fatalf("expected 'query too long' error for 101 chars, got %v", err)
		}
	})

	t.Run("Input: literal % or _ is treated as text", func(t *testing.T) {
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, "%_", 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("expected 0 results for literal %%_, got %d", len(results))
		}
	})

	t.Run("Input: SQL-looking strings are inert", func(t *testing.T) {
		sqlInjection := "'; DROP TABLE tenants; --"
		_, _, results, _, err := searchSvc.Search(ctx, domain.RoleOwner, propID, nil, sqlInjection, 10, search.ModeLexical, nil)
		if err != nil {
			t.Fatalf("search failed: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("expected 0 hits for SQL injection string, got %d", len(results))
		}
		var count int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE property_id = $1`, propID).Scan(&count)
		if count == 0 {
			t.Fatalf("CRITICAL SECURITY FLAW: SQL injection succeeded!")
		}
	})
}
