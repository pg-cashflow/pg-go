package api

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ctxKey string

const (
	RequestIDHeader          = "X-Request-ID"
	CorrelationIDHeader      = "X-Correlation-ID"
	requestIDCtxKey   ctxKey = "request_id"
)

// RequestIDMiddleware injects or propagates a unique correlation ID for every request.
// It checks incoming headers X-Request-ID and X-Correlation-ID, generates a UUID if absent,
// sets the response header, and populates both the gin context and the Go standard context.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := strings.TrimSpace(c.GetHeader(RequestIDHeader))
		if reqID == "" {
			reqID = strings.TrimSpace(c.GetHeader(CorrelationIDHeader))
		}
		if reqID == "" {
			reqID = uuid.New().String()
		}

		c.Header(RequestIDHeader, reqID)
		c.Set(string(requestIDCtxKey), reqID)

		ctx := context.WithValue(c.Request.Context(), requestIDCtxKey, reqID)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}

// RequestIDFromContext extracts the request correlation ID from standard context.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDCtxKey).(string); ok {
		return v
	}
	return ""
}
