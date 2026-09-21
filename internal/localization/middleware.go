package localization

import (
	"context"

	"github.com/gin-gonic/gin"
)

type contextKey string

const (
	ContextLocaleKey    contextKey = "locale"
	GinContextLocaleKey            = "locale"
)

// Middleware attaches the resolved request locale to gin.Context and request context.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		locale := ResolveLocale(nil, c.Request)
		c.Set(GinContextLocaleKey, locale)
		ctx := context.WithValue(c.Request.Context(), ContextLocaleKey, locale)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// FromContext extracts the effective locale from context, or returns DefaultLocale.
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultLocale
	}
	if v, ok := ctx.Value(ContextLocaleKey).(string); ok && v != "" {
		return v
	}
	return DefaultLocale
}

// FromGinContext extracts the effective locale from gin.Context, or returns DefaultLocale.
func FromGinContext(c *gin.Context) string {
	if c == nil {
		return DefaultLocale
	}
	if v, exists := c.Get(GinContextLocaleKey); exists {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return DefaultLocale
}
