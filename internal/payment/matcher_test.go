package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubDues struct {
	byCode   map[string]*domain.Due
	byID     map[uuid.UUID]*domain.Due
	window   []domain.Due
	windowFn func(propertyID uuid.UUID, amount int, from, to time.Time) ([]domain.Due, error)
}

func (s *stubDues) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	d, ok := s.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *d
	return &cp, nil
}

func (s *stubDues) GetByDueCode(_ context.Context, code string) (*domain.Due, error) {
	d, ok := s.byCode[code]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *d
	return &cp, nil
}

func (s *stubDues) Update(_ context.Context, d *domain.Due) error {
	if s.byID == nil {
		s.byID = map[uuid.UUID]*domain.Due{}
	}
	cp := *d
	s.byID[d.ID] = &cp
	if s.byCode != nil && d.DueCode != "" {
		s.byCode[d.DueCode] = &cp
	}
	return nil
}

func (s *stubDues) ListByTenant(context.Context, uuid.UUID) ([]domain.Due, error) {
	return nil, nil
}

func (s *stubDues) FindByAmountAndDateWindow(_ context.Context, propertyID uuid.UUID, amount int, from, to time.Time) ([]domain.Due, error) {
	if s.windowFn != nil {
		return s.windowFn(propertyID, amount, from, to)
	}
	return s.window, nil
}

func TestExtractDueCode(t *testing.T) {
	cases := []struct {
		note string
		want string
	}{
		{"UPI/PG-A3X9KR/foo", "A3X9KR"},
		{"payment pg-abc123 received", "ABC123"},
		{"no code here", ""},
		{"PG-AB12", ""}, // too short
		{"prefix PG-ZZ99AA suffix", "ZZ99AA"},
	}
	for _, tc := range cases {
		if got := ExtractDueCode(tc.note); got != tc.want {
			t.Fatalf("ExtractDueCode(%q)=%q want %q", tc.note, got, tc.want)
		}
	}
}

func TestDueCodeMatcher(t *testing.T) {
	dueID := uuid.New()
	dues := &stubDues{byCode: map[string]*domain.Due{
		"A3X9KR": {ID: dueID, DueCode: "A3X9KR", Amount: 1000, Status: domain.DueStatusPending},
	}}
	m := NewDueCodeMatcher(dues)
	due, err := m.Match(context.Background(), "IMPS-PG-A3X9KR-OK")
	if err != nil {
		t.Fatal(err)
	}
	if due == nil || due.ID != dueID {
		t.Fatalf("expected due %s, got %#v", dueID, due)
	}
	due, err = m.Match(context.Background(), "no code")
	if err != nil || due != nil {
		t.Fatalf("expected nil due, got %#v err=%v", due, err)
	}
}

func TestAmountDateWindowMatcherAmbiguous(t *testing.T) {
	prop := uuid.New()
	dues := &stubDues{window: []domain.Due{
		{ID: uuid.New(), Amount: 5000, Status: domain.DueStatusPending},
		{ID: uuid.New(), Amount: 5000, Status: domain.DueStatusPending},
	}}
	m := NewAmountDateWindowMatcher(dues)
	_, err := m.Match(context.Background(), prop, 5000, time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("want ErrAmbiguous, got %v", err)
	}
}

func TestAmountDateWindowMatcherSingle(t *testing.T) {
	prop := uuid.New()
	id := uuid.New()
	dues := &stubDues{window: []domain.Due{
		{ID: id, Amount: 5000, Status: domain.DueStatusPending},
	}}
	m := NewAmountDateWindowMatcher(dues)
	due, err := m.Match(context.Background(), prop, 5000, time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if due.ID != id {
		t.Fatalf("got %s want %s", due.ID, id)
	}
}

func TestMatcherPrefersDueCode(t *testing.T) {
	prop := uuid.New()
	codeDue := &domain.Due{ID: uuid.New(), DueCode: "ZZZZ01", PropertyID: prop, Amount: 100, Status: domain.DueStatusPending}
	windowDue := domain.Due{ID: uuid.New(), Amount: 100, Status: domain.DueStatusPending}
	dues := &stubDues{
		byCode: map[string]*domain.Due{"ZZZZ01": codeDue},
		window: []domain.Due{windowDue},
	}
	m := NewMatcher(dues)
	res, err := m.Match(context.Background(), prop, 100, time.Now(), "note PG-ZZZZ01 end")
	if err != nil {
		t.Fatal(err)
	}
	if res.MatchedBy != domain.MatchedByDueCode || res.Due.ID != codeDue.ID {
		t.Fatalf("expected due_code match, got %#v", res)
	}
}

func TestMatcherDueCodeCrossPropertyRejected(t *testing.T) {
	propA := uuid.New()
	propB := uuid.New()
	codeDue := &domain.Due{ID: uuid.New(), DueCode: "XXPROP", PropertyID: propB, Amount: 100, Status: domain.DueStatusPending}
	dues := &stubDues{byCode: map[string]*domain.Due{"XXPROP": codeDue}}
	m := NewMatcher(dues)
	_, err := m.Match(context.Background(), propA, 100, time.Now(), "PG-XXPROP")
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want ErrNoMatch for cross-property, got %v", err)
	}
}

func TestMatcherDueCodeUnderpayStillMatches(t *testing.T) {
	prop := uuid.New()
	codeDue := &domain.Due{ID: uuid.New(), DueCode: "PART01", PropertyID: prop, Amount: 10000, Status: domain.DueStatusPending}
	dues := &stubDues{byCode: map[string]*domain.Due{"PART01": codeDue}}
	m := NewMatcher(dues)
	res, err := m.Match(context.Background(), prop, 5000, time.Now(), "PG-PART01")
	if err != nil {
		t.Fatal(err)
	}
	if res.MatchedBy != domain.MatchedByDueCode {
		t.Fatalf("want due_code, got %s", res.MatchedBy)
	}
}

