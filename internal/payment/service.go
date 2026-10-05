package payment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	ErrEmptyTxnID            = errors.New("payment: upi txn id is required")
	ErrRequiresConfirmation  = errors.New("payment: heuristic match requires owner confirmation")
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
	onSettle  func(ctx context.Context, p *domain.Payment, due *domain.Due)
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

// SetSettlementHook runs after a successful collection. Hook must be idempotent (journal unique keys).
func (s *Service) SetSettlementHook(fn func(ctx context.Context, p *domain.Payment, due *domain.Due)) {
	s.onSettle = fn
}

func (s *Service) fireSettle(ctx context.Context, p *domain.Payment, dueID uuid.UUID) {
	if s.onSettle == nil || p == nil {
		return
	}
	due, err := s.dues.GetByID(ctx, dueID)
	if err != nil {
		return
	}
	s.onSettle(ctx, p, due)
}

// MatchPayment matches a CSV bank row to a due and records the payment.
// Invariant: ONLY deterministic matches (DueCode) are auto-settled. Heuristic matches (AmountDateWindow)
// return ErrRequiresConfirmation to prevent silent cross-attribution between identical due amounts.
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

	if !res.IsDeterministic {
		_ = s.publishMatchFailed(ctx, propertyID, txnID, amountPaise, note, ErrRequiresConfirmation)
		return nil, ErrRequiresConfirmation
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

// SuggestMatch evaluates a bank statement row and returns candidate match results without mutating or settling the due.
func (s *Service) SuggestMatch(ctx context.Context, propertyID uuid.UUID, amountPaise int, txnDate time.Time, note string) (*MatchResult, error) {
	return s.matcher.Match(ctx, propertyID, amountPaise, txnDate, note)
}

// ManualMatch records an owner-confirmed match with UTR normalization and idempotent replay.
func (s *Service) ManualMatch(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, recordedBy uuid.UUID) (*domain.Payment, error) {
	due, err := s.dues.GetByID(ctx, dueID)
	if err != nil {
		return nil, err
	}
	if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
		return nil, ErrDueNotOpen
	}
	if txnID != "" {
		if norm, nErr := domain.NormalizeUTR(txnID); nErr == nil {
			txnID = norm
		}
		if existing, err := s.payments.GetByUPITxnID(ctx, txnID); err == nil && existing != nil {
			if existing.DueID == dueID && existing.Amount == amountPaise {
				return existing, nil
			}
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

// VerifyPaymentInput defines validated arguments for the atomic payment verification operation.
type VerifyPaymentInput struct {
	PropertyID  uuid.UUID
	DueID       uuid.UUID
	AmountPaise int
	UTR         string
	RecordedBy  uuid.UUID
	Note        string
}

// VerifyPayment implements requirement 4: Atomic, idempotent server-side payment verification.
// 1. Validates the payment and property scope.
// 2. Validates the amount (> 0 integer paise).
// 3. Normalizes and validates the UTR format.
// 4. Enforces UTR uniqueness and idempotent replay for the property.
// 5. Atomically creates payment, updates due, and records statistics within one transaction.
func (s *Service) VerifyPayment(ctx context.Context, in VerifyPaymentInput) (*domain.Payment, error) {
	if in.AmountPaise <= 0 {
		return nil, errors.New("payment: amount must be greater than zero paise")
	}
	normUTR, err := domain.NormalizeUTR(in.UTR)
	if err != nil {
		return nil, err
	}

	due, err := s.dues.GetByID(ctx, in.DueID)
	if err != nil {
		return nil, err
	}
	if in.PropertyID != uuid.Nil && due.PropertyID != in.PropertyID {
		return nil, errors.New("payment: property mismatch")
	}
	if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
		return nil, ErrDueNotOpen
	}

	// Idempotency and UTR uniqueness check
	if existing, err := s.payments.GetByUPITxnID(ctx, normUTR); err == nil && existing != nil {
		if existing.DueID == in.DueID && existing.Amount == in.AmountPaise {
			return existing, nil
		}
		return nil, ErrDuplicateTxn
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var notePtr *string
	if in.Note != "" {
		notePtr = &in.Note
	}
	return s.settleMatched(ctx, in.DueID, in.AmountPaise, domain.MatchedByManual, &normUTR, &in.RecordedBy, notePtr)
}

// CorrectPaymentInput defines inputs to correct a previously verified payment.
type CorrectPaymentInput struct {
	PropertyID      uuid.UUID
	PaymentID       uuid.UUID
	CorrectedAmount int
	CorrectedUTR    string
	Reason          string
	CorrectedBy     uuid.UUID
}

// CorrectPayment implements requirement 16: Immutable audit trail for financial corrections.
// Reversing entry + corrected entry + reason + user + timestamp. Original record is never modified.
func (s *Service) CorrectPayment(ctx context.Context, in CorrectPaymentInput) (*domain.FinancialCorrection, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return nil, errors.New("payment: correction reason is required")
	}
	if in.CorrectedAmount <= 0 {
		return nil, errors.New("payment: corrected amount must be greater than zero")
	}
	normUTR := ""
	if in.CorrectedUTR != "" {
		nu, err := domain.NormalizeUTR(in.CorrectedUTR)
		if err != nil {
			return nil, err
		}
		normUTR = nu
	}

	orig, err := s.payments.GetByID(ctx, in.PaymentID)
	if err != nil {
		return nil, err
	}
	due, err := s.dues.GetByID(ctx, orig.DueID)
	if err != nil {
		return nil, err
	}
	if in.PropertyID != uuid.Nil && due.PropertyID != in.PropertyID {
		return nil, errors.New("payment: property mismatch")
	}

	var correction *domain.FinancialCorrection
	err = s.runInTx(ctx, func(dues DueRepository, payments PaymentRepository, tenants TenantRepository, pub events.Publisher) error {
		at := s.now()
		// 1. Reversing entry
		revNote := fmt.Sprintf("Reversal of payment %s: %s", orig.ID, in.Reason)
		reversal := &domain.Payment{
			PropertyID: &due.PropertyID,
			DueID:      orig.DueID,
			TenantID:   orig.TenantID,
			Amount:     -orig.Amount,
			MatchedBy:  domain.MatchedByManual,
			RecordedBy: &in.CorrectedBy,
			MatchedAt:  at,
			RawNote:    &revNote,
		}
		if err := payments.Create(ctx, reversal); err != nil {
			return fmt.Errorf("create reversal payment: %w", err)
		}

		// 2. Corrected payment entry
		corrNote := fmt.Sprintf("Correction for payment %s: %s", orig.ID, in.Reason)
		var corrUTRPtr *string
		if normUTR != "" {
			corrUTRPtr = &normUTR
		} else {
			corrUTRPtr = orig.UPITxnID
		}
		corrected := &domain.Payment{
			PropertyID: &due.PropertyID,
			DueID:      orig.DueID,
			TenantID:   orig.TenantID,
			UPITxnID:   corrUTRPtr,
			Amount:     in.CorrectedAmount,
			MatchedBy:  domain.MatchedByManual,
			RecordedBy: &in.CorrectedBy,
			MatchedAt:  at,
			RawNote:    &corrNote,
		}
		if err := payments.Create(ctx, corrected); err != nil {
			return fmt.Errorf("create corrected payment: %w", err)
		}

		// 3. Update due amount
		netDiff := in.CorrectedAmount - orig.Amount
		if netDiff != 0 {
			due.Amount -= netDiff
			if due.Amount <= 0 {
				due.Amount = 0
				due.MarkPaid(at)
			} else {
				due.Status = domain.DueStatusPartial
			}
			if err := dues.Update(ctx, due); err != nil {
				return fmt.Errorf("update due: %w", err)
			}
		}

		// 4. Record audit correction entry
		corr := &domain.FinancialCorrection{
			PropertyID:         due.PropertyID,
			OriginalPaymentID:  orig.ID,
			ReversalPaymentID:  &reversal.ID,
			CorrectedPaymentID: &corrected.ID,
			Reason:             in.Reason,
			CorrectedBy:        in.CorrectedBy,
			OccurredAt:         at,
		}
		if recorder, ok := payments.(interface {
			RecordCorrection(ctx context.Context, c *domain.FinancialCorrection) error
		}); ok {
			if err := recorder.RecordCorrection(ctx, corr); err != nil {
				return fmt.Errorf("record financial correction: %w", err)
			}
		}
		correction = corr
		return nil
	})
	return correction, err
}

// GatewaySettle records a payment-gateway capture (Cashfree webhook/poll). Idempotent on upi_txn_id and optional dedupKey.
func (s *Service) GatewaySettle(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, dedupKey ...string) (*domain.Payment, error) {
	if strings.TrimSpace(txnID) == "" {
		return nil, ErrEmptyTxnID
	}
	if existing, err := s.payments.GetByUPITxnID(ctx, txnID); err == nil && existing != nil {
		return existing, nil
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var dedup string
	if len(dedupKey) > 0 {
		dedup = dedupKey[0]
	}
	return s.settleMatched(ctx, dueID, amountPaise, domain.MatchedByCashfree, &txnID, nil, nil, dedup)
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
	if err == nil {
		s.fireSettle(ctx, out, dueID)
	}
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
	dedupKey ...string,
) (*domain.Payment, error) {
	var dedup string
	if len(dedupKey) > 0 {
		dedup = dedupKey[0]
	}
	var out *domain.Payment
	err := s.runInTx(ctx, func(dues DueRepository, payments PaymentRepository, tenants TenantRepository, pub events.Publisher) error {
		if dedup != "" {
			if recorder, ok := payments.(interface {
				RecordWebhookEvent(ctx context.Context, dedupKey, provider, eventType string) (bool, error)
			}); ok {
				firstSeen, err := recorder.RecordWebhookEvent(ctx, dedup, "cashfree", "PAYMENT_SUCCESS_WEBHOOK")
				if err != nil {
					return err
				}
				if !firstSeen {
					return nil
				}
			}
		}
		due, err := getDueForUpdate(ctx, dues, dueID)
		if err != nil {
			return err
		}
		if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
			// If money was captured by Cashfree gateway on an already closed due (race loser vs cash/UTR confirmation),
			// never drop the payment write. Persist payment and credit 100% of the amount to tenant credit balance.
			if matchedBy == domain.MatchedByCashfree {
				at := s.now()
				overpaymentNote := "overpayment: due already closed, converted to tenant credit"
				if rawNote != nil && *rawNote != "" {
					overpaymentNote = *rawNote + " (" + overpaymentNote + ")"
				}
				p := &domain.Payment{
					DueID:      due.ID,
					TenantID:   due.TenantID,
					UPITxnID:   txnID,
					Amount:     amountPaise,
					MatchedBy:  matchedBy,
					RecordedBy: recordedBy,
					MatchedAt:  at,
					RawNote:    &overpaymentNote,
				}
				if err := payments.Create(ctx, p); err != nil {
					return err
				}
				if err := addTenantCredit(ctx, tenants, due.TenantID, amountPaise); err != nil {
					return err
				}
				overpayPayload, _ := json.Marshal(domain.OverpaymentCreditedPayload{
					DueID:       due.ID.String(),
					PaymentID:   p.ID.String(),
					TenantID:    due.TenantID.String(),
					AmountPaise: int64(amountPaise),
					MatchedBy:   string(matchedBy),
					Reason:      "due_already_closed",
				})
				did := due.ID
				if err := pub.Publish(ctx, domain.Event{
					TenantID:   domain.Ptr(due.TenantID),
					PropertyID: due.PropertyID,
					EventType:  domain.EvtOverpaymentCredited,
					DueID:      &did,
					OccurredAt: at,
					Payload:    overpayPayload,
				}); err != nil {
					return err
				}
				out = p
				return nil
			}
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
	if err == nil {
		if out == nil && txnID != nil && *txnID != "" {
			out, _ = s.payments.GetByUPITxnID(ctx, *txnID)
		}
		s.fireSettle(ctx, out, dueID)
	}
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
