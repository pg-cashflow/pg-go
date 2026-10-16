package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockManagerUserStore struct {
	createdUsers []*domain.User
}

func (m *mockManagerUserStore) Create(_ context.Context, u *domain.User) error {
	m.createdUsers = append(m.createdUsers, u)
	return nil
}
func (m *mockManagerUserStore) GetByPhone(_ context.Context, _ string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (m *mockManagerUserStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (m *mockManagerUserStore) GetByPropertyAndRole(_ context.Context, _ uuid.UUID, _ domain.Role) ([]domain.User, error) {
	return nil, nil
}
func (m *mockManagerUserStore) SetPropertyID(_ context.Context, _, _ uuid.UUID) error {
	return nil
}
func (m *mockManagerUserStore) IncrementTokenVersion(_ context.Context, _ uuid.UUID) error {
	return nil
}

func TestOwnerCreateManager_PhoneNormalization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()

	ownerToken, err := auth.IssueToken(jwtSecret, &domain.User{
		ID:         uuid.New(),
		Role:       domain.RoleOwner,
		PropertyID: &propID,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	tests := []struct {
		name          string
		inputPhone    string
		inputEmail    string
		propertyID    string
		wantStatus    int
		wantNormPhone string
		wantEmail     string
		wantErrSubstr string
	}{
		{
			name:          "10-digit Indian phone gets normalized to +91",
			inputPhone:    "9876543210",
			propertyID:    propID.String(),
			wantStatus:    http.StatusOK,
			wantNormPhone: "+919876543210",
		},
		{
			name:          "11-digit phone with leading 0 gets normalized",
			inputPhone:    "09876543210",
			propertyID:    propID.String(),
			wantStatus:    http.StatusOK,
			wantNormPhone: "+919876543210",
		},
		{
			name:          "E.164 phone preserved",
			inputPhone:    "+919876543210",
			propertyID:    propID.String(),
			wantStatus:    http.StatusOK,
			wantNormPhone: "+919876543210",
		},
		{
			name:          "Valid optional email is stored",
			inputPhone:    "+919876543210",
			inputEmail:    "Manager@PGBooks.com",
			propertyID:    propID.String(),
			wantStatus:    http.StatusOK,
			wantNormPhone: "+919876543210",
			wantEmail:     "manager@pgbooks.com",
		},
		{
			name:          "Invalid email format rejected with 400",
			inputPhone:    "+919876543210",
			inputEmail:    "invalid-email",
			propertyID:    propID.String(),
			wantStatus:    http.StatusBadRequest,
			wantErrSubstr: "invalid email format",
		},
		{
			name:          "Malformed short phone rejected with 400",
			inputPhone:    "12345",
			propertyID:    propID.String(),
			wantStatus:    http.StatusBadRequest,
			wantErrSubstr: "invalid phone number format",
		},
		{
			name:          "Foreign property rejected with 403",
			inputPhone:    "9876543210",
			propertyID:    uuid.New().String(),
			wantStatus:    http.StatusForbidden,
			wantErrSubstr: "not authorized for this property",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockManagerUserStore{}
			router := gin.New()
			h := &Handlers{Deps: Deps{
				JWTSecret: jwtSecret,
				UserStore: store,
			}}

			// Mount route with owner auth
			owner := router.Group("/api/owner", auth.RequireOwner(jwtSecret, nil), h.ResolveOwnerPropertyScope())
			owner.POST("/managers", h.OwnerCreateManager)

			payload := map[string]any{
				"phone":       tt.inputPhone,
				"property_id": tt.propertyID,
			}
			if tt.inputEmail != "" {
				payload["email"] = tt.inputEmail
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/api/owner/managers", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+ownerToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d body=%s", tt.wantStatus, w.Code, w.Body.String())
			}

			if tt.wantErrSubstr != "" && !strings.Contains(w.Body.String(), tt.wantErrSubstr) {
				t.Fatalf("expected error body to contain %q, got %s", tt.wantErrSubstr, w.Body.String())
			}

			if tt.wantNormPhone != "" {
				if len(store.createdUsers) != 1 {
					t.Fatalf("expected 1 user created, got %d", len(store.createdUsers))
				}
				if store.createdUsers[0].Phone != tt.wantNormPhone {
					t.Errorf("expected stored phone %q, got %q", tt.wantNormPhone, store.createdUsers[0].Phone)
				}
				if tt.wantEmail != "" && store.createdUsers[0].Email != tt.wantEmail {
					t.Errorf("expected stored email %q, got %q", tt.wantEmail, store.createdUsers[0].Email)
				}
			}
		})
	}
}
