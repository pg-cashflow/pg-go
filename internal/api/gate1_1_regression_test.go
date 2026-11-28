package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

type subsecondPaymentStore struct {
	payments  []domain.Payment
	gotLimit  int
	gotBefore *time.Time
	gotID     *uuid.UUID
}

func (s *subsecondPaymentStore) ListByProperty(_ context.Context, _ uuid.UUID, _ *domain.MatchedBy) ([]domain.Payment, error) {
	return s.payments, nil
}

func (s *subsecondPaymentStore) ListByTenant(_ context.Context, _ uuid.UUID) ([]domain.Payment, error) {
	return nil, nil
}

func (s *subsecondPaymentStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.Payment, error) {
	return nil, nil
}

func (s *subsecondPaymentStore) ListByPropertyPaginated(
	_ context.Context,
	_ uuid.UUID,
	_ *domain.MatchedBy,
	limit int,
	before *time.Time,
	beforeID *uuid.UUID,
) ([]domain.Payment, error) {
	s.gotLimit = limit
	s.gotBefore = before
	s.gotID = beforeID
	if before == nil || beforeID == nil {
		if len(s.payments) > limit {
			return s.payments[:limit], nil
		}
		return s.payments, nil
	}
	var out []domain.Payment
	for _, p := range s.payments {
		if p.MatchedAt.Before(*before) || (p.MatchedAt.Equal(*before) && p.ID.String() < beforeID.String()) {
			out = append(out, p)
		}
	}
	if len(out) > limit {
		return out[:limit], nil
	}
	return out, nil
}

func TestListPayments_SubsecondCursorPrecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	id1 := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	id2 := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	t1 := time.Date(2026, 11, 28, 6, 0, 0, 500000000, time.UTC)
	t2 := time.Date(2026, 11, 28, 6, 0, 0, 200000000, time.UTC)

	store := &subsecondPaymentStore{
		payments: []domain.Payment{
			{ID: id1, MatchedAt: t1},
			{ID: id2, MatchedAt: t2},
		},
	}
	h := &Handlers{Deps: Deps{PaymentStore: store}}
	r := gin.New()
	r.GET("/owner/payments", func(c *gin.Context) {
		c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), propID))
		h.ListPayments(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/owner/payments?limit=1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var page1 struct {
		Payments     []domain.Payment `json:"payments"`
		NextCursor   string           `json:"next_cursor"`
		NextCursorID string           `json:"next_cursor_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page1); err != nil {
		t.Fatalf("unmarshal page1: %v", err)
	}
	if page1.NextCursor != t1.Format(time.RFC3339Nano) {
		t.Fatalf("expected subsecond cursor %q, got %q", t1.Format(time.RFC3339Nano), page1.NextCursor)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/owner/payments?limit=1&cursor="+page1.NextCursor+"&cursor_id="+page1.NextCursorID, nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 on page2, got %d", w2.Code)
	}
	var page2 struct {
		Payments []domain.Payment `json:"payments"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("unmarshal page2: %v", err)
	}
	if len(page2.Payments) != 1 || page2.Payments[0].ID != id2 {
		t.Fatalf("expected second subsecond payment %s, got %+v", id2, page2.Payments)
	}
}

func TestOwnerListUnbatchedPayoutItems_CursorValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	h := &Handlers{Deps: Deps{PayoutRepo: &postgres.PayoutRepo{}}}
	r := gin.New()
	r.GET("/owner/payouts/items/unbatched", func(c *gin.Context) {
		c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), propID))
		h.OwnerListUnbatchedPayoutItems(c)
	})

	cases := []string{
		"/owner/payouts/items/unbatched?cursor=not-a-time&cursor_id=" + uuid.NewString(),
		"/owner/payouts/items/unbatched?cursor=2026-11-28T06:00:00Z&cursor_id=bad-uuid",
		"/owner/payouts/items/unbatched?cursor=2026-11-28T06:00:00Z",
		"/owner/payouts/items/unbatched?cursor_id=" + uuid.NewString(),
	}
	for _, target := range cases {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d (%s)", target, w.Code, w.Body.String())
		}
	}
}

type webhookCtxSettler struct {
	gotEvtID uuid.UUID
	gotOK    bool
}

func (s *webhookCtxSettler) SettleGatewayOrder(ctx context.Context, _, _, _ string, _ int64, _ *domain.PaymentIntent, _ []byte) error {
	s.gotEvtID, s.gotOK = payment.WebhookEventIDFromContext(ctx)
	return nil
}

type failingAuditGatewayRepo struct {
	stubGatewayRepo
	failUpdate bool
}

func (f *failingAuditGatewayRepo) UpdateWebhookEventStatus(_ context.Context, _ uuid.UUID, _ string, _ *string) error {
	if f.failUpdate {
		return errors.New("audit update failed")
	}
	return nil
}

func TestHandlePaymentSuccessWebhook_PropagatesEventIDAndChecksAuditUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	evtID := uuid.New()
	intent := &domain.PaymentIntent{ID: uuid.New(), AmountPaise: 50000}
	settler := &webhookCtxSettler{}
	gwRepo := &failingAuditGatewayRepo{failUpdate: true}
	h := &Handlers{Deps: Deps{
		OrderSettler:       settler,
		GatewayPaymentRepo: gwRepo,
		IntentStore:        &stubIntentStore{byOrder: map[string]*domain.PaymentIntent{"ord_1": intent}},
	}}

	r := gin.New()
	r.POST("/wh", func(c *gin.Context) {
		h.handlePaymentSuccessWebhook(c, cashfree.SuccessWebhook{
			OrderID:     "ord_1",
			CFPaymentID: "cf_1",
			AmountPaise: 50000,
		}, &domain.WebhookEvent{ID: evtID}, "")
	})

	req := httptest.NewRequest(http.MethodPost, "/wh", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if !settler.gotOK || settler.gotEvtID != evtID {
		t.Fatalf("expected WebhookEventID %s in settler ctx, got %s (ok=%v)", evtID, settler.gotEvtID, settler.gotOK)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when no-pool UpdateWebhookEventStatus fails, got %d", w.Code)
	}
}
