package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/timeutil"
)

// EventPublisher publishes domain events for audit and notifications.
type EventPublisher interface {
	Publish(ctx context.Context, e domain.Event) error
}

type Service struct {
	Store Store
	Pub   EventPublisher
	Now   func() time.Time
}

func NewService(store Store, pub EventPublisher) *Service {
	return &Service{
		Store: store,
		Pub:   pub,
		Now:   timeutil.System.Now,
	}
}

func (s *Service) publish(ctx context.Context, propertyID uuid.UUID, typ domain.EventType, payload any) {
	if s.Pub == nil {
		return
	}
	b, _ := json.Marshal(payload)
	_ = s.Pub.Publish(ctx, domain.Event{
		PropertyID: propertyID,
		EventType:  typ,
		OccurredAt: s.Now(),
		Payload:    b,
	})
}

func (s *Service) AddCapital(ctx context.Context, propertyID, ownerID uuid.UUID, kind domain.CapitalKind, amount int64, purpose, idem string) (*domain.CapitalTransaction, error) {
	if idem == "" {
		return nil, ErrIdempotencyRequired
	}
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	switch kind {
	case domain.CapitalInitial, domain.CapitalAdditional, domain.CapitalWithdrawal:
	default:
		return nil, ErrInvalidKind
	}
	_ = s.Store.EnsureDefaults(ctx, propertyID)
	n, err := s.Store.CountCapital(ctx, propertyID)
	if err != nil {
		return nil, err
	}
	at := s.Now()
	tx := &domain.CapitalTransaction{
		ID:             uuid.New(),
		PropertyID:     propertyID,
		OwnerUserID:    ownerID,
		Kind:           kind,
		AmountPaise:    amount,
		Purpose:        purpose,
		Reference:      nextCapitalRef(n),
		IdempotencyKey: idem,
		OccurredAt:     at,
		CreatedAt:      at,
	}
	cashAcct := domain.AcctBank
	var specs []LineSpec
	if kind == domain.CapitalWithdrawal {
		specs = []LineSpec{
			{Account: domain.AcctOwnerCapital, Debit: amount, LineKind: "capital_out"},
			{Account: cashAcct, Credit: amount, LineKind: "cash_out"},
		}
	} else {
		specs = []LineSpec{
			{Account: cashAcct, Debit: amount, LineKind: "cash_in"},
			{Account: domain.AcctOwnerCapital, Credit: amount, LineKind: "capital_in"},
		}
	}
	lines, err := MakeLines(propertyID, tx.ID, "capital", at, specs)
	if err != nil {
		return nil, err
	}
	if err := s.Store.InsertCapitalAtomic(ctx, tx, lines); err != nil {
		return nil, err
	}
	if kind == domain.CapitalWithdrawal {
		s.publish(ctx, propertyID, domain.EvtCapitalWithdrawn, tx)
	} else {
		s.publish(ctx, propertyID, domain.EvtCapitalAdded, tx)
	}
	return tx, nil
}

type CreateExpenseInput struct {
	PropertyID     uuid.UUID
	ActorID        uuid.UUID
	ActorRole      string
	CategoryCode   string
	VendorName     string
	Description    string
	AmountPaise    int64
	Emergency      bool
	IsRecurring    bool
	RoomID         *uuid.UUID
	IdempotencyKey string
	OccurredAt     time.Time
}

func (s *Service) CreateExpense(ctx context.Context, in CreateExpenseInput) (*domain.Expense, *domain.ApprovalRequest, error) {
	if in.IdempotencyKey == "" {
		return nil, nil, ErrIdempotencyRequired
	}
	if in.AmountPaise <= 0 {
		return nil, nil, ErrInvalidAmount
	}
	if in.CategoryCode == "" {
		in.CategoryCode = "vendor"
	}
	_ = s.Store.EnsureDefaults(ctx, in.PropertyID)
	at := in.OccurredAt
	if at.IsZero() {
		at = s.Now()
	}
	e := &domain.Expense{
		ID:             uuid.New(),
		PropertyID:     in.PropertyID,
		CategoryCode:   in.CategoryCode,
		VendorName:     in.VendorName,
		Description:    in.Description,
		AmountPaise:    in.AmountPaise,
		Status:         domain.ExpenseApproved,
		Emergency:      in.Emergency,
		IsRecurring:    in.IsRecurring,
		RoomID:         in.RoomID,
		CreatedBy:      in.ActorID,
		CreatedByRole:  in.ActorRole,
		IdempotencyKey: in.IdempotencyKey,
		OccurredAt:     at,
		CreatedAt:      at,
	}

	lines, err := MakeLines(e.PropertyID, e.ID, "expense", e.OccurredAt, []LineSpec{
		{Account: domain.AcctOperatingExpense, Debit: e.AmountPaise, LineKind: "expense_dr"},
		{Account: domain.AcctAccountsPayable, Credit: e.AmountPaise, LineKind: "payable_cr"},
	})
	if err != nil {
		return nil, nil, err
	}

	approval := &domain.ApprovalRequest{
		ID:          uuid.New(),
		PropertyID:  in.PropertyID,
		Kind:        "expense",
		SubjectID:   e.ID,
		AmountPaise: e.AmountPaise,
		RequestedBy: in.ActorID,
		Status:      "pending",
		CreatedAt:   at,
	}

	if err := s.Store.InsertExpenseAtomic(ctx, e, lines, approval); err != nil {
		return nil, nil, err
	}
	if e.Status == domain.ExpenseApproved {
		approval = nil
	}
	s.publish(ctx, in.PropertyID, domain.EvtExpenseCreated, e)
	if in.Emergency {
		s.publish(ctx, in.PropertyID, domain.EvtExpenseApproved, map[string]any{"expense_id": e.ID, "emergency": true})
	}
	return e, approval, nil
}

