package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/localization"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ListLocales handles GET /api/locales — returns supported locale registry.
func (h *Handlers) ListLocales(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"locales":        localization.SupportedLocales,
		"default_locale": localization.DefaultLocale,
	})
}

// GetPreferences handles GET /api/me/preferences — returns authenticated user's preferences.
func (h *Handlers) GetPreferences(c *gin.Context) {
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	if h.PreferencesStore == nil {
		resolved := localization.ResolveLocale(nil, c.Request)
		c.JSON(http.StatusOK, gin.H{
			"locale":               resolved,
			"has_saved_preference": false,
		})
		return
	}
	pref, err := h.PreferencesStore.GetByUserID(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			resolved := localization.ResolveLocale(nil, c.Request)
			c.JSON(http.StatusOK, gin.H{
				"locale":               resolved,
				"has_saved_preference": false,
			})
			return
		}
		respondErr(c, err)
		return
	}
	resolved := localization.ResolveLocale(&pref.Locale, c.Request)
	c.JSON(http.StatusOK, gin.H{
		"locale":               resolved,
		"has_saved_preference": true,
	})
}

type patchPreferencesBody struct {
	Locale string `json:"locale" binding:"required"`
}

// PatchPreferences handles PATCH /api/me/preferences — updates authenticated user's locale.
func (h *Handlers) PatchPreferences(c *gin.Context) {
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	var body patchPreferencesBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "locale is required", apierr.CodePreferencesLocaleRequired)
		return
	}

	if !localization.IsValid(body.Locale) {
		respondErr(c, clientErrWithCode(http.StatusBadRequest, "unsupported locale", apierr.CodePreferencesInvalidLocale))
		return
	}

	if h.PreferencesStore == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}

	pref, err := h.PreferencesStore.Upsert(c.Request.Context(), uid, body.Locale)
	if err != nil {
		if errors.Is(err, postgres.ErrInvalidLocale) {
			respondErr(c, clientErrWithCode(http.StatusBadRequest, "invalid locale", apierr.CodePreferencesInvalidLocale))
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"locale":               pref.Locale,
		"has_saved_preference": true,
	})
}
