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

var validThemes = map[string]bool{"system": true, "light": true, "dark": true}

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
		resolved := localization.FromGinContext(c)
		c.JSON(http.StatusOK, gin.H{
			"locale":               resolved,
			"has_saved_preference": false,
			"theme":                nil,
			"has_saved_theme":      false,
		})
		return
	}
	pref, err := h.PreferencesStore.GetByUserID(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			resolved := localization.FromGinContext(c)
			c.JSON(http.StatusOK, gin.H{
				"locale":               resolved,
				"has_saved_preference": false,
				"theme":                nil,
				"has_saved_theme":      false,
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
		"theme":                pref.Theme,
		"has_saved_theme":      pref.Theme != nil,
	})
}

type patchPreferencesBody struct {
	Locale *string `json:"locale"`
	Theme  *string `json:"theme"`
}

// PatchPreferences handles PATCH /api/me/preferences — partially updates the
// authenticated user's locale and/or theme. Omitted fields are left unchanged.
func (h *Handlers) PatchPreferences(c *gin.Context) {
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	var body patchPreferencesBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid request body", apierr.CodePreferencesNoFields)
		return
	}

	if body.Locale == nil && body.Theme == nil {
		respondErr(c, clientErrWithCode(http.StatusBadRequest, "at least one of locale or theme is required", apierr.CodePreferencesNoFields))
		return
	}
	if body.Locale != nil && !localization.IsValid(*body.Locale) {
		respondErr(c, clientErrWithCode(http.StatusBadRequest, "unsupported locale", apierr.CodePreferencesInvalidLocale))
		return
	}
	if body.Theme != nil && !validThemes[*body.Theme] {
		respondErr(c, clientErrWithCode(http.StatusBadRequest, "invalid theme", apierr.CodePreferencesInvalidTheme))
		return
	}

	if h.PreferencesStore == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}

	pref, err := h.PreferencesStore.Upsert(c.Request.Context(), uid, body.Locale, body.Theme)
	if err != nil {
		if errors.Is(err, postgres.ErrInvalidLocale) {
			respondErr(c, clientErrWithCode(http.StatusBadRequest, "invalid locale", apierr.CodePreferencesInvalidLocale))
			return
		}
		if errors.Is(err, postgres.ErrInvalidTheme) {
			respondErr(c, clientErrWithCode(http.StatusBadRequest, "invalid theme", apierr.CodePreferencesInvalidTheme))
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"locale":               pref.Locale,
		"has_saved_preference": true,
		"theme":                pref.Theme,
		"has_saved_theme":      pref.Theme != nil,
	})
}
