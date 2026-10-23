package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type countingCapabilityChecker struct {
	count atomic.Int64
	allow bool
}

func (c *countingCapabilityChecker) HasActive(ctx context.Context, propertyID, userID uuid.UUID, capability string) (bool, error) {
	c.count.Add(1)
	return c.allow, nil
}

func TestRequireCapability_BoundedCacheAndEviction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	userID := uuid.New()
	const testCap = "inspections.write"

	checker := &countingCapabilityChecker{allow: true}
	handler := RequireCapability(testCap, checker)

	makeRequest := func(pID, uID uuid.UUID, role domain.Role) int {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			claims := &Claims{
				UserID: uID,
				Role:   role,
			}
			if pID != uuid.Nil {
				claims.PropertyID = &pID
			}
			c.Set(ContextClaimsKey, claims)
			c.Next()
		})
		r.GET("/protected", handler, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		r.ServeHTTP(w, req)
		return w.Code
	}

	// 1. Owner passes immediately without querying checker.
	code := makeRequest(propID, userID, domain.RoleOwner)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for owner, got %d", code)
	}
	if checker.count.Load() != 0 {
		t.Fatalf("expected 0 checks for owner, got %d", checker.count.Load())
	}

	// 2. Manager request queries checker and caches result.
	code = makeRequest(propID, userID, domain.RoleManager)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for manager, got %d", code)
	}
	if checker.count.Load() != 1 {
		t.Fatalf("expected 1 check for manager, got %d", checker.count.Load())
	}

	// 3. Second manager request hits cache, does not increment checker.
	code = makeRequest(propID, userID, domain.RoleManager)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for cached manager, got %d", code)
	}
	if checker.count.Load() != 1 {
		t.Fatalf("expected cache hit (count=1), got %d", checker.count.Load())
	}
}

func TestRequireCapability_DisallowedForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	userID := uuid.New()
	checker := &countingCapabilityChecker{allow: false}
	handler := RequireCapability("violations.delete", checker)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(ContextClaimsKey, &Claims{
			UserID:     userID,
			Role:       domain.RoleManager,
			PropertyID: &propID,
		})
		c.Next()
	})
	r.GET("/protected", handler, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 forbidden, got %d", w.Code)
	}
}

type multiCapChecker struct {
	allowedCaps map[string]bool
}

func (m *multiCapChecker) HasActive(_ context.Context, _, _ uuid.UUID, capability string) (bool, error) {
	return m.allowedCaps[capability], nil
}

func TestRequireAnyCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	userID := uuid.New()

	checker := &multiCapChecker{
		allowedCaps: map[string]bool{
			"complaints.respond": true,
			"inspections.write":  false,
		},
	}
	handler := RequireAnyCapability(checker, "complaints.respond", "inspections.write")

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(ContextClaimsKey, &Claims{
			UserID:     userID,
			Role:       domain.RoleManager,
			PropertyID: &propID,
		})
		c.Next()
	})
	r.GET("/hazards", handler, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hazards", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for manager with complaints.respond, got %d", w.Code)
	}

	// Test manager without either capability gets 403
	checkerNoCap := &multiCapChecker{allowedCaps: map[string]bool{}}
	handlerNoCap := RequireAnyCapability(checkerNoCap, "complaints.respond", "inspections.write")
	r2 := gin.New()
	r2.Use(func(c *gin.Context) {
		c.Set(ContextClaimsKey, &Claims{
			UserID:     userID,
			Role:       domain.RoleManager,
			PropertyID: &propID,
		})
		c.Next()
	})
	r2.GET("/hazards", handlerNoCap, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/hazards", nil)
	r2.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager with neither cap, got %d", w2.Code)
	}
}

