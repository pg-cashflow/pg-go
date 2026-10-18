package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubPayoutBatchRepo struct {
	batch *domain.PayoutBatch
}

func (s *stubPayoutBatchRepo) GetBatchByID(_ context.Context, id uuid.UUID) (*domain.PayoutBatch, error) {
	if s.batch != nil && s.batch.ID == id {
		return s.batch, nil
	}
	return nil, domain.ErrNotFound
}

type stubPayoutAuthService struct {
	channel     string
	destination string
	err         error
	calledPhone string
}

func (s *stubPayoutAuthService) RequestOTP(context.Context, string) error { return nil }
func (s *stubPayoutAuthService) RequestOTPWithPurpose(context.Context, string, string) error {
	return nil
}
func (s *stubPayoutAuthService) RequestStepUpOTP(context.Context, string, string, *uuid.UUID) error {
	return nil
}
func (s *stubPayoutAuthService) RequestStepUpOTPWithChannel(_ context.Context, phone, _ string, _ *uuid.UUID) (string, string, error) {
	s.calledPhone = phone
	return s.channel, s.destination, s.err
}
func (s *stubPayoutAuthService) VerifyOTPAndIssueToken(context.Context, string, string) (string, *domain.User, error) {
	return "", nil, nil
}
func (s *stubPayoutAuthService) VerifyStepUpOTP(context.Context, string, string) error { return nil }
func (s *stubPayoutAuthService) VerifyStepUpOTPSpecific(context.Context, string, string, string, *uuid.UUID) error {
	return nil
}
func (s *stubPayoutAuthService) VerifyFirebaseAndIssueToken(context.Context, string, string) (string, *domain.User, error) {
	return "", nil, nil
}
func (s *stubPayoutAuthService) VerifyFirebaseStepUp(context.Context, string, time.Duration) (auth.FirebaseIdentity, error) {
	return auth.FirebaseIdentity{}, nil
}
func (s *stubPayoutAuthService) IssueSession(context.Context, *domain.User) (string, string, error) {
	return "", "", nil
}
func (s *stubPayoutAuthService) RotateRefreshToken(context.Context, string) (string, string, *domain.User, error) {
	return "", "", nil, nil
}
func (s *stubPayoutAuthService) RevokeSession(context.Context, string) error    { return nil }
func (s *stubPayoutAuthService) RevokeUserSessions(context.Context, uuid.UUID) error { return nil }
func (s *stubPayoutAuthService) IssueSessionWithClient(context.Context, *domain.User, string, string) (string, string, error) {
	return "", "", nil
}
func (s *stubPayoutAuthService) RotateRefreshTokenWithClient(context.Context, string, string, string) (string, string, *domain.User, error) {
	return "", "", nil, nil
}
func (s *stubPayoutAuthService) ListSessions(context.Context, uuid.UUID, *uuid.UUID, string) ([]domain.ActiveSession, error) {
	return nil, nil
}
func (s *stubPayoutAuthService) RevokeDeviceSession(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (s *stubPayoutAuthService) RevokeOtherSessions(context.Context, uuid.UUID, *uuid.UUID, string) error {
	return nil
}

type stubPayoutUserStore struct {
	user *domain.User
}

func (s *stubPayoutUserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if s.user != nil && s.user.ID == id {
		return s.user, nil
	}
	return nil, domain.ErrNotFound
}
func (s *stubPayoutUserStore) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, nil
}
func (s *stubPayoutUserStore) GetByPropertyAndRole(context.Context, uuid.UUID, domain.Role) ([]domain.User, error) {
	return nil, nil
}
func (s *stubPayoutUserStore) Create(context.Context, *domain.User) error                 { return nil }
func (s *stubPayoutUserStore) SetPropertyID(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (s *stubPayoutUserStore) IncrementTokenVersion(context.Context, uuid.UUID) error    { return nil }

func TestOwnerRequestPayoutBatchOTP_ChannelAwareAndMasked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pid := uuid.New()
	uid := uuid.New()
	batchID := uuid.New()

	testUser := &domain.User{
		ID:         uid,
		Phone:      "+919876543210",
		Email:      "owner@pgcashflow.com",
		Role:       domain.RoleOwner,
		PropertyID: &pid,
	}

	testBatch := &domain.PayoutBatch{
		ID:         batchID,
		PropertyID: pid,
		Status:     domain.BatchDraft,
	}

	cases := []struct {
		name            string
		authChannel     string
		authDest        string
		authErr         error
		wantStatus      int
		wantMessage     string
		wantChannel     string
		wantDestination string
	}{
		{
			name:            "Email delivery returns masked email and dynamic message",
			authChannel:     "email",
			authDest:        "o***r@pgcashflow.com",
			wantStatus:      http.StatusOK,
			wantMessage:     "payout approval code sent via email",
			wantChannel:     "email",
			wantDestination: "o***r@pgcashflow.com",
		},
		{
			name:            "SMS delivery returns masked phone and dynamic message",
			authChannel:     "sms",
			authDest:        "+919876543210",
			wantStatus:      http.StatusOK,
			wantMessage:     "payout approval code sent via sms",
			wantChannel:     "sms",
			wantDestination: "*********3210",
		},
		{
			name:        "No delivery channel returns 422 Unprocessable Entity",
			authErr:     auth.ErrNoDeliveryChannel,
			wantStatus:  http.StatusUnprocessableEntity,
			wantMessage: "no delivery channel available for payout approval",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authStub := &stubPayoutAuthService{
				channel:     tc.authChannel,
				destination: tc.authDest,
				err:         tc.authErr,
			}
			userStore := &stubPayoutUserStore{user: testUser}
			payoutRepo := &stubPayoutBatchRepo{batch: testBatch}

			h := &Handlers{
				Deps: Deps{
					Auth:            authStub,
					UserStore:       userStore,
					PayoutBatchRepo: payoutRepo,
				},
			}

			router := gin.New()
			router.POST("/owner/payouts/batches/:id/approve/request-otp", func(c *gin.Context) {
				c.Set(auth.ContextClaimsKey, &auth.Claims{
					UserID:     uid,
					PropertyID: &pid,
					Role:       domain.RoleOwner,
				})
				c.Next()
			}, h.OwnerRequestPayoutBatchOTP)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/owner/payouts/batches/"+batchID.String()+"/approve/request-otp", bytes.NewReader([]byte("{}")))
			router.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d body=%s", tc.wantStatus, w.Code, w.Body.String())
			}

			var resp map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &resp)

			if tc.wantStatus == http.StatusOK {
				if resp["message"] != tc.wantMessage {
					t.Errorf("expected message %q, got %q", tc.wantMessage, resp["message"])
				}
				if resp["channel"] != tc.wantChannel {
					t.Errorf("expected channel %q, got %q", tc.wantChannel, resp["channel"])
				}
				if resp["destination"] != tc.wantDestination {
					t.Errorf("expected destination %q, got %q", tc.wantDestination, resp["destination"])
				}
			}
		})
	}
}
