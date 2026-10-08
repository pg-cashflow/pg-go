package config

import (
	"fmt"
	"os"
	"strings"
)

// ValidateForRealDeployment enforces security invariants that must hold
// whenever real credentials are present — regardless of APP_ENV.
//
// Rationale (ADR-2, Batch 1): checks keyed on APP_ENV == "production" create
// a class of bugs where staging/dev servers with real credentials bypass guards.
// The correct invariant is "real credentials present", not "env label says production".
func (c *Config) ValidateForRealDeployment() error {
	var errs []string

	// --- HMAC/JWT secret quality ---
	type secretCheck struct {
		name string
		val  string
	}
	secrets := []secretCheck{
		{"JWT_SECRET", c.JWTSecret},
		{"OTP_HMAC_SECRET", c.OTPHMACSecret},
		{"MAGIC_LINK_HMAC_SECRET", c.MagicLinkHMACSecret},
	}
	if strings.TrimSpace(c.KYCIdentitySecret) != "" {
		secrets = append(secrets, secretCheck{"KYC_IDENTITY_SECRET", c.KYCIdentitySecret})
	}
	if strings.TrimSpace(c.PayoutEncryptionSecret) != "" {
		secrets = append(secrets, secretCheck{"PAYOUT_ENCRYPTION_SECRET", c.PayoutEncryptionSecret})
	}
	for _, s := range secrets {
		if strings.HasPrefix(s.val, "change-me") {
			errs = append(errs, fmt.Sprintf("%s must not be a placeholder value (starts with 'change-me')", s.name))
		} else if len(s.val) < 32 {
			errs = append(errs, fmt.Sprintf("%s must be at least 32 characters (got %d)", s.name, len(s.val)))
		}
	}

	// --- Cashfree PG and KYC key pairs must be fully provided if either is set ---
	pgAppID := strings.TrimSpace(c.CashfreePGAppID)
	if pgAppID == "" {
		pgAppID = strings.TrimSpace(c.CashfreeAppID)
	}
	pgSecret := strings.TrimSpace(c.CashfreePGSecretKey)
	if pgSecret == "" {
		pgSecret = strings.TrimSpace(c.CashfreeSecretKey)
	}
	if (pgAppID != "" && pgSecret == "") || (pgAppID == "" && pgSecret != "") {
		errs = append(errs, "both CASHFREE_PG_APP_ID and CASHFREE_PG_SECRET_KEY must be provided")
	}
	kycAppID := strings.TrimSpace(c.CashfreeKYCAppID)
	kycSecret := strings.TrimSpace(c.CashfreeKYCSecretKey)
	if (kycAppID != "" && kycSecret == "") || (kycAppID == "" && kycSecret != "") {
		errs = append(errs, "both CASHFREE_KYC_APP_ID and CASHFREE_KYC_SECRET_KEY must be provided")
	}

	// --- Cashfree Payout key pairs, fundsource, and webhook secret ---
	payoutClientID := strings.TrimSpace(c.CashfreePayoutClientID)
	payoutClientSecret := strings.TrimSpace(c.CashfreePayoutClientSecret)
	if (payoutClientID != "" && payoutClientSecret == "") || (payoutClientID == "" && payoutClientSecret != "") {
		errs = append(errs, "both CF_PAYOUT_CLIENT_ID and CF_PAYOUT_CLIENT_SECRET must be provided")
	}
	if payoutClientID != "" {
		if strings.TrimSpace(c.CashfreePayoutWebhookSecret) == "" {
			errs = append(errs, "CF_PAYOUT_WEBHOOK_SECRET is required whenever CF_PAYOUT_CLIENT_ID is set (any environment)")
		} else if strings.HasPrefix(c.CashfreePayoutWebhookSecret, "change-me") {
			errs = append(errs, "CF_PAYOUT_WEBHOOK_SECRET must not be a placeholder value (starts with 'change-me')")
		}
		if strings.TrimSpace(c.CashfreePayoutFundsourceID) == "" {
			errs = append(errs, "CF_PAYOUT_FUNDSOURCE_ID is required whenever CF_PAYOUT_CLIENT_ID is set (Cashfree Transfers V2 requires debit fund source)")
		}
	}

	// --- Cashfree webhook secret required whenever Cashfree keys are set ---
	cashfreeEnabled := pgAppID != "" || pgSecret != "" || payoutClientID != ""
	if (pgAppID != "" || pgSecret != "") && strings.TrimSpace(c.CashfreeWebhookSecret) == "" {
		errs = append(errs, "CASHFREE_WEBHOOK_SECRET is required whenever CASHFREE_APP_ID or CASHFREE_SECRET_KEY is set (any environment)")
	}

	// --- Admin email required for critical operator alerts (outbox dead-letters, financial desynchronization) ---
	if (c.AppEnv == "production" || cashfreeEnabled) && strings.TrimSpace(c.AdminEmail) == "" {
		errs = append(errs, "ADMIN_EMAIL (or SMTP_FROM) is required for critical operator alerts when in production or when payment gateways are configured")
	}

	// --- AUTO_MIGRATE safety ---
	if (os.Getenv("AUTO_MIGRATE") == "1" || os.Getenv("AUTO_MIGRATE") == "true") && c.AppEnv == "production" {
		errs = append(errs, "AUTO_MIGRATE=1 is disallowed in production (APP_ENV=production); use cmd/migrate instead")
	}

	if len(errs) > 0 {
		return fmt.Errorf("startup validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	// --- Non-fatal operational warnings (logged by caller) ---
	// L1: CASHFREE_WEBHOOK_SECRET fallback
	// L3: sandbox mode with live keys
	// These are returned via WarnStrings() for the caller to log.
	return nil
}

// ValidateWarnings returns non-fatal startup warnings (config smells that are
// not errors but indicate potential misconfiguration).
func (c *Config) ValidateWarnings() []string {
	var warns []string

	cashfreeEnabled := strings.TrimSpace(c.CashfreeAppID) != "" || strings.TrimSpace(c.CashfreeSecretKey) != ""

	// L1: webhook secret fell back to API secret key
	if cashfreeEnabled && os.Getenv("CASHFREE_WEBHOOK_SECRET") == "" && c.CashfreeWebhookSecret != "" {
		warns = append(warns, "CASHFREE_WEBHOOK_SECRET not set; falling back to CASHFREE_SECRET_KEY — update if Cashfree webhook and API secrets differ")
	}

	// L3: sandbox mode with Cashfree keys active
	if cashfreeEnabled && !strings.EqualFold(c.CashfreeEnv, "production") {
		warns = append(warns, "Cashfree is in sandbox mode (CASHFREE_ENV != production) — payments are not real")
	}

	// KYC enabled without Cashfree (DigiLocker unavailable, QR-only mode)
	if strings.TrimSpace(c.KYCIdentitySecret) != "" && !cashfreeEnabled {
		warns = append(warns, "KYC_IDENTITY_SECRET is set but Cashfree is not configured — KYC is active in QR-only mode; DigiLocker initiation will return 503")
	}

	// Admin email unset warning in non-production
	if strings.TrimSpace(c.AdminEmail) == "" {
		warns = append(warns, "ADMIN_EMAIL is not configured — outbox dead-letter alerts and operator failure notices will only be logged locally")
	}

	return warns
}
