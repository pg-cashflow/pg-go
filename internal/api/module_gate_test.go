package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestRequireActiveModuleDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	pid := uuid.New()
	st := domain.DefaultPropertySettings(pid)
	st.ActiveModules["kyc"] = false

	ok := requireActiveModule(c, &st, "kyc")
	if ok {
		t.Fatal("expected module gate to abort")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}

func TestRequireActiveModuleMissingFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	pid := uuid.New()
	st := domain.DefaultPropertySettings(pid)

	if !requireActiveModule(c, &st, "kyc") {
		t.Fatal("expected pass when module key unset")
	}
}
