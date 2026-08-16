package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubTenantStore struct {
	t       *domain.Tenant
	updates int
}

func (s *stubTenantStore) GetByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return s.t, nil
}
func (s *stubTenantStore) ListByProperty(context.Context, uuid.UUID) ([]domain.Tenant, error) {
	return nil, nil
}
func (s *stubTenantStore) Update(_ context.Context, t *domain.Tenant) error {
	s.updates++
	s.t = t
	return nil
}

func TestTenantAadhaarUnverifiedXMLDoesNotPersist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ten := &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()}
	store := &stubTenantStore{t: ten}
	h := &Handlers{Deps: Deps{TenantStore: store}}
	r := gin.New()
	r.POST("/tenant/aadhaar", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		h.TenantAadhaar(c)
	})
	body := `{"consent":true,"confirm":true,"qr_payload":"<PrintLetterBarcodeData uid=\"123456789012\" name=\"Ram Kumar\" gender=\"M\" dob=\"01-01-1990\"/>"}`
	req := httptest.NewRequest(http.MethodPost, "/tenant/aadhaar", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if store.updates != 0 || ten.AadhaarLast4 != nil {
		t.Fatalf("unverified XML must not persist last-4 updates=%d last4=%v", store.updates, ten.AadhaarLast4)
	}
}

func TestTenantAadhaarRejectsOverwrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	existing := "9012"
	ten := &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New(), AadhaarLast4: &existing}
	store := &stubTenantStore{t: ten}
	h := &Handlers{Deps: Deps{TenantStore: store}}
	r := gin.New()
	r.POST("/tenant/aadhaar", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		h.TenantAadhaar(c)
	})
	body, _ := json.Marshal(map[string]any{"consent": true, "uid_last4": "1111"})
	req := httptest.NewRequest(http.MethodPost, "/tenant/aadhaar", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAllowedReportImage(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	if !allowedReportImage(jpeg) {
		t.Fatal("jpeg")
	}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if !allowedReportImage(png) {
		t.Fatal("png")
	}
	if allowedReportImage([]byte("%PDF-1.4")) {
		t.Fatal("pdf must be rejected")
	}
}
