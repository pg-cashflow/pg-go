package qr

import (
	"errors"
	"fmt"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// ErrConflict signals the generated due code is already taken; GenerateDueCode will retry.
var ErrConflict = errors.New("qr: due code conflict")

const maxDueCodeAttempts = 16

// GenerateDueCode wraps domain.GenerateDueCode and retries when try returns ErrConflict.
// try should reserve/insert the code and return ErrConflict on UNIQUE violation.
func GenerateDueCode(try func(code string) error) (string, error) {
	if try == nil {
		return "", fmt.Errorf("qr: try callback is required")
	}
	for i := 0; i < maxDueCodeAttempts; i++ {
		code, err := domain.GenerateDueCode()
		if err != nil {
			return "", err
		}
		err = try(code)
		if err == nil {
			return code, nil
		}
		if errors.Is(err, ErrConflict) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("qr: exhausted due code generation after %d attempts", maxDueCodeAttempts)
}
