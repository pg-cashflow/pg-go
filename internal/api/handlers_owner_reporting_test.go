package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/intelligence"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/roi"
)

type reportingPayments struct {
	api.PaymentService
	summaries map[uuid.UUID]*payment.ReconciliationSummary
}

func (p *reportingPayments) BuildSummary(_ context.Context, propID uuid.UUID, _ string) (*payment.ReconciliationSummary, error) {
	if s, ok := p.summaries[propID]; ok {
		return s, nil
	}
	return &payment.ReconciliationSummary{RentCollected: 0}, nil
}

type reportingEventStore struct {
	events []domain.Event
}

func (s *reportingEventStore) List(_ context.Context, f postgres.EventFilter) ([]domain.Event, error) {
	var out []domain.Event
	for _, e := range s.events {
		if e.PropertyID == f.PropertyID {
			out = append(out, e)
		}
	}
	return out, nil
}

type reportingPropStore struct {
	props map[uuid.UUID]*domain.Property
}

func (s *reportingPropStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	if p, ok := s.props[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, fmt.Errorf("property not found")
}
func (s *reportingPropStore) List(context.Context) ([]domain.Property, error) { return nil, nil }
func (s *reportingPropStore) GetByOwnerPhone(context.Context, string) (*domain.Property, error) {
	return nil, nil
}
func (s *reportingPropStore) GetByInviteCode(context.Context, string) (*domain.Property, error) {
	return nil, nil
}
func (s *reportingPropStore) Create(context.Context, *domain.Property) error { return nil }

type reportingTenantStore struct{}

func (s *reportingTenantStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
	return nil, nil
}
func (s *reportingTenantStore) ListByProperty(_ context.Context, _ uuid.UUID) ([]domain.Tenant, error) {
	return []domain.Tenant{}, nil
}
func (s *reportingTenantStore) Update(_ context.Context, _ *domain.Tenant) error { return nil }
func (s *reportingTenantStore) GetIDPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return nil, nil
}

