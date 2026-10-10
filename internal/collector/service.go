package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/qr"
	"golang.org/x/sync/singleflight"
)

type IntentStore interface {
	Create(ctx context.Context, p *domain.PaymentIntent) error
	LatestOpenForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error)
}

type CashfreeOrders interface {
	CreateUPIOrder(ctx context.Context, orderID string, amountPaise int64, customerPhone, note string) (sessionID string, expiresAt *time.Time, err error)
}

type Service struct {
	intents  IntentStore
	cashfree CashfreeOrders
	sf       singleflight.Group
}

func New(intents IntentStore, cashfree CashfreeOrders) *Service {
	return &Service{intents: intents, cashfree: cashfree}
}

func (s *Service) PayIntent(ctx context.Context, due *domain.Due, prop *domain.Property, room, pngURL, customerPhone string) (*domain.PayIntent, []byte, error) {
	note := domain.UPINote(due.DueCode)
	payable := due.Status == domain.DueStatusPending || due.Status == domain.DueStatusPartial
	out := &domain.PayIntent{
		Mode:        domain.PaymentModeManual,
		Note:        note,
		DueCode:     due.DueCode,
		AmountPaise: due.Amount,
		QRPNGURL:    pngURL,
		Payable:     payable,
	}
	if !payable {
		return out, nil, nil
	}

	if prop.PaymentMode == domain.PaymentModeCashfree && s.cashfree != nil && customerPhone != "" {
		session, err := s.ensureCashfree(ctx, due, customerPhone)
		if err != nil {
			return nil, nil, err
		}
		out.Mode = domain.PaymentModeCashfree
		out.PaymentSessionID = session
		out.VPA = ""
		out.UPILink = ""
		out.QRPNGURL = ""
		return out, nil, nil
	}

	link := qr.GenerateUPILink(prop.UPIVPA, prop.OwnerName, int64(due.Amount), due.DueCode, room)
	png, err := qr.GenerateQR(link)
	if err != nil {
		return nil, nil, err
	}
	out.VPA = prop.UPIVPA
	out.UPILink = link
	return out, png, nil
}

