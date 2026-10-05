package apierr

import "github.com/gin-gonic/gin"

// Code represents a machine-readable, domain-qualified error identifier.
type Code string

const (
	// Auth
	CodeAuthMissingToken          Code = "auth.missingToken"         // #nosec G101
	CodeAuthInvalidToken          Code = "auth.invalidToken"         // #nosec G101
	CodeAuthAccessRevoked         Code = "auth.accessRevoked"
	CodeAuthProfileIncomplete     Code = "auth.profileIncomplete"
	CodeAuthForbidden             Code = "auth.forbidden"
	CodeAuthAlreadyActivated      Code = "auth.alreadyActivated"
	CodeAuthInvalidOtp            Code = "auth.invalidOtp"
	CodeAuthOtpExpired            Code = "auth.otpExpired"
	CodeAuthOtpLocked             Code = "auth.otpLocked"
	CodeAuthRateLimited           Code = "auth.rateLimited"
	CodeAuthNoAccount             Code = "auth.noAccount"
	CodeAuthInvalidInvite         Code = "auth.invalidInvite"
	CodeAuthFirebaseNotConfigured Code = "auth.firebaseNotConfigured"
	CodeAuthEmailNotVerified      Code = "auth.emailNotVerified"
	CodeAuthInvalidFirebaseToken  Code = "auth.invalidFirebaseToken" // #nosec G101
	CodeAuthUnauthorized          Code = "auth.unauthorized"
	CodeAuthNoPropertyScope       Code = "auth.noPropertyScope"

	// Join
	CodeJoinInvalidInvite         Code = "join.invalidInvite"
	CodeJoinNoPendingRequest      Code = "join.noPendingRequest"
	CodeJoinNotFound              Code = "join.notFound"
	CodeJoinAlreadyOnboarded      Code = "join.alreadyOnboarded"
	CodeJoinAlreadyActive         Code = "join.alreadyActive"
	CodeJoinNameRequired          Code = "join.nameRequired"
	CodeJoinConsentRequired       Code = "join.consentRequired"
	CodeJoinPhotoRequired         Code = "join.photoRequired"
	CodeJoinProfileIncomplete     Code = "join.profileIncomplete"
	CodeJoinRequestNotPending     Code = "join.requestNotPending"
	CodeJoinNotAwaitingAssignment Code = "join.notAwaitingAssignment"

	// Payment
	CodePaymentDuplicateTxn          Code = "payment.duplicateTxn"
	CodePaymentCashPartialNotAllowed Code = "payment.cashPartialNotAllowed"
	CodePaymentDueNotOpen            Code = "payment.dueNotOpen"
	CodePaymentNoDepositDue          Code = "payment.noDepositDue"
	CodePaymentEmptyTxnId            Code = "payment.emptyTxnId"
	CodePaymentAmbiguousMatch        Code = "payment.ambiguousMatch"
	CodePaymentNoMatch               Code = "payment.noMatch"
	CodePaymentInvalidAmount         Code = "payment.invalidAmount"

	// Finance
	CodeFinanceDuplicateRequest   Code = "finance.duplicateRequest"
	CodeFinanceIdempotencyRequired Code = "finance.idempotencyRequired"
	CodeFinanceInvalidAmount      Code = "finance.invalidAmount"
	CodeFinanceInvalidKind        Code = "finance.invalidKind"
	CodeFinanceOverpay            Code = "finance.overpay"
	CodeFinanceExpenseNotPayable  Code = "finance.expenseNotPayable"
	CodeFinancePolicyExceeded     Code = "finance.policyExceeded"
	CodeFinanceApprovalRequired   Code = "finance.approvalRequired"
	CodeFinancePeriodNotCloseable Code = "finance.periodNotCloseable"
	CodeFinancePeriodNotReopenable Code = "finance.periodNotReopenable"
	CodeFinancePeriodClosed       Code = "finance.periodClosed"
	CodeFinanceNotFound           Code = "finance.notFound"
	CodeFinanceForbidden          Code = "finance.forbidden"
	CodeFinanceDisabled           Code = "finance.disabled"

	// Request & Bind
	CodeRequestInvalidBody       Code = "request.invalidBody"
	CodeRequestInvalidId         Code = "request.invalidId"
	CodeRequestDueDayInvalid     Code = "request.dueDayInvalid"
	CodeRequestImageTooLarge     Code = "request.imageTooLarge"
	CodeRequestImageReadFailed   Code = "request.imageReadFailed"

	// Preferences
	CodePreferencesLocaleRequired Code = "preferences.localeRequired"
	CodePreferencesInvalidLocale  Code = "preferences.invalidLocale"
)

