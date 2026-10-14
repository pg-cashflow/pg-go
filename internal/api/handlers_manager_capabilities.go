package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ManagerMyCapabilities handles GET /manager/me/capabilities.
func (h *Handlers) ManagerMyCapabilities(c *gin.Context) {
	if h.CapabilityStore == nil {
		c.JSON(http.StatusOK, gin.H{"capabilities": []string{}})
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	caps, err := h.CapabilityStore.ListActive(c.Request.Context(), pid, uid)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "capability lookup failed"})
		return
	}
	if caps == nil {
		caps = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"capabilities": caps})
}
