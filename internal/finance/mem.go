package finance

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// MemoryStore is an in-process Store for unit tests.
type MemoryStore struct {
	mu            sync.Mutex
	policies      map[uuid.UUID]domain.ApprovalPolicy
	settings      map[uuid.UUID]domain.PropertyFinanceSettings
	capital       []domain.CapitalTransaction
	expenses      map[uuid.UUID]domain.Expense
	payments      []domain.ExpensePayment
	advances      []domain.ManagerAdvance
	reimburse     []domain.ManagerReimbursement
	journal       []domain.JournalLine
	budgets       map[string]domain.Budget
	rewards       []domain.RewardLiabilityTxn
	tieouts       map[string]domain.PeriodTieOut
	approvals     map[uuid.UUID]domain.ApprovalRequest
	kpis          []domain.KPISnapshot
	rois          []domain.ROISnapshot
	leakage       map[uuid.UUID]domain.LeakageEvent
	recs          map[uuid.UUID]domain.Recommendation
	forecasts     []domain.ForecastSnapshot
	imports       map[string]domain.ExpenseImportSuggestion
	prep          []domain.MealPrepActual
	loyalty       map[uuid.UUID]domain.PropertyGamificationSettings
	idempotency   map[string]struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		policies:    map[uuid.UUID]domain.ApprovalPolicy{},
		settings:    map[uuid.UUID]domain.PropertyFinanceSettings{},
		loyalty:     map[uuid.UUID]domain.PropertyGamificationSettings{},
		expenses:    map[uuid.UUID]domain.Expense{},
		budgets:     map[string]domain.Budget{},
		tieouts:     map[string]domain.PeriodTieOut{},
		approvals:   map[uuid.UUID]domain.ApprovalRequest{},
		leakage:     map[uuid.UUID]domain.LeakageEvent{},
		recs:        map[uuid.UUID]domain.Recommendation{},
		imports:     map[string]domain.ExpenseImportSuggestion{},
		idempotency: map[string]struct{}{},
	}
}

func (m *MemoryStore) claim(key string) error {
	if key == "" {
		return ErrIdempotencyRequired
	}
	if _, ok := m.idempotency[key]; ok {
		return ErrDuplicateIdempotency
	}
	m.idempotency[key] = struct{}{}
	return nil
}

func (m *MemoryStore) EnsureDefaults(_ context.Context, propertyID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.policies[propertyID]; !ok {
		m.policies[propertyID] = defaultPolicy(propertyID)
	}
	if _, ok := m.settings[propertyID]; !ok {
		m.settings[propertyID] = defaultSettings(propertyID)
	}
	return nil
}

func (m *MemoryStore) GetPolicy(_ context.Context, propertyID uuid.UUID) (domain.ApprovalPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.policies[propertyID]
	if !ok {
		p = defaultPolicy(propertyID)
		m.policies[propertyID] = p
	}
	return p, nil
}

func (m *MemoryStore) SavePolicy(_ context.Context, p domain.ApprovalPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies[p.PropertyID] = p
	return nil
}

func (m *MemoryStore) GetSettings(_ context.Context, propertyID uuid.UUID) (domain.PropertyFinanceSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.settings[propertyID]
	if !ok {
		s = defaultSettings(propertyID)
		m.settings[propertyID] = s
	}
	return s, nil
}

func (m *MemoryStore) SaveSettings(_ context.Context, s domain.PropertyFinanceSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[s.PropertyID] = s
	return nil
}

func (m *MemoryStore) SaveUnifiedSettings(_ context.Context, propertyID uuid.UUID, settings *domain.PropertyFinanceSettings, policy *domain.ApprovalPolicy, loyalty *domain.PropertyGamificationSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if settings != nil {
		settings.PropertyID = propertyID
		m.settings[propertyID] = *settings
	}
	if policy != nil {
		policy.PropertyID = propertyID
		m.policies[propertyID] = *policy
	}
	if loyalty != nil {
		loyalty.PropertyID = propertyID
		if m.loyalty == nil {
			m.loyalty = map[uuid.UUID]domain.PropertyGamificationSettings{}
		}
		m.loyalty[propertyID] = *loyalty
	}
	return nil
}

