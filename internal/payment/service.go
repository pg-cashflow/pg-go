package payment

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

var (
	ErrCashPartialNotAllowed = errors.New("payment: cash must equal remaining due amount (D2)")
	ErrDuplicateTxn          = errors.New("payment: duplicate upi txn id")
	ErrDueNotOpen            = errors.New("payment: due is not open for payment")
	ErrNoDepositDue          = errors.New("payment: no deposit due for tenant")
)

// txFn runs work with optionally transactional repos.
type txFn func(ctx context.Context, fn func(dues DueRepository, payments PaymentRepository, tenants TenantRepository, pub events.Publisher) error) error

// Service matches and records payments, settles deposits, and builds summaries.
type Service struct {
	dues      DueRepository
	payments  PaymentRepository
	tenants   TenantRepository
	summaries SummaryRepository
	matcher   *Matcher
	pub       events.Publisher
	runInTx   txFn
	now       func() time.Time
}

func NewService(
	dues DueRepository,
	payments PaymentRepository,
	tenants TenantRepository,
	summaries SummaryRepository,
	pub events.Publisher,
) *Service {
	s := &Service{
		dues:      dues,
		payments:  payments,
		tenants:   tenants,
		summaries: summaries,
		matcher:   NewMatcher(dues),
		pub:       pub,
		now:       func() time.Time { return time.Now().UTC() },
	}
	s.runInTx = func(ctx context.Context, fn func(DueRepository, PaymentRepository, TenantRepository, events.Publisher) error) error {
		return fn(s.dues, s.payments, s.tenants, s.pub)
	}
	return s
}

// NewServiceWithPool wires transactional settle paths using postgres repos.
func NewServiceWithPool(
	pool *pgxpool.Pool,
	dues *postgres.DueRepo,
	payments *postgres.PaymentRepo,
	tenants *postgres.TenantRepo,
	eventRepo *postgres.EventRepo,
	summaries SummaryRepository,
) *Service {
	pub := events.NewPostgresPublisher(eventRepo)
	s := NewService(dues, payments, tenants, summaries, pub)
	s.runInTx = func(ctx context.Context, fn func(DueRepository, PaymentRepository, TenantRepository, events.Publisher) error) error {
		return postgres.WithinTx(ctx, pool, func(tx pgx.Tx) error {
			return fn(dues.WithTx(tx), payments.WithTx(tx), tenants.WithTx(tx), events.NewPostgresPublisher(eventRepo.WithTx(tx)))
		})
	}
	return s
}