func (s *Service) ensureCashfree(ctx context.Context, due *domain.Due, customerPhone string) (string, error) {
	key := due.ID.String()
	res, err, _ := s.sf.Do(key, func() (any, error) {
		return s.doEnsureCashfree(ctx, due, customerPhone)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

type PaymentIntentManager interface {
	ClaimInitiatingIntent(ctx context.Context, tenantID uuid.UUID, dueIDs []uuid.UUID, amounts []int64, totalAmountPaise int64, orderID string, minRemaining time.Duration) (claimed *domain.PaymentIntent, isReused bool, isInFlight bool, err error)
	TransitionCreated(ctx context.Context, intentID uuid.UUID, sessionID string, expiresAt *time.Time) error
	TransitionFailed(ctx context.Context, intentID uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentIntent, error)
}

func (s *Service) doEnsureCashfree(ctx context.Context, due *domain.Due, customerPhone string) (string, error) {
	minRemaining := 10 * time.Minute

	if mgr, ok := s.intents.(PaymentIntentManager); ok {
		orderID := fmt.Sprintf("pg-%s-%d", due.DueCode, time.Now().UnixNano()/1000)
		claimed, isReused, isInFlight, err := mgr.ClaimInitiatingIntent(ctx, due.TenantID, []uuid.UUID{due.ID}, []int64{due.Amount}, due.Amount, orderID, minRemaining)
		if err != nil {
			return "", err
		}
		if isReused && claimed != nil && claimed.PaymentSessionID != nil {
			return *claimed.PaymentSessionID, nil
		}
		if isInFlight && claimed != nil {
			for i := 0; i < 15; i++ {
				time.Sleep(100 * time.Millisecond)
				polled, pErr := mgr.GetByID(ctx, claimed.ID)
				if pErr == nil && polled != nil {
					if polled.Status == domain.IntentCreated && polled.PaymentSessionID != nil {
						return *polled.PaymentSessionID, nil
					}
					if polled.Status == domain.IntentFailed || polled.Status == domain.IntentSuperseded {
						break
					}
				}
			}
			claimed, isReused, _, err = mgr.ClaimInitiatingIntent(ctx, due.TenantID, []uuid.UUID{due.ID}, []int64{due.Amount}, due.Amount, orderID, minRemaining)
			if err != nil {
				return "", err
			}
			if isReused && claimed != nil && claimed.PaymentSessionID != nil {
				return *claimed.PaymentSessionID, nil
			}
		}

		session, exp, err := s.cashfree.CreateUPIOrder(ctx, claimed.ProviderOrderID, due.Amount, customerPhone, domain.UPINote(due.DueCode))
		if err != nil {
			_ = mgr.TransitionFailed(ctx, claimed.ID)
			return "", err
		}
		if err := mgr.TransitionCreated(ctx, claimed.ID, session, exp); err != nil {
			return "", err
		}
		return session, nil
	}

	if reusableLocker, ok := s.intents.(interface {
		GetReusableIntentUnderLock(ctx context.Context, tenantID, dueID uuid.UUID, minRemaining time.Duration) (*domain.PaymentIntent, error)
		SupersedeOpenIntentsForDue(ctx context.Context, dueID uuid.UUID) error
		CreateWithDues(ctx context.Context, p *domain.PaymentIntent, dueIDs []uuid.UUID, amounts []int64) error
	}); ok {
		// Under-lock inspection: acquires tenants -> dues -> payment_intents locks in order
		existing, err := reusableLocker.GetReusableIntentUnderLock(ctx, due.TenantID, due.ID, minRemaining)
		if err == nil && existing != nil && existing.PaymentSessionID != nil && existing.AmountPaise == due.Amount {
			return *existing.PaymentSessionID, nil
		} else if err != nil && !isNoRows(err) {
			return "", err
		}

		// Re-check under lock confirmed no reusable active intent with >=10m remaining.
		// Supersede older open intents for this due.
		_ = reusableLocker.SupersedeOpenIntentsForDue(ctx, due.ID)

		orderID := fmt.Sprintf("pg-%s-%d", due.DueCode, time.Now().Unix())
		session, exp, err := s.cashfree.CreateUPIOrder(ctx, orderID, due.Amount, customerPhone, domain.UPINote(due.DueCode))
		if err != nil {
			return "", err
		}
		intent := &domain.PaymentIntent{
			DueID:            due.ID,
			Provider:         "cashfree",
			ProviderOrderID:  orderID,
			PaymentSessionID: &session,
			AmountPaise:      due.Amount,
			Status:           domain.IntentCreated,
			ExpiresAt:        exp,
		}
		if err := reusableLocker.CreateWithDues(ctx, intent, []uuid.UUID{due.ID}, []int64{int64(due.Amount)}); err != nil {
			return "", err
		}
		return session, nil
	}

	// Fallback path when store is not a full SQL repository
	if existing, err := s.intents.LatestOpenForDue(ctx, due.ID); err == nil && existing.PaymentSessionID != nil && existing.AmountPaise == due.Amount {
		if existing.ExpiresAt == nil || existing.ExpiresAt.After(time.Now().Add(minRemaining)) {
			return *existing.PaymentSessionID, nil
		}
	} else if err != nil && !isNoRows(err) {
		return "", err
	}
	orderID := fmt.Sprintf("pg-%s-%d", due.DueCode, time.Now().Unix())
	session, exp, err := s.cashfree.CreateUPIOrder(ctx, orderID, due.Amount, customerPhone, domain.UPINote(due.DueCode))
	if err != nil {
		return "", err
	}
	intent := &domain.PaymentIntent{
		DueID:            due.ID,
		Provider:         "cashfree",
		ProviderOrderID:  orderID,
		PaymentSessionID: &session,
		AmountPaise:      due.Amount,
		Status:           domain.IntentCreated,
		ExpiresAt:        exp,
	}
	if err := s.intents.Create(ctx, intent); err != nil {
		return "", err
	}
	return session, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func (s *Service) MultiDuePayIntent(ctx context.Context, dues []*domain.Due, prop *domain.Property, room, customerPhone string) (*domain.PayIntent, error) {
	if len(dues) == 0 {
		return nil, errors.New("no dues provided")
	}

	dueIDs := make([]uuid.UUID, len(dues))
	totalAmountPaise := int64(0)
	payable := true
	for i, d := range dues {
		dueIDs[i] = d.ID
		totalAmountPaise += d.Amount
		if d.Status != domain.DueStatusPending && d.Status != domain.DueStatusPartial {
			payable = false
		}
	}

	note := fmt.Sprintf("PG-%s-BATCH-%d", dues[0].DueCode, len(dues))
	out := &domain.PayIntent{
		Mode:        domain.PaymentModeManual,
		Note:        note,
		DueCode:     dues[0].DueCode,
		AmountPaise: totalAmountPaise,
		DueCount:    len(dues),
		DueIDs:      dueIDs,
		Payable:     payable,
	}

	if !payable {
		return out, nil
	}

	if prop.PaymentMode == domain.PaymentModeCashfree && s.cashfree != nil && customerPhone != "" {
		session, err := s.ensureCashfreeMultiDue(ctx, dues, totalAmountPaise, customerPhone)
		if err != nil {
			return nil, err
		}
		out.Mode = domain.PaymentModeCashfree
		out.PaymentSessionID = session
		out.VPA = ""
		out.UPILink = ""
		out.QRPNGURL = ""
		return out, nil
	}

	link := qr.GenerateUPILink(prop.UPIVPA, prop.OwnerName, totalAmountPaise, note, room)
	out.VPA = prop.UPIVPA
	out.UPILink = link
	return out, nil
}

func (s *Service) ensureCashfreeMultiDue(ctx context.Context, dues []*domain.Due, totalAmountPaise int64, customerPhone string) (string, error) {
	var sb strings.Builder
	sb.WriteString("multi:")
	for _, d := range dues {
		sb.WriteString(d.ID.String())
		sb.WriteString(":")
	}
	key := sb.String()

	res, err, _ := s.sf.Do(key, func() (any, error) {
		return s.doEnsureCashfreeMultiDue(ctx, dues, totalAmountPaise, customerPhone)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

func (s *Service) doEnsureCashfreeMultiDue(ctx context.Context, dues []*domain.Due, totalAmountPaise int64, customerPhone string) (string, error) {
	minRemaining := 10 * time.Minute
	dueIDs := make([]uuid.UUID, len(dues))
	amounts := make([]int64, len(dues))
	for i, d := range dues {
		dueIDs[i] = d.ID
		amounts[i] = d.Amount
	}
	tenantID := dues[0].TenantID

	if mgr, ok := s.intents.(PaymentIntentManager); ok {
		orderID := fmt.Sprintf("pg-m-%s-%d", dues[0].DueCode, time.Now().UnixNano()/1000)
		note := fmt.Sprintf("PG-%s-BATCH-%d", dues[0].DueCode, len(dues))
		claimed, isReused, isInFlight, err := mgr.ClaimInitiatingIntent(ctx, tenantID, dueIDs, amounts, totalAmountPaise, orderID, minRemaining)
		if err != nil {
			return "", err
		}
		if isReused && claimed != nil && claimed.PaymentSessionID != nil {
			return *claimed.PaymentSessionID, nil
		}
		if isInFlight && claimed != nil {
			for i := 0; i < 15; i++ {
				time.Sleep(100 * time.Millisecond)
				polled, pErr := mgr.GetByID(ctx, claimed.ID)
				if pErr == nil && polled != nil {
					if polled.Status == domain.IntentCreated && polled.PaymentSessionID != nil {
						return *polled.PaymentSessionID, nil
					}
					if polled.Status == domain.IntentFailed || polled.Status == domain.IntentSuperseded {
						break
					}
				}
			}
			claimed, isReused, _, err = mgr.ClaimInitiatingIntent(ctx, tenantID, dueIDs, amounts, totalAmountPaise, orderID, minRemaining)
			if err != nil {
				return "", err
			}
			if isReused && claimed != nil && claimed.PaymentSessionID != nil {
				return *claimed.PaymentSessionID, nil
			}
		}

		session, exp, err := s.cashfree.CreateUPIOrder(ctx, claimed.ProviderOrderID, totalAmountPaise, customerPhone, note)
		if err != nil {
			_ = mgr.TransitionFailed(ctx, claimed.ID)
			return "", err
		}
		if err := mgr.TransitionCreated(ctx, claimed.ID, session, exp); err != nil {
			return "", err
		}
		return session, nil
	}

	if multiLocker, ok := s.intents.(interface {
		GetReusableMultiDueIntentUnderLock(ctx context.Context, tenantID uuid.UUID, dueIDs []uuid.UUID, totalAmountPaise int64, minRemaining time.Duration) (*domain.PaymentIntent, error)
		SupersedeOpenIntentsForDues(ctx context.Context, dueIDs []uuid.UUID) error
		CreateWithDues(ctx context.Context, p *domain.PaymentIntent, dueIDs []uuid.UUID, amounts []int64) error
	}); ok {
		existing, err := multiLocker.GetReusableMultiDueIntentUnderLock(ctx, tenantID, dueIDs, totalAmountPaise, minRemaining)
		if err == nil && existing != nil && existing.PaymentSessionID != nil && existing.AmountPaise == totalAmountPaise {
			return *existing.PaymentSessionID, nil
		} else if err != nil && !isNoRows(err) {
			return "", err
		}

		_ = multiLocker.SupersedeOpenIntentsForDues(ctx, dueIDs)

		orderID := fmt.Sprintf("pg-m-%s-%d", dues[0].DueCode, time.Now().Unix())
		note := fmt.Sprintf("PG-%s-BATCH-%d", dues[0].DueCode, len(dues))
		session, exp, err := s.cashfree.CreateUPIOrder(ctx, orderID, totalAmountPaise, customerPhone, note)
		if err != nil {
			return "", err
		}
		intent := &domain.PaymentIntent{
			DueID:            dues[0].ID,
			Provider:         "cashfree",
			ProviderOrderID:  orderID,
			PaymentSessionID: &session,
			AmountPaise:      totalAmountPaise,
			Status:           domain.IntentCreated,
			ExpiresAt:        exp,
		}
		if err := multiLocker.CreateWithDues(ctx, intent, dueIDs, amounts); err != nil {
			return "", err
		}
		return session, nil
	}

	orderID := fmt.Sprintf("pg-m-%s-%d", dues[0].DueCode, time.Now().Unix())
	note := fmt.Sprintf("PG-%s-BATCH-%d", dues[0].DueCode, len(dues))
	session, exp, err := s.cashfree.CreateUPIOrder(ctx, orderID, totalAmountPaise, customerPhone, note)
	if err != nil {
		return "", err
	}
	intent := &domain.PaymentIntent{
		DueID:            dues[0].ID,
		Provider:         "cashfree",
		ProviderOrderID:  orderID,
		PaymentSessionID: &session,
		AmountPaise:      totalAmountPaise,
		Status:           domain.IntentCreated,
		ExpiresAt:        exp,
	}
	if err := s.intents.Create(ctx, intent); err != nil {
		return "", err
	}
	return session, nil
}

func PNGURL(role string, dueID uuid.UUID) string {
	if role == "owner" {
		return "/api/owner/dues/" + dueID.String() + "/qr"
	}
	return "/api/tenant/dues/" + dueID.String() + "/qr"
}
