package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestCheckoutClientErrUsesStableRetryContracts(t *testing.T) {
	tests := []struct {
		name       string
		input      error
		wantStatus int
		wantCode   apierr.Code
	}{
		{
			name:       "quarantined previous order",
			input:      postgres.ErrPaymentIntentQuarantinePending,
			wantStatus: http.StatusConflict,
			wantCode:   apierr.CodePaymentCheckoutPending,
		},
		{
			name:       "provider order remains unresolved",
			input:      collector.ErrCashfreeOrderPending,
			wantStatus: http.StatusConflict,
			wantCode:   apierr.CodePaymentCheckoutPending,
		},
		{
			name:       "provider order is terminal",
			input:      collector.ErrCashfreeOrderExpired,
			wantStatus: http.StatusConflict,
			wantCode:   apierr.CodePaymentCheckoutExpired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapped, ok := checkoutClientErr(tt.input).(*ClientError)
			if !ok {
				t.Fatalf("mapped error type = %T, want *ClientError", checkoutClientErr(tt.input))
			}
			if mapped.HTTPStatus != tt.wantStatus || mapped.Code != tt.wantCode {
				t.Fatalf("mapped error = status %d code %q, want status %d code %q", mapped.HTTPStatus, mapped.Code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestRespondErr(t *testing.T) {
	tests := []struct {
		name           string
		inputErr       error
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "Untyped standard error returns 500 internal error",
			inputErr:       errors.New("pgx: relation does not exist"),
			expectedStatus: http.StatusInternalServerError,
			expectedError:  "internal error",
		},
		{
			name:           "ClientError 400 returns 400 with custom message",
			inputErr:       clientErr(http.StatusBadRequest, "invalid payload"),
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid payload",
		},
		{
			name:           "ClientError 404 returns 404 with custom message",
			inputErr:       clientErr(http.StatusNotFound, "account not found"),
			expectedStatus: http.StatusNotFound,
			expectedError:  "account not found",
		},
		{
			name:           "ClientError 409 returns 409 with custom message",
			inputErr:       clientErr(http.StatusConflict, "duplicate upi txn id"),
			expectedStatus: http.StatusConflict,
			expectedError:  "duplicate upi txn id",
		},
		{
			name:           "gamificationClientErr wraps message with status code",
			inputErr:       gamificationClientErr(http.StatusBadRequest, errors.New("meal RSVP cutoff has passed")),
			expectedStatus: http.StatusBadRequest,
			expectedError:  "meal RSVP cutoff has passed",
		},
		{
			name:           "ClientError with code returns error and code",
			inputErr:       clientErrWithCode(http.StatusBadRequest, "unsupported locale", "preferences.invalidLocale"),
			expectedStatus: http.StatusBadRequest,
			expectedError:  "unsupported locale",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/test", nil)

			respondErr(c, tt.inputErr)

			if w.Code != tt.expectedStatus {
				t.Errorf("status code = %d, want %d", w.Code, tt.expectedStatus)
			}

			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to decode response JSON: %v", err)
			}

			if body["error"] != tt.expectedError {
				t.Errorf("response error = %q, want %q", body["error"], tt.expectedError)
			}
		})
	}
}
