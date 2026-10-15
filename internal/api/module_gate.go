package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// requireActiveModule aborts with 404 when the property disabled the module (ADR-032 D6).
func requireActiveModule(c *gin.Context, settings *domain.PropertySettings, module string) bool {
	if settings == nil || settings.ActiveModules == nil {
		return true
	}
	if enabled, ok := settings.ActiveModules[module]; ok && !enabled {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "module disabled"})
		return false
	}
	return true
}

func (h *Handlers) requirePropertyActiveModule(c *gin.Context, propertyID uuid.UUID, module string) bool {
	st, err := h.getPropertySettings(c.Request.Context(), propertyID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "settings unavailable"})
		return false
	}
	return requireActiveModule(c, st, module)
}