// MatchPayment matches a CSV bank row to a due and records the payment.
func (s *Service) MatchPayment(ctx context.Context, propertyID uuid.UUID, txnID string, amountPaise int, txnDate time.Time, note string) (*domain.Payment, error) {
	if txnID != "" {
		if existing, err := s.payments.GetByUPITxnID(ctx, txnID); err == nil && existing != nil {
			return nil, ErrDuplicateTxn
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	res, err := s.matcher.Match(ctx, propertyID, amountPaise, txnDate, note)
	if err != nil {
		_ = s.publishMatchFailed(ctx, propertyID, txnID, amountPaise, note, err)
		return nil, err
	}

	var txnPtr *string
	if txnID != "" {
		txnPtr = &txnID
	}
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	return s.settleMatched(ctx, res.Due.ID, amountPaise, res.MatchedBy, txnPtr, nil, notePtr)
}

// ManualMatch records an owner-confirmed match.
func (s *Service) ManualMatch(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, recordedBy uuid.UUID) (*domain.Payment, error) {
	due, err := s.dues.GetByID(ctx, dueID)
	if err != nil {
		return nil, err
	}
	if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
		return nil, ErrDueNotOpen
	}
	if txnID != "" {
		if existing, err := s.payments.GetByUPITxnID(ctx, txnID); err == nil && existing != nil {
			return nil, ErrDuplicateTxn
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	var txnPtr *string
	if txnID != "" {
		txnPtr = &txnID
	}
	return s.settleMatched(ctx, dueID, amountPaise, domain.MatchedByManual, txnPtr, &recordedBy, nil)
}

// MarkCashPaid records a full cash settlement of the remaining due amount.
// D2 LOCKED: amount MUST equal due.Amount (current remaining). No cash partials.
func (s *Service) MarkCashPaid(ctx context.Context, dueID uuid.UUID, amountPaise int, recordedBy uuid.UUID, note string) (*domain.Payment, error) {
	var notePtr *string
	if note != "" {
		notePtr = &note
	}

	var out *domain.Payment
	err := s.runInTx(ctx, func(dues DueRepository, payments PaymentRepository, tenants TenantRepository, pub events.Publisher) error {
		due, err := getDueForUpdate(ctx, dues, dueID)
		if err != nil {
			return err
		}
		if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
			return ErrDueNotOpen
		}
		if amountPaise != due.Amount {
			return ErrCashPartialNotAllowed
		}

		at := s.now()
		credit := due.ApplyPayment(amountPaise, at)
		if err := dues.Update(ctx, due); err != nil {
			return err
		}

		p := &domain.Payment{
			DueID:      due.ID,
			TenantID:   due.TenantID,
			Amount:     amountPaise,
			MatchedBy:  domain.MatchedByCash,
			RecordedBy: &recordedBy,
			MatchedAt:  at,
			RawNote:    notePtr,
		}
		if err := payments.Create(ctx, p); err != nil {
			return err
		}
		if credit > 0 {
			if err := addTenantCredit(ctx, tenants, due.TenantID, credit); err != nil {
				return err
			}
		}

		cashPayload, _ := json.Marshal(domain.CashPaymentRecordedPayload{
			DueID:       due.ID.String(),
			DueCode:     due.DueCode,
			AmountPaise: int64(amountPaise),
			RecordedBy:  recordedBy.String(),
			Note:        note,
		})
		did := due.ID
		if err := pub.Publish(ctx, domain.Event{
			TenantID:   domain.Ptr(due.TenantID),
			PropertyID: due.PropertyID,
			EventType:  domain.EvtCashPaymentRecorded,
			DueID:      &did,
			OccurredAt: at,
			Payload:    cashPayload,
		}); err != nil {
			return err
		}
		if due.Status == domain.DueStatusPaid {
			if err := publishDuePaid(ctx, pub, due, at, string(domain.MatchedByCash)); err != nil {
				return err
			}
		}
		out = p
		return nil
	})
	return out, err
}

// SettleDeposit records a deposit refund decision (owner moves money manually).
func (s *Service) SettleDeposit(ctx context.Context, tenantID uuid.UUID, refundedPaise int64, reason string) error {
	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return err
	}
	deposit, err := s.findDepositDue(ctx, tenantID)
	if err != nil {
		return err
	}
	at := s.now()
	noticeDays, policyMet := billing.NoticeCompliance(tenant, at)
	payload, _ := json.Marshal(domain.DepositSettledPayload{
		DepositDueID:    deposit.ID.String(),
		OriginalPaise:   int64(deposit.OriginalAmount),
		RefundedPaise:   refundedPaise,
		NoticeDaysGiven: noticeDays,
		PolicyMet:       policyMet,
		Reason:          reason,
	})
	did := deposit.ID
	return s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(tenant.ID),
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtDepositSettled,
		DueID:      &did,
		OccurredAt: at,
		Payload:    payload,
	})
}

