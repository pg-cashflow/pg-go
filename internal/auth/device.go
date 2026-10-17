package auth

import "github.com/pg-cashflow/pg-go/internal/domain"

// ParseDeviceLabel aliases domain.ParseDeviceLabel.
func ParseDeviceLabel(ua string) string {
	return domain.ParseDeviceLabel(ua)
}
