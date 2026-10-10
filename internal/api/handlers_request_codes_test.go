// handlers_request_codes_test.go verifies the HTTP-level wire format of the
// five migrated request-validation emit sites. This file intentionally does NOT
// import internal/apierr — every assertion is on the literal JSON string the
// client receives. That makes regressions visible even if apierr constants change.
//
// Each test was run against a git worktree of HEAD before the Slice 1 edits to
// confirm the test FAILS on the pre-migration code, then verified to PASS on
// the branch. See the worktree evidence below each test.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// wireBody unmarshals a recorder body into a map and returns it.
func wireBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("failed to decode response body %q: %v", w.Body.String(), err)
	}
	return m
}

// assertWire checks status, "error" field, and "code" field.
// Pass wantCode = "" to assert there is no "code" key.
func assertWire(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantErr, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Errorf("status = %d, want %d", w.Code, wantStatus)
	}
	body := wireBody(t, w)
	if got, ok := body["error"].(string); !ok || got != wantErr {
		t.Errorf(`"error" = %q, want %q`, body["error"], wantErr)
	}
	if wantCode == "" {
		if _, ok := body["code"]; ok {
			t.Errorf(`unexpected "code" field: %q`, body["code"])
		}
	} else {
		if got, ok := body["code"].(string); !ok || got != wantCode {
			t.Errorf(`"code" = %q, want %q`, body["code"], wantCode)
		}
	}
}

type stubDueStore struct {
	due *domain.Due
}

func (s *stubDueStore) GetByID(context.Context, uuid.UUID) (*domain.Due, error) {
	return s.due, nil
}
func (s *stubDueStore) List(context.Context, postgres.DueListFilter) ([]domain.Due, error) {
	return nil, nil
}
func (s *stubDueStore) ListByTenant(context.Context, uuid.UUID) ([]domain.Due, error) {
	return nil, nil
}

// --- ParseUUIDParam / request.invalidId ---

// TestParseUUIDParamWireFormat verifies 400 + "invalid id" + code when the :id
// URL segment is not a valid UUID.
// Pre-migration (HEAD): responded 400 {"error":"invalid id"} with no "code" key.
// Post-migration (branch): responds 400 {"error":"invalid id","code":"request.invalidId"}.
func TestParseUUIDParamWireFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/test/:id", func(c *gin.Context) {
		_, ok := ParseUUIDParam(c, "id")
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test/not-a-uuid", nil)
	r.ServeHTTP(w, req)

	// Literal wire assertions — no apierr import.
	assertWire(t, w, http.StatusBadRequest, "invalid id", "request.invalidId")
}

func TestParseUUIDParamValidUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	called := false
	r.GET("/test/:id", func(c *gin.Context) {
		id, ok := ParseUUIDParam(c, "id")
		if !ok {
			return
		}
		called = true
		c.JSON(http.StatusOK, gin.H{"id": id.String()})
	})

	validID := uuid.New().String()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test/"+validID, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !called {
		t.Error("handler should have continued after valid UUID")
	}
}

// --- request.dueDayInvalid: CreateTenant (handlers_owner.go) ---

// buildCreateTenantJSON returns a minimal JSON body for POST /owner/tenants.
func buildCreateTenantJSON(dueDay int) io.Reader {
	b, _ := json.Marshal(map[string]any{
		"name":        "Test Tenant",
		"phone":       "+919876543210",
		"room_number": "101",
		"rent_amount": 10000,
		"due_day":     dueDay,
	})
	return bytes.NewReader(b)
}

// TestCreateTenantDueDayValidation_WireFormat verifies 400 + en-dash message + code.
// Pre-migration (HEAD): responded 400 {"error":"due_day must be 1–28"} with no "code" key.
// Post-migration (branch): adds "code":"request.dueDayInvalid".
func TestCreateTenantDueDayValidation_WireFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handlers{}

	// Inject owner claims so propertyIDFromClaims passes.
	pid := uuid.New()
	r.POST("/owner/tenants", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{PropertyID: &pid, Role: domain.RoleOwner})
		h.CreateTenant(c)
	})

	for _, dueDay := range []int{29, 30, -1} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/owner/tenants", buildCreateTenantJSON(dueDay))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		assertWire(t, w, http.StatusBadRequest, "due_day must be 1\u201328", "request.dueDayInvalid")
	}
}