func TestOwnerReportingRoutes_T4b(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-32-chars-long-reporting!!"

	propA := uuid.New()
	propB := uuid.New()

	ownerA := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propA, Phone: "+919876500001"}
	ownerB := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propB, Phone: "+919876500002"}
	tokenA, _ := auth.IssueToken(jwtSecret, ownerA)
	tokenB, _ := auth.IssueToken(jwtSecret, ownerB)

	memStore := finance.NewMemoryStore()
	finSvc := finance.NewService(memStore, nil)
	roiSvc := roi.NewService(finSvc)
	intelSvc := intelligence.NewService(memStore)

	ctx := context.Background()
	now := time.Now().UTC()

	// Seed property A financial data:
	// 2 approved expenses: 20000 + 30000 = 50000 paise
	_ = memStore.InsertExpense(ctx, &domain.Expense{
		ID:             uuid.New(),
		PropertyID:     propA,
		CategoryCode:   "maintenance",
		AmountPaise:    20000,
		Status:         domain.ExpenseApproved,
		IdempotencyKey: "seed-exp-a1",
		OccurredAt:     now,
		CreatedAt:      now,
	})
	_ = memStore.InsertExpense(ctx, &domain.Expense{
		ID:             uuid.New(),
		PropertyID:     propA,
		CategoryCode:   "utilities",
		AmountPaise:    30000,
		Status:         domain.ExpenseApproved,
		IdempotencyKey: "seed-exp-a2",
		OccurredAt:     now,
		CreatedAt:      now,
	})

	// Seed journal lines for Property A (matching plain sum = 50000 paise)
	_ = memStore.InsertJournal(ctx, []domain.JournalLine{
		{PropertyID: propA, SourceType: "expense", SourceID: uuid.New(), LineKind: "dr", AccountCode: domain.AcctOperatingExpense, DebitPaise: 20000, OccurredAt: now},
		{PropertyID: propA, SourceType: "expense", SourceID: uuid.New(), LineKind: "dr", AccountCode: domain.AcctOperatingExpense, DebitPaise: 30000, OccurredAt: now},
	})

	// Seed property B financial data:
	// 1 approved expense: 77000 paise
	_ = memStore.InsertExpense(ctx, &domain.Expense{
		ID:             uuid.New(),
		PropertyID:     propB,
		CategoryCode:   "repairs",
		AmountPaise:    77000,
		Status:         domain.ExpenseApproved,
		IdempotencyKey: "seed-exp-b1",
		OccurredAt:     now,
		CreatedAt:      now,
	})

	// Seed journal line for Property B (matching plain sum = 77000 paise)
	_ = memStore.InsertJournal(ctx, []domain.JournalLine{
		{PropertyID: propB, SourceType: "expense", SourceID: uuid.New(), LineKind: "dr", AccountCode: domain.AcctOperatingExpense, DebitPaise: 77000, OccurredAt: now},
	})

	// Seed capital for property A: 100000 paise
	_ = memStore.InsertCapital(ctx, &domain.CapitalTransaction{
		ID:             uuid.New(),
		PropertyID:     propA,
		OwnerUserID:    ownerA.ID,
		Kind:           domain.CapitalInitial,
		AmountPaise:    100000,
		IdempotencyKey: "seed-cap-a1",
		OccurredAt:     now,
		CreatedAt:      now,
	})

	// Seed advance for property A: 15000 paise
	mgrA := uuid.New()
	_ = memStore.InsertAdvance(ctx, &domain.ManagerAdvance{
		ID:            uuid.New(),
		PropertyID:    propA,
		ManagerUserID: mgrA,
		AmountPaise:   15000,
		OccurredAt:    now,
	})

	// Seed advance for property B: 22000 paise
	mgrB := uuid.New()
	_ = memStore.InsertAdvance(ctx, &domain.ManagerAdvance{
		ID:            uuid.New(),
		PropertyID:    propB,
		ManagerUserID: mgrB,
		AmountPaise:   22000,
		OccurredAt:    now,
	})

	// Seed leakage and recommendations
	_ = memStore.InsertLeakage(ctx, &domain.LeakageEvent{
		ID:             uuid.New(),
		PropertyID:     propA,
		Category:       "water",
		EstimatedPaise: 12000,
		Severity:       2,
		Status:         "open",
	})
	_ = memStore.InsertRecommendation(ctx, &domain.Recommendation{
		ID:                   uuid.New(),
		PropertyID:           propA,
		Issue:                "water leakage",
		SuggestedAction:      "fix valves",
		ExpectedSavingsPaise: 6000,
		Status:               "open",
	})

	_ = memStore.InsertLeakage(ctx, &domain.LeakageEvent{
		ID:             uuid.New(),
		PropertyID:     propB,
		Category:       "electricity",
		EstimatedPaise: 33000,
		Severity:       2,
		Status:         "open",
	})

	// Seed events
	eventStore := &reportingEventStore{
		events: []domain.Event{
			{ID: 1, PropertyID: propA, EventType: domain.EventType("expense.created"), Payload: []byte("{}")},
			{ID: 2, PropertyID: propA, EventType: domain.EventType("capital.infused"), Payload: []byte("{}")},
			{ID: 3, PropertyID: propB, EventType: domain.EventType("expense.created"), Payload: []byte("{}")},
		},
	}

	paySvc := &reportingPayments{
		summaries: map[uuid.UUID]*payment.ReconciliationSummary{
			propA: {RentCollected: 200000, OutstandingRent: 50000},
			propB: {RentCollected: 50000, OutstandingRent: 10000},
		},
	}

	propStore := &reportingPropStore{
		props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, Name: "PG A", OwnerPhone: ownerA.Phone},
			propB: {ID: propB, Name: "PG B", OwnerPhone: ownerB.Phone},
		},
	}

	router := api.NewRouter(api.Deps{
		JWTSecret:           jwtSecret,
		Finance:             finSvc,
		FinanceEnabled:      true,
		ROI:                 roiSvc,
		Intelligence:        intelSvc,
		IntelligenceEnabled: true,
		Payments:            paySvc,
		EventStore:          eventStore,
		PropertyStore:       propStore,
		TenantStore:         &reportingTenantStore{},
	})

	doGet := func(path, token string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}

	// 1. FinanceSummary: GET /api/owner/finance/summary
	t.Run("FinanceSummary: Scoped to property and totals match plain sum baseline", func(t *testing.T) {
		status, body := doGet("/api/owner/finance/summary", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			Period    string                   `json:"period"`
			Operating finance.OperatingSummary `json:"operating"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal FinanceSummary: %v", err)
		}
		// Baseline check: property A expenses plain sum = 20000 + 30000 = 50000 paise
		if res.Operating.OpexPaise != 50000 {
			t.Fatalf("expected 50000 operating expenses, got %d", res.Operating.OpexPaise)
		}

		// Verify Owner B sees only Property B expenses: 77000 paise
		statusB, bodyB := doGet("/api/owner/finance/summary", tokenB)
		if statusB != http.StatusOK {
			t.Fatalf("expected 200 for Owner B, got %d", statusB)
		}
		var resB struct {
			Period    string                   `json:"period"`
			Operating finance.OperatingSummary `json:"operating"`
		}
		if err := json.Unmarshal(bodyB, &resB); err != nil {
			t.Fatalf("unmarshal FinanceSummary B: %v", err)
		}
		if resB.Operating.OpexPaise != 77000 {
			t.Fatalf("expected 77000 operating expenses for B, got %d", resB.Operating.OpexPaise)
		}
	})

	// 2. FinanceLedger: GET /api/owner/finance/ledger
	t.Run("FinanceLedger: Scoped to caller property", func(t *testing.T) {
		status, body := doGet("/api/owner/finance/ledger", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			Entries []domain.JournalLine `json:"entries"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal FinanceLedger: %v", err)
		}
		for _, e := range res.Entries {
			if e.PropertyID != propA {
				t.Fatalf("expected entry for propA, got %v", e.PropertyID)
			}
		}
	})

	// 3. ListAdvances: GET /api/owner/finance/advances
	t.Run("ListAdvances: Scoped to property and matches advance baseline", func(t *testing.T) {
		status, body := doGet("/api/owner/finance/advances", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			Advances         []domain.ManagerAdvance `json:"advances"`
			OutstandingPaise int64                   `json:"outstanding_paise"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal ListAdvances: %v", err)
		}
		if len(res.Advances) != 1 || res.Advances[0].AmountPaise != 15000 {
			t.Fatalf("expected 1 advance of 15000 paise for Prop A, got %+v", res.Advances)
		}
	})

	// 4. FinanceVarianceBridge: GET /api/owner/finance/variance-bridge
	t.Run("FinanceVarianceBridge: Valid response shape for caller property", func(t *testing.T) {
		status, body := doGet("/api/owner/finance/variance-bridge", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res map[string]any
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal FinanceVarianceBridge: %v", err)
		}
	})

	// 5. ListExpenseImports: GET /api/owner/finance/imports
	t.Run("ListExpenseImports: Valid response shape for caller property", func(t *testing.T) {
		status, body := doGet("/api/owner/finance/imports", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			Suggestions []domain.ExpenseImportSuggestion `json:"suggestions"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal ListExpenseImports: %v", err)
		}
	})

	// 6. OwnerROI*: GET /api/owner/roi, /recovery, /break-even, POST /scenario
	t.Run("OwnerROI: Scoped to caller property", func(t *testing.T) {
		status, body := doGet("/api/owner/roi", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			ROI roi.Report `json:"roi"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal OwnerROI: %v", err)
		}
		if res.ROI.Operating == nil || res.ROI.Operating.OpexPaise != 50000 {
			t.Fatalf("expected Operating.OpexPaise 50000, got %+v", res.ROI.Operating)
		}

		statusRec, bodyRec := doGet("/api/owner/roi/recovery", tokenA)
		if statusRec != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", statusRec, string(bodyRec))
		}

		statusBE, bodyBE := doGet("/api/owner/roi/break-even", tokenA)
		if statusBE != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", statusBE, string(bodyBE))
		}

		// Scenario POST
		reqScen := httptest.NewRequest(http.MethodPost, "/api/owner/roi/scenario", bytes.NewBufferString(`{"capacity_beds":20,"occupancy_bps":8000,"rent_per_bed_paise":1000000,"fixed_opex_paise":500000}`))
		reqScen.Header.Set("Authorization", "Bearer "+tokenA)
		reqScen.Header.Set("Content-Type", "application/json")
		wScen := httptest.NewRecorder()
		router.ServeHTTP(wScen, reqScen)
		if wScen.Code != http.StatusOK {
			t.Fatalf("expected 200 for scenario, got %d body=%s", wScen.Code, wScen.Body.String())
		}
	})

	// 7. InsightsSummary: GET /api/owner/insights
	t.Run("InsightsSummary: Scoped to caller property and matches leakage sum baseline", func(t *testing.T) {
		status, body := doGet("/api/owner/insights", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			Leakage         []domain.LeakageEvent   `json:"leakage"`
			Recommendations []domain.Recommendation `json:"recommendations"`
			COI6mPaise      int64                   `json:"coi_6m_paise"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal InsightsSummary: %v", err)
		}
		if len(res.Leakage) != 1 || res.Leakage[0].EstimatedPaise != 12000 {
			t.Fatalf("expected 1 leakage of 12000 paise for Prop A, got %+v", res.Leakage)
		}
		if len(res.Recommendations) != 1 {
			t.Fatalf("expected 1 recommendation for Prop A, got %+v", res.Recommendations)
		}
	})

	// 8. OwnerCOI: GET /api/owner/coi
	t.Run("OwnerCOI: Scoped to caller property", func(t *testing.T) {
		status, body := doGet("/api/owner/coi", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			COI6mPaise int64 `json:"coi_6m_paise"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal OwnerCOI: %v", err)
		}
		if res.COI6mPaise != intelligence.COI(12000, 6) {
			t.Fatalf("expected COI %d, got %d", intelligence.COI(12000, 6), res.COI6mPaise)
		}
	})

	// 9. OwnerForecast: GET /api/owner/forecast
	t.Run("OwnerForecast: Scoped to caller property", func(t *testing.T) {
		status, body := doGet("/api/owner/forecast", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res map[string]any
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal OwnerForecast: %v", err)
		}
	})

	// 10. Reconciliation: GET /api/owner/reconciliation
	t.Run("Reconciliation: Scoped to caller property and matches payments baseline", func(t *testing.T) {
		status, body := doGet("/api/owner/reconciliation", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res payment.ReconciliationSummary
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal Reconciliation: %v", err)
		}
		if res.RentCollected != 200000 {
			t.Fatalf("expected Reconciliation for Prop A with 200000 rent collected, got %+v", res)
		}
	})

	// 11. ListEvents: GET /api/owner/events
	t.Run("ListEvents: Scoped to caller property", func(t *testing.T) {
		status, body := doGet("/api/owner/events", tokenA)
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", status, string(body))
		}
		var res struct {
			Events []domain.Event `json:"events"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("unmarshal ListEvents: %v", err)
		}
		if len(res.Events) != 2 {
			t.Fatalf("expected 2 events for Prop A, got %d", len(res.Events))
		}
		for _, e := range res.Events {
			if e.PropertyID != propA {
				t.Fatalf("expected event for Prop A, got %v", e.PropertyID)
			}
		}
	})
}
