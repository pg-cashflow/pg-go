package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type fakeNotificationService struct {
	readEvents map[uuid.UUID]uuid.UUID // notifID -> recipientID
	allRead    map[uuid.UUID]bool      // recipientID -> true
}

func (s *fakeNotificationService) List(_ context.Context, _ uuid.UUID, _ string, _ int) ([]*domain.Notification, string, int, error) {
	return nil, "", 0, nil
}

func (s *fakeNotificationService) MarkAsRead(_ context.Context, notifID, recipientID uuid.UUID) error {
	// If notification belongs to someone else, fail with not found error
	if expectedRecipient, ok := s.readEvents[notifID]; ok {
		if expectedRecipient != recipientID {
			return fmt.Errorf("notification not found")
		}
	}
	s.readEvents[notifID] = recipientID
	return nil
}

func (s *fakeNotificationService) MarkAllAsRead(_ context.Context, recipientID uuid.UUID) error {
	s.allRead[recipientID] = true
	return nil
}

// TestNotificationIsolation_T4c verifies that a user cannot mark another user's notifications as read
func TestNotificationIsolation_T4c(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	userAliceID := uuid.New()
	userBobID := uuid.New()
	propID := uuid.New()

	alice := &domain.User{ID: userAliceID, Role: domain.RoleTenant, PropertyID: &propID}
	bob := &domain.User{ID: userBobID, Role: domain.RoleTenant, PropertyID: &propID}

	aliceToken, _ := auth.IssueToken(jwtSecret, alice)
	bobToken, _ := auth.IssueToken(jwtSecret, bob)

	aliceNotifID := uuid.New()
	notifSvc := &fakeNotificationService{
		readEvents: map[uuid.UUID]uuid.UUID{
			aliceNotifID: userAliceID,
		},
		allRead: make(map[uuid.UUID]bool),
	}

	h := &api.Handlers{
		Deps: api.Deps{
			JWTSecret:       jwtSecret,
			NotificationSvc: notifSvc,
		},
	}

	router := gin.New()
	authGroup := router.Group("/api", auth.RequireOwnerOrManagerOrTenant(jwtSecret, nil))
	authGroup.PATCH("/notifications/:id/read", h.MarkNotificationRead)
	authGroup.PATCH("/notifications/read-all", h.MarkAllNotificationsRead)

	t.Run("Alice marks her own notification read: 200 OK", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/notifications/"+aliceNotifID.String()+"/read", nil)
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("Bob tries to mark Alice's notification read: fails and does not tamper", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/notifications/"+aliceNotifID.String()+"/read", nil)
		req.Header.Set("Authorization", "Bearer "+bobToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Fatalf("expected non-200 when Bob tampers with Alice's notification, got 200")
		}
	})

	t.Run("Alice marks all read: 200 OK", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/notifications/read-all", nil)
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		if !notifSvc.allRead[userAliceID] {
			t.Fatalf("expected Alice's notifications to be marked all read")
		}
	})
}