func (s *Service) settleMatched(
	ctx context.Context,
	dueID uuid.UUID,
	amountPaise int,
	matchedBy domain.MatchedBy,
	txnID *string,
	recordedBy *uuid.UUID,
	rawNote *string,
) (*domain.Payment, error) {
	var out *domain.Payment
	err := s.runInTx(ctx, func(dues DueRepository, payments PaymentRepository, tenants TenantRepository, pub events.Publisher) error {
		due, err := getDueForUpdate(ctx, dues, dueID)
		if err != nil {
			return err
		}
		if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
			return ErrDueNotOpen
		}
		at := s.now()
		credit := due.ApplyPayment(amountPaise, at)
		if err := dues.Update(ctx, due); err != nil {
			return err
		}

		p := &domain.Payment{
			DueID:      due.ID,
			TenantID:   due.TenantID,
			UPITxnID:   txnID,
			Amount:     amountPaise,
			MatchedBy:  matchedBy,
			RecordedBy: recordedBy,
			MatchedAt:  at,
			RawNote:    rawNote,
		}
		if err := payments.Create(ctx, p); err != nil {
			return err
		}
		if credit > 0 {
			if err := addTenantCredit(ctx, tenants, due.TenantID, credit); err != nil {
				return err
			}
		}

		matchPayload, _ := json.Marshal(domain.PaymentMatchedPayload{
			DueID:       due.ID.String(),
			PaymentID:   p.ID.String(),
			MatchedBy:   string(matchedBy),
			AmountPaise: int64(amountPaise),
		})
		did := due.ID
		if err := pub.Publish(ctx, domain.Event{
			TenantID:   domain.Ptr(due.TenantID),
			PropertyID: due.PropertyID,
			EventType:  domain.EvtPaymentMatched,
			DueID:      &did,
			OccurredAt: at,
			Payload:    matchPayload,
		}); err != nil {
			return err
		}
		if due.Status == domain.DueStatusPaid {
			if err := publishDuePaid(ctx, pub, due, at, string(matchedBy)); err != nil {
				return err
			}
		}
		out = p
		return nil
	})
	return out, err
}

func getDueForUpdate(ctx context.Context, dues DueRepository, id uuid.UUID) (*domain.Due, error) {
	if locker, ok := dues.(interface {
		GetByIDForUpdate(context.Context, uuid.UUID) (*domain.Due, error)
	}); ok {
		return locker.GetByIDForUpdate(ctx, id)
	}
	return dues.GetByID(ctx, id)
}

func addTenantCredit(ctx context.Context, tenants TenantRepository, tenantID uuid.UUID, creditPaise int) error {
	tenant, err := tenants.GetByID(ctx, tenantID)
	if err != nil {
		return err
	}
	tenant.CreditBalancePaise += creditPaise
	return tenants.Update(ctx, tenant)
}

func (s *Service) findDepositDue(ctx context.Context, tenantID uuid.UUID) (*domain.Due, error) {
	list, err := s.dues.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var best *domain.Due
	for i := range list {
		d := &list[i]
		if d.Kind != domain.DueKindDeposit {
			continue
		}
		if best == nil || d.CreatedAt.After(best.CreatedAt) {
			best = d
		}
	}
	if best == nil {
		return nil, ErrNoDepositDue
	}
	return best, nil
}

func (s *Service) publishMatchFailed(ctx context.Context, propertyID uuid.UUID, txnID string, amountPaise int, note string, matchErr error) error {
	payload, _ := json.Marshal(map[string]any{
		"upi_txn_id":   txnID,
		"amount_paise": amountPaise,
		"note":         note,
		"error":        matchErr.Error(),
	})
	return s.pub.Publish(ctx, domain.Event{
		TenantID:   nil,
		PropertyID: propertyID,
		EventType:  domain.EvtPaymentMatchFailed,
		OccurredAt: s.now(),
		Payload:    payload,
	})
}

func publishDuePaid(ctx context.Context, pub events.Publisher, due *domain.Due, paidAt time.Time, matchedBy string) error {
	days := daysEarlyOrLate(due.DueDate, paidAt)
	evt := domain.EvtDuePaidOnTime
	if days > 0 {
		evt = domain.EvtDuePaidLate
	}
	payload, _ := json.Marshal(domain.DuePaidPayload{
		DueID:           due.ID.String(),
		DueCode:         due.DueCode,
		AmountPaise:     int64(due.OriginalAmount),
		PaidAt:          paidAt.UTC().Format(time.RFC3339),
		DaysEarlyOrLate: days,
		MatchedBy:       matchedBy,
	})
	did := due.ID
	return pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(due.TenantID),
		PropertyID: due.PropertyID,
		EventType:  evt,
		DueID:      &did,
		OccurredAt: paidAt,
		Payload:    payload,
	})
}

func daysEarlyOrLate(dueDate, paidAt time.Time) int {
	d := dateOnly(dueDate)
	p := dateOnly(paidAt)
	return int(p.Sub(d).Hours() / 24)
}