func (s *Service) postExpenseAccrual(ctx context.Context, e *domain.Expense) error {
	lines, err := MakeLines(e.PropertyID, e.ID, "expense", e.OccurredAt, []LineSpec{
		{Account: domain.AcctOperatingExpense, Debit: e.AmountPaise, LineKind: "expense_dr"},
		{Account: domain.AcctAccountsPayable, Credit: e.AmountPaise, LineKind: "payable_cr"},
	})
	if err != nil {
		return err
	}
	return s.Store.InsertJournal(ctx, lines)
}

type VoidExpenseInput struct {
	PropertyID uuid.UUID
	ExpenseID  uuid.UUID
	ActorID    uuid.UUID
	ActorRole  domain.PayerRole
	Reason     string
}

const (
	voidMaxAttempts      = 3
	minVoidReasonRunes   = 3
	maxVoidReasonRunes   = 500
	maxExpenseBackdate   = 90 * 24 * time.Hour
	maxExpenseFutureSkew = 5 * time.Minute
)

// ValidateExpenseDate rejects an expense date that is in the future or older than 90 days.
// Closed accounting periods stay protected separately by the ledger period-lock trigger.
func ValidateExpenseDate(now, at time.Time) error {
	if at.After(now.Add(maxExpenseFutureSkew)) || at.Before(now.Add(-maxExpenseBackdate)) {
		return ErrDateOutOfRange
	}
	return nil
}

// VoidExpense cancels an expense that is pending approval or approved and not yet paid.
// Rules:
//   - A reason of 3 to 500 characters is required and stored for audit.
//   - A non-owner can void only an expense the same user created.
//   - A non-owner cannot void an approved expense above the owner-approval threshold.
//   - The store checks the expense status under a row lock. If the status changed since
//     the read, the service re-reads and tries again (at most voidMaxAttempts).
func (s *Service) VoidExpense(ctx context.Context, in VoidExpenseInput) (*domain.Expense, error) {
	reason := strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(reason); n < minVoidReasonRunes || n > maxVoidReasonRunes {
		return nil, ErrReasonRequired
	}
	for attempt := 0; attempt < voidMaxAttempts; attempt++ {
		e, err := s.voidExpenseOnce(ctx, in, reason)
		if errors.Is(err, ErrExpenseStateChanged) {
			continue
		}
		return e, err
	}
	return nil, ErrExpenseStateChanged
}

