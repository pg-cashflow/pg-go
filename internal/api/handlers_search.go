package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

// Search handles GET /api/search (RBAC federated lookup).
func (h *Handlers) Search(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if claims.PropertyID == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return
	}

	q := c.Query("q")
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	var typeFilter []search.EntityType
	if raw := strings.TrimSpace(c.Query("types")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				typeFilter = append(typeFilter, search.EntityType(part))
			}
		}
	}

	var tenantScope *uuid.UUID
	if claims.Role == domain.RoleTenant {
		tenantScope = claims.TenantID
		if tenantScope == nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "complete your profile to continue"})
			return
		}
	}

	if h.SearchSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "search not configured"})
		return
	}

	nq, usedMode, results, partial, err := h.SearchSvc.Search(
		c.Request.Context(),
		claims.Role,
		*claims.PropertyID,
		tenantScope,
		q,
		limit,
		search.ModeLexical,
		typeFilter,
	)
	if err != nil {
		if errors.Is(err, search.ErrQueryRequired) ||
			errors.Is(err, search.ErrQueryTooShort) ||
			errors.Is(err, search.ErrQueryTooLong) {
			respondErr(c, clientErr(http.StatusBadRequest, err.Error()))
			return
		}
		respondErr(c, err)
		return
	}

	out := make([]gin.H, 0, len(results))
	for _, r := range results {
		out = append(out, gin.H{
			"type":     r.Type,
			"id":       r.ID,
			"title":    r.Title,
			"subtitle": r.Subtitle,
		})
	}
	resp := gin.H{
		"q":       nq,
		"mode":    usedMode,
		"results": out,
	}
	if partial {
		resp["partial"] = true
	}
	c.JSON(http.StatusOK, resp)
}
