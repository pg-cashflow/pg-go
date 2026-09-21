package api

import (
	"net/http"
	"net/url"
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
	mode := search.ModeLexical
	if strings.EqualFold(c.Query("mode"), "hybrid") {
		mode = search.ModeHybrid
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

	nq, usedMode, results, err := h.SearchSvc.Search(
		c.Request.Context(),
		claims.Role,
		*claims.PropertyID,
		tenantScope,
		q,
		limit,
		mode,
		typeFilter,
	)
	if err != nil {
		if strings.Contains(err.Error(), "too short") || strings.Contains(err.Error(), "required") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search failed"})
		return
	}

	rewriteSearchPaths(claims.Role, results)

	out := make([]gin.H, 0, len(results))
	for _, r := range results {
		out = append(out, gin.H{
			"type":     r.Type,
			"id":       r.ID,
			"title":    r.Title,
			"subtitle": r.Subtitle,
			"path":     r.Path,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"q":       nq,
		"mode":    usedMode,
		"results": out,
	})
}

func rewriteSearchPaths(role domain.Role, results []search.Result) {
	for i := range results {
		switch results[i].Type {
		case search.TypeTenant:
			if role == domain.RoleManager {
				results[i].Path = "/manager/inspections?q=" + url.QueryEscape(results[i].Title)
			}
		case search.TypeDue, search.TypePayment:
			if role == domain.RoleTenant {
				if results[i].Type == search.TypeDue {
					results[i].Path = "/tenant/dues?q=" + url.QueryEscape(extractQueryToken(results[i].Path))
				} else {
					results[i].Path = "/tenant/payments?q=" + url.QueryEscape(extractQueryToken(results[i].Path))
				}
			}
		}
	}
}

func extractQueryToken(path string) string {
	if idx := strings.Index(path, "?q="); idx >= 0 {
		return path[idx+3:]
	}
	return path
}
