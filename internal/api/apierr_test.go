package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
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
