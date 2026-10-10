package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestVerifyDualControlOrStepUp_FailsClosedWhenUnconfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Completely unconfigured: UserStore == nil, Auth == nil
	h := &Handlers{
		Deps: Deps{},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	pid := uuid.New()
	uid := uuid.New()

	mode, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, StepUpAuthInput{})
	if ok {
		t.Fatalf("expected verifyDualControlOrStepUp to fail closed when unconfigured, but returned ok=true, mode=%q", mode)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500 InternalServerError, got %d", w.Code)
	}
}

func TestVerifyDualControlOrStepUp_DualControlWithMultipleOwners(t *testing.T) {
	gin.SetMode(gin.TestMode)
	makerID := uuid.New()
	checkerID := uuid.New()
	propID := uuid.New()

	h := &Handlers{
		Deps: Deps{
			UserStore: &mockDualControlUserStore{
				owners: []domain.User{
					{ID: makerID},
					{ID: checkerID},
				},
			},
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	// Checker approving maker's action
	mode, ok := h.verifyDualControlOrStepUp(c, propID, checkerID, &makerID, StepUpAuthInput{})
	if !ok || mode != "dual_control" {
		t.Fatalf("expected mode dual_control and ok=true, got ok=%v, mode=%q", ok, mode)
	}
}

func TestVerifyDualControlOrStepUp_MakerCannotBeChecker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	makerID := uuid.New()
	checkerID := uuid.New()
	propID := uuid.New()

	h := &Handlers{
		Deps: Deps{
			UserStore: &mockDualControlUserStore{
				owners: []domain.User{
					{ID: makerID},
					{ID: checkerID},
				},
			},
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	// Maker attempting to approve their own action
	mode, ok := h.verifyDualControlOrStepUp(c, propID, makerID, &makerID, StepUpAuthInput{})
	if ok {
		t.Fatalf("expected maker approval to be rejected, got ok=true, mode=%q", mode)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 Forbidden, got %d", w.Code)
	}
}