func TestMatcherFallsBackToAmountDate(t *testing.T) {
	prop := uuid.New()
	id := uuid.New()
	dues := &stubDues{
		byCode: map[string]*domain.Due{},
		window: []domain.Due{{ID: id, Amount: 2000, Status: domain.DueStatusPending}},
	}
	m := NewMatcher(dues)
	res, err := m.Match(context.Background(), prop, 2000, time.Now(), "no code")
	if err != nil {
		t.Fatal(err)
	}
	if res.MatchedBy != domain.MatchedByAmountDateWindow || res.Due.ID != id {
		t.Fatalf("expected amount_date_window, got %#v", res)
	}
	if res.IsDeterministic {
		t.Fatalf("amount_date_window match must NOT be marked deterministic")
	}
}

func TestMatcher_IsDeterministicFlag(t *testing.T) {
	prop := uuid.New()
	codeDue := &domain.Due{ID: uuid.New(), DueCode: "DET001", PropertyID: prop, Amount: 5000, Status: domain.DueStatusPending}
	windowDue := domain.Due{ID: uuid.New(), Amount: 5000, PropertyID: prop, Status: domain.DueStatusPending}

	dues := &stubDues{
		byCode: map[string]*domain.Due{"DET001": codeDue},
		window: []domain.Due{windowDue},
	}
	m := NewMatcher(dues)

	// DueCode match MUST be deterministic
	resCode, err := m.Match(context.Background(), prop, 5000, time.Now(), "PG-DET001")
	if err != nil {
		t.Fatalf("due code match failed: %v", err)
	}
	if !resCode.IsDeterministic {
		t.Errorf("expected due_code match to be deterministic, got false")
	}

	// AmountDate window match MUST NOT be deterministic
	resWindow, err := m.Match(context.Background(), prop, 5000, time.Now(), "no code in note")
	if err != nil {
		t.Fatalf("amount date match failed: %v", err)
	}
	if resWindow.IsDeterministic {
		t.Errorf("expected amount_date match to NOT be deterministic, got true")
	}
}

func TestMatchPayment_HeuristicDoesNotAutoSettle(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	tenant1 := uuid.New()
	tenant2 := uuid.New()

	due1ID := uuid.New()
	due2ID := uuid.New()
	now := time.Now().UTC()

	// Two identical dues for two different tenants with the same amount (₹10,000) on the same property
	due1 := &domain.Due{
		ID: due1ID, TenantID: tenant1, PropertyID: propID,
		DueCode: "CODE01", Amount: 1000000, OriginalAmount: 1000000,
		Status: domain.DueStatusPending, DueDate: now,
	}
	due2 := &domain.Due{
		ID: due2ID, TenantID: tenant2, PropertyID: propID,
		DueCode: "CODE02", Amount: 1000000, OriginalAmount: 1000000,
		Status: domain.DueStatusPending, DueDate: now,
	}

	dues := &stubDues{
		byID:   map[uuid.UUID]*domain.Due{due1ID: due1, due2ID: due2},
		byCode: map[string]*domain.Due{"CODE01": due1, "CODE02": due2},
		// When querying by amount and date window, simulate single candidate (e.g. if tenant2's due was 1 day earlier)
		window: []domain.Due{*due1},
	}
	pays := &stubPayments{}
	tenants := &stubTenants{byID: map[uuid.UUID]*domain.Tenant{
		tenant1: {ID: tenant1, PropertyID: propID},
		tenant2: {ID: tenant2, PropertyID: propID},
	}}
	pub := &recordingPublisher{}
	svc := NewService(dues, pays, tenants, nil, pub)

	// Case 1: Heuristic bank row (amount matches, but NO due code in narration)
	// MUST NOT auto-settle the due! Must return ErrRequiresConfirmation.
	p, err := svc.MatchPayment(ctx, propID, "TXN_HEURISTIC_01", 1000000, now, "UPI/9876543210/Transfer")
	if !errors.Is(err, ErrRequiresConfirmation) {
		t.Fatalf("expected ErrRequiresConfirmation for heuristic match, got: %v (payment=%v)", err, p)
	}

	// Verify due1 was NOT settled and remains pending
	if due1.Status != domain.DueStatusPending {
		t.Fatalf("critical invariant violated: due was auto-settled on amount+date alone! status=%s", due1.Status)
	}
	if len(pays.created) > 0 {
		t.Fatalf("expected 0 payments created for heuristic match, got %d", len(pays.created))
	}

	// Case 2: Deterministic bank row (explicit DueCode PG-CODE01 in narration)
	// MUST safely auto-settle the exact due.
	pDet, err := svc.MatchPayment(ctx, propID, "TXN_DETERMINISTIC_01", 1000000, now, "UPI/9876543210/Rent PG-CODE01")
	if err != nil {
		t.Fatalf("expected deterministic due code match to succeed, got: %v", err)
	}
	if pDet == nil || pDet.DueID != due1ID {
		t.Fatalf("expected payment on due1, got: %v", pDet)
	}
	updatedDue, _ := dues.GetByID(ctx, due1ID)
	if updatedDue.Status != domain.DueStatusPaid {
		t.Fatalf("expected due1 to be marked paid, got %s", updatedDue.Status)
	}
	if len(pays.created) != 1 {
		t.Fatalf("expected 1 payment created for deterministic match, got %d", len(pays.created))
	}
}