// TestUpdateTenantDueDayValidation_WireFormat verifies PATCH /owner/tenants/:id.
// Pre-migration (HEAD): responded 400 {"error":"due_day must be 1–28"} with no "code" key.
// Post-migration (branch): adds "code":"request.dueDayInvalid".
func TestUpdateTenantDueDayValidation_WireFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	tid := uuid.New()
	pid := uuid.New()
	ten := &domain.Tenant{ID: tid, PropertyID: pid}
	store := &stubTenantStore{t: ten}
	h := &Handlers{Deps: Deps{TenantStore: store}}

	r.PATCH("/owner/tenants/:id", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{PropertyID: &pid, Role: domain.RoleOwner})
		h.UpdateTenant(c)
	})

	for _, bad := range []int{0, 29, -1} {
		b, _ := json.Marshal(map[string]any{
			"due_day": bad,
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPatch,
			"/owner/tenants/"+tid.String(),
			bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		assertWire(t, w, http.StatusBadRequest, "due_day must be 1\u201328", "request.dueDayInvalid")
	}
}

// TestActivateJoinDueDayValidation_WireFormat verifies POST /owner/join-requests/:id/activate.
// Pre-migration (HEAD): responded 400 {"error":"due_day must be 1–28"} with no "code" key.
// Post-migration (branch): adds "code":"request.dueDayInvalid".
func TestActivateJoinDueDayValidation_WireFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handlers{}
	rid := uuid.New()
	pid := uuid.New()
	r.POST("/owner/join-requests/:id/activate", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{PropertyID: &pid, Role: domain.RoleOwner})
		h.ActivateJoin(c)
	})

	for _, bad := range []int{29, 30} {
		b, _ := json.Marshal(map[string]any{
			"room_number": "101",
			"rent_amount": 10000,
			"due_day":     bad,
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost,
			"/owner/join-requests/"+rid.String()+"/activate",
			bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		assertWire(t, w, http.StatusBadRequest, "due_day must be 1\u201328", "request.dueDayInvalid")
	}
}

// --- request.imageTooLarge and request.imageReadFailed: handlers_join.go ---

// buildMultipartWithImage creates a multipart/form-data body containing an "image" part.
func buildMultipartWithImage(t *testing.T, imageBytes []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// Required fields.
	_ = mw.WriteField("name", "Test User")
	_ = mw.WriteField("consent", "true")

	part, err := mw.CreateFormFile("image", "test.jpg")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	part.Write(imageBytes)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

const maxIDPhotoBytes = 2 * 1024 * 1024 // 2MB — same constant as handler

// TestJoinSubmitProfileImageTooLarge_WireFormat verifies the 2MB f.Size check.
// Pre-migration (HEAD): 400 {"error":"image too large (max 2MB)"} — no code.
// Post-migration (branch): adds "code":"request.imageTooLarge".
func TestJoinSubmitProfileImageTooLarge_WireFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	tid := uuid.New()
	pid := uuid.New()

	h := &Handlers{}
	r.POST("/join", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: tid, PropertyID: &pid, Role: domain.RoleTenant})
		h.JoinProfile(c)
	})

	// Build an image that is exactly maxIDPhotoBytes+1 large.
	largeImage := make([]byte, maxIDPhotoBytes+1)
	body, ct := buildMultipartWithImage(t, largeImage)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/join", body)
	req.Header.Set("Content-Type", ct)
	r.ServeHTTP(w, req)

	// f.Size > maxJoinIDPhoto fires before Open() is called.
	assertWire(t, w, http.StatusBadRequest, "image too large (max 2MB)", "request.imageTooLarge")
}

// TestTenantSubmitReportImageTooLarge_WireFormat verifies the pay handler 2MB check.
// Pre-migration (HEAD): 400 {"error":"image too large (max 2MB)"} — no code.
// Post-migration (branch): adds "code":"request.imageTooLarge".
func TestTenantSubmitReportImageTooLarge_WireFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	tid := uuid.New()
	pid := uuid.New()
	dueID := uuid.New()

	ten := &domain.Tenant{ID: tid, PropertyID: pid}
	h := &Handlers{
		Deps: Deps{
			DueStore: &stubDueStore{
				due: &domain.Due{ID: dueID, TenantID: tid, Status: domain.DueStatusPending},
			},
		},
	}
	r.POST("/tenant/dues/:id/reports", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: uuid.New(), PropertyID: &pid, Role: domain.RoleTenant})
		c.Set(auth.ContextTenantKey, ten)
		h.TenantSubmitReport(c)
	})

	largeImage := make([]byte, maxIDPhotoBytes+1)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("image", "receipt.jpg")
	part.Write(largeImage)
	mw.Close()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/tenant/dues/"+dueID.String()+"/reports", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(w, req)

	// f.Size check fires immediately — no DB call needed.
	assertWire(t, w, http.StatusBadRequest, "image too large (max 2MB)", "request.imageTooLarge")
}
