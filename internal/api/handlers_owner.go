package api

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ListProperties handles GET /owner/properties — returns all properties owned by the authenticated owner.
func (h *Handlers) ListProperties(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	var phone, email string
	if h.UserStore != nil {
		if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			phone = u.Phone
			email = u.Email
		}
	} else if h.AuthUserRepo != nil {
		if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			phone = u.Phone
			email = u.Email
		}
	}
	all, err := h.PropertyStore.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	out := make([]domain.Property, 0, len(all))
	for _, p := range all {
		isMatch := false
		if claims.PropertyID != nil && p.ID == *claims.PropertyID {
			isMatch = true
		} else if phone != "" && p.OwnerPhone == phone {
			isMatch = true
		} else if email != "" && strings.EqualFold(p.OwnerEmail, email) {
			isMatch = true
		}
		if isMatch {
			p.UPIVPA = "" // never expose in API
			out = append(out, p)
		}
	}
	c.JSON(http.StatusOK, gin.H{"properties": out})
}

type createPropertyBody struct {
	Name        string `json:"name" binding:"required"`
	Address     string `json:"address"`
	OwnerName   string `json:"owner_name"`
	PaymentMode string `json:"payment_mode"`
}

// CreateProperty handles POST /owner/properties — creates an additional property for the authenticated owner.
func (h *Handlers) CreateProperty(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
		return
	}
	var body createPropertyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid property body", apierr.CodeRequestInvalidBody)
		return
	}

	var userPhone, userEmail string
	if h.UserStore != nil {
		if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			userPhone = u.Phone
			userEmail = u.Email
		}
	} else if h.AuthUserRepo != nil {
		if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			userPhone = u.Phone
			userEmail = u.Email
		}
	}

	prop := domain.Property{
		Name:        strings.TrimSpace(body.Name),
		OwnerPhone:  userPhone,
		OwnerEmail:  userEmail,
		OwnerName:   strings.TrimSpace(body.OwnerName),
		PaymentMode: body.PaymentMode,
	}
	if body.Address != "" {
		trimmed := strings.TrimSpace(body.Address)
		prop.Address = &trimmed
	}
	if err := h.PropertyStore.Create(c.Request.Context(), &prop); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create property failed"})
		return
	}
	prop.UPIVPA = ""
	c.JSON(http.StatusCreated, gin.H{"property": prop})
}

// OwnerSwitchProperty handles POST /owner/properties/:id/switch — switches active property for single login.
func (h *Handlers) OwnerSwitchProperty(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
		return
	}
	propIDStr := c.Param("id")
	targetID, err := uuid.Parse(propIDStr)
	if err != nil || targetID == uuid.Nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid property id", apierr.CodeRequestInvalidId)
		return
	}

	prop, err := h.PropertyStore.GetByID(c.Request.Context(), targetID)
	if err != nil || prop == nil {
		apierr.RespondClientErr(c, http.StatusForbidden, "property not found or not owned", apierr.CodeAuthForbidden)
		return
	}

	var userPhone, userEmail string
	var user *domain.User
	if h.UserStore != nil {
		user, _ = h.UserStore.GetByID(c.Request.Context(), claims.UserID)
	} else if h.AuthUserRepo != nil {
		user, _ = h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID)
	}
	if user != nil {
		userPhone = user.Phone
		userEmail = user.Email
	}

	authorized := false
	if claims.PropertyID != nil && *claims.PropertyID == targetID {
		authorized = true
	} else if userPhone != "" && prop.OwnerPhone == userPhone {
		authorized = true
	} else if userEmail != "" && strings.EqualFold(prop.OwnerEmail, userEmail) {
		authorized = true
	}

	if !authorized {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
		return
	}

	if h.UserStore != nil {
		if err := h.UserStore.SetPropertyID(c.Request.Context(), claims.UserID, targetID); err != nil {
			respondErr(c, err)
			return
		}
	}

	prop.UPIVPA = ""
	c.JSON(http.StatusOK, gin.H{"ok": true, "property": prop})
}

// ResolveOwnerPropertyScope inspects X-Property-ID header or property_id query param
// and binds the verified property ID into requestscope.
func (h *Handlers) ResolveOwnerPropertyScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := auth.ClaimsFromContext(c)
		if !ok || claims.Role != domain.RoleOwner {
			c.Next()
			return
		}

		targetStr := c.GetHeader("X-Property-ID")
		if targetStr == "" {
			targetStr = c.Query("property_id")
		}

		if targetStr == "" {
			if claims.PropertyID != nil && *claims.PropertyID != uuid.Nil {
				c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), *claims.PropertyID))
			}
			c.Next()
			return
		}

		targetID, err := uuid.Parse(strings.TrimSpace(targetStr))
		if err != nil || targetID == uuid.Nil {
			apierr.RespondClientErr(c, http.StatusBadRequest, "invalid property id", apierr.CodeRequestInvalidId)
			c.Abort()
			return
		}

		if claims.PropertyID != nil && *claims.PropertyID == targetID {
			c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), targetID))
			c.Next()
			return
		}

		// Fail closed: a cross-property switch must be verified against the property
		// store. Without a store we cannot prove ownership, so the request is refused
		// rather than binding an arbitrary property ID into the request scope.
		if h.PropertyStore == nil {
			apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}
		prop, err := h.PropertyStore.GetByID(c.Request.Context(), targetID)
		if err != nil || prop == nil {
			apierr.RespondClientErr(c, http.StatusForbidden, "property not found or not owned", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}

		var userPhone, userEmail string
		if h.UserStore != nil {
			if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
				userPhone = u.Phone
				userEmail = u.Email
			}
		} else if h.AuthUserRepo != nil {
			if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
				userPhone = u.Phone
				userEmail = u.Email
			}
		}

		authorized := false
		if userPhone != "" && prop.OwnerPhone == userPhone {
			authorized = true
		} else if userEmail != "" && strings.EqualFold(prop.OwnerEmail, userEmail) {
			authorized = true
		}

		if !authorized {
			apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}

		c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), targetID))
		c.Next()
	}
}