func (s *Service) voidExpenseOnce(ctx context.Context, in VoidExpenseInput, reason string) (*domain.Expense, error) {
	e, err := s.Store.GetExpense(ctx, in.ExpenseID)
	if err != nil {
		return nil, err
	}
	if e.PropertyID != in.PropertyID {
		return nil, ErrForbidden
	}
	if e.Status != domain.ExpensePendingApproval && e.Status != domain.ExpenseApproved {
		return nil, fmt.Errorf("%w: status is %s", ErrExpenseNotVoidable, e.Status)
	}

	if in.ActorRole != domain.PayerOwner {
		if e.CreatedBy != in.ActorID {
			return nil, ErrForbidden
		}
		if e.Status == domain.ExpenseApproved {
			pol, err := s.Store.GetPolicy(ctx, e.PropertyID)
			if err != nil {
				return nil, err
			}
			if e.AmountPaise > pol.OwnerApprovalThresholdPaise {
				return nil, ErrApprovalRequired
			}
		}
	}

	at := s.Now()
	var lines []domain.JournalLine
	if e.Status == domain.ExpenseApproved {
		lines, err = MakeLines(e.PropertyID, e.ID, "expense_void", at, []LineSpec{
			{Account: domain.AcctAccountsPayable, Debit: e.AmountPaise, LineKind: "void_payable_dr"},
			{Account: domain.AcctOperatingExpense, Credit: e.AmountPaise, LineKind: "void_expense_cr"},
		})
		if err != nil {
			return nil, err
		}
	}

	v := domain.ExpenseVoid{Reason: reason, VoidedBy: in.ActorID, VoidedAt: at}
	if err := s.Store.VoidExpenseAtomic(ctx, e.ID, e.Status, lines, v); err != nil {
		return nil, err
	}

	e.Status = domain.ExpenseCancelled
	s.publish(ctx, in.PropertyID, domain.EvtExpenseVoided, map[string]any{
		"expense_id": e.ID,
		"voided_by":  in.ActorID,
		"reason":     reason,
	})
	return e, nil
}

type PayExpenseInput struct {
	ExpenseID      uuid.UUID
	PropertyID     uuid.UUID
	ActorID        uuid.UUID
	ActorRole      domain.PayerRole
	AmountPaise    int64
	Method         string
	IdempotencyKey string
}

func (s *Service) PayExpense(ctx context.Context, in PayExpenseInput) (*domain.ExpensePayment, error) {
	if in.IdempotencyKey == "" {
		return nil, ErrIdempotencyRequired
	}
	if in.AmountPaise <= 0 {
		return nil, ErrInvalidAmount
	}
	if in.Method == "" {
		in.Method = "cash"
	}
	at := s.Now()
	if in.ActorRole == domain.PayerManager {
		pol, err := s.Store.GetPolicy(ctx, in.PropertyID)
		if err != nil {
			return nil, err
		}
		dayFrom := dayStart(at)
		monthFrom, monthTo, err := PeriodBounds(at.Format("2006-01"))
		if err != nil {
			return nil, err
		}
		daily, err := s.Store.SumManagerSpend(ctx, in.PropertyID, in.ActorID, dayFrom, dayFrom.AddDate(0, 0, 1))
		if err != nil {
			return nil, err
		}
		monthly, err := s.Store.SumManagerSpend(ctx, in.PropertyID, in.ActorID, monthFrom, monthTo)
		if err != nil {
			return nil, err
		}
		chk := evaluateManagerSpend(pol, in.AmountPaise, daily, monthly, false)
		if chk.Reject != nil {
			return nil, chk.Reject
		}
	}
	p := &domain.ExpensePayment{
		ID:             uuid.New(),
		ExpenseID:      in.ExpenseID,
		PropertyID:     in.PropertyID,
		AmountPaise:    in.AmountPaise,
		PayerRole:      in.ActorRole,
		PayerUserID:    in.ActorID,
		Method:         in.Method,
		IdempotencyKey: in.IdempotencyKey,
		OccurredAt:     at,
	}
	acct := domain.AcctCash
	if in.Method == "bank" || in.Method == "upi" {
		acct = domain.AcctBank
	}
	var specs []LineSpec
	var adv *domain.ManagerAdvance
	if in.ActorRole == domain.PayerManager {
		specs = []LineSpec{
			{Account: domain.AcctAccountsPayable, Debit: in.AmountPaise, LineKind: "payable_clear"},
			{Account: domain.AcctManagerAdvancePayable, Credit: in.AmountPaise, LineKind: "advance_cr"},
		}
		adv = &domain.ManagerAdvance{
			ID:               uuid.New(),
			PropertyID:       in.PropertyID,
			ManagerUserID:    in.ActorID,
			ExpensePaymentID: p.ID,
			AmountPaise:      in.AmountPaise,
			OccurredAt:       at,
		}
	} else {
		specs = []LineSpec{
			{Account: domain.AcctAccountsPayable, Debit: in.AmountPaise, LineKind: "payable_clear"},
			{Account: acct, Credit: in.AmountPaise, LineKind: "cash_out"},
		}
	}
	lines, err := MakeLines(in.PropertyID, p.ID, "expense_payment", at, specs)
	if err != nil {
		return nil, err
	}

	exp, err := s.Store.RecordExpensePaymentAtomic(ctx, p, lines, adv)
	if err != nil {
		return nil, err
	}

	if in.ActorRole == domain.PayerManager && adv != nil {
		s.publish(ctx, exp.PropertyID, domain.EvtManagerAdvanceCreated, adv)
	}
	if exp.Status == domain.ExpensePaid {
		s.publish(ctx, exp.PropertyID, domain.EvtExpensePaid, p)
	}
	return p, nil
}

