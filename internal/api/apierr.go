package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// ClientError is the only error type whose message is ever written to an HTTP
// response body. All handler error paths must go through respondErr — raw
// err.Error() must never reach a gin.H{"error": ...} response directly.
//
// See ADR-2, Batch 2: "typed-error matching, not status-code cutoff".
type ClientError struct {
	HTTPStatus int
	Message    string
}

func (e *ClientError) Error() string { return e.Message }

// clientErr constructs a ClientError for explicit 4xx business-logic responses.
// Use this whenever a service call fails with a known, user-safe message.
func clientErr(status int, msg string) *ClientError {
	return &ClientError{HTTPStatus: status, Message: msg}
}

// respondErr is the single function all handlers must use to write error
// responses. It guarantees:
//   - Only explicitly-typed ClientErrors reach the client response body.
//   - Everything else produces {"error": "internal error"} with a server-side log.
//   - Raw err.Error() from DB drivers, pgx, or internal services never leaks.
func respondErr(c *gin.Context, err error) {
	var ce *ClientError
	if errors.As(err, &ce) {
		c.JSON(ce.HTTPStatus, gin.H{"error": ce.Message})
		return
	}
	slog.Error("unhandled internal error", "path", c.FullPath(), "method", c.Request.Method, "err", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

// gamificationClientErr wraps a gamification service error as a ClientError.
// Gamification errors are inline errors.New() strings (not package-level vars)
// so errors.Is() cannot match them. They are authored as user-facing constraint
// messages and are safe to expose directly.
//
// ASSUMPTION: gamification service methods never wrap raw DB/pgx errors in
// their returned errors. If that changes, this helper must be updated to use
// an allowlist instead of pass-through.
func gamificationClientErr(status int, err error) *ClientError {
	return &ClientError{HTTPStatus: status, Message: err.Error()}
}
