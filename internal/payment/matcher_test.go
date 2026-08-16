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
}