func (s *Service) ReimburseManager(ctx context.Context, propertyID, ownerID, managerID uuid.UUID, amount int64, idem string) (*domain.ManagerReimbursement, error) {
	if idem == "" {
		return nil, ErrIdempotencyRequired
	}
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	outstanding, err := s.Store.ManagerAdvanceOutstanding(ctx, propertyID, managerID)
	if err != nil {
		return nil, err
	}
	if amount > outstanding {
		return nil, ErrOverpay
	}
	at := s.Now()
	r := &domain.ManagerReimbursement{
		ID:             uuid.New(),
		PropertyID:     propertyID,
		ManagerUserID:  managerID,
		AmountPaise:    amount,
		RecordedBy:     ownerID,
		IdempotencyKey: idem,
		OccurredAt:     at,
	}
	lines, err := MakeLines(propertyID, r.ID, "reimbursement", at, []LineSpec{
		{Account: domain.AcctManagerAdvancePayable, Debit: amount, LineKind: "advance_clear"},
		{Account: domain.AcctBank, Credit: amount, LineKind: "cash_out"},
	})
	if err != nil {
		return nil, err
	}
	if err := s.Store.InsertReimbursementAtomic(ctx, r, lines); err != nil {
		return nil, err
	}
	s.publish(ctx, propertyID, domain.EvtManagerAdvanceReimbursed, r)
	return r, nil
}

func (s *Service) DecideApproval(ctx context.Context, propertyID, ownerID, approvalID uuid.UUID, approve bool, note string) error {
	a, err := s.Store.GetApproval(ctx, approvalID)
	if err != nil {
		return err
	}
	if a.PropertyID != propertyID || a.Status != "pending" {
		return ErrForbidden
	}
	now := s.Now()
	a.DecidedBy = &ownerID
	a.DecidedAt = &now
	a.Note = note

	var expenseStatus *domain.ExpenseStatus
	var lines []domain.JournalLine
	if approve {
		a.Status = "approved"
		if a.Kind == "expense" {
			st := domain.ExpenseApproved
			expenseStatus = &st
			e, err := s.Store.GetExpense(ctx, a.SubjectID)
			if err != nil {
				return err
			}
			lines, err = MakeLines(e.PropertyID, e.ID, "expense", e.OccurredAt, []LineSpec{
				{Account: domain.AcctOperatingExpense, Debit: e.AmountPaise, LineKind: "expense_dr"},
				{Account: domain.AcctAccountsPayable, Credit: e.AmountPaise, LineKind: "payable_cr"},
			})
			if err != nil {
				return err
			}
		}
	} else {
		a.Status = "rejected"
		if a.Kind == "expense" {
			st := domain.ExpenseCancelled
			expenseStatus = &st
		}
	}
	if err := s.Store.DecideApprovalAtomic(ctx, a, expenseStatus, lines); err != nil {
		return err
	}
	if approve && a.Kind == "expense" {
		s.publish(ctx, propertyID, domain.EvtExpenseApproved, a)
	} else if !approve {
		s.publish(ctx, propertyID, domain.EvtExpenseRejected, a)
	}
	return nil
}

func (s *Service) SaveBudget(ctx context.Context, b *domain.Budget) error {
	if b.AmountPaise < 0 {
		return ErrInvalidAmount
	}
	if err := s.Store.UpsertBudget(ctx, b); err != nil {
		return err
	}
	s.publish(ctx, b.PropertyID, domain.EvtBudgetChanged, b)
	return nil
}

func (s *Service) PatchSettings(ctx context.Context, propertyID uuid.UUID, settings *domain.PropertyFinanceSettings, policy *domain.ApprovalPolicy) (*domain.PropertyFinanceSettings, *domain.ApprovalPolicy, error) {
	st, pol, _, err := s.PatchUnifiedSettings(ctx, propertyID, settings, policy, nil)
	return st, pol, err
}