func (m *MemoryStore) InsertCapital(_ context.Context, tx *domain.CapitalTransaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.claim(tx.PropertyID.String() + ":cap:" + tx.IdempotencyKey); err != nil {
		return err
	}
	if tx.ID == uuid.Nil {
		tx.ID = uuid.New()
	}
	m.capital = append(m.capital, *tx)
	return nil
}

func (m *MemoryStore) ListCapital(_ context.Context, propertyID uuid.UUID) ([]domain.CapitalTransaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.CapitalTransaction
	for _, c := range m.capital {
		if c.PropertyID == propertyID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *MemoryStore) CountCapital(_ context.Context, propertyID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.capital {
		if c.PropertyID == propertyID {
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) InsertExpense(_ context.Context, e *domain.Expense) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.claim(e.PropertyID.String() + ":exp:" + e.IdempotencyKey); err != nil {
		return err
	}
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	m.expenses[e.ID] = *e
	return nil
}

func (m *MemoryStore) GetExpense(_ context.Context, id uuid.UUID) (*domain.Expense, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.expenses[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := e
	return &cp, nil
}

func (m *MemoryStore) ListExpenses(_ context.Context, propertyID uuid.UUID) ([]domain.Expense, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Expense
	for _, e := range m.expenses {
		if e.PropertyID == propertyID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryStore) UpdateExpenseStatus(_ context.Context, id uuid.UUID, status domain.ExpenseStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.expenses[id]
	if !ok {
		return ErrNotFound
	}
	e.Status = status
	m.expenses[id] = e
	return nil
}

func (m *MemoryStore) InsertExpensePayment(_ context.Context, p *domain.ExpensePayment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.claim(p.PropertyID.String() + ":pay:" + p.IdempotencyKey); err != nil {
		return err
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	m.payments = append(m.payments, *p)
	return nil
}

func (m *MemoryStore) ListExpensePayments(_ context.Context, expenseID uuid.UUID) ([]domain.ExpensePayment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ExpensePayment
	for _, p := range m.payments {
		if p.ExpenseID == expenseID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *MemoryStore) SumExpensePayments(_ context.Context, expenseID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s int64
	for _, p := range m.payments {
		if p.ExpenseID == expenseID {
			s += p.AmountPaise
		}
	}
	return s, nil
}

func (m *MemoryStore) SumManagerSpend(_ context.Context, propertyID, managerID uuid.UUID, from, to time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s int64
	for _, p := range m.payments {
		if p.PropertyID == propertyID && p.PayerUserID == managerID && p.PayerRole == domain.PayerManager &&
			!p.OccurredAt.Before(from) && p.OccurredAt.Before(to) {
			s += p.AmountPaise
		}
	}
	return s, nil
}

func (m *MemoryStore) InsertAdvance(_ context.Context, a *domain.ManagerAdvance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	m.advances = append(m.advances, *a)
	return nil
}

func (m *MemoryStore) InsertReimbursement(_ context.Context, r *domain.ManagerReimbursement) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.claim(r.PropertyID.String() + ":reimb:" + r.IdempotencyKey); err != nil {
		return err
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	m.reimburse = append(m.reimburse, *r)
	return nil
}

func (m *MemoryStore) AdvanceOutstanding(_ context.Context, propertyID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s int64
	for _, a := range m.advances {
		if a.PropertyID == propertyID {
			s += a.AmountPaise
		}
	}
	for _, r := range m.reimburse {
		if r.PropertyID == propertyID {
			s -= r.AmountPaise
		}
	}
	return s, nil
}

func (m *MemoryStore) ListAdvances(_ context.Context, propertyID uuid.UUID) ([]domain.ManagerAdvance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ManagerAdvance
	for _, a := range m.advances {
		if a.PropertyID == propertyID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *MemoryStore) InsertJournal(_ context.Context, lines []domain.JournalLine) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]struct{}{}
	for _, l := range m.journal {
		seen[l.SourceType+l.SourceID.String()+l.LineKind] = struct{}{}
	}
	for _, l := range lines {
		k := l.SourceType + l.SourceID.String() + l.LineKind
		if _, ok := seen[k]; ok {
			return ErrDuplicateIdempotency
		}
	}
	m.journal = append(m.journal, lines...)
	return nil
}

func (m *MemoryStore) ListJournal(_ context.Context, propertyID uuid.UUID, from, to time.Time, account string) ([]domain.JournalLine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.JournalLine
	for _, l := range m.journal {
		if l.PropertyID != propertyID {
			continue
		}
		if !from.IsZero() && l.OccurredAt.Before(from) {
			continue
		}
		if !to.IsZero() && !l.OccurredAt.Before(to) {
			continue
		}
		if account != "" && l.AccountCode != account {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func (m *MemoryStore) SumAccount(ctx context.Context, propertyID uuid.UUID, account string, from, to time.Time) (int64, int64, error) {
	lines, err := m.ListJournal(ctx, propertyID, from, to, account)
	if err != nil {
		return 0, 0, err
	}
	var d, c int64
	for _, l := range lines {
		d += l.DebitPaise
		c += l.CreditPaise
	}
	return d, c, nil
}

func (m *MemoryStore) SumAccountNetCredit(ctx context.Context, propertyID uuid.UUID, account string, from, to time.Time) (int64, error) {
	d, c, err := m.SumAccount(ctx, propertyID, account, from, to)
	return c - d, err
}

func (m *MemoryStore) UpsertBudget(_ context.Context, b *domain.Budget) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	m.budgets[b.PropertyID.String()+b.CategoryCode+b.PeriodMonth] = *b
	return nil
}

func (m *MemoryStore) GetBudget(_ context.Context, propertyID uuid.UUID, category, period string) (*domain.Budget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.budgets[propertyID.String()+category+period]
	if !ok {
		return nil, ErrNotFound
	}
	cp := b
	return &cp, nil
}

func (m *MemoryStore) ListBudgets(_ context.Context, propertyID uuid.UUID, period string) ([]domain.Budget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Budget
	for _, b := range m.budgets {
		if b.PropertyID == propertyID && (period == "" || b.PeriodMonth == period) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *MemoryStore) InsertRewardLiability(_ context.Context, t *domain.RewardLiabilityTxn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.rewards {
		if x.SourceType == t.SourceType && x.SourceID == t.SourceID && x.Kind == t.Kind {
			return ErrDuplicateIdempotency
		}
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	m.rewards = append(m.rewards, *t)
	return nil
}

func (m *MemoryStore) SumRewardLiability(_ context.Context, propertyID uuid.UUID, kind string, from, to time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s int64
	for _, t := range m.rewards {
		if t.PropertyID != propertyID {
			continue
		}
		if kind != "" && t.Kind != kind {
			continue
		}
		if !from.IsZero() && t.OccurredAt.Before(from) {
			continue
		}
		if !to.IsZero() && !t.OccurredAt.Before(to) {
			continue
		}
		s += t.AmountPaise
	}
	return s, nil
}

func (m *MemoryStore) SumRewardPointsIssued(_ context.Context, propertyID uuid.UUID, from, to time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s int
	for _, t := range m.rewards {
		if t.PropertyID == propertyID && t.Kind == "issued" &&
			(from.IsZero() || !t.OccurredAt.Before(from)) &&
			(to.IsZero() || t.OccurredAt.Before(to)) {
			s += t.Points
		}
	}
	return s, nil
}

func (m *MemoryStore) GetTieOut(_ context.Context, propertyID uuid.UUID, period string) (*domain.PeriodTieOut, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tieouts[propertyID.String()+period]
	if !ok {
		return nil, ErrNotFound
	}
	cp := t
	return &cp, nil
}

func (m *MemoryStore) SaveTieOut(_ context.Context, t *domain.PeriodTieOut) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	m.tieouts[t.PropertyID.String()+t.PeriodMonth] = *t
	return nil
}

func (m *MemoryStore) ListTieOuts(_ context.Context, propertyID uuid.UUID, limit int) ([]domain.PeriodTieOut, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.PeriodTieOut
	for _, t := range m.tieouts {
		if t.PropertyID == propertyID {
			out = append(out, t)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) InsertApproval(_ context.Context, a *domain.ApprovalRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	m.approvals[a.ID] = *a
	return nil
}

func (m *MemoryStore) GetApproval(_ context.Context, id uuid.UUID) (*domain.ApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := a
	return &cp, nil
}

func (m *MemoryStore) ListApprovals(_ context.Context, propertyID uuid.UUID, status string) ([]domain.ApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ApprovalRequest
	for _, a := range m.approvals {
		if a.PropertyID == propertyID && (status == "" || a.Status == status) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *MemoryStore) UpdateApproval(_ context.Context, a *domain.ApprovalRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approvals[a.ID] = *a
	return nil
}

func (m *MemoryStore) InsertKPI(_ context.Context, s *domain.KPISnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kpis = append(m.kpis, *s)
	return nil
}

func (m *MemoryStore) InsertROI(_ context.Context, s *domain.ROISnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rois = append(m.rois, *s)
	return nil
}

func (m *MemoryStore) LatestROI(_ context.Context, propertyID uuid.UUID) (*domain.ROISnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *domain.ROISnapshot
	for i := range m.rois {
		if m.rois[i].PropertyID != propertyID {
			continue
		}
		if best == nil || m.rois[i].SnapshotDate.After(best.SnapshotDate) {
			cp := m.rois[i]
			best = &cp
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

func (m *MemoryStore) InsertLeakage(_ context.Context, e *domain.LeakageEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.Evidence == nil {
		e.Evidence = json.RawMessage(`{}`)
	}
	m.leakage[e.ID] = *e
	return nil
}

func (m *MemoryStore) ListLeakage(_ context.Context, propertyID uuid.UUID) ([]domain.LeakageEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.LeakageEvent
	for _, e := range m.leakage {
		if e.PropertyID == propertyID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *MemoryStore) GetLeakage(_ context.Context, id uuid.UUID) (*domain.LeakageEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.leakage[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := e
	return &cp, nil
}

func (m *MemoryStore) InsertRecommendation(_ context.Context, r *domain.Recommendation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	m.recs[r.ID] = *r
	return nil
}

func (m *MemoryStore) ListRecommendations(_ context.Context, propertyID uuid.UUID) ([]domain.Recommendation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Recommendation
	for _, r := range m.recs {
		if r.PropertyID == propertyID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *MemoryStore) GetRecommendation(_ context.Context, id uuid.UUID) (*domain.Recommendation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := r
	return &cp, nil
}

func (m *MemoryStore) UpdateRecommendation(_ context.Context, r *domain.Recommendation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[r.ID] = *r
	return nil
}

func (m *MemoryStore) InsertForecast(_ context.Context, f *domain.ForecastSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forecasts = append(m.forecasts, *f)
	return nil
}

func (m *MemoryStore) LatestForecast(_ context.Context, propertyID uuid.UUID, horizon int) (*domain.ForecastSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *domain.ForecastSnapshot
	for i := range m.forecasts {
		if m.forecasts[i].PropertyID != propertyID || m.forecasts[i].HorizonDays != horizon {
			continue
		}
		if best == nil || m.forecasts[i].AsOf.After(best.AsOf) {
			cp := m.forecasts[i]
			best = &cp
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

func (m *MemoryStore) UpsertImportSuggestion(_ context.Context, s *domain.ExpenseImportSuggestion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	m.imports[s.PropertyID.String()+s.TxnID] = *s
	return nil
}

func (m *MemoryStore) ListImportSuggestions(_ context.Context, propertyID uuid.UUID) ([]domain.ExpenseImportSuggestion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ExpenseImportSuggestion
	for _, s := range m.imports {
		if s.PropertyID == propertyID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MemoryStore) UpsertMealPrep(_ context.Context, meal *domain.MealPrepActual) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prep = append(m.prep, *meal)
	return nil
}

func (m *MemoryStore) GetMealPrep(_ context.Context, propertyID uuid.UUID, date time.Time) ([]domain.MealPrepActual, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := date.Format("2006-01-02")
	var out []domain.MealPrepActual
	for _, p := range m.prep {
		if p.PropertyID == propertyID && p.MealDate.Format("2006-01-02") == d {
			out = append(out, p)
		}
	}
	return out, nil
}

var _ Store = (*MemoryStore)(nil)