// AllCodes is the canonical slice of every error code.
var AllCodes = []Code{
	CodeAuthMissingToken,
	CodeAuthInvalidToken,
	CodeAuthAccessRevoked,
	CodeAuthProfileIncomplete,
	CodeAuthForbidden,
	CodeAuthAlreadyActivated,
	CodeAuthInvalidOtp,
	CodeAuthOtpExpired,
	CodeAuthOtpLocked,
	CodeAuthRateLimited,
	CodeAuthNoAccount,
	CodeAuthInvalidInvite,
	CodeAuthFirebaseNotConfigured,
	CodeAuthEmailNotVerified,
	CodeAuthInvalidFirebaseToken,
	CodeAuthUnauthorized,
	CodeAuthNoPropertyScope,

	CodeJoinInvalidInvite,
	CodeJoinNoPendingRequest,
	CodeJoinNotFound,
	CodeJoinAlreadyOnboarded,
	CodeJoinAlreadyActive,
	CodeJoinNameRequired,
	CodeJoinConsentRequired,
	CodeJoinPhotoRequired,
	CodeJoinProfileIncomplete,
	CodeJoinRequestNotPending,
	CodeJoinNotAwaitingAssignment,

	CodePaymentDuplicateTxn,
	CodePaymentCashPartialNotAllowed,
	CodePaymentDueNotOpen,
	CodePaymentNoDepositDue,
	CodePaymentEmptyTxnId,
	CodePaymentAmbiguousMatch,
	CodePaymentNoMatch,
	CodePaymentInvalidAmount,

	CodeFinanceDuplicateRequest,
	CodeFinanceIdempotencyRequired,
	CodeFinanceInvalidAmount,
	CodeFinanceInvalidKind,
	CodeFinanceOverpay,
	CodeFinanceExpenseNotPayable,
	CodeFinancePolicyExceeded,
	CodeFinanceApprovalRequired,
	CodeFinancePeriodNotCloseable,
	CodeFinancePeriodNotReopenable,
	CodeFinancePeriodClosed,
	CodeFinanceNotFound,
	CodeFinanceForbidden,
	CodeFinanceDisabled,

	CodeRequestInvalidBody,
	CodeRequestInvalidId,
	CodeRequestDueDayInvalid,
	CodeRequestImageTooLarge,
	CodeRequestImageReadFailed,

	CodePreferencesLocaleRequired,
	CodePreferencesInvalidLocale,
}

// ErrorEnvelope is the standard JSON envelope emitted for client-facing errors.
type ErrorEnvelope struct {
	Error string `json:"error"`
	Code  Code   `json:"code,omitempty"`
}

// Abort aborts the Gin context with the given status and writes an ErrorEnvelope.
func Abort(c *gin.Context, status int, msg string, code Code) {
	c.AbortWithStatusJSON(status, ErrorEnvelope{
		Error: msg,
		Code:  code,
	})
}

// RespondBindErr responds with HTTP 400 Bad Request and an ErrorEnvelope.
func RespondBindErr(c *gin.Context, msg string, code Code) {
	c.JSON(400, ErrorEnvelope{
		Error: msg,
		Code:  code,
	})
}

// RespondClientErr responds with the specified status and an ErrorEnvelope.
func RespondClientErr(c *gin.Context, status int, msg string, code Code) {
	c.JSON(status, ErrorEnvelope{
		Error: msg,
		Code:  code,
	})
}