func (s *Service) PatchUnifiedSettings(ctx context.Context, propertyID uuid.UUID, settings *domain.PropertyFinanceSettings, policy *domain.ApprovalPolicy, loyalty *domain.PropertyGamificationSettings) (*domain.PropertyFinanceSettings, *domain.ApprovalPolicy, *domain.PropertyGamificationSettings, error) {
	_ = s.Store.EnsureDefaults(ctx, propertyID)
	curS, err := s.Store.GetSettings(ctx, propertyID)
	if err != nil {
		return nil, nil, nil, err
	}
	curP, err := s.Store.GetPolicy(ctx, propertyID)
	if err != nil {
		return nil, nil, nil, err
	}
	if settings != nil {
		settings.PropertyID = propertyID
		if settings.FiscalMonthStartDay == 0 {
			settings.FiscalMonthStartDay = curS.FiscalMonthStartDay
		}
		curS = *settings
	}
	if policy != nil {
		policy.PropertyID = propertyID
		curP = *policy
	}
	if err := s.Store.SaveUnifiedSettings(ctx, propertyID, &curS, &curP, loyalty); err != nil {
		return nil, nil, nil, err
	}
	evtPayload := map[string]any{"settings": curS, "policy": curP}
	if loyalty != nil {
		evtPayload["loyalty"] = loyalty
	}
	s.publish(ctx, propertyID, domain.EvtFinancePolicyChanged, evtPayload)
	return &curS, &curP, loyalty, nil
}

type OperatingSummary struct {
	PeriodMonth               string `json:"period_month"`
	CollectionsRentPaise      int64  `json:"collections_rent_paise"`
	OperatingRevenuePaise     int64  `json:"operating_revenue_paise"`
	OpexPaise                 int64  `json:"opex_paise"`
	OCFPaise                  int64  `json:"ocf_paise"`
	ManagerAdvanceOutstanding int64  `json:"manager_advance_outstanding_paise"`
	TDRExpensePaise           int64  `json:"tdr_expense_paise"`
	TDRIsEstimated            bool   `json:"tdr_is_estimated"`
	LoyaltyIssuedPaise        int64  `json:"loyalty_issued_paise"`
	LabelCollections          string `json:"label_collections"`
	LabelOperating            string `json:"label_operating"`
}

func (s *Service) OperatingSummary(ctx context.Context, propertyID uuid.UUID, period string, collectionsRent int64) (*OperatingSummary, error) {
	from, to, err := PeriodBounds(period)
	if err != nil {
		return nil, err
	}
	rent, err := s.Store.SumAccountNetCredit(ctx, propertyID, domain.AcctRentRevenue, from, to)
	if err != nil {
		return nil, err
	}
	util, _ := s.Store.SumAccountNetCredit(ctx, propertyID, domain.AcctUtilityRecoveryRevenue, from, to)
	opexDr, _, _ := s.Store.SumAccount(ctx, propertyID, domain.AcctOperatingExpense, from, to)
	tdrDr, _, _ := s.Store.SumAccount(ctx, propertyID, domain.AcctPaymentProcessingExpense, from, to)
	loyaltyDr, _, _ := s.Store.SumAccount(ctx, propertyID, domain.AcctLoyaltyExpense, from, to)
	adv, _ := s.Store.AdvanceOutstanding(ctx, propertyID)
	st, _ := s.Store.GetSettings(ctx, propertyID)
	opex := opexDr + tdrDr + loyaltyDr
	rev := rent + util
	return &OperatingSummary{
		PeriodMonth:               period,
		CollectionsRentPaise:      collectionsRent,
		OperatingRevenuePaise:     rev,
		OpexPaise:                 opex,
		OCFPaise:                  rev - opex,
		ManagerAdvanceOutstanding: adv,
		TDRExpensePaise:           tdrDr,
		TDRIsEstimated:            st.TDRIsEstimated,
		LoyaltyIssuedPaise:        loyaltyDr,
		LabelCollections:          "Collections (reconciliation)",
		LabelOperating:            "Operating view (ledger)",
	}, nil
}

func (s *Service) CapitalTotals(ctx context.Context, propertyID uuid.UUID) (invested, withdrawn int64, err error) {
	list, err := s.Store.ListCapital(ctx, propertyID)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range list {
		if c.Kind == domain.CapitalWithdrawal {
			withdrawn += c.AmountPaise
		} else {
			invested += c.AmountPaise
		}
	}
	return invested, withdrawn, nil
}

func (s *Service) CategoryOpex(ctx context.Context, propertyID uuid.UUID, from, to time.Time) (map[string]int64, error) {
	exps, err := s.Store.ListExpenses(ctx, propertyID)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, e := range exps {
		if e.Status == domain.ExpenseCancelled || e.Status == domain.ExpenseDraft || e.Status == domain.ExpensePendingApproval {
			continue
		}
		if e.OccurredAt.Before(from) || !e.OccurredAt.Before(to) {
			continue
		}
		out[e.CategoryCode] += e.AmountPaise
	}
	return out, nil
}

func fmtINR(p int64) string { return fmt.Sprintf("%d", p) }
