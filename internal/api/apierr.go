package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ClientError is the only error type whose message is ever written to an HTTP
// response body. All handler error paths must go through respondErr — raw
// err.Error() must never reach a gin.H{"error": ...} response directly.
//
// See ADR-2, Batch 2: "typed-error matching, not status-code cutoff".
type ClientError struct {
	HTTPStatus int
	Message    string
	Code       apierr.Code
}

func (e *ClientError) Error() string { return e.Message }

// clientErr constructs a ClientError for explicit 4xx business-logic responses.
// Use this whenever a service call fails with a known, user-safe message.
func clientErr(status int, msg string) *ClientError {
	return &ClientError{HTTPStatus: status, Message: msg}
}

// clientErrWithCode constructs a ClientError with an additive machine-readable error code.
func clientErrWithCode(status int, msg string, code apierr.Code) *ClientError {
	return &ClientError{HTTPStatus: status, Message: msg, Code: code}
}

// respondErr is the single function all handlers must use to write error
// responses. It guarantees:
//   - Only explicitly-typed ClientErrors reach the client response body.
//   - Everything else produces {"error": "internal error"} with a server-side log.
//   - Raw err.Error() from DB drivers, pgx, or internal services never leaks.
func typedClientErr(status int, err error, known ...error) error {
	for _, k := range known {
		if errors.Is(err, k) {
			return clientErr(status, err.Error())
		}
	}
	return err
}

func paymentClientErr(err error) error {
	switch {
	case errors.Is(err, payment.ErrDuplicateTxn):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.CodePaymentDuplicateTxn)
	case errors.Is(err, payment.ErrCashPartialNotAllowed):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentCashPartialNotAllowed)
	case errors.Is(err, payment.ErrDueNotOpen):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentDueNotOpen)
	case errors.Is(err, payment.ErrNoDepositDue):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentNoDepositDue)
	case errors.Is(err, payment.ErrEmptyTxnID):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentEmptyTxnId)
	case errors.Is(err, payment.ErrAmbiguous):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentAmbiguousMatch)
	case errors.Is(err, payment.ErrNoMatch):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentNoMatch)
	case errors.Is(err, payment.ErrInvalidAmount), errors.Is(err, postgres.ErrInvalidRefundAmount):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentInvalidAmount)
	case errors.Is(err, postgres.ErrDepositAlreadySettled):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.CodePaymentDuplicateTxn)
	case errors.Is(err, postgres.ErrDueNotPaidDeposit):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodePaymentDueNotOpen)
	default:
		return err
	}
}

func financeClientErr(err error) error {
	switch {
	case errors.Is(err, finance.ErrDuplicateIdempotency):
		return clientErrWithCode(http.StatusConflict, "duplicate request", apierr.CodeFinanceDuplicateRequest)
	case errors.Is(err, finance.ErrIdempotencyRequired):
		return clientErrWithCode(http.StatusBadRequest, "Idempotency-Key required", apierr.CodeFinanceIdempotencyRequired)
	case errors.Is(err, finance.ErrInvalidAmount):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinanceInvalidAmount)
	case errors.Is(err, finance.ErrInvalidKind):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinanceInvalidKind)
	case errors.Is(err, finance.ErrOverpay):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinanceOverpay)
	case errors.Is(err, finance.ErrExpenseNotPayable):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinanceExpenseNotPayable)
	case errors.Is(err, finance.ErrPolicyExceeded):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinancePolicyExceeded)
	case errors.Is(err, finance.ErrApprovalRequired):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinanceApprovalRequired)
	case errors.Is(err, finance.ErrPeriodNotCloseable):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinancePeriodNotCloseable)
	case errors.Is(err, finance.ErrPeriodNotReopenable):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinancePeriodNotReopenable)
	case errors.Is(err, finance.ErrPeriodClosed):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.CodeFinancePeriodClosed)
	case errors.Is(err, finance.ErrNotFound):
		return clientErrWithCode(http.StatusNotFound, "not found", apierr.CodeFinanceNotFound)
	case errors.Is(err, finance.ErrForbidden):
		return clientErrWithCode(http.StatusForbidden, "forbidden", apierr.CodeFinanceForbidden)
	case errors.Is(err, finance.ErrExpenseNotVoidable):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.CodeFinanceExpenseNotVoidable)
	case errors.Is(err, finance.ErrExpenseStateChanged):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.CodeFinanceExpenseStateChanged)
	case errors.Is(err, finance.ErrReasonRequired):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinanceReasonRequired)
	case errors.Is(err, finance.ErrDateOutOfRange):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.CodeFinanceDateOutOfRange)
	case errors.Is(err, finance.ErrDisabled):
		return clientErrWithCode(http.StatusServiceUnavailable, "finance disabled", apierr.CodeFinanceDisabled)
	default:
		return err
	}
}

func respondErr(c *gin.Context, err error) {
	var ce *ClientError
	if errors.As(err, &ce) {
		apierr.RespondClientErr(c, ce.HTTPStatus, ce.Message, ce.Code)
		return
	}
	slog.Error("unhandled internal error", "path", c.FullPath(), "method", c.Request.Method, "err", err)
	c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
}

// gamificationClientErr wraps a gamification service error as a ClientError.
// Gamification errors are inline errors.New() strings (not package-level vars)
// so errors.Is() cannot match them. They are authored as user-facing constraint
// messages and are safe to expose directly.
func gamificationClientErr(status int, err error) *ClientError {
	return &ClientError{HTTPStatus: status, Message: err.Error()}
}

// ParseUUIDParam parses a named Gin URL parameter as a UUID.
// On failure it responds 400 with code request.invalidId and returns uuid.Nil, false.
// The caller must return immediately when ok is false.
func ParseUUIDParam(c *gin.Context, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid id", apierr.CodeRequestInvalidId)
		return uuid.UUID{}, false
	}
	return id, true
}
