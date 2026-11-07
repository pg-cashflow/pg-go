package cashfree

import (
	"errors"
	"net/http"
	"strings"
)

// IsRefundDuplicateError reports whether a gateway error represents an already-existing refund
// (such as 409 Conflict, or 400 Bad Request with an already-exists message body).
func IsRefundDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusConflict {
			return true
		}
		bodyLower := strings.ToLower(apiErr.Body)
		if strings.Contains(bodyLower, "already_exists") || strings.Contains(bodyLower, "already exists") {
			return true
		}
	}
	errStrLower := strings.ToLower(err.Error())
	return strings.Contains(errStrLower, "already_exists") || strings.Contains(errStrLower, "already exists")
}
